package filesys

import (
	"math"
	"testing"
)

func TestWarningsSuppressedCountSaturates(t *testing.T) {
	var w Warnings
	for i := range MaxWarnings {
		w.Add("kept %d", i)
	}
	w.dropped = math.MaxUint64 - 1
	w.Add("over a")
	w.Add("over b")
	w.Add("over c")
	got := w.Snapshot()
	if len(got) != MaxWarnings+1 {
		t.Fatalf("len = %d, want %d", len(got), MaxWarnings+1)
	}
	last := got[MaxWarnings]
	if n, ok := ParseSuppressedNote(last); !ok || n != math.MaxUint64 {
		t.Fatalf("note %q parses to (%d, %v), want (MaxUint64, true)", last, n, ok)
	}
	if want := "further warnings suppressed: 18446744073709551615 occurrences not recorded"; last != want {
		t.Errorf("note = %q, want %q", last, want)
	}
}
