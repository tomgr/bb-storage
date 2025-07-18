package winfsp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual"
	"github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual/winfsp/ffi"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	path "github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"golang.org/x/sys/windows"
)

// Some notes on locking.
//
// We're using WinFSP's default concurrency mode, which is
// FSP_FILE_SYSTEM_OPERATION_GUARD_STRATEGY_FINE. This means that WinFSP will
// effectively use a single sync.RWMutex. Operations acquire this as follows:
//   - Writers: SetVolumeLabel, Flush(Volume), Create, Cleanup(Delete),
//     SetInformation(Rename).
//   - Readers: GetVolumeInfo, Open, SetInformation(Disposition),
//     ReadDirectory.
// All other operations do not acquire any locks. However, WinFSP still applies
// a sync.RWLock lock per individual file (inside the WinFSP driver itself), so
// concurrent reads/writes/etc. are still possible, but not in a conflicting
// way on the same file.
//
// On Windows there are quite a few different operations that might conflict.
// For example, you need to prevent deletion of files if they're open, and also
// enforce sharing modes when opening files. Thankfully, WinFSP does all of
// this for us.

func NewWinFSPFileSystem(rootDirectory virtual.Directory, mountpoint string) (*ffi.FileSystem, error) {
	// Set the debug log handle to standard error.
	debugHandle, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err != nil {
		return nil, err
	}

	fileSystem, err := ffi.Mount(
		&winfspFileSystem{
			rootDirectory: rootDirectory,
			openFiles:     newOpenedFilesPool(),
		},
		mountpoint,
		ffi.Attributes(
			ffi.FspFSAttributeUmNoReparsePointsDirCheck,
		),
		ffi.CaseSensitive(true),
		ffi.DebugHandle(debugHandle),
		ffi.FileSystemName("bb_worker"),
	)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to mount WinFSP file system")
	}
	return fileSystem, nil
}

type winfspFileSystem struct {
	rootDirectory virtual.Directory
	openFiles     openedFilesPool

	// This does not need to be locked; WinFSP enforces a read/write lock.
	label []uint16
}

const (
	// attributesMaskForWinFSPAttr is the attributes mask to use for
	// VirtualGetAttributes() to populate all relevant fields of
	// FSP_FSCTL_FILE_INFO.
	attributesMaskForWinFSPAttr = virtual.AttributesMaskDeviceNumber |
		virtual.AttributesMaskFileType |
		virtual.AttributesMaskInodeNumber |
		virtual.AttributesMaskLastDataModificationTime |
		virtual.AttributesMaskLinkCount |
		virtual.AttributesMaskOwnerGroupID |
		virtual.AttributesMaskOwnerUserID |
		virtual.AttributesMaskPermissions |
		virtual.AttributesMaskSizeBytes

		// unsupportedCreateOptions are the options that are not supported.
		// TODO: do we need this? What do these do? Does WinFSP even support them?
	unsupportedCreateOptions = windows.FILE_CREATE_TREE_CONNECTION |
		windows.FILE_NO_EA_KNOWLEDGE |
		windows.FILE_OPEN_BY_FILE_ID |
		windows.FILE_RESERVE_OPFILTER |
		windows.FILE_OPEN_REQUIRING_OPLOCK |
		windows.FILE_COMPLETE_IF_OPLOCKED

	fileAndDirectoryFlag = windows.FILE_DIRECTORY_FILE | windows.FILE_NON_DIRECTORY_FILE

	// Attributes we support being set via SetBasicInfo.
	setBasicInfoSupportedAttributes = windows.FILE_ATTRIBUTE_DIRECTORY |
		windows.FILE_ATTRIBUTE_NORMAL |
		windows.FILE_ATTRIBUTE_READONLY

	// The Unix Epoch as a Windows FILETIME.
	unixEpochAsFiletime = 116444736000000000
)

var (
	// Stores the current processes' uid and gid, calculated once.
	currentUserID  *windows.SID
	currentGroupID *windows.SID
	userInfoError  error
	userInfoOnce   sync.Once
)

func calculateCurrentUserAndGroup() (uid, gid *windows.SID, err error) {
	token := windows.GetCurrentProcessToken()

	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}

	group, err := token.GetTokenPrimaryGroup()
	if err != nil {
		return nil, nil, err
	}

	return user.User.Sid, group.PrimaryGroup, nil
}

func getCurrentProcessUserAndGroup() (uid, gid *windows.SID, err error) {
	userInfoOnce.Do(func() {
		currentUserID, currentGroupID, userInfoError = calculateCurrentUserAndGroup()
	})
	return currentUserID, currentGroupID, userInfoError
}

func toNTStatus(status virtual.Status) windows.NTStatus {
	switch status {
	case virtual.StatusOK:
		return windows.STATUS_SUCCESS
	case virtual.StatusErrAccess:
		return windows.STATUS_ACCESS_DENIED
	case virtual.StatusErrBadHandle:
		return windows.STATUS_INVALID_HANDLE
	case virtual.StatusErrExist:
		return windows.STATUS_OBJECT_NAME_COLLISION
	case virtual.StatusErrInval:
		return windows.STATUS_INVALID_PARAMETER
	case virtual.StatusErrIO:
		return windows.STATUS_UNEXPECTED_IO_ERROR
	case virtual.StatusErrIsDir:
		return windows.STATUS_FILE_IS_A_DIRECTORY
	case virtual.StatusErrNoEnt:
		return windows.STATUS_OBJECT_NAME_NOT_FOUND
	case virtual.StatusErrNotDir:
		return windows.STATUS_NOT_A_DIRECTORY
	case virtual.StatusErrNotEmpty:
		return windows.STATUS_DIRECTORY_NOT_EMPTY
	case virtual.StatusErrNXIO:
		return windows.STATUS_NO_SUCH_DEVICE
	case virtual.StatusErrPerm:
		return windows.STATUS_ACCESS_DENIED
	case virtual.StatusErrROFS:
		return windows.STATUS_MEDIA_WRITE_PROTECTED
	case virtual.StatusErrStale:
		return windows.STATUS_OBJECT_NAME_NOT_FOUND
	case virtual.StatusErrSymlink:
		// TODO: not sure about this error code
		return windows.STATUS_REPARSE
	case virtual.StatusErrWrongType:
		return windows.STATUS_OBJECT_TYPE_MISMATCH
	case virtual.StatusErrXDev:
		return windows.STATUS_NOT_SUPPORTED
	default:
		// For any other status, we return a generic error.
		return windows.STATUS_UNSUCCESSFUL
	}
}

func toShareMask(grantedAccess uint32) virtual.ShareMask {
	var shareMask virtual.ShareMask
	if grantedAccess&windows.FILE_READ_DATA != 0 {
		shareMask |= virtual.ShareMaskRead
	}
	if grantedAccess&(windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA) != 0 {
		shareMask |= virtual.ShareMaskWrite
	}
	return shareMask
}

// Convert FILETIME to a time.Time object.
func filetimeToTime(ft uint64) time.Time {
	nanoseconds := int64(ft-unixEpochAsFiletime) * 100
	return time.Unix(0, nanoseconds).UTC()
}

func toVirtualAttributes(leaf virtual.DirectoryChild, attribute uint32, newAttributes *virtual.Attributes, attributesMask *virtual.AttributesMask) error {
	if attribute != windows.INVALID_FILE_ATTRIBUTES {
		if attribute&^setBasicInfoSupportedAttributes != 0 {
			return windows.STATUS_INVALID_PARAMETER
		}
		if attribute&windows.FILE_ATTRIBUTE_READONLY != 0 {
			permissions := virtual.PermissionsRead
			_, file := leaf.GetPair()
			if file != nil {
				permissions |= virtual.PermissionsExecute
			}
			newAttributes.SetPermissions(permissions)
			*attributesMask |= virtual.AttributesMaskPermissions
		}
		if attribute&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			dir, _ := leaf.GetPair()
			if dir == nil {
				println("NOT A DIRECTORY")
				return windows.STATUS_NOT_A_DIRECTORY
			}
		}
	}
	return nil
}

func toWinFSPFileAttributes(attributes *virtual.Attributes) uint32 {
	var fileAttributes uint32
	switch attributes.GetFileType() {
	case filesystem.FileTypeRegularFile:
		fileAttributes |= windows.FILE_ATTRIBUTE_NORMAL
	case filesystem.FileTypeDirectory:
		fileAttributes |= windows.FILE_ATTRIBUTE_DIRECTORY
	case filesystem.FileTypeSymlink:
		fileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
	default:
	}
	if permissions, ok := attributes.GetPermissions(); ok && permissions&virtual.PermissionsWrite == 0 {
		fileAttributes |= windows.FILE_ATTRIBUTE_READONLY
	}
	return fileAttributes
}

func toWinFSPFileInfo(attributes *virtual.Attributes, info *ffi.FSP_FSCTL_FILE_INFO) {
	info.EaSize = 0
	info.HardLinks = attributes.GetLinkCount()
	info.IndexNumber = attributes.GetInodeNumber()
	info.FileAttributes = toWinFSPFileAttributes(attributes)
	info.ReparseTag = 0

	if attributes.GetFileType() == filesystem.FileTypeSymlink {
		info.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	}

	if sizeBytes, ok := attributes.GetSizeBytes(); ok {
		info.FileSize = sizeBytes
		// Set allocation size (rounded up to 4KB boundaries like in go-winfsp)
		info.AllocationSize = ((sizeBytes + 4095) / 4096) * 4096
	}

	if lastWriteTime, present := attributes.GetLastDataModificationTime(); present {
		info.LastWriteTime = ffi.Timestamp(lastWriteTime)
	}
}

const (
	fileDeleteChild windows.ACCESS_MASK = 0x40
)

func mapPermissionToAccessMask(mode virtual.Permissions) windows.ACCESS_MASK {
	var result windows.ACCESS_MASK
	if mode&virtual.PermissionsRead != 0 {
		result |= windows.FILE_GENERIC_READ
	}
	if mode&virtual.PermissionsWrite != 0 {
		result |= windows.FILE_GENERIC_WRITE
	}
	if mode&virtual.PermissionsExecute != 0 {
		result |= windows.FILE_GENERIC_EXECUTE
	}
	return result
}

// Calculates a security descriptor for the attributes by looking at the
// permissions and owner/group IDs in the attributes.
func toSecurityDescriptor(attributes *virtual.Attributes) (*windows.SECURITY_DESCRIPTOR, error) {
	var err error

	// Compute the owner, falling back to the current processes' user.
	var ownerSid *windows.SID
	if uid, ok := attributes.GetOwnerUserID(); ok {
		ownerSid, err = ffi.PosixMapUidToSid(uid)
		if err != nil {
			return nil, err
		}
	} else {
		ownerSid, _, err = getCurrentProcessUserAndGroup()
		if err != nil {
			return nil, err
		}
	}

	// Get group SID.
	var groupSid *windows.SID
	if gid, ok := attributes.GetOwnerGroupID(); ok {
		groupSid, err = ffi.PosixMapUidToSid(gid)
		if err != nil {
			return nil, err
		}
	} else {
		_, groupSid, err = getCurrentProcessUserAndGroup()
		if err != nil {
			return nil, err
		}
	}

	mode, ok := attributes.GetPermissions()
	if !ok {
		panic("Attributes do not contain mandatory permissions attribute")
	}

	accessPermissions := mapPermissionToAccessMask(mode)
	if mode&virtual.PermissionsWrite != 0 && attributes.GetFileType() == filesystem.FileTypeDirectory {
		accessPermissions |= fileDeleteChild
	}

	// TODO: cache
	everyoneSid, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return nil, err
	}

	var aclEntries []windows.EXPLICIT_ACCESS

	// Since the VFS has the same permissions for user/group/everyone, we only
	// need one ACL entry.
	aclEntries = append(aclEntries, windows.EXPLICIT_ACCESS{
		// AccessPermissions: fspPosixDefaultPerm | mapPermissionToAccessMask(mode),
		AccessPermissions: accessPermissions,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyoneSid),
		},
	})

	// Build the security descriptor.
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	if err = sd.SetOwner(ownerSid, false); err != nil {
		return nil, err
	}
	if err = sd.SetGroup(groupSid, false); err != nil {
		return nil, err
	}
	dacl, err := windows.ACLFromEntries(aclEntries, nil)
	if err != nil {
		return nil, err
	}
	if err = sd.SetDACL(dacl, true, false); err != nil {
		return nil, err
	}

	return sd.ToSelfRelative()
}

// Sets the attributes of node, and stores the updated attributes in info.
func setAndGetAttributes(ctx context.Context, newAttributesMask virtual.AttributesMask, node virtual.Node, newAttributes virtual.Attributes, info *ffi.FSP_FSCTL_FILE_INFO) error {
	var attributesOut virtual.Attributes
	if newAttributesMask != 0 {
		status := node.VirtualSetAttributes(ctx, &newAttributes, attributesMaskForWinFSPAttr, &attributesOut)
		if status != virtual.StatusOK {
			return toNTStatus(status)
		}
	} else {
		node.VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributesOut)
	}
	toWinFSPFileInfo(&attributesOut, info)
	return nil
}

func (fs *winfspFileSystem) resolveDirectory(ctx context.Context, name string) (virtual.Directory, error) {
	var current virtual.Directory = fs.rootDirectory
	if name[0] != '\\' {
		return nil, windows.STATUS_OBJECT_NAME_NOT_FOUND
	}
	name = name[1:]
	for {
		if name == "" {
			return current, nil
		}
		splitAt := strings.IndexByte(name, '\\')
		var component string
		if splitAt == -1 {
			component = name
			name = ""
		} else {
			component = name[:splitAt]
			name = name[splitAt+1:]
		}
		var attributes virtual.Attributes
		child, status := current.VirtualLookup(ctx, path.MustNewComponent(component), virtual.AttributesMaskFileType, &attributes)
		if status != virtual.StatusOK {
			return nil, toNTStatus(status)
		}
		current, _ = child.GetPair()
		if current == nil {
			return nil, windows.STATUS_NOT_A_DIRECTORY
		}
	}
}

func (fs *winfspFileSystem) openOrCreateDir(ctx context.Context, parent virtual.Directory, leafName path.Component, disposition uint32, attributes *virtual.Attributes) (virtual.DirectoryChild, error) {
	switch disposition {
	case windows.FILE_OPEN, windows.FILE_OPEN_IF:
		// Ty and open an existing directory
		child, status := parent.VirtualLookup(ctx, leafName, attributesMaskForWinFSPAttr, attributes)
		if status == virtual.StatusOK {
			if directory, _ := child.GetPair(); directory == nil {
				if attributes.GetFileType() == filesystem.FileTypeSymlink {
					// If it's a symlink then we have no way of checking
					// whether the target is a directory or not, since the
					// symlink might be to a file that is outside of the VFS.
					// We therefore do not enforce that the file is a
					// directory. This is like WinFSP's fuse implementation.
					return child, nil
				}
				return virtual.DirectoryChild{}, windows.STATUS_NOT_A_DIRECTORY
			}
			return child, nil
		}
		// Fallthrough to creation.

	case windows.FILE_CREATE:
		// Fallthrough to creation.

	default:
		return virtual.DirectoryChild{}, windows.STATUS_INVALID_PARAMETER
	}

	// Create new directory.
	childDir, _, status := parent.VirtualMkdir(leafName, attributesMaskForWinFSPAttr, attributes)
	if status != virtual.StatusOK {
		return virtual.DirectoryChild{}, toNTStatus(status)
	}
	return virtual.DirectoryChild{}.FromDirectory(childDir), nil
}

// openOrCreateFileOrDir opens an existing file or directory, or creates a new file.
func (fs *winfspFileSystem) openOrCreateFileOrDir(ctx context.Context, parent virtual.Directory, leafName path.Component, disposition uint32, grantedAccess uint32, createPermissions virtual.Permissions, attributes *virtual.Attributes) (virtual.DirectoryChild, error) {
	var createAttributes *virtual.Attributes
	var existingOptions *virtual.OpenExistingOptions

	switch disposition {
	case windows.FILE_OPEN:
		existingOptions = &virtual.OpenExistingOptions{}
	case windows.FILE_CREATE:
		createAttributes = &virtual.Attributes{}
	case windows.FILE_OPEN_IF:
		createAttributes = &virtual.Attributes{}
		existingOptions = &virtual.OpenExistingOptions{}
	case windows.FILE_OVERWRITE:
		existingOptions = &virtual.OpenExistingOptions{Truncate: true}
	case windows.FILE_OVERWRITE_IF:
		createAttributes = &virtual.Attributes{}
		existingOptions = &virtual.OpenExistingOptions{Truncate: true}
	case windows.FILE_SUPERSEDE:
		// We treat this like a standard truncate, which is not strictly
		// speaking correct: Supersede really means "delete and recreate",
		// which would mean throwing away some attributes. However, it's not
		// uncommon to treat it like O_TRUNC; some SMB implementations do
		// for example.
		createAttributes = &virtual.Attributes{}
		existingOptions = &virtual.OpenExistingOptions{Truncate: true}
	default:
		return virtual.DirectoryChild{}, windows.STATUS_INVALID_PARAMETER
	}

	shareMask := toShareMask(grantedAccess)
	if createAttributes != nil && createPermissions != 0 {
		createAttributes.SetPermissions(createPermissions)
	}
	leaf, _, _, status := parent.VirtualOpenChild(ctx, leafName, shareMask, createAttributes, existingOptions, attributesMaskForWinFSPAttr, attributes)
	println("Open or create file:", leafName.String(), "with status:", status)
	if status == virtual.StatusErrSymlink && existingOptions != nil {
		println("Symlink detected, returning a reference to that instead")
		leaf, status = parent.VirtualLookup(ctx, leafName, attributesMaskForWinFSPAttr, attributes)
	}
	if status != virtual.StatusOK {
		println("Failed to open or create file:", leafName.String(), "with status:", status)
		return virtual.DirectoryChild{}, toNTStatus(status)
	}
	return leaf, nil

}

func (fs *winfspFileSystem) openFile(ctx context.Context, name string, createOptions, grantedAccess uint32, createPermissions virtual.Permissions, info *ffi.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	if createOptions&unsupportedCreateOptions != 0 {
		return 0, windows.STATUS_INVALID_PARAMETER
	}
	if createOptions&fileAndDirectoryFlag == fileAndDirectoryFlag {
		// Can't be both
		return 0, windows.STATUS_INVALID_PARAMETER
	}
	disposition := (createOptions >> 24) & 0x0ff

	var attributes virtual.Attributes
	var parent virtual.Directory
	var node virtual.DirectoryChild
	if name == "\\" {
		// This is the root: this is a bit special
		if createOptions&windows.FILE_NON_DIRECTORY_FILE != 0 {
			return 0, windows.STATUS_FILE_IS_A_DIRECTORY
		}

		switch disposition {
		case windows.FILE_OPEN, windows.FILE_OPEN_IF:
			node = virtual.DirectoryChild{}.FromDirectory(fs.rootDirectory)
			node.GetNode().VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)

		case windows.FILE_CREATE:
			return 0, windows.STATUS_OBJECT_NAME_COLLISION
		default:
			return 0, windows.STATUS_INVALID_PARAMETER
		}
	} else {
		parentName, leafName, err := parsePath(name)
		if err != nil {
			return 0, err
		}
		// lastSeparator := strings.LastIndexByte(name, '\\')
		// parentName := name[:lastSeparator+1]
		// leafName := path.MustNewComponent(name[lastSeparator+1:])
		parent, err = fs.resolveDirectory(ctx, parentName)
		if err != nil {
			println("Failed to resolve parent directory:", parentName, "error:", err)
			return 0, err
		}

		// Handle dispositions through the appropriate function based on whether it's a directory or file
		if createOptions&windows.FILE_DIRECTORY_FILE != 0 {
			println("Looking for a directory")
			node, err = fs.openOrCreateDir(ctx, parent, leafName, disposition, &attributes)
		} else {
			node, err = fs.openOrCreateFileOrDir(ctx, parent, leafName, disposition, grantedAccess, createPermissions, &attributes)
		}
		if err != nil {
			return 0, err
		}
	}

	handle := fs.openFiles.createHandle(parent, node)
	toWinFSPFileInfo(&attributes, info)
	return handle, nil
}

func (fs *winfspFileSystem) createContext() (context.Context, error) {
	// TODO: authenticate
	return context.TODO(), nil
}

func (fs *winfspFileSystem) Create(ref *ffi.FileSystemRef, name string, createOptions, grantedAccess, fileAttributes uint32, securityDescriptor *windows.SECURITY_DESCRIPTOR, allocationSize uint64, info *ffi.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}
	// We need to set the default permissions for this file. We don't have
	// much to set this based on as Windows doesn't have an equivalent of
	// umask; instead in Windows files normally inherit their permissions
	// from the parent directory. However this doesn't work well with the VFS
	// as it stores permissions per node and nodes always have permissions
	// attached.
	createPermissions := virtual.PermissionsExecute | virtual.PermissionsRead
	if fileAttributes&windows.FILE_ATTRIBUTE_READONLY == 0 {
		createPermissions |= virtual.PermissionsWrite
	}
	println("Create called for file:", name, "with createOptions:", createOptions, "and grantedAccess:", grantedAccess)
	return fs.openFile(ctx, name, createOptions, grantedAccess, createPermissions, info)
}

func (fs *winfspFileSystem) Open(ref *ffi.FileSystemRef, name string, createOptions, grantedAccess uint32, info *ffi.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}
	println("Open called for file:", name, "with createOptions:", createOptions, "and grantedAccess:", grantedAccess)
	return fs.openFile(ctx, name, createOptions, grantedAccess, 0, info)
}

func (fs *winfspFileSystem) Close(ref *ffi.FileSystemRef, handle uintptr) {
	fs.openFiles.removeHandle(handle)
}

// Writes directory entries into a buffer in the WinFSP format.
type dirBufferWriter struct {
	buffer       []byte
	cookieOffset uint64
	exhausted    bool
	writtenBytes int
}

// Adds a directory to the buffer, returning false if the buffer has been
// exhausted.
func (dc *dirBufferWriter) append(name string, nextCookie uint64, info *ffi.FSP_FSCTL_FILE_INFO) bool {
	written := ffi.FileSystemAddDirInfo(name, nextCookie, info, dc.buffer[dc.writtenBytes:])
	if written == 0 {
		// The buffer is too small.
		dc.exhausted = true
		return false
	}
	dc.writtenBytes += written
	return true
}

func (dc *dirBufferWriter) ReportEntry(nextCookie uint64, name path.Component, child virtual.DirectoryChild, attributes *virtual.Attributes) bool {
	var info ffi.FSP_FSCTL_FILE_INFO
	toWinFSPFileInfo(attributes, &info)
	return dc.append(name.String(), nextCookie+dc.cookieOffset, &info)
}

func (fs *winfspFileSystem) ReadDirectoryOffset(ref *ffi.FileSystemRef, handle uintptr, pattern *uint16, offset uint64, buffer []byte) (int, error) {
	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}

	directory, err := fs.openFiles.nodeForDirectoryHandle(handle)
	if err != nil {
		return 0, err
	}

	dc := dirBufferWriter{
		buffer: buffer,
	}

	var readDirCookie uint64
	if directory == fs.rootDirectory {
		readDirCookie = offset
	} else {
		// For everything aside from the root we have to include "." and ".."
		// entries. Thus we offset the VFS cookies by 2.
		dc.cookieOffset = 2
		if offset >= dc.cookieOffset {
			readDirCookie = offset - dc.cookieOffset
		}

		if offset <= 0 {
			if !dc.append(".", 1, &ffi.FSP_FSCTL_FILE_INFO{
				FileAttributes: windows.FILE_ATTRIBUTE_DIRECTORY,
			}) {
				return dc.writtenBytes, nil
			}
		}
		if offset <= 1 {
			if !dc.append("..", 2, &ffi.FSP_FSCTL_FILE_INFO{
				FileAttributes: windows.FILE_ATTRIBUTE_DIRECTORY,
			}) {
				return dc.writtenBytes, nil
			}
		}
	}

	status := directory.VirtualReadDir(ctx, readDirCookie, attributesMaskForWinFSPAttr, &dc)
	if status != virtual.StatusOK {
		return 0, toNTStatus(status)
	}
	if !dc.exhausted {
		// Must have reached the end: add the null entry.
		dc.append("", 0, nil)
	}
	return dc.writtenBytes, nil
}

func (fs *winfspFileSystem) GetFileInfo(ref *ffi.FileSystemRef, handle uintptr, info *ffi.FSP_FSCTL_FILE_INFO) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}
	node, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}
	var attributes virtual.Attributes
	node.GetNode().VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)
	toWinFSPFileInfo(&attributes, info)
	return nil
}

func (fs *winfspFileSystem) GetVolumeInfo(ref *ffi.FileSystemRef, info *ffi.FSP_FSCTL_VOLUME_INFO) error {
	// Sizes are 0 as the VFS does not maintain them.
	info.FreeSize = 0
	info.TotalSize = 0
	info.VolumeLabelLength = 2 * uint16(copy(info.VolumeLabel[:], fs.label[:]))
	return nil
}

func (fs *winfspFileSystem) Overwrite(ref *ffi.FileSystemRef, handle uintptr, winfspAttributes uint32, replaceAttributes bool, allocationSize uint64, info *ffi.FSP_FSCTL_FILE_INFO) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	openNode, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}
	_, file := openNode.GetPair()
	if file == nil {
		return windows.STATUS_FILE_IS_A_DIRECTORY
	}

	var newAttributes virtual.Attributes
	var newAttributesMask virtual.AttributesMask
	if !replaceAttributes {
		// Then initialise based on the current attributes.
		file.VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &newAttributes)
	}

	// Add additional attributes
	toVirtualAttributes(openNode, winfspAttributes, &newAttributes, &newAttributesMask)
	newAttributesMask |= virtual.AttributesMaskSizeBytes
	newAttributes.SetSizeBytes(0)

	return setAndGetAttributes(ctx, newAttributesMask, file, newAttributes, info)
}

func (fs *winfspFileSystem) Read(ref *ffi.FileSystemRef, handle uintptr, buffer []byte, offset uint64) (int, error) {
	_, err := fs.createContext()
	if err != nil {
		return 0, err
	}

	file, err := fs.openFiles.nodeForLeafHandle(handle)
	if err != nil {
		return 0, err
	}
	read, eof, status := file.VirtualRead(buffer, offset)
	if status != virtual.StatusOK {
		return read, toNTStatus(status)
	}
	if read == 0 && eof {
		// Must have tried to read at the end of the file
		return 0, windows.STATUS_END_OF_FILE
	}
	return read, nil
}

func (fs *winfspFileSystem) Write(ref *ffi.FileSystemRef, handle uintptr, buf []byte, offset uint64, writeToEndOfFile, constrainedIo bool, info *ffi.FSP_FSCTL_FILE_INFO) (int, error) {
	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}

	file, err := fs.openFiles.nodeForLeafHandle(handle)
	if err != nil {
		return 0, err
	}

	// TODO: race condition here between getting the size and doing the write
	// Handle write to end of file
	if writeToEndOfFile {
		// Get current file size to append at the end
		var attributes virtual.Attributes
		file.VirtualGetAttributes(ctx, virtual.AttributesMaskSizeBytes, &attributes)
		if sizeBytes, ok := attributes.GetSizeBytes(); ok {
			offset = sizeBytes
		}
	}

	written, status := file.VirtualWrite(buf, offset)
	if status != virtual.StatusOK {
		return written, toNTStatus(status)
	}

	// Update file info with new attributes after write
	var attributes virtual.Attributes
	file.VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)
	toWinFSPFileInfo(&attributes, info)

	return written, nil
}

func (fs *winfspFileSystem) Flush(ref *ffi.FileSystemRef, handle uintptr, info *ffi.FSP_FSCTL_FILE_INFO) error {
	// Like the other VFS frontends (e.g. FUSE) we don't actually implement flush.
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	// Check if it's a volume flush (file handle is 0)
	if handle == 0 {
		return nil
	}

	openNode, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}
	var attributes virtual.Attributes
	openNode.GetNode().VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)
	toWinFSPFileInfo(&attributes, info)
	return nil
}

func (fs *winfspFileSystem) SetBasicInfo(ref *ffi.FileSystemRef, handle uintptr, flags ffi.SetBasicInfoFlags, attribute uint32, creationTime, lastAccessTime, lastWriteTime, changeTime uint64, info *ffi.FSP_FSCTL_FILE_INFO) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}
	println("SetBasicInfo")

	openNode, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}
	node := openNode.GetNode()

	var newAttributes virtual.Attributes
	attributesMask := virtual.AttributesMask(0)

	toVirtualAttributes(openNode, attribute, &newAttributes, &attributesMask)

	if flags&ffi.SetBasicInfoLastWriteTime != 0 {
		attributesMask |= virtual.AttributesMaskLastDataModificationTime
		newAttributes.SetLastDataModificationTime(filetimeToTime(lastWriteTime))
	}

	return setAndGetAttributes(ctx, attributesMask, node, newAttributes, info)
}

func (fs *winfspFileSystem) SetVolumeLabel(ref *ffi.FileSystemRef, volumeLabel string, info *ffi.FSP_FSCTL_VOLUME_INFO) error {
	println("SetVolumeLabel")

	utf16, err := windows.UTF16FromString(volumeLabel)
	if err != nil {
		return err
	}
	fs.label = utf16
	return nil
}

func (fs *winfspFileSystem) SetFileSize(ref *ffi.FileSystemRef, handle uintptr, newSize uint64, setAllocationSize bool, info *ffi.FSP_FSCTL_FILE_INFO) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	file, err := fs.openFiles.nodeForLeafHandle(handle)
	if err != nil {
		return err
	}

	var setSize bool
	if setAllocationSize {
		// We need to ensure that file size is less than the allocation size,
		// so might still need to do truncation here even though we don't
		// maintain allocation size in the VFS.
		var attributes virtual.Attributes
		file.VirtualGetAttributes(ctx, virtual.AttributesMaskSizeBytes, &attributes)
		if sizeBytes, ok := attributes.GetSizeBytes(); ok && sizeBytes > newSize {
			// Truncate the file to the new size
			setSize = true
		}
	} else {
		setSize = true
	}

	var newAttributes virtual.Attributes
	var attributesMask virtual.AttributesMask
	if setSize {
		newAttributes.SetSizeBytes(newSize)
		attributesMask |= virtual.AttributesMaskSizeBytes
	}
	return setAndGetAttributes(ctx, attributesMask, file, newAttributes, info)
}

func (fs *winfspFileSystem) CanDelete(ref *ffi.FileSystemRef, handle uintptr, name string) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	openNode, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}

	directory, _ := openNode.GetPair()
	if directory == nil {
		// Leaves can always be deleted.
		return nil
	}

	// For directories, check if they are empty.
	isEmpty := true
	status := directory.VirtualReadDir(ctx, 0, 0, &emptyDirectoryChecker{&isEmpty})
	if status != virtual.StatusOK {
		return toNTStatus(status)
	}
	if !isEmpty {
		return windows.STATUS_DIRECTORY_NOT_EMPTY
	}
	return nil
}

type emptyDirectoryChecker struct {
	isEmpty *bool
}

func (e *emptyDirectoryChecker) ReportEntry(nextCookie uint64, name path.Component, child virtual.DirectoryChild, attributes *virtual.Attributes) bool {
	*e.isEmpty = false
	// Stop iterating.
	return false
}

func (fs *winfspFileSystem) Cleanup(ref *ffi.FileSystemRef, handle uintptr, name string, cleanupFlags uint32) {
	ctx, err := fs.createContext()
	if err != nil {
		return
	}

	println("Cleanup ", name, " with flags:", cleanupFlags)

	openNode, err := fs.openFiles.trackedNodeForHandle(handle)
	if err != nil {
		return
	}

	var attributes virtual.Attributes
	var attributesMask virtual.AttributesMask

	if cleanupFlags&ffi.FspCleanupSetLastWriteTime != 0 {
		attributes.SetLastDataModificationTime(time.Now())
		attributesMask |= virtual.AttributesMaskLastDataModificationTime
	}

	if attributesMask != 0 {
		var outAttributes virtual.Attributes
		openNode.node.GetNode().VirtualSetAttributes(ctx, &attributes, attributesMask, &outAttributes)
	}

	// Check if we should delete the file.
	if cleanupFlags&ffi.FspCleanupDelete != 0 && openNode.parent != nil {
		directory, _ := openNode.node.GetPair()
		isDirectory := directory != nil
		fileNameStart := strings.LastIndexByte(name, '\\')
		fileName := name[fileNameStart+1:]
		println("Removing file:", fileName, "isDirectory:", isDirectory, "from directory", name[:fileNameStart+1])
		_, status := openNode.parent.VirtualRemove(path.MustNewComponent(fileName), isDirectory, !isDirectory)
		if status != virtual.StatusOK {
			println("Failed to remove file during cleanup:", status)
		}
	}
}

func (fs *winfspFileSystem) GetDirInfoByName(ref *ffi.FileSystemRef, parentHandle uintptr, name string, dirInfo *ffi.FSP_FSCTL_DIR_INFO) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	println("GetDirInfoByName called for parentDirFile:", parentHandle, "name:", name)
	// Get the parent directory
	parentDirectory, err := fs.openFiles.nodeForDirectoryHandle(parentHandle)
	if err != nil {
		return err
	}

	var attributes virtual.Attributes
	_, status := parentDirectory.VirtualLookup(ctx, path.MustNewComponent(name), attributesMaskForWinFSPAttr, nil)
	if status != virtual.StatusOK {
		return toNTStatus(status)
	}

	toWinFSPFileInfo(&attributes, &dirInfo.FileInfo)

	return nil
}

func (fs *winfspFileSystem) GetSecurity(ref *ffi.FileSystemRef, handle uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	ctx, err := fs.createContext()
	if err != nil {
		return nil, err
	}
	node, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return nil, err
	}
	// TODO: leaks
	var attributes virtual.Attributes
	node.GetNode().VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)
	return toSecurityDescriptor(&attributes)
}

func (fs *winfspFileSystem) containsReparsePoint(ref *ffi.FileSystemRef, fileName string) bool {
	if found, _, _ := ffi.FileSystemFindReparsePoint(ref, fileName); found {
		return found
	}
	return false
}

func (fs *winfspFileSystem) GetSecurityByName(ref *ffi.FileSystemRef, name string, flags ffi.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	println("GetSecurityByName called for name:", name, "with flags:", flags)
	ctx, err := fs.createContext()
	if err != nil {
		return 0, nil, err
	}

	var attributes virtual.Attributes
	if name == "\\" {
		fs.rootDirectory.VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)
	} else {
		parentName, leafName, err := parsePath(name)
		if err != nil {
			println("Failed to parse path", err)
			return 0, nil, err
		}
		parent, err := fs.resolveDirectory(ctx, parentName)
		if err != nil {
			println(fmt.Sprintf("Failed to reslolve dir %v", err))
			if fs.containsReparsePoint(ref, name) {
				return 0, nil, windows.STATUS_REPARSE
			}
			return 0, nil, err
		}
		_, status := parent.VirtualLookup(ctx, leafName, attributesMaskForWinFSPAttr, &attributes)
		if status != virtual.StatusOK {
			if fs.containsReparsePoint(ref, name) {
				return 0, nil, windows.STATUS_REPARSE
			}
			return 0, nil, toNTStatus(status)
		}
	}

	if flags == ffi.GetExistenceOnly {
		return 0, nil, nil
	}

	var sd *windows.SECURITY_DESCRIPTOR
	if (flags & ffi.GetSecurityByName) != 0 {
		sd, err = toSecurityDescriptor(&attributes)
		if err != nil {
			println("Failed to get SD", err)
			return 0, nil, err
		}
	}
	println("Returning sd:", sd.String())
	return toWinFSPFileAttributes(&attributes), sd, nil
}

func parsePath(name string) (parentName string, leafName path.Component, err error) {
	if name == "\\" {
		return "\\", path.Component{}, nil
	}
	lastSeparator := strings.LastIndexByte(name, '\\')
	if lastSeparator < 0 {
		return "", path.MustNewComponent(""), windows.STATUS_INVALID_PARAMETER
	}
	return name[:lastSeparator+1], path.MustNewComponent(name[lastSeparator+1:]), nil
}

func (fs *winfspFileSystem) Rename(ref *ffi.FileSystemRef, handle uintptr, source, target string, replaceIfExist bool) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}
	println("Reaname called for source:", source, "target:", target, "replaceIfExist:", replaceIfExist)

	openNode, err := fs.openFiles.trackedNodeForHandle(handle)
	if err != nil {
		return err
	}
	if openNode.parent == nil {
		return windows.STATUS_INVALID_PARAMETER
	}

	_, sourceLeafName, err := parsePath(source)
	if err != nil {
		return err
	}
	targetParentName, targetLeafName, err := parsePath(target)
	if err != nil {
		return err
	}
	targetParent, err := fs.resolveDirectory(ctx, targetParentName)
	if err != nil {
		return err
	}

	// TODO: race condition here: we should check if the target exists atomically
	// Check if target exists and handle replaceIfExist
	var attributes virtual.Attributes
	if _, status := targetParent.VirtualLookup(ctx, targetLeafName, virtual.AttributesMaskFileType, &attributes); status == virtual.StatusOK {
		if attributes.GetFileType() == filesystem.FileTypeDirectory {
			// Windows never allows renames over an existing directory.
			return windows.STATUS_ACCESS_DENIED
		}
		if !replaceIfExist {
			return windows.STATUS_OBJECT_NAME_COLLISION
		}
	}

	println("Will do rename from", sourceLeafName.String(), "to", targetLeafName.String(), "in parent", targetParentName)

	// Perform the rename operation
	if _, _, status := openNode.parent.VirtualRename(sourceLeafName, targetParent, targetLeafName); status != virtual.StatusOK {
		return toNTStatus(status)
	}
	return nil
}

func (fs *winfspFileSystem) SetSecurity(ref *ffi.FileSystemRef, handle uintptr, info windows.SECURITY_INFORMATION, modificationDesc *windows.SECURITY_DESCRIPTOR) error {
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	println("SetSecurity")

	openNode, err := fs.openFiles.nodeForHandle(handle)
	if err != nil {
		return err
	}

	var attributes virtual.Attributes
	openNode.GetNode().VirtualGetAttributes(ctx, attributesMaskForWinFSPAttr, &attributes)

	currentSd, err := toSecurityDescriptor(&attributes)
	if err != nil {
		return err
	}

	// TODO: this can be massively simplified given the VFS only stores one permission bit
	newSd, err := ffi.SetSecurityDescriptor(currentSd, info, modificationDesc)
	if err != nil {
		return err
	}
	// TODO: remove this and improve the interface
	defer ffi.DeleteSecurityDescriptor(newSd)

	uid, gid, mode, err := ffi.PosixMapSecurityDescriptorToPermissions(newSd)
	if err != nil {
		return err
	}

	attributes.SetOwnerGroupID(gid)
	attributes.SetOwnerUserID(uid)
	attributes.SetPermissions(virtual.NewPermissionsFromMode(mode))
	var outAttributes virtual.Attributes
	openNode.GetNode().VirtualSetAttributes(ctx,
		&attributes,
		virtual.AttributesMaskOwnerGroupID|virtual.AttributesMaskOwnerUserID|virtual.AttributesMaskPermissions,
		&outAttributes)

	return nil
}

func (fs *winfspFileSystem) DeleteReparsePoint(ref *ffi.FileSystemRef, file uintptr, name string, buffer []byte) error {
	println(fmt.Sprintf("ZZZZ DeleteReparsePoint called for file: %d, name: %s, buffer: %v", file, name, buffer))
	return windows.STATUS_NOT_IMPLEMENTED
}

// func (fs *winfspFileSystem) DeviceIoControl(ref *winfsp.FileSystemRef, file uintptr, code uint32, data []byte) ([]byte, error) {
// 	println(fmt.Sprintf("ZZZZ DeviceIoControl called for file: %d, code: %d, data: %v", file, code, data))
// 	return nil, windows.STATUS_NOT_IMPLEMENTED
// }

func (fs *winfspFileSystem) getReparsePointForLeaf(ctx context.Context, ref *ffi.FileSystemRef, leaf virtual.Leaf, buffer []byte) (int, error) {
	target, status := leaf.VirtualReadlink(ctx)
	if status != virtual.StatusOK {
		return 0, toNTStatus(status)
	}
	println(fmt.Sprintf("Resolved symlink target: %s", target))
	targetUTF16, err := windows.UTF16FromString(string(target))
	if err != nil {
		return 0, err
	}

	// utf-16 encoded so 2 bytes per character; no null terminator needed.
	targetUTF16Len := len(targetUTF16) - 1
	targetUTF16Bytes := targetUTF16Len * 2
	symbolicLinkReparseSize := int(unsafe.Sizeof(SymbolicLinkReparseBuffer{})) -
		// Exclude the PathBuffer member.
		2 +
		// Two copies of the target path.
		targetUTF16Bytes*2
	requiredSize := int(unsafe.Sizeof(REPARSE_DATA_BUFFER_HEADER{})) + symbolicLinkReparseSize
	if len(buffer) < requiredSize {
		return 0, windows.STATUS_BUFFER_TOO_SMALL
	}

	rdb := (*REPARSE_DATA_BUFFER)(unsafe.Pointer(&buffer[0]))
	rdb.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	rdb.ReparseDataLength = uint16(symbolicLinkReparseSize)
	rdb.Reserved = 0

	slrb := (*SymbolicLinkReparseBuffer)(unsafe.Pointer(&rdb.DUMMYUNIONNAME))
	slrb.SubstituteNameOffset = 0
	slrb.SubstituteNameLength = uint16(targetUTF16Bytes)
	slrb.PrintNameOffset = uint16(targetUTF16Bytes)
	slrb.PrintNameLength = uint16(targetUTF16Bytes)

	slrb.Flags = 0

	// TODO
	// Set flags - relative if the path doesn't start with drive letter or UNC
	if len(target) >= 3 && target[1] == ':' && target[2] == '\\' {
		// Absolute path (C:\...)
		slrb.Flags = 0
	} else if len(target) >= 2 && target[0] == '\\' && target[1] == '\\' {
		// UNC path (\\server\share)
		slrb.Flags = 0
	} else if len(target) >= 3 && target[0] == '\\' && target[1] == '?' && target[2] == '?' {
		// extended path (\\?\)
		slrb.Flags = 0
	} else {
		// Relative path
		println("Setting relative symlink flag for target:", target)
		slrb.Flags = SYMLINK_FLAG_RELATIVE
		// slrb.Flags = 0
	}

	pathBuffer := unsafe.Slice(&slrb.PathBuffer[0], 2*targetUTF16Bytes)
	copy(pathBuffer[0:targetUTF16Len], targetUTF16)
	copy(pathBuffer[targetUTF16Len:targetUTF16Len*2], targetUTF16)

	// TODO: not convinced I've got this right
	println(fmt.Sprintf("Returning encoded buffer %v", buffer[:requiredSize]))

	return requiredSize, nil
}

func (fs *winfspFileSystem) GetReparsePoint(ref *ffi.FileSystemRef, file uintptr, name string, buffer []byte) (int, error) {
	println(fmt.Sprintf("ZZZZ GetReparsePoint called for file: %d, name: %s", file, name))
	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}
	leaf, err := fs.openFiles.nodeForLeafHandle(file)
	if err != nil {
		return 0, err
	}
	return fs.getReparsePointForLeaf(ctx, ref, leaf, buffer)
}

func (fs *winfspFileSystem) GetReparsePointByName(ref *ffi.FileSystemRef, name string, isDirectory bool, buffer []byte) (int, error) {
	println(fmt.Sprintf("ZZZZ GetReparsePointByName called for name: %s, isDirectory: %v", name, isDirectory))

	ctx, err := fs.createContext()
	if err != nil {
		return 0, err
	}
	parentName, leafName, err := parsePath(name)
	if err != nil {
		return 0, err
	}
	parent, err := fs.resolveDirectory(ctx, parentName)
	if err != nil {
		return 0, err
	}
	var attributes virtual.Attributes
	node, status := parent.VirtualLookup(ctx, leafName, attributesMaskForWinFSPAttr, &attributes)
	if status != virtual.StatusOK {
		return 0, toNTStatus(status)
	}
	if attributes.GetFileType() != filesystem.FileTypeSymlink {
		return 0, windows.STATUS_NOT_A_REPARSE_POINT
	}
	_, leaf := node.GetPair()
	if leaf == nil {
		return 0, windows.STATUS_NOT_A_REPARSE_POINT
	}
	return fs.getReparsePointForLeaf(ctx, ref, leaf, buffer)
}

func (fs *winfspFileSystem) SetReparsePoint(ref *ffi.FileSystemRef, handle uintptr, name string, buffer []byte) error {
	println(fmt.Sprintf("ZZZZ SetReparsePoint called for file: %d, name: %s, buffer: %v", handle, name, buffer))
	ctx, err := fs.createContext()
	if err != nil {
		return err
	}

	node, err := fs.openFiles.trackedNodeForHandle(handle)
	if err != nil {
		return windows.STATUS_INVALID_HANDLE
	}
	if node.parent == nil {
		// TODO
		return windows.STATUS_INVALID_PARAMETER
	}

	_, linkName, err := parsePath(name)
	if err != nil {
		return err
	}

	if len(buffer) < int(unsafe.Sizeof(REPARSE_DATA_BUFFER{})) {
		return windows.STATUS_INVALID_PARAMETER
	}
	reparseBuffer := (*REPARSE_DATA_BUFFER)(unsafe.Pointer(&buffer[0]))

	println("Got reparse tag :", reparseBuffer.ReparseTag)

	switch reparseBuffer.ReparseTag {
	case windows.IO_REPARSE_TAG_SYMLINK:
		// Handle symbolic link reparse point
		if reparseBuffer.ReparseDataLength < uint16(unsafe.Sizeof(SymbolicLinkReparseBuffer{})) {
			return windows.STATUS_INVALID_PARAMETER
		}
		symbolicLinkBuffer := (*SymbolicLinkReparseBuffer)(unsafe.Pointer(&reparseBuffer.DUMMYUNIONNAME))
		targetPath := symbolicLinkBuffer.Path()
		println(fmt.Sprintf("Setting symbolic link reparse point for file: %d, target: %s", handle, targetPath))
		// TODO: ugly
		if _, s := node.parent.VirtualRemove(linkName, true, true); s != virtual.StatusOK {
			println("Failed to remove existing symbolic link:", s)
			return toNTStatus(s)
		}
		var outAttributes virtual.Attributes
		if _, _, s := node.parent.VirtualSymlink(ctx, []byte(targetPath), linkName, virtual.AttributesMaskFileType, &outAttributes); s != virtual.StatusOK {
			println("Failed to set symbolic link reparse point:", s)
			return toNTStatus(s)
		}

		return nil

	default:
		// TODO: check for the correct error code
		return windows.STATUS_DEVICE_NOT_READY
	}
}

// Check we implement the relevant interfaces. go-winfsp uses which interfaces
// we implement to determine capabilities.
var _ ffi.BehaviourBase = (*winfspFileSystem)(nil)
var _ ffi.BehaviourCanDelete = (*winfspFileSystem)(nil)
var _ ffi.BehaviourCleanup = (*winfspFileSystem)(nil)
var _ ffi.BehaviourCreate = (*winfspFileSystem)(nil)
var _ ffi.BehaviourDeleteReparsePoint = (*winfspFileSystem)(nil)
var _ ffi.BehaviourFlush = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetDirInfoByName = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetFileInfo = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetReparsePoint = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetReparsePointByName = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetSecurity = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetSecurityByName = (*winfspFileSystem)(nil)
var _ ffi.BehaviourGetVolumeInfo = (*winfspFileSystem)(nil)
var _ ffi.BehaviourOverwrite = (*winfspFileSystem)(nil)
var _ ffi.BehaviourRead = (*winfspFileSystem)(nil)
var _ ffi.BehaviourReadDirectoryOffset = (*winfspFileSystem)(nil)
var _ ffi.BehaviourRename = (*winfspFileSystem)(nil)
var _ ffi.BehaviourSetBasicInfo = (*winfspFileSystem)(nil)
var _ ffi.BehaviourSetFileSize = (*winfspFileSystem)(nil)
var _ ffi.BehaviourSetReparsePoint = (*winfspFileSystem)(nil)
var _ ffi.BehaviourSetSecurity = (*winfspFileSystem)(nil)
var _ ffi.BehaviourSetVolumeLabel = (*winfspFileSystem)(nil)
var _ ffi.BehaviourWrite = (*winfspFileSystem)(nil)
