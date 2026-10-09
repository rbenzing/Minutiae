package evidence

import (
	"slices"
	"testing"
	"time"
)

// A v1 case holds no records: verify reports the phases that exist for it once each, in order, and
// none of the record phases that need the v2 tables.
func TestVerifyPhasesOfAV1CaseAreOnceEachInOrder(t *testing.T) {
	c := openV1Case(t)
	var seen []string
	c.verifyPhaseHook = func(name string, d time.Duration) {
		if d < 0 {
			t.Errorf("phase %s has a negative duration", name)
		}
		seen = append(seen, name)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %v", err, rep.Problems)
	}
	if want := []string{"audit", "artifacts", "records.schema"}; !slices.Equal(seen, want) {
		t.Fatalf("phases %v, want %v", seen, want)
	}
}
