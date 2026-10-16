package evidence_test

import (
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestRunsSum(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []evidence.Run
		sum  int64
		err  string
	}{
		{"empty", nil, 0, ""},
		{"holes and data", []evidence.Run{{Offset: -1, Length: 4}, {Offset: 7, Length: 5}}, 9, ""},
		{"offset below -1", []evidence.Run{{Offset: -2, Length: 1}}, 0, "run 0 is not valid"},
		{"zero length", []evidence.Run{{Offset: 0, Length: 1}, {Offset: 0, Length: 0}}, 0, "run 1 is not valid"},
		{"negative length", []evidence.Run{{Offset: 0, Length: -3}}, 0, "run 0 is not valid"},
		{"end beyond int64", []evidence.Run{{Offset: math.MaxInt64 - 1, Length: 5}}, 0, "ends beyond the largest offset"},
		{"sum beyond int64", []evidence.Run{{Offset: -1, Length: math.MaxInt64}, {Offset: -1, Length: 1}}, 0, "add up to more than"},
		{"exactly int64", []evidence.Run{{Offset: -1, Length: math.MaxInt64 - 1}, {Offset: -1, Length: 1}}, math.MaxInt64, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evidence.RunsSum(tc.runs)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.sum {
				t.Fatalf("sum %d, error %v, want %d", got, err, tc.sum)
			}
		})
	}
}
