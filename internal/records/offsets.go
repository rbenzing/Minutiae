package records

import (
	"errors"
	"fmt"
	"math"

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

// TranslateRange maps the byte range r of an artifact onto image extents, one
// extent per run piece (image-contiguous runs are not merged and runs are not
// assumed disjoint in the image). A run with Offset -1 is a hole. It is pure
// and checked: any refusal returns the zero Translation. For an incomplete
// artifact the runs may describe more than the bytes held; only offsets
// inside [0, artifactSize) translate, and RunsExceedSize says so.
func TranslateRange(runs []evidence.Run, artifactSize int64, incomplete bool, r Range) (Translation, error) {
	var sum int64
	for i, ru := range runs {
		switch {
		case ru.Offset < -1:
			return Translation{}, unavailable("runs are invalid: run %d starts at %d", i, ru.Offset)
		case ru.Length <= 0:
			return Translation{}, unavailable("runs are invalid: run %d has length %d", i, ru.Length)
		case ru.Offset >= 0 && ru.Offset > math.MaxInt64-ru.Length:
			return Translation{}, unavailable("runs are invalid: run %d ends beyond the largest offset", i)
		case sum > math.MaxInt64-ru.Length:
			return Translation{}, unavailable("runs are invalid: the run lengths overflow")
		}
		sum += ru.Length
	}
	var exceed bool
	switch {
	case sum == artifactSize:
	case !incomplete:
		return Translation{}, unavailable("runs cover %d bytes but the artifact has %d", sum, artifactSize)
	case sum < artifactSize:
		return Translation{}, unavailable("runs cover only %d of the artifact's %d bytes", sum, artifactSize)
	default:
		exceed = true
	}
	if r.Offset < 0 || r.Length < 0 || r.Offset > math.MaxInt64-r.Length {
		return Translation{}, unavailable("range is negative or overflows")
	}
	if r.Offset+r.Length > artifactSize {
		return Translation{}, unavailable("range lies outside the artifact (%d+%d > %d)", r.Offset, r.Length, artifactSize)
	}
	t := Translation{RunsExceedSize: exceed}
	if r.Length == 0 {
		return t, nil
	}
	end := r.Offset + r.Length
	var pos int64 // artifact offset of the start of the current run
	for _, ru := range runs {
		if pos >= end {
			break
		}
		runEnd := pos + ru.Length // cannot overflow: bounded by sum
		if runEnd > r.Offset {
			from := max(pos, r.Offset)
			to := min(runEnd, end)
			e := ImageExtent{ArtifactOffset: from, Length: to - from, ImageOffset: -1, Hole: ru.Offset < 0}
			if !e.Hole {
				e.ImageOffset = ru.Offset + (from - pos)
			}
			t.Total++
			if len(t.Extents) < MaxExtentsPerHop {
				t.Extents = append(t.Extents, e)
			} else {
				t.Truncated = true
			}
		}
		pos = runEnd
	}
	return t, nil
}
