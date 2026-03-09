package path

// WindowsPathFormat represents different format options for Windows path strings.
type WindowsPathFormat int

const (
	// WindowsPathFormatStandard represents the standard Windows path
	// format. If a namespace prefix (\\?\, \??\ or \\.\) was present
	// when the path was parsed, it is preserved in the output.
	WindowsPathFormatStandard WindowsPathFormat = iota
	// WindowsPathFormatDevicePath represents a Windows NT device path format.
	// Note that not all paths can be printed like this (e.g. relative paths
	// cannot), so this will fallback to WindowsPathFormatStandard.
	WindowsPathFormatDevicePath
	// WindowsPathFormatNoTrailingSeparator is like
	// WindowsPathFormatStandard, but suppresses the trailing "\"
	// that is normally emitted when the path refers to a
	// directory. This is needed when constructing symlink
	// substitute names for Windows reparse points, where a
	// trailing backslash can produce consecutive backslashes
	// after path substitution.
	WindowsPathFormatNoTrailingSeparator
)

// WindowsPrefixKind describes the namespace prefix that was present on a
// Windows path when it was parsed.
type WindowsPrefixKind int

const (
	// WindowsPrefixNone indicates no special prefix was present.
	WindowsPrefixNone WindowsPrefixKind = iota
	// WindowsPrefixExtendedLength indicates the \\?\ prefix.
	WindowsPrefixExtendedLength
	// WindowsPrefixNtNamespace indicates the \??\ prefix.
	WindowsPrefixNtNamespace
	// WindowsPrefixDevice indicates the \\.\ prefix.
	WindowsPrefixDevice
)

// Stringer is implemented by path types in this package that can be
// converted to string representations.
type Stringer interface {
	GetUNIXString() string
	GetWindowsString(format WindowsPathFormat) (string, error)
}
