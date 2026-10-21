package filesys

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MaxWarnings bounds the distinct messages a Warnings collector keeps: a
// hostile image can make every directory block or extent node report a
// problem.
const MaxWarnings = 1000

// SuppressedWarning is the prefix of the note that ends a full Snapshot.
const SuppressedWarning = "further warnings suppressed"

const (
	suppressedNotePrefix = SuppressedWarning + ": "
	suppressedNoteSuffix = " occurrences not recorded"
)

// SuppressedNote is element MaxWarnings of a full Snapshot:
// "further warnings suppressed: N occurrences not recorded". N counts every
// Add the collector dropped, saturating at math.MaxUint64.
func SuppressedNote(n uint64) string {
	return suppressedNotePrefix + strconv.FormatUint(n, 10) + suppressedNoteSuffix
}

// ParseSuppressedNote reports the count of a note produced by SuppressedNote
// and nothing else: canonical decimal (no sign, no leading zero, no extra
// text), at least 1.
func ParseSuppressedNote(s string) (n uint64, ok bool) {
	num, found := strings.CutPrefix(s, suppressedNotePrefix)
	if !found {
		return 0, false
	}
	num, found = strings.CutSuffix(num, suppressedNoteSuffix)
	if !found || num == "" || num[0] < '1' || num[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseUint(num, 10, 64) // rejects a sign, a space, a non-ASCII digit and overflow
	if err != nil || strconv.FormatUint(n, 10) != num {
		return 0, false
	}
	return n, true
}

// Warnings collects the non-fatal problems a filesystem reader meets (a
// checksum mismatch, a damaged directory block, a truncated chain ...) for
// Info().Warnings. The zero value is ready to use. Identical messages are
// recorded once; after MaxWarnings distinct messages every further message
// is counted, and Snapshot ends the list with one SuppressedNote stating the
// count. A repeat of a dropped message counts again (it cannot be remembered
// without unbounded memory); a repeat of a kept one is a duplicate, not a
// loss. It is safe for concurrent use and must not be copied after first use.
type Warnings struct {
	mu      sync.Mutex
	list    []string
	seen    map[string]struct{}
	dropped uint64 // Adds not recorded, saturating at math.MaxUint64
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
		if w.dropped < math.MaxUint64 {
			w.dropped++
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
// order, followed by SuppressedNote when any were dropped (so at most
// MaxWarnings+1 entries, the note at index MaxWarnings). It returns nil when
// there are none.
func (w *Warnings) Snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := slices.Clone(w.list)
	if w.dropped > 0 {
		out = append(out, SuppressedNote(w.dropped))
	}
	return out
}
