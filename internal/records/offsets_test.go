package records

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func rs(pairs ...int64) []evidence.Run {
	var out []evidence.Run
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, evidence.Run{Offset: pairs[i], Length: pairs[i+1]})
	}
	return out
}

func ext(a, l, img int64) ImageExtent {
	return ImageExtent{ArtifactOffset: a, Length: l, ImageOffset: img, Hole: img < 0}
}

func TestTranslateRangeTable(t *testing.T) {
	many := func(n int) []evidence.Run {
		var out []evidence.Run
		for i := 0; i < n; i++ {
			out = append(out, evidence.Run{Offset: int64(i) * 100, Length: 10})
		}
		return out
	}
	tests := []struct {
		name  string
		runs  []evidence.Run
		size  int64
		r     Range
		want  []ImageExtent
		total int
	}{
		{"one run", rs(1000, 100), 100, Range{10, 20}, []ImageExtent{ext(10, 20, 1010)}, 1},
		{"first run", rs(1000, 50, 5000, 50), 100, Range{0, 50}, []ImageExtent{ext(0, 50, 1000)}, 1},
		{"spanning", rs(1000, 50, 5000, 50), 100, Range{40, 20}, []ImageExtent{ext(40, 10, 1040), ext(50, 10, 5000)}, 2},
		{"boundaries", rs(1000, 50, 5000, 50), 100, Range{50, 50}, []ImageExtent{ext(50, 50, 5000)}, 1},
		{"ends at end", rs(1000, 100), 100, Range{99, 1}, []ImageExtent{ext(99, 1, 1099)}, 1},
		{"len0 at 0", rs(1000, 100), 100, Range{0, 0}, nil, 0},
		{"len0 middle", rs(1000, 100), 100, Range{50, 0}, nil, 0},
		{"len0 at end", rs(1000, 100), 100, Range{100, 0}, nil, 0},
		{"hole", rs(-1, 100), 100, Range{10, 5}, []ImageExtent{ext(10, 5, -1)}, 1},
		{
			"run hole run", rs(1000, 10, -1, 10, 3000, 10), 30,
			Range{5, 20},
			[]ImageExtent{ext(5, 5, 1005), ext(10, 10, -1), ext(20, 5, 3000)},
			3,
		},
		{
			"contiguous not merged", rs(1000, 10, 1010, 10), 20,
			Range{0, 20},
			[]ImageExtent{ext(0, 10, 1000), ext(10, 10, 1010)},
			2,
		},
		{
			"overlapping image runs (E18)", rs(1000, 10, 1005, 10), 20,
			Range{0, 20},
			[]ImageExtent{ext(0, 10, 1000), ext(10, 10, 1005)},
			2,
		},
		{
			"5000 fragments", many(5000), 50000,
			Range{25000, 30},
			[]ImageExtent{ext(25000, 10, 250000), ext(25010, 10, 250100), ext(25020, 10, 250200)},
			3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TranslateRange(tc.runs, tc.size, false, tc.r)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Extents, tc.want) && !(len(got.Extents) == 0 && len(tc.want) == 0) {
				t.Fatalf("extents %+v want %+v", got.Extents, tc.want)
			}
			if got.Total != tc.total || got.Truncated || got.RunsExceedSize {
				t.Fatalf("total %d truncated %v exceed %v", got.Total, got.Truncated, got.RunsExceedSize)
			}
		})
	}
	t.Run("300 fragments", func(t *testing.T) {
		got, err := TranslateRange(many(300), 3000, false, Range{0, 3000})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Extents) != MaxExtentsPerHop || MaxExtentsPerHop != 256 || got.Total != 300 || !got.Truncated {
			t.Fatalf("len %d total %d truncated %v", len(got.Extents), got.Total, got.Truncated)
		}
		if got.Extents[255] != ext(2550, 10, 25500) {
			t.Fatalf("last kept %+v", got.Extents[255])
		}
	})
}

func checkRefusal(t *testing.T, got Translation, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted: %+v", got)
	}
	if !errors.Is(err, ErrOffsetUnavailable) || !strings.HasPrefix(err.Error(), "image offset unavailable") {
		t.Fatalf("error %q does not wrap ErrOffsetUnavailable", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q lacks %q", err, want)
	}
	if !reflect.DeepEqual(got, Translation{}) {
		t.Fatalf("partial result returned: %+v", got)
	}
}

func TestTranslateRangeRefusals(t *testing.T) {
	M := int64(math.MaxInt64)
	tests := []struct {
		name string
		runs []evidence.Run
		size int64
		r    Range
		want string
	}{
		{"one past end", rs(0, 100), 100, Range{99, 2}, "range lies outside the artifact (99+2 > 100)"},
		{"start at size len 1", rs(0, 100), 100, Range{100, 1}, "range lies outside the artifact (100+1 > 100)"},
		{"negative offset", rs(0, 100), 100, Range{-1, 1}, "range is negative or overflows"},
		{"negative length", rs(0, 100), 100, Range{0, -1}, "range is negative or overflows"},
		{"offset maxint", rs(0, 100), 100, Range{M, 1}, "range is negative or overflows"},
		{"offset+length overflow", rs(0, 100), 100, Range{5, M}, "range is negative or overflows"},
		{"run start -2", rs(-2, 100), 100, Range{0, 1}, "runs are invalid: "},
		{"run length 0", rs(0, 0, 0, 100), 100, Range{0, 1}, "runs are invalid: "},
		{"run length negative", rs(0, -5), 100, Range{0, 1}, "runs are invalid: "},
		{"run end overflow", rs(M, 2), 2, Range{0, 1}, "runs are invalid: "},
		{"length sum overflow", rs(0, M, 0, M, 0, 2), 100, Range{0, 1}, "runs are invalid: "},
		{"complete shorter", rs(0, 60), 100, Range{0, 1}, "runs cover 60 bytes but the artifact has 100"},
		{"complete longer", rs(0, 160), 100, Range{0, 1}, "runs cover 160 bytes but the artifact has 100"},
		{"no runs size>0", nil, 100, Range{0, 0}, "runs cover 0 bytes but the artifact has 100"},
		{"no runs size 0 one byte", nil, 0, Range{0, 1}, "range lies outside the artifact (0+1 > 0)"},
		{"straddles end after extents", rs(0, 50, 100, 50), 100, Range{90, 20}, "range lies outside the artifact"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TranslateRange(tc.runs, tc.size, false, tc.r)
			checkRefusal(t, got, err, tc.want)
		})
	}
	t.Run("no runs size 0 empty range", func(t *testing.T) {
		got, err := TranslateRange(nil, 0, false, Range{0, 0})
		if err != nil || got.Total != 0 || len(got.Extents) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestTranslateRangeIncompleteArtifact(t *testing.T) {
	t.Run("cancelled copy", func(t *testing.T) {
		runs := rs(5000, 400, 9000, 600)
		got, err := TranslateRange(runs, 100, true, Range{50, 50})
		if err != nil || !got.RunsExceedSize || !reflect.DeepEqual(got.Extents, []ImageExtent{ext(50, 50, 5050)}) {
			t.Fatalf("%+v %v", got, err)
		}
		got, err = TranslateRange(runs, 100, true, Range{50, 51})
		checkRefusal(t, got, err, "range lies outside the artifact (50+51 > 100)")
	})
	t.Run("ends at 101", func(t *testing.T) {
		got, err := TranslateRange(rs(0, 1000), 100, true, Range{100, 1})
		checkRefusal(t, got, err, "range lies outside the artifact")
	})
	t.Run("sum smaller", func(t *testing.T) {
		got, err := TranslateRange(rs(0, 60), 100, true, Range{0, 10})
		checkRefusal(t, got, err, "runs cover only 60 of the artifact's 100 bytes")
	})
	t.Run("sum equals size", func(t *testing.T) {
		got, err := TranslateRange(rs(0, 100), 100, true, Range{0, 100})
		if err != nil || got.RunsExceedSize || got.Total != 1 {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("partial unallocated export order", func(t *testing.T) {
		runs := rs(8000, 300, 100, 700)
		got, err := TranslateRange(runs, 100, true, Range{250, 1})
		checkRefusal(t, got, err, "range lies outside the artifact")
		got, err = TranslateRange(runs, 100, true, Range{0, 100})
		if err != nil || !got.RunsExceedSize || !reflect.DeepEqual(got.Extents, []ImageExtent{ext(0, 100, 8000)}) {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestTranslateRangeNeverReturnsPartial(t *testing.T) {
	// A refusal found only when the walk reaches the end of the runs must
	// still return the zero Translation (checkRefusal asserts it).
	got, err := TranslateRange(rs(0, 50, 100, 50), 100, false, Range{90, 20})
	checkRefusal(t, got, err, "range lies outside the artifact")
}
