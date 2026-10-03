package examine

import (
	"errors"
	"syscall"
)

// Windows error codes the syscall package does not name.
const (
	errorInvalidName        = syscall.Errno(123) // ERROR_INVALID_NAME
	errorBadPathname        = syscall.Errno(161) // ERROR_BAD_PATHNAME
	errorFilenameExcedRange = syscall.Errno(206) // ERROR_FILENAME_EXCED_RANGE
)

func isLocalNameErrorOS(err error) bool {
	for _, target := range []error{errorInvalidName, errorBadPathname, errorFilenameExcedRange} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
