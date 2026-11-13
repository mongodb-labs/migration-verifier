// Package fstools exposes filesystem-based conveniences.
package fstools

import (
	"fmt"
	"io/fs"
)

// DescribeFileType returns a string that describes the file type. This lets
// you create error messages like `%#q is wrong type (%s; need %s)`, which
// is more helpful than, e.g., `%#q must be a socket`.
func DescribeFileType(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular file"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeCharDevice != 0:
		return "character device"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return fmt.Sprintf("type %s", mode.Type())
	}
}
