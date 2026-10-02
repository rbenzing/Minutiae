package evidence

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockHandle takes a non-blocking exclusive LockFileEx lock on f.
func lockHandle(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrCaseInUse
	}
	return err
}

func unlockHandle(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
