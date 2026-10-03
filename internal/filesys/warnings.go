package filesys

import (
	"fmt"
	"slices"
	"sync"
)

// MaxWarnings bounds the distinct messages a Warnings collector keeps: a
// hostile image can make every directory block or extent node report a
// problem.
const MaxWarnings = 1000

// SuppressedWarning is the single line that stands for every warning dropped
// once MaxWarnings distinct messages are held.
const SuppressedWarning = "further warnings suppressed"

// Warnings collects the non-fatal problems a filesystem reader meets (a
// checksum mismatch, a damaged directory block, a truncated chain ...) for
// Info().Warnings. The zero value is ready to use. Identical messages are
// recorded once; after MaxWarnings distinct messages a single
// SuppressedWarning line stands for the rest. It is safe for concurrent use
// and must not be copied after first use.
type Warnings struct {
	mu   sync.Mutex
	list []string
	seen map[string]struct{}
	full bool // the cap was reached and the suppressed line is in list
}

// Add records a problem. The message is format when there are no args, else
// fmt.Sprintf(format, args...).
func (w *Warnings) Add(format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.seen[msg]; dup {
		return
	}
	if len(w.list) >= MaxWarnings {
		if !w.full {
			w.full = true
			w.list = append(w.list, SuppressedWarning)
		}
		return
	}
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	w.seen[msg] = struct{}{}
	w.list = append(w.list, msg)
}

// Snapshot returns a copy of the warnings recorded so far, in first-seen
// order. It returns nil when there are none.
func (w *Warnings) Snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}
