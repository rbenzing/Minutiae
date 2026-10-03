package examine

import (
	"errors"
	"syscall"
)

// localNameError marks a failure to create an artifact because the examiner's
// operating system rejected its local path (a name or path too long, or not
// valid there). It is a problem of one entry's name, not of the case, so the
// entry is skipped with a warning instead of aborting the run.
type localNameError struct{ err error }

func (e *localNameError) Error() string {
	return "local path rejected by the operating system: " + e.err.Error()
}
func (e *localNameError) Unwrap() error { return e.err }

// isLocalNameError reports whether err is an operating-system path-syntax or
// name-too-long error.
func isLocalNameError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || isLocalNameErrorOS(err)
}
