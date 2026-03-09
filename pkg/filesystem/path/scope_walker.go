package path

// ScopeWalker is an interface that is called into by Resolve(). An
// implementation can use it to capture the path that is resolved.
// ScopeWalker is called into once for every path that is processed.
type ScopeWalker interface {
	// One of these functions is called right before processing the
	// first component in the path (if any). Based on the
	// characteristics of the path. Absolute paths are handled through
	// OnAbsolute(), and relative paths require OnRelative(). On
	// Windows, absolute paths can also start with a drive letter or
	// a UNC server/share, which are handled through OnWindowsRoot().
	//
	// These functions can be used by the implementation to determine
	// whether path resolution needs to be relative to the current
	// directory (e.g., working directory or parent directory of the
	// previous symlink encountered) or the root directory.
	//
	// For every instance of ScopeWalker, one of OnAbsolute(),
	// OnRelative() or OnWindowsRoot() may be called at most once.
	// Resolve() will always call into one of the interface functions
	// for every ScopeWalker presented, though decorators such as
	// VirtualRootScopeWalkerFactory may only call it when the path
	// is known to be valid. Absence of calls to OnAbsolute(),
	// OnRelative() or OnWindowsRoot() are used to indicate that the
	// provided path does not resolve to a location inside the file
	// system.
	OnAbsolute() (ComponentWalker, error)
	OnRelative() (ComponentWalker, error)
	OnWindowsRoot(root WindowsRootKind) (ComponentWalker, error)
}

// WindowsRootKind describes the root of a Windows path: either a drive
// letter (e.g. C:) or a UNC share (e.g. \\server\share). It also
// carries the optional namespace prefix (\\?\, \??\ or \\.\) that was
// present when the path was parsed.
type WindowsRootKind interface {
	isWindowsRootKind()
	WindowsPrefix() WindowsPrefixKind
}

// WindowsRootDriveLetter represents a Windows path rooted at a drive
// letter, such as "C:\".
type WindowsRootDriveLetter struct {
	Prefix WindowsPrefixKind
	Drive  rune
}

func (WindowsRootDriveLetter) isWindowsRootKind() {}

// WindowsPrefix returns the namespace prefix of the path.
func (r WindowsRootDriveLetter) WindowsPrefix() WindowsPrefixKind { return r.Prefix }

// WindowsRootShare represents a Windows UNC path rooted at a
// server/share, such as "\\server\share".
type WindowsRootShare struct {
	Prefix WindowsPrefixKind
	Server string
	Share  string
}

func (WindowsRootShare) isWindowsRootKind() {}

// WindowsPrefix returns the namespace prefix of the path.
func (r WindowsRootShare) WindowsPrefix() WindowsPrefixKind { return r.Prefix }
