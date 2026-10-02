//go:build unix

package evidence

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockHandle takes a non-blocking exclusive flock on f. flock locks belong to
// the open file description, so a second open in the same process conflicts too.
func lockHandle(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrCaseInUse
	}
	return err
}

func unlockHandle(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
