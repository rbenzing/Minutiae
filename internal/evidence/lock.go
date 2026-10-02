package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// caseLock is an exclusive OS-level lock on <case>/case.lock, held for the
// life of an open Case so that two Minutiae processes (or two Case values in
// one process) can never interleave writes to the audit log, manifest or db.
// The lock file itself is not evidence; it lives outside artifacts/.
type caseLock struct{ f *os.File }

func acquireCaseLock(dir string) (*caseLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open case lock: %w", err)
	}
	if err := lockHandle(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrCaseInUse) {
			return nil, fmt.Errorf("%w: %s", ErrCaseInUse, dir)
		}
		return nil, fmt.Errorf("lock case %s: %w", dir, err)
	}
	return &caseLock{f: f}, nil
}

// release unlocks and closes the lock file. It is safe to call more than once.
func (l *caseLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := errors.Join(unlockHandle(l.f), l.f.Close())
	l.f = nil
	return err
}
