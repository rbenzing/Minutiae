package examine

import (
	"errors"
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The output limits of one derived-artifact analysis and the free-space check that precedes its first
// write. A limit is a safety cap on what a hostile image can make an analysis write, never a reason to
// drop evidence silently: the caller records what the cap cut off.
const (
	// DefaultMaxFiles is the default cap on artifacts one analysis writes.
	DefaultMaxFiles = 100_000
	// DefaultMaxBytes is the default cap on the bytes one analysis writes.
	DefaultMaxBytes int64 = 8 << 30
	// freeSpaceReserve is the free space that must remain after the planned output.
	freeSpaceReserve int64 = 64 << 20
)

// ErrInvalidLimit is returned for a negative output limit (0 means the default).
var ErrInvalidLimit = errors.New("invalid output limit")

// ErrInsufficientSpace is returned when the case directory cannot hold the planned output plus the reserve.
var ErrInsufficientSpace = errors.New("not enough free space in the case directory for the planned output")

// budget counts what one analysis has been admitted to write.
type budget struct {
	maxFiles int
	maxBytes int64
	files    int
	bytes    int64
}

// newBudget returns a budget; a limit of 0 selects the default and a negative one is ErrInvalidLimit.
func newBudget(maxFiles int, maxBytes int64) (*budget, error) {
	if maxFiles < 0 || maxBytes < 0 {
		return nil, fmt.Errorf("%w: max files %d, max bytes %d (0 selects the default)", ErrInvalidLimit, maxFiles, maxBytes)
	}
	if maxFiles == 0 {
		maxFiles = DefaultMaxFiles
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	return &budget{maxFiles: maxFiles, maxBytes: maxBytes}, nil
}

// admit asks to write one file of size bytes. It returns "" when the file is admitted and counted, and
// "max-files" or "max-bytes" when a limit refuses it, in which case nothing is counted. A refusal is
// final: the caller stops at the first refusal and does not try a smaller candidate. The files limit is
// checked first. A negative size is refused as "max-bytes" (it must never lower the count). The check
// cannot overflow.
func (b *budget) admit(size int64) string {
	if b.files >= b.maxFiles {
		return "max-files"
	}
	if size < 0 || size > b.maxBytes-b.bytes { // bytes <= maxBytes always holds, so the subtraction is exact
		return "max-bytes"
	}
	b.files++
	b.bytes += size
	return ""
}

// availBytes is blocks x unit for a filesystem report, overflow-safe: a product above MaxInt64 (more
// than 8 EiB, which only an implausible report claims) is MaxInt64. A unit that is not positive is an
// error, never "no space" or "all space".
func availBytes(blocks uint64, unit int64) (int64, error) {
	if unit <= 0 {
		return 0, fmt.Errorf("filesystem reports a block size of %d", unit)
	}
	if blocks > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	n, ok := filesys.MulOK(int64(blocks), unit)
	if !ok {
		return math.MaxInt64, nil
	}
	return n, nil
}

// checkSpace is the refusal before the first write: the case directory must hold planned bytes plus
// freeSpaceReserve. A failure to find out is returned as it is, never read as "enough".
func (s *Session) checkSpace(planned int64) error {
	probe := s.freeBytes
	if probe == nil {
		probe = diskFree
	}
	free, err := probe(s.Case.Dir)
	if err != nil {
		return fmt.Errorf("free space of %s: %w", s.Case.Dir, err)
	}
	planned = max(planned, 0)
	need, ok := filesys.AddOK(planned, freeSpaceReserve)
	if !ok || free < need {
		return fmt.Errorf("%w: %d bytes free, planned output %d bytes plus a reserve of %d bytes (need %d)",
			ErrInsufficientSpace, free, planned, freeSpaceReserve, need)
	}
	return nil
}
