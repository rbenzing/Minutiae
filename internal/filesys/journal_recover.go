package filesys

import (
	"context"
	"fmt"
)

// JournalUse says how far a recovery may rely on a filesystem journal (3G). The journal holds old
// copies of metadata blocks: they are historical, never live data.
type JournalUse string

// The journal uses. None is the zero value's meaning ("" parses to it): the journal is not consulted.
const (
	JournalUseNone      JournalUse = "none"
	JournalUseCommitted JournalUse = "committed" // copies from committed transactions in the live region
	JournalUseStale     JournalUse = "stale"     // plus committed copies found outside the live region
)

// ParseJournalUse parses the text of a journal use: "" is none; anything but the three names is an
// error naming the value (the match is exact and case-sensitive).
func ParseJournalUse(s string) (JournalUse, error) {
	switch JournalUse(s) {
	case "", JournalUseNone:
		return JournalUseNone, nil
	case JournalUseCommitted, JournalUseStale:
		return JournalUse(s), nil
	}
	return JournalUseNone, fmt.Errorf("unknown journal use %q (want none, committed or stale)", s)
}

// JournalRecoverer is implemented by a filesystem whose Recoverer can use its journal once it has built
// an index of it. Recovery planning calls it once when the examiner asked for the journal. It must honour
// ctx, be idempotent and leave nothing half-built when cancelled. Callers find it with As.
type JournalRecoverer interface {
	PrepareJournalRecovery(ctx context.Context, use JournalUse) error
}

// ErrNoJournalRecovery means the filesystem cannot recover from its journal.
var ErrNoJournalRecovery = fmt.Errorf("%w: filesystem has no journal recovery", ErrUnsupported)

// PrepareJournalRecovery runs the JournalRecoverer of fsys (found with As). With use none nothing is
// asked of the filesystem and the answer is nil; otherwise a filesystem without one is
// ErrNoJournalRecovery.
func PrepareJournalRecovery(ctx context.Context, fsys FileSystem, use JournalUse) error {
	u, err := ParseJournalUse(string(use))
	if err != nil {
		return err
	}
	if u == JournalUseNone {
		return nil
	}
	r, ok := As[JournalRecoverer](fsys)
	if !ok {
		return ErrNoJournalRecovery
	}
	return r.PrepareJournalRecovery(ctx, u)
}

// SupportsJournalRecovery reports whether fsys (or one it wraps) implements JournalRecoverer. A wrapper
// that always has the method but forwards to something that may lack it (the panic-protecting wrapper of
// internal/examine) says so with a SupportsJournalRecovery() bool method, which wins.
func SupportsJournalRecovery(fsys FileSystem) bool {
	if s, ok := fsys.(interface{ SupportsJournalRecovery() bool }); ok {
		return s.SupportsJournalRecovery()
	}
	_, ok := As[JournalRecoverer](fsys)
	return ok
}
