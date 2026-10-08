package filesys

import (
	"errors"
	"fmt"
	"sort"
)

// Recoverer is implemented by a reader that can say where the content of a deleted entry may still be.
// Callers find it with As (or SupportsRecovery), never with a type assertion, so a wrapping filesystem
// does not hide it.
type Recoverer interface {
	// Recoverable returns where the content of the deleted entry e may still be. e is identified by e.ID
	// alone: every other field is ignored; the ID is re-resolved from the disk. A live object is
	// ErrNotDeleted, an unknown or forged ID is ErrNotFound at once (no directory search, no read budget
	// spent), an ID held by more than one object is a *CorruptError. It returns maps, never bytes, and
	// never validates them against free space (the caller does).
	Recoverable(e Entry) ([]Candidate, error)
}

// Candidate is one claim of where the content of a deleted entry may be. Everything in it is a claim of
// the reader, validated by CheckCandidate and by the caller, never believed.
type Candidate struct {
	Method      string   // a token, see CheckCandidate
	Size        int64    // bytes the runs are claimed to reproduce
	Runs        []Run    // filesystem-relative, in file order; no holes, no empty runs
	Basis       []string // what the claim rests on
	Assumptions []string // what must hold for the claim to be right
	Times       Times    // as found in the stale inode or entry; untrusted
	Mode        uint32   // idem
	Encrypted   bool     // reader-asserted: the content is stored encrypted
	Warnings    []string
}

// Limits of a recovery map.
const (
	MaxCandidateRuns      = 1 << 20
	MaxCandidatesPerEntry = 16

	maxCandidateStrings = 64
	maxCandidateString  = 256
)

var (
	// ErrNotDeleted is returned by Recoverer.Recoverable for a live entry.
	ErrNotDeleted = errors.New("entry is not deleted")
	// ErrNoRecovery means the filesystem has no recovery support.
	ErrNoRecovery = fmt.Errorf("%w: filesystem does not support recovery", ErrUnsupported)
)

// validMethodToken reports whether m matches [a-z][a-z0-9-]{1,31}. This is a strict subset of the
// evidence method token ([a-z][a-z0-9_-]{1,63}), so every method a reader may offer can be recorded in
// a case; TestReaderMethodTokenIsEvidenceValid (internal/examine) keeps the two in step.
func validMethodToken(m string) bool {
	if len(m) < 2 || len(m) > 32 || m[0] < 'a' || m[0] > 'z' {
		return false
	}
	for i := 1; i < len(m); i++ {
		c := m[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// CheckCandidate verifies a recovery map against a filesystem of fsSize bytes and returns the number of
// leading bytes of the file the runs cover. It is stricter than CheckRunsPrefix: no holes, no empty runs
// and no overlapping runs (adjacent runs are fine); the total may be less than Size (a prefix, which the
// caller flags) but not more. The method must be a token [a-z][a-z0-9-]{1,31}; Basis and Assumptions hold
// at most 64 strings of at most 256 bytes. Every violation is a *CorruptError of structure "recovery
// map". It is total: any input yields a result or an error, never a panic, and c is not modified.
func CheckCandidate(c Candidate, fsSize int64) (covered int64, err error) {
	if c.Size < 0 || fsSize < 0 {
		return 0, mapErr("negative size %d or filesystem size %d", c.Size, fsSize)
	}
	if len(c.Runs) > MaxCandidateRuns {
		return 0, mapErr("too many runs (%d, at most %d)", len(c.Runs), MaxCandidateRuns)
	}
	if c.Size > fsSize {
		return 0, mapErr("size %d exceeds the %d-byte filesystem", c.Size, fsSize)
	}
	if !validMethodToken(c.Method) {
		return 0, mapErr("method %q is not a token [a-z][a-z0-9-]{1,31}", c.Method)
	}
	if err := checkStrings("basis", c.Basis); err != nil {
		return 0, err
	}
	if err := checkStrings("assumption", c.Assumptions); err != nil {
		return 0, err
	}
	var total int64
	for i, r := range c.Runs {
		if r.Offset == -1 {
			return 0, mapErr("run %d is a hole", i)
		}
		if r.Offset < 0 {
			return 0, mapErr("run %d has offset %d", i, r.Offset)
		}
		if r.Length <= 0 {
			return 0, mapErr("run %d has length %d", i, r.Length)
		}
		if end, ok := AddOK(r.Offset, r.Length); !ok || end > fsSize {
			return 0, mapErr("run %d (%d+%d) lies outside the %d-byte filesystem", i, r.Offset, r.Length, fsSize)
		}
		var ok bool
		if total, ok = AddOK(total, r.Length); !ok {
			return 0, mapErr("run lengths overflow at run %d", i)
		}
		if total > c.Size {
			return 0, mapErr("runs cover more than the size %d (%d bytes by run %d)", c.Size, total, i)
		}
	}
	idx := make([]int, len(c.Runs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return c.Runs[idx[a]].Offset < c.Runs[idx[b]].Offset })
	for k := 1; k < len(idx); k++ {
		prev, cur := c.Runs[idx[k-1]], c.Runs[idx[k]]
		if prev.Offset+prev.Length > cur.Offset { // both ends were checked above
			a, b := idx[k-1], idx[k]
			if a > b {
				a, b = b, a
			}
			return 0, mapErr("runs %d and %d overlap", a, b)
		}
	}
	return total, nil
}

func checkStrings(what string, ss []string) error {
	if len(ss) > maxCandidateStrings {
		return mapErr("%d %s strings, at most %d", len(ss), what, maxCandidateStrings)
	}
	for i, s := range ss {
		if len(s) > maxCandidateString {
			return mapErr("%s %d is %d bytes, at most %d", what, i, len(s), maxCandidateString)
		}
	}
	return nil
}

func mapErr(format string, a ...any) error {
	return &CorruptError{Structure: "recovery map", Offset: -1, Reason: fmt.Sprintf(format, a...)}
}
