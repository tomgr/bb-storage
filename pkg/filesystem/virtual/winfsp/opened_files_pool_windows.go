package winfsp

import (
	"sync"

	"github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual"
	"golang.org/x/sys/windows"
)

// openedFilesPool holds the state of files that are opened by WinFSP.
type openedFilesPool struct {
	nodeLock   sync.Mutex
	handles    map[uintptr]*openedFile
	nextHandle uintptr
}

type openedFile struct {
	parent virtual.Directory
	node   virtual.DirectoryChild
	handle uintptr
}

func newOpenedFilesPool() openedFilesPool {
	return openedFilesPool{
		handles:    make(map[uintptr]*openedFile),
		nextHandle: 1,
	}
}

func (ofp *openedFilesPool) nodeForHandle(handle uintptr) (virtual.DirectoryChild, error) {
	ofp.nodeLock.Lock()
	defer ofp.nodeLock.Unlock()
	node, ok := ofp.handles[handle]
	if !ok {
		return virtual.DirectoryChild{}, windows.STATUS_INVALID_HANDLE
	}
	return node.node, nil
}

func (ofp *openedFilesPool) nodeForDirectoryHandle(handle uintptr) (virtual.Directory, error) {
	child, err := ofp.nodeForHandle(handle)
	if err != nil {
		return nil, err
	}
	dir, _ := child.GetPair()
	if dir == nil {
		return nil, windows.STATUS_NOT_A_DIRECTORY
	}
	return dir, nil
}

func (ofp *openedFilesPool) nodeForLeafHandle(handle uintptr) (virtual.Leaf, error) {
	child, err := ofp.nodeForHandle(handle)
	if err != nil {
		return nil, err
	}
	_, leaf := child.GetPair()
	if leaf == nil {
		return nil, windows.STATUS_FILE_IS_A_DIRECTORY
	}
	return leaf, nil
}

func (ofp *openedFilesPool) trackedNodeForHandle(handle uintptr) (*openedFile, error) {
	ofp.nodeLock.Lock()
	defer ofp.nodeLock.Unlock()
	node, ok := ofp.handles[handle]
	if !ok {
		return nil, windows.STATUS_INVALID_HANDLE
	}
	return node, nil
}

func (ofp *openedFilesPool) createHandle(parent virtual.Directory, node virtual.DirectoryChild) uintptr {
	ofp.nodeLock.Lock()
	defer ofp.nodeLock.Unlock()
	handle := ofp.nextHandle
	ofp.nextHandle++
	ofp.handles[handle] = &openedFile{
		parent: parent,
		node:   node,
		handle: handle,
	}
	return handle
}

func (ofp *openedFilesPool) removeHandle(handle uintptr) {
	ofp.nodeLock.Lock()
	defer ofp.nodeLock.Unlock()
	delete(ofp.handles, handle)
}
