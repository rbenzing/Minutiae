package records

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// MaxExtentsPerHop caps the extents stored in one Translation.
const MaxExtentsPerHop = 256

// ErrOffsetUnavailable wraps every refusal of TranslateRange.
var ErrOffsetUnavailable = errors.New("image offset unavailable")

// ImageExtent is one piece of a translated range: Length bytes at
// ArtifactOffset of the artifact, found at ImageOffset of the image (-1 and
// Hole true for a hole of a sparse file).
type ImageExtent struct {
	ArtifactOffset int64
	Length         int64
	ImageOffset    int64
	Hole           bool
}

// Translation is the result of TranslateRange. Total counts every extent,
// Extents keeps the first MaxExtentsPerHop (Truncated says it cut). RunsExceedSize
// is set for an incomplete artifact whose runs describe more than the bytes held.
type Translation struct {
	Extents        []ImageExtent
	Total          int
	Truncated      bool
	RunsExceedSize bool
}

func unavailable(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrOffsetUnavailable}, args...)...)
}

// errRangeOutside marks the refusals caused by the requested range, as opposed to the runs.
var errRangeOutside = errors.New("range outside the artifact")

// outsideError is an ErrOffsetUnavailable that also matches errRangeOutside; its text is the plain
// refusal text.
type outsideError struct{ msg string }

func (e outsideError) Error() string { return ErrOffsetUnavailable.Error() + ": " + e.msg }
func (e outsideError) Is(target error) bool {
	return target == ErrOffsetUnavailable || target == errRangeOutside
}

func outside(format string, args ...any) error {
	return outsideError{msg: fmt.Sprintf(format, args...)}
}

// runIndex is the validated runs of one artifact plus their prefix sums, so that a range is placed by
// binary search instead of a walk over the runs. steps counts every probe and every extent emitted
// (test-visible: the cost is proven by this counter, never by wall time).
type runIndex struct {
	runs   []evidence.Run
	ends   []int64 // ends[i] is the artifact offset where run i ends
	size   int64
	exceed bool
	steps  int
}

// newRunIndex validates runs (every error text is the one TranslateRange has always given) and checks
// them against the artifact's size.
func newRunIndex(runs []evidence.Run, artifactSize int64, incomplete bool) (*runIndex, error) {
	ends := make([]int64, len(runs))
	var sum int64
	for i, ru := range runs {
		switch {
		case ru.Offset < -1:
			return nil, unavailable("runs are invalid: run %d starts at %d", i, ru.Offset)
		case ru.Length <= 0:
			return nil, unavailable("runs are invalid: run %d has length %d", i, ru.Length)
		case ru.Offset >= 0 && ru.Offset > math.MaxInt64-ru.Length:
			return nil, unavailable("runs are invalid: run %d ends beyond the largest offset", i)
		case sum > math.MaxInt64-ru.Length:
			return nil, unavailable("runs are invalid: the run lengths overflow")
		}
		sum += ru.Length
		ends[i] = sum
	}
	var exceed bool
	switch {
	case sum == artifactSize:
	case !incomplete:
		return nil, unavailable("runs cover %d bytes but the artifact has %d", sum, artifactSize)
	case sum < artifactSize:
		return nil, unavailable("runs cover only %d of the artifact's %d bytes", sum, artifactSize)
	default:
		exceed = true
	}
	return &runIndex{runs: runs, ends: ends, size: artifactSize, exceed: exceed}, nil
}

// translate places the range r. Total comes from the index arithmetic; at most MaxExtentsPerHop
// extents are emitted.
func (x *runIndex) translate(r Range) (Translation, error) {
	if r.Offset < 0 || r.Length < 0 || r.Offset > math.MaxInt64-r.Length {
		return Translation{}, outside("range is negative or overflows")
	}
	if r.Offset+r.Length > x.size {
		return Translation{}, outside("range lies outside the artifact (%d+%d > %d)", r.Offset, r.Length, x.size)
	}
	t := Translation{RunsExceedSize: x.exceed}
	if r.Length == 0 {
		return t, nil
	}
	end := r.Offset + r.Length
	// the first run that ends after the start, and the first that ends at or after the end: both exist
	// because the runs cover at least the artifact's size
	first := sort.Search(len(x.ends), func(i int) bool { x.steps++; return x.ends[i] > r.Offset })
	last := sort.Search(len(x.ends), func(i int) bool { x.steps++; return x.ends[i] >= end })
	t.Total = last - first + 1
	t.Truncated = t.Total > MaxExtentsPerHop
	stop := min(last, first+MaxExtentsPerHop-1)
	t.Extents = make([]ImageExtent, 0, stop-first+1)
	for i := first; i <= stop; i++ {
		x.steps++
		ru := x.runs[i]
		pos := x.ends[i] - ru.Length
		from := max(pos, r.Offset)
		to := min(x.ends[i], end)
		e := ImageExtent{ArtifactOffset: from, Length: to - from, ImageOffset: -1, Hole: ru.Offset < 0}
		if !e.Hole {
			e.ImageOffset = ru.Offset + (from - pos)
		}
		t.Extents = append(t.Extents, e)
	}
	return t, nil
}

// TranslateRange maps the byte range r of an artifact onto image extents, one
// extent per run piece (image-contiguous runs are not merged and runs are not
// assumed disjoint in the image). A run with Offset -1 is a hole. It is pure
// and checked: any refusal returns the zero Translation. For an incomplete
// artifact the runs may describe more than the bytes held; only offsets
// inside [0, artifactSize) translate, and RunsExceedSize says so.
func TranslateRange(runs []evidence.Run, artifactSize int64, incomplete bool, r Range) (Translation, error) {
	x, err := newRunIndex(runs, artifactSize, incomplete)
	if err != nil {
		return Translation{}, err
	}
	return x.translate(r)
}
