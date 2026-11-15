package examine

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
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
