package winfsp

import (
	"io/fs"
	"os"

	winfsp "github.com/aegistudio/go-winfsp"
	"github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual"
	"github.com/buildbarn/bb-storage/pkg/util"
	"golang.org/x/sys/windows"
)

type winfspFileSystem struct {
	// labelLen int
	// label    [32]uint16
}

func NewWinFSPFileSystem(rootDirectory virtual.Directory, mountpoint string) (*winfsp.FileSystem, error) {
	// Print a logging message indicating the filesystem is being mounted
	// println("Mounting WinFSP file system at:", mountpoint)
	winfsp, err := winfsp.Mount(&winfspFileSystem{}, mountpoint, winfsp.FileSystemName("bb_worker"),
		winfsp.CaseSensitive(false), winfsp.PassPattern(true))
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to mount WinFSP file system")
	}
	return winfsp, nil
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

func (winfsp *winfspFileSystem) Open(fileSystemRef *winfsp.FileSystemRef, name string,
	createOptions, grantedAccess uint32,
	info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	print("Opening file:", name, "with create options:", createOptions, "and granted access:", grantedAccess)
	return 0, fs.ErrNotExist
}

func (winfsp *winfspFileSystem) Close(fileSystemRef *winfsp.FileSystemRef, file uintptr) {
	println("Closing file with handle:", file)
}

func (winfsp *winfspFileSystem) Stat(
	name string,
) (os.FileInfo, error) {
	print("Stat file:", name)
	return nil, fs.ErrNotExist
}

func (winfsp *winfspFileSystem) GetOrNewDirBuffer(
	fileSystemRef *winfsp.FileSystemRef, file uintptr,
) (*winfsp.DirBuffer, error) {
	println("GetOrNewDirBuffer called for file handle:", file)
	return nil, fs.ErrNotExist
}

func (winfsp *winfspFileSystem) ReadDirectory(
	fileSystemRef *winfsp.FileSystemRef, file uintptr, pattern string,
	fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error),
) error {
	println("ReadDirectory called for file handle:", file, "pattern:", pattern)
	return fs.ErrNotExist

}

func (winfsp *winfspFileSystem) GetDirInfoByName(
	fileSystemRef *winfsp.FileSystemRef, parentDirFile uintptr,
	name string, dirInfo *winfsp.FSP_FSCTL_DIR_INFO,
) error {
	println("GetDirInfoByName called for parentDirFile:", parentDirFile, "name:", name)
	return fs.ErrNotExist
}

func (winfsp *winfspFileSystem) GetFileInfo(
	ref *winfsp.FileSystemRef, file uintptr,
	info *winfsp.FSP_FSCTL_FILE_INFO,
) error {
	println("GetFileInfo called for file handle:", file)
	return fs.ErrNotExist
}

func (winfsp *winfspFileSystem) SetBasicInfo(
	ref *winfsp.FileSystemRef, file uintptr,
	flags winfsp.SetBasicInfoFlags, attribute uint32,
	creationTime, lastAccessTime, lastWriteTime, changeTime uint64,
	info *winfsp.FSP_FSCTL_FILE_INFO,
) error {
	println("SetBasicInfo called for file handle:", file,
		"flags:", flags, "attribute:", attribute,
		"creationTime:", creationTime, "lastAccessTime:", lastAccessTime,
		"lastWriteTime:", lastWriteTime, "changeTime:", changeTime)
	return fs.ErrNotExist

}

func (winfsp *winfspFileSystem) Overwrite(
	ref *winfsp.FileSystemRef, file uintptr,
	attributes uint32, replaceAttributes bool,
	allocationSize uint64,
	info *winfsp.FSP_FSCTL_FILE_INFO,
) error {
	println("Overwrite called for file handle:", file,
		"attributes:", attributes, "replaceAttributes:", replaceAttributes,
		"allocationSize:", allocationSize)
	return fs.ErrNotExist
}

var _ winfsp.BehaviourBase = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourCreate = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourGetFileInfo = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourReadDirectory = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourSetBasicInfo = (*winfspFileSystem)(nil)
var _ winfsp.BehaviourOverwrite = (*winfspFileSystem)(nil)

// var _ winfsp.BehaviourGetSecurityByName = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourGetVolumeInfo = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourSetVolumeLabel = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourSetFileSize = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourRead = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourWrite = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourFlush = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourCanDelete = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourCleanup = (*winfspFileSystem)(nil)
// var _ winfsp.BehaviourRename = (*winfspFileSystem)(nil)
