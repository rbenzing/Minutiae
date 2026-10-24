package records

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

const maxInt64 = int64(^uint64(0) >> 1)

// translateLinear is the oracle: the 5C linear translation, copied verbatim from TranslateRange.
func translateLinear(runs []evidence.Run, artifactSize int64, incomplete bool, r Range) (Translation, error) {
	var sum int64
	for i, ru := range runs {
		switch {
		case ru.Offset < -1:
			return Translation{}, unavailable("runs are invalid: run %d starts at %d", i, ru.Offset)
		case ru.Length <= 0:
			return Translation{}, unavailable("runs are invalid: run %d has length %d", i, ru.Length)
		case ru.Offset >= 0 && ru.Offset > maxInt64-ru.Length:
			return Translation{}, unavailable("runs are invalid: run %d ends beyond the largest offset", i)
		case sum > maxInt64-ru.Length:
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
	if r.Offset < 0 || r.Length < 0 || r.Offset > maxInt64-r.Length {
		return Translation{}, outside("range is negative or overflows")
	}
	if r.Offset+r.Length > artifactSize {
		return Translation{}, outside("range lies outside the artifact (%d+%d > %d)", r.Offset, r.Length, artifactSize)
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
		runEnd := pos + ru.Length
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

// viaIndex is the code under test, shaped like TranslateRange.
func viaIndex(runs []evidence.Run, artifactSize int64, incomplete bool, r Range) (Translation, error) {
	x, err := newRunIndex(runs, artifactSize, incomplete)
	if err != nil {
		return Translation{}, err
	}
	return x.translate(r)
}

func sameResult(t *testing.T, runs []evidence.Run, size int64, incomplete bool, r Range) {
	t.Helper()
	want, werr := translateLinear(runs, size, incomplete, r)
	for name, f := range map[string]func([]evidence.Run, int64, bool, Range) (Translation, error){"index": viaIndex, "public": TranslateRange} {
		got, gerr := f(runs, size, incomplete, r)
		if (werr == nil) != (gerr == nil) || (werr != nil && werr.Error() != gerr.Error()) {
			t.Fatalf("%s: runs %v size %d inc %v range %+v: error %v, want %v", name, runs, size, incomplete, r, gerr, werr)
		}
		if errors.Is(werr, errRangeOutside) != errors.Is(gerr, errRangeOutside) || errors.Is(werr, ErrOffsetUnavailable) != errors.Is(gerr, ErrOffsetUnavailable) {
			t.Fatalf("%s: range %+v: error class differs: %v vs %v", name, r, gerr, werr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: runs %v size %d inc %v range %+v:\n got  %+v\n want %+v", name, runs, size, incomplete, r, got, want)
		}
	}
}

func randomRuns(rng *rand.Rand) ([]evidence.Run, int64) {
	n := 1 + rng.Intn(64)
	runs := make([]evidence.Run, n)
	var sum, img int64
	for i := range runs {
		l := int64(1 + rng.Intn(9))
		off := int64(-1)
		if rng.Intn(5) != 0 {
			img += int64(rng.Intn(20))
			off = img
			img += l
		}
		runs[i] = evidence.Run{Offset: off, Length: l}
		sum += l
	}
	return runs, sum
}

// edgeRanges holds, for every run boundary b and d in -1..1, ranges starting at b+d with several
// lengths and ranges ending at every boundary +-1, plus the whole artifact and empty ranges.
func edgeRanges(runs []evidence.Run, size int64) []Range {
	rs := []Range{{0, 0}, {0, size}, {size, 0}, {0, size + 1}, {-1, 1}, {1, size}}
	bounds := []int64{0}
	var pos int64
	for _, ru := range runs {
		pos += ru.Length
		bounds = append(bounds, pos)
	}
	for _, b := range bounds {
		for d := int64(-1); d <= 1; d++ {
			s := b + d
			for _, l := range []int64{0, 1, 2, 3, size - s, size - s - 1} {
				rs = append(rs, Range{s, l})
			}
		}
	}
	// ends: a few start points against every boundary +-1 (bounded)
	for _, s := range []int64{0, size / 2} {
		for _, e := range bounds {
			for de := int64(-1); de <= 1; de++ {
				rs = append(rs, Range{s, e + de - s})
			}
		}
	}
	return rs
}

func TestRunIndexAgreesWithLinearTranslation(t *testing.T) {
	rng := rand.New(rand.NewSource(70)) //nolint:gosec // deterministic test data
	for c := range 5000 {
		runs, sum := randomRuns(rng)
		incomplete := rng.Intn(2) == 0
		size := sum
		switch rng.Intn(3) {
		case 1:
			size = max(sum-3, 0)
		case 2:
			size = sum + 2
		}
		rs := edgeRanges(runs, size)
		for range 20 {
			o := rng.Int63n(size + 1)
			rs = append(rs, Range{o, rng.Int63n(size - o + 2)})
		}
		if c%10 != 0 { // all edge ranges for one case in ten, a sample for the rest
			rng.Shuffle(len(rs), func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
			rs = rs[:60]
		}
		for _, r := range rs {
			sameResult(t, runs, size, incomplete, r)
		}
	}
}

func TestRunIndexRefusalsAgree(t *testing.T) {
	bad := [][]evidence.Run{
		{{Offset: -2, Length: 3}},
		{{Offset: 0, Length: 0}},
		{{Offset: 0, Length: -1}},
		{{Offset: maxInt64 - 1, Length: 5}},
		{{Offset: -1, Length: maxInt64}, {Offset: -1, Length: 1}},
		nil,
	}
	for _, runs := range bad {
		for _, inc := range []bool{false, true} {
			for _, size := range []int64{0, 1, 5} {
				sameResult(t, runs, size, inc, Range{0, 0})
				sameResult(t, runs, size, inc, Range{0, 1})
			}
		}
	}
}

func FuzzRunIndexAgreesWithLinear(f *testing.F) {
	f.Add(int64(1), uint8(10), int64(3), int64(7), true)
	f.Add(int64(2), uint8(64), int64(0), int64(0), false)
	f.Add(int64(3), uint8(1), int64(-1), int64(2), true)
	f.Fuzz(func(t *testing.T, seed int64, n uint8, off, length int64, incomplete bool) {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
		runs, _ := randomRuns(rng)
		if k := int(n)%64 + 1; k < len(runs) {
			runs = runs[:k]
		}
		var sum int64
		for _, r := range runs {
			sum += r.Length
		}
		size := max(sum+seed%5-2, 0)
		sameResult(t, runs, size, incomplete, Range{off, length})
		rs := edgeRanges(runs, size)
		for range 30 {
			sameResult(t, runs, size, incomplete, rs[rng.Intn(len(rs))])
		}
	})
}

func TestRunIndexBoundaries(t *testing.T) {
	runs := make([]evidence.Run, 300)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(1000 + 2*i), Length: 1}
	}
	x, err := newRunIndex(runs, 300, false)
	if err != nil {
		t.Fatal(err)
	}
	all, err := x.translate(Range{0, 300})
	if err != nil || all.Total != 300 || len(all.Extents) != MaxExtentsPerHop || !all.Truncated {
		t.Fatalf("%+v %v", all, err)
	}
	// a range starting exactly at the end of run 5 (offset 6) starts in run 6
	from, err := x.translate(Range{6, 3})
	if err != nil || from.Total != 3 || from.Extents[0].ArtifactOffset != 6 || from.Extents[0].ImageOffset != 1012 {
		t.Fatalf("%+v %v", from, err)
	}
	last, err := x.translate(Range{299, 1})
	if err != nil || len(last.Extents) != 1 || last.Extents[0].ImageOffset != 1598 {
		t.Fatalf("%+v %v", last, err)
	}
}

func TestRunIndexWorkIsLogarithmic(t *testing.T) {
	const n = 1_000_000
	runs := make([]evidence.Run, n)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(i) * 2, Length: 1}
	}
	x, err := newRunIndex(runs, n, false)
	if err != nil {
		t.Fatal(err)
	}
	x.steps = 0
	var ranges []Range
	var got []Translation
	for i := range 256 {
		r := Range{Offset: int64(i) * (n / 256), Length: 1}
		tr, err := x.translate(r)
		if err != nil || tr.Total != 1 || len(tr.Extents) != 1 || tr.Extents[0].ImageOffset != r.Offset*2 {
			t.Fatalf("range %+v: %+v %v", r, tr, err)
		}
		ranges, got = append(ranges, r), append(got, tr)
	}
	if limit := 256 * (2*21 + 1); x.steps > limit {
		t.Fatalf("%d steps for 256 one-byte ranges over %d runs, want at most %d", x.steps, n, limit)
	}
	for _, i := range []int{0, 255} {
		want, _ := translateLinear(runs, n, false, ranges[i])
		if !reflect.DeepEqual(got[i], want) {
			t.Fatalf("%+v vs %+v", got[i], want)
		}
	}
}
