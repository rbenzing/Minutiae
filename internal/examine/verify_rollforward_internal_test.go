package examine

import (
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

func TestRunsArePrefix(t *testing.T) {
	f := []evidence.Run{{Offset: 100, Length: 10}, {Offset: 300, Length: 20}}
	r := func(o, l int64) evidence.Run { return evidence.Run{Offset: o, Length: l} }
	for _, tc := range []struct {
		name string
		rec  []evidence.Run
		want bool
	}{
		{"empty", nil, true},
		{"whole", []evidence.Run{r(100, 10), r(300, 20)}, true},
		{"first run only", []evidence.Run{r(100, 10)}, true},
		{"cut inside the last run", []evidence.Run{r(100, 10), r(300, 5)}, true},
		{"cut inside the first run", []evidence.Run{r(100, 4)}, true},
		{"cut inside a run then another", []evidence.Run{r(100, 4), r(300, 20)}, false},
		{"shifted", []evidence.Run{r(104, 6)}, false},
		{"skips a run", []evidence.Run{r(300, 20)}, false},
		{"longer than the map", []evidence.Run{r(100, 10), r(300, 21)}, false},
		{"more runs than the map", []evidence.Run{r(100, 10), r(300, 20), r(400, 1)}, false},
		{"a hole", []evidence.Run{r(-1, 10)}, false},
	} {
		if got := runsArePrefix(tc.rec, f); got != tc.want {
			t.Errorf("%s: runsArePrefix = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFreshRunsOverflowIsRefused(t *testing.T) {
	if got, ok := freshRuns(100, []filesys.Run{{Offset: 5, Length: 1}, {Offset: -1, Length: 2}}); !ok || got[0].Offset != 105 || got[1].Offset != -1 {
		t.Errorf("ordinary runs: %v %v", got, ok)
	}
	if _, ok := freshRuns(100, []filesys.Run{{Offset: math.MaxInt64 - 50, Length: 1}}); ok {
		t.Error("an overflowing run was accepted")
	}
	if runsArePrefix([]evidence.Run{{Offset: 0, Length: 1}}, []evidence.Run{{Offset: math.MaxInt64, Length: 1}}) {
		t.Error("a run at MaxInt64 matched")
	}
}
