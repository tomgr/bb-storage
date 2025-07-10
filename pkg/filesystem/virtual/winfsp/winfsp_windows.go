package winfsp

import (
	"context"
	"io/fs"
	"os"
	"strings"

	winfsp "github.com/aegistudio/go-winfsp"
	filetime "github.com/aegistudio/go-winfsp/filetime"
	"github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	path "github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"golang.org/x/sys/windows"
)

func NewWinFSPFileSystem(rootDirectory virtual.Directory, mountpoint string) (*winfsp.FileSystem, error) {
	// Print a logging message indicating the filesystem is being mounted
	// println("Mounting WinFSP file system at:", mountpoint)

	winfsp, err := winfsp.Mount(
		&winfspFileSystem{
			rootDirectory: rootDirectory,
			openFiles:     make(map[uintptr]*openNode),
			nextHandle:    1,
		},
		mountpoint,
		winfsp.FileSystemName("bb_worker"),
		winfsp.CaseSensitive(false),
	)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to mount WinFSP file system")
	}
	return winfsp, nil
}

const (
	// AttributesMaskForWinFSPAttr is the attributes mask to use for
	// VirtualGetAttributes() to populate all relevant fields of
	// fuse.Attr.
	AttributesMaskForWinFSPAttr = virtual.AttributesMaskDeviceNumber |
		virtual.AttributesMaskFileType |
		virtual.AttributesMaskInodeNumber |
		virtual.AttributesMaskLastDataModificationTime |
		virtual.AttributesMaskLinkCount |
		virtual.AttributesMaskOwnerGroupID |
		virtual.AttributesMaskOwnerUserID |
		virtual.AttributesMaskPermissions |
		virtual.AttributesMaskSizeBytes

	// unsupportedCreateOptions are the options that are not
	// supported by the file system driver.
	//
	// There're many of them, but it is good to eliminate
	// behaviours that might violates the intention of the
	// caller processes and maintain the integrity of the
	// inner file system.
	unsupportedCreateOptions = windows.FILE_WRITE_THROUGH |
		windows.FILE_CREATE_TREE_CONNECTION |
		windows.FILE_NO_EA_KNOWLEDGE |
		windows.FILE_OPEN_BY_FILE_ID |
		windows.FILE_RESERVE_OPFILTER |
		windows.FILE_OPEN_REQUIRING_OPLOCK |
		windows.FILE_COMPLETE_IF_OPLOCKED |
		windows.FILE_OPEN_NO_RECALL

	// bothDirectoryFlags are the flags of directory or-ing
	// the non directory flags. If both flags are set, this
	// is obsolutely an invalid flag, you know.
	bothDirectoryFlags = windows.FILE_DIRECTORY_FILE |
		windows.FILE_NON_DIRECTORY_FILE
)

func convertAttributes(attributes *virtual.Attributes, info *winfsp.FSP_FSCTL_FILE_INFO) {
	info.HardLinks = attributes.GetLinkCount()
	info.EaSize = 0
	info.IndexNumber = attributes.GetInodeNumber()
	info.FileAttributes = 0
	info.ReparseTag = 0

	switch attributes.GetFileType() {
	case filesystem.FileTypeRegularFile:
		info.FileAttributes |= windows.FILE_ATTRIBUTE_NORMAL
	case filesystem.FileTypeDirectory:
		info.FileAttributes |= windows.FILE_ATTRIBUTE_DIRECTORY
	case filesystem.FileTypeSymlink:
		info.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
		info.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	default:
	}

	if sizeBytes, ok := attributes.GetSizeBytes(); ok {
		info.FileSize = sizeBytes
		// Set allocation size (rounded up to 4KB boundaries like in go-winfsp)
		info.AllocationSize = ((sizeBytes + 4095) / 4096) * 4096
	}

	if lastWriteTime, present := attributes.GetLastDataModificationTime(); present {
		info.LastWriteTime = filetime.Timestamp(lastWriteTime)
	}
}

type openNode struct {
	node            virtual.DirectoryChild
	handle          uintptr
	directoryBuffer winfsp.DirBuffer
}

type winfspFileSystem struct {
	rootDirectory virtual.Directory
	// all open files, indexed by handle
	openFiles  map[uintptr]*openNode
	nextHandle uintptr
	labelLen   int
	label      [32]uint16
}

func (winfsp *winfspFileSystem) resolve(name string) (*virtual.Directory, *virtual.DirectoryChild, error) {
	var parent *virtual.Directory = nil
	var current virtual.DirectoryChild = virtual.DirectoryChild{}.FromDirectory(winfsp.rootDirectory)
	if name == "" || name == "\\" {
		return nil, &current, nil
	}
	// TODO; neaten
	if name[0] == '\\' {
		name = name[1:]
	}
	// TODO: cache?
	for {
		var component string
		var found bool
		component, name, found = strings.Cut(name, "\\")
		currentDirectory, _ := current.GetPair()
		if currentDirectory == nil {
			return nil, nil, windows.STATUS_NOT_A_DIRECTORY
		}
		parent = &currentDirectory
		var attributes virtual.Attributes
		current, _ = currentDirectory.VirtualLookup(context.TODO(), path.MustNewComponent(component), 0, &attributes)
		if !found {
			return parent, &current, nil
		}
	}
}

func (winfsp *winfspFileSystem) openFile(
	ref *winfsp.FileSystemRef, name string,
	createOptions, grantedAccess uint32, mode os.FileMode,
	info *winfsp.FSP_FSCTL_FILE_INFO,

) (uintptr, error) {

	// Print the directory structure for debugging
	println("Directory structure:")
	printDirectoryContentsRecursive(winfsp.rootDirectory, "")

	if createOptions&unsupportedCreateOptions != 0 {
		return 0, windows.STATUS_INVALID_PARAMETER
	}
	if createOptions&bothDirectoryFlags == bothDirectoryFlags {
		return 0, windows.STATUS_INVALID_PARAMETER
	}

	disposition := (createOptions >> 24) & 0x0ff
	println("Disposition for file:", name, "is", disposition)
	switch disposition {
	case windows.FILE_SUPERSEDE:
		// XXX: FILE_SUPERSEDE means to remove the file on disk
		// and then replace it by our file, we don't support
		// removing file while there's open file handles. But
		// it can still be open when it is the only one to open
		// the specified file.
		// flags |= os.O_CREATE | os.O_TRUNC
		// TODO
		return 0, windows.STATUS_DEVICE_NOT_READY
	case windows.FILE_CREATE:
		// flags |= os.O_CREATE | os.O_EXCL
		// TODO
		return 0, windows.STATUS_DEVICE_NOT_READY
	case windows.FILE_OPEN, windows.FILE_OPEN_IF:
		_, entry, err := winfsp.resolve(name)
		if err != nil {
			return 0, err
		}
		var attributes virtual.Attributes
		entry.GetNode().VirtualGetAttributes(context.TODO(), AttributesMaskForWinFSPAttr, &attributes)
		convertAttributes(&attributes, info)
		// TODO: check if already open
		handle := winfsp.nextHandle
		winfsp.nextHandle++
		winfsp.openFiles[handle] = &openNode{
			node:   *entry,
			handle: handle,
		}
		return handle, nil
	case windows.FILE_OVERWRITE:
		// flags |= os.O_TRUNC
		// TODO
		return 0, windows.STATUS_DEVICE_NOT_READY
	case windows.FILE_OVERWRITE_IF:
		// flags |= os.O_CREATE | os.O_TRUNC
		// TODO
		return 0, windows.STATUS_DEVICE_NOT_READY
	default:
		return 0, windows.STATUS_INVALID_PARAMETER
	}

	println("Failed to open file:", name,
		"with create options:", createOptions, "and granted access:", grantedAccess)
	return 0, windows.STATUS_DEVICE_PROTOCOL_ERROR
}

func (winfsp *winfspFileSystem) Create(
	ref *winfsp.FileSystemRef, name string,
	createOptions, grantedAccess, fileAttributes uint32,
	securityDescriptor *windows.SECURITY_DESCRIPTOR,
	allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO,
) (uintptr, error) {
	println("Creating file:", name, "with create options:", createOptions,
		"granted access:", grantedAccess, "file attributes:", fileAttributes,
		"allocation size:", allocationSize)
	return 0, fs.ErrNotExist
}

func (winfsp *winfspFileSystem) Open(ref *winfsp.FileSystemRef, name string,
	createOptions, grantedAccess uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	println("Opening file:", name, "with create options:", createOptions, "and granted access:", grantedAccess)
	return winfsp.openFile(ref, name, createOptions, grantedAccess, 0, info)
}

func (winfsp *winfspFileSystem) Close(ref *winfsp.FileSystemRef, file uintptr) {
	println("Closing file handle:", file)
	if _, ok := winfsp.openFiles[file]; ok {
		delete(winfsp.openFiles, file)
	}
}

// func (winfsp *winfspFileSystem) Stat(
// 	name string,
// ) (os.FileInfo, error) {
// 	print("Stat file:", name)
// 	return nil, fs.ErrNotExist
// }

func (winfsp *winfspFileSystem) GetOrNewDirBuffer(ref *winfsp.FileSystemRef, file uintptr) (*winfsp.DirBuffer, error) {
	println("GetOrNewDirBuffer called for file handle:", file)
	openNode, ok := winfsp.openFiles[file]
	if !ok {
		return nil, windows.STATUS_INVALID_HANDLE
	}
	return &openNode.directoryBuffer, nil
}

type dirConverter struct {
	fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)
}

// ReportEntry implements virtual.DirectoryEntryReporter.
func (dc *dirConverter) ReportEntry(nextCookie uint64, name path.Component, child virtual.DirectoryChild, attributes *virtual.Attributes) bool {
	var info winfsp.FSP_FSCTL_FILE_INFO
	convertAttributes(attributes, &info)
	shouldContinue, err := dc.fill(name.String(), &info)
	if err != nil {
		return false
	}
	return shouldContinue
}

func (winfsp *winfspFileSystem) ReadDirectory(
	ref *winfsp.FileSystemRef, file uintptr, pattern string,
	fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error),
) error {
	println("ReadDirectory called for file handle:", file, "pattern:", pattern)

	// Get the open node for this file handle
	openNode, ok := winfsp.openFiles[file]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}

	// Get the directory from the open node
	directory, _ := openNode.node.GetPair()
	if directory == nil {
		return windows.STATUS_NOT_A_DIRECTORY
	}

	converter := dirConverter{
		fill: fill,
	}
	status := directory.VirtualReadDir(context.TODO(), 0, AttributesMaskForWinFSPAttr, &converter)
	if status != virtual.StatusOK {
		return fs.ErrNotExist
	}

	return nil
}

// func (winfsp *winfspFileSystem) GetDirInfoByName(
// 	ref *winfsp.FileSystemRef, parentDirFile uintptr,
// 	name string, dirInfo *winfsp.FSP_FSCTL_DIR_INFO,
// ) error {
// 	println("GetDirInfoByName called for parentDirFile:", parentDirFile, "name:", name)
// 	return fs.ErrNotExist
// }

func (winfsp *winfspFileSystem) GetFileInfo(
	ref *winfsp.FileSystemRef, file uintptr,
	info *winfsp.FSP_FSCTL_FILE_INFO,
) error {
	println("GetFileInfo called for file handle:", file)
	openNode, ok := winfsp.openFiles[file]
	if !ok {
		return windows.STATUS_INVALID_HANDLE
	}
	// Get the attributes from the open node's child
	var attributes virtual.Attributes
	openNode.node.GetNode().VirtualGetAttributes(context.TODO(), AttributesMaskForWinFSPAttr, &attributes)
	convertAttributes(&attributes, info)
	return nil
}

func (winfsp *winfspFileSystem) GetVolumeInfo(
	ref *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO,
) error {
	// TODO: support file system remaining size query.
	info.TotalSize = 0 // 8 * 1024 * 1024 * 1024 * 1024 // 8TB
	info.FreeSize = 0  //info.TotalSize
	length := winfsp.labelLen
	info.VolumeLabelLength = 2 * uint16(copy(info.VolumeLabel[:length], winfsp.label[:length]))
	return nil
}

func (winfsp *winfspFileSystem) Overwrite(
	ref *winfsp.FileSystemRef, file uintptr, attributes uint32, replaceAttributes bool, allocationSize uint64,
	info *winfsp.FSP_FSCTL_FILE_INFO,
) error {
	println("Overwrite called for file handle:", file,
		"attributes:", attributes, "replaceAttributes:", replaceAttributes,
		"allocationSize:", allocationSize)
	return fs.ErrNotExist
}

// func (winfsp *winfspFileSystem) SetBasicInfo(
// 	ref *winfsp.FileSystemRef, file uintptr,
// 	flags winfsp.SetBasicInfoFlags, attribute uint32,
// 	creationTime, lastAccessTime, lastWriteTime, changeTime uint64,
// 	info *winfsp.FSP_FSCTL_FILE_INFO,
// ) error {
// 	println("SetBasicInfo called for file handle:", file,
// 		"flags:", flags, "attribute:", attribute,
// 		"creationTime:", creationTime, "lastAccessTime:", lastAccessTime,
// 		"lastWriteTime:", lastWriteTime, "changeTime:", changeTime)
// 	return fs.ErrNotExist

// }

// printDirectoryContentsRecursive recursively prints the contents of a virtual.Directory
func printDirectoryContentsRecursive(directory virtual.Directory, indent string) {
	// Create a custom reporter that prints each entry
	reporter := &directoryPrinter{
		indent: indent,
	}

	// Read the directory contents
	status := directory.VirtualReadDir(context.TODO(), 0, AttributesMaskForWinFSPAttr, reporter)
	if status != virtual.StatusOK {
		println(indent+"Error reading directory:", status)
	}
}

// directoryPrinter implements virtual.DirectoryEntryReporter to print directory entries
type directoryPrinter struct {
	indent string
}

// ReportEntry prints each directory entry and recursively prints subdirectories
func (p *directoryPrinter) ReportEntry(nextCookie uint64, name path.Component, child virtual.DirectoryChild, attributes *virtual.Attributes) bool {
	nameStr := name.String()

	// Get the directory or leaf from the child
	directory, leaf := child.GetPair()

	if directory != nil {
		// It's a directory
		println(p.indent + "[DIR]  " + nameStr)
		// Recursively print the contents of the subdirectory
		printDirectoryContentsRecursive(directory, p.indent+"  ")
	} else if leaf != nil {
		// It's a leaf (file, symlink, etc.)
		fileType := "FILE"
		if attributes != nil {
			switch attributes.GetFileType() {
			case filesystem.FileTypeSymlink:
				fileType = "SYMLINK"
			case filesystem.FileTypeCharacterDevice:
				fileType = "CHAR_DEV"
			case filesystem.FileTypeBlockDevice:
				fileType = "BLOCK_DEV"
			case filesystem.FileTypeFIFO:
				fileType = "FIFO"
			case filesystem.FileTypeSocket:
				fileType = "SOCKET"
			}
		}
		println(p.indent + "[" + fileType + "] " + nameStr)
	} else {
		println(p.indent + "[UNKNOWN] " + nameStr)
	}

	// Continue reading more entries
	return true
}

var _ winfsp.BehaviourBase = (*winfspFileSystem)(nil)

var _ winfsp.BehaviourCreate = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourGetFileInfo = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourGetVolumeInfo = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourOverwrite = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourReadDirectory = (*winfspFileSystem)(nil)

// var _ winfsp.BehaviourSetBasicInfo = (*winfspFileSystem)(nil)

// var _ winfsp.BehaviourGetSecurityByName = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourSetVolumeLabel = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourSetFileSize = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourRead = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourWrite = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourFlush = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourCanDelete = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourCleanup = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourRename = (*winfspFileSystem)(nil)
