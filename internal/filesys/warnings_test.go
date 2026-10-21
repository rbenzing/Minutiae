package filesys_test

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

func TestWarningsZeroValueAndDedup(t *testing.T) {
	var w filesys.Warnings
	if got := w.Snapshot(); len(got) != 0 {
		t.Fatalf("zero value Snapshot = %v, want empty", got)
	}
	w.Add("literal text") // no args: used as is
	w.Add("block %d bad", 3)
	w.Add("block %d bad", 3) // duplicate text
	w.Add("block %d bad", 4)
	w.Add("literal text")
	want := []string{"literal text", "block 3 bad", "block 4 bad"}
	if got := w.Snapshot(); !slices.Equal(got, want) {
		t.Fatalf("Snapshot = %q, want %q", got, want)
	}
}

func TestWarningsSnapshotIsACopy(t *testing.T) {
	var w filesys.Warnings
	w.Add("a")
	s := w.Snapshot()
	s[0] = "changed"
	w.Add("b")
	if got := w.Snapshot(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Snapshot = %q after the caller modified an earlier snapshot", got)
	}
}

func TestWarningsCap(t *testing.T) {
	var w filesys.Warnings
	for i := range filesys.MaxWarnings + 50 {
		w.Add("warning %d", i)
	}
	got := w.Snapshot()
	if len(got) != filesys.MaxWarnings+1 {
		t.Fatalf("len = %d, want %d (cap plus one suppressed line)", len(got), filesys.MaxWarnings+1)
	}
	if got[filesys.MaxWarnings-1] != fmt.Sprintf("warning %d", filesys.MaxWarnings-1) {
		t.Errorf("last kept = %q", got[filesys.MaxWarnings-1])
	}
	if got[filesys.MaxWarnings] != "further warnings suppressed: 50 occurrences not recorded" {
		t.Errorf("last = %q, want the note counting 50", got[filesys.MaxWarnings])
	}
	// More distinct warnings, and repeats of kept ones, change nothing.
	w.Add("another new one")
	w.Add("warning 0")
	if n := len(w.Snapshot()); n != filesys.MaxWarnings+1 {
		t.Fatalf("len = %d after more adds, want %d", n, filesys.MaxWarnings+1)
	}
}

func TestWarningsConcurrent(t *testing.T) {
	var w filesys.Warnings
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				w.Add("shared %d", i%100) // heavy duplication across goroutines
				w.Add("own %d/%d", g, i)  // 2400 distinct: forces the cap
				_ = w.Snapshot()
			}
		}()
	}
	wg.Wait()
	got := w.Snapshot()
	if len(got) != filesys.MaxWarnings+1 || !strings.HasPrefix(got[len(got)-1], filesys.SuppressedWarning+": ") {
		t.Fatalf("len = %d, last = %q; want %d entries ending in the suppressed line", len(got), got[len(got)-1], filesys.MaxWarnings+1)
	}
	seen := map[string]bool{}
	for _, m := range got {
		if seen[m] {
			t.Fatalf("duplicate warning %q", m)
		}
		seen[m] = true
	}
}

func TestWarningsCountsSuppressed(t *testing.T) {
	var w filesys.Warnings
	for i := range filesys.MaxWarnings + 50 {
		w.Add("warning %d", i)
	}
	got := w.Snapshot()
	if len(got) != filesys.MaxWarnings+1 {
		t.Fatalf("len = %d, want %d", len(got), filesys.MaxWarnings+1)
	}
	last := got[len(got)-1]
	if last != "further warnings suppressed: 50 occurrences not recorded" {
		t.Fatalf("note = %q", last)
	}
	if n, ok := filesys.ParseSuppressedNote(last); n != 50 || !ok {
		t.Errorf("ParseSuppressedNote = (%d, %v), want (50, true)", n, ok)
	}
	for i := range 7 {
		w.Add("more %d", i)
	}
	w.Add("warning 0") // a repeat of a kept message is a duplicate, not a loss
	w.Add("warning 1")
	w.Add("warning 2")
	got = w.Snapshot()
	if len(got) != filesys.MaxWarnings+1 || got[len(got)-1] != "further warnings suppressed: 57 occurrences not recorded" {
		t.Fatalf("len %d, last %q; want 57", len(got), got[len(got)-1])
	}
	if n, ok := filesys.ParseSuppressedNote(got[len(got)-1]); n != 57 || !ok {
		t.Errorf("ParseSuppressedNote = (%d, %v), want (57, true)", n, ok)
	}
}

func TestWarningsRepeatOfDroppedMessageIsCounted(t *testing.T) {
	var w filesys.Warnings
	for i := range filesys.MaxWarnings {
		w.Add("kept %d", i)
	}
	w.Add("dropped")
	w.Add("dropped")
	got := w.Snapshot()
	if last := got[len(got)-1]; last != "further warnings suppressed: 2 occurrences not recorded" {
		t.Errorf("note = %q, want a count of 2", last)
	}
}

func TestWarningsConcurrentCount(t *testing.T) {
	var w filesys.Warnings
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				w.Add("shared %d", i%100)
				w.Add("own %d/%d", g, i)
			}
		}()
	}
	wg.Wait()
	// The count is deterministic only because all 100 shared messages are added in iterations 0-99 of
	// every goroutine, before the goroutines can together have added the 900 own messages that fill
	// the list: the 1000 kept are 100 shared + 900 own, so 2400 own adds - 900 kept own are dropped.
	// Do not change the shape of the loop without redoing this argument.
	const want = 2400 - (1000 - 100)
	got := w.Snapshot()
	n, ok := filesys.ParseSuppressedNote(got[len(got)-1])
	if len(got) != filesys.MaxWarnings+1 || !ok || n != want {
		t.Fatalf("len %d, last %q (%d, %v); want a count of %d", len(got), got[len(got)-1], n, ok, want)
	}
}

func TestSuppressedNoteRoundTrip(t *testing.T) {
	for _, n := range []uint64{1, 2, 9, 10, 1000, math.MaxUint64} {
		got, ok := filesys.ParseSuppressedNote(filesys.SuppressedNote(n))
		if !ok || got != n {
			t.Errorf("round trip of %d = (%d, %v)", n, got, ok)
		}
	}
	if got := filesys.SuppressedNote(3); got != "further warnings suppressed: 3 occurrences not recorded" {
		t.Errorf("SuppressedNote(3) = %q", got)
	}
}

func TestParseSuppressedNoteRejects(t *testing.T) {
	const p, s = "further warnings suppressed: ", " occurrences not recorded"
	for _, in := range []string{
		p + "0" + s, p + "-1" + s, p + "+3" + s, p + "03" + s, p + " 3" + s, p + "3 " + s,
		p + "3" + s + " extra", p + "3 occurrences not recorded.", p + "3",
		"other prefix: 3" + s, p + "18446744073709551616" + s, p + "" + s, "", filesys.SuppressedWarning,
		p + "3" + "\n" + s, p + "３" + s,
	} {
		if n, ok := filesys.ParseSuppressedNote(in); ok {
			t.Errorf("ParseSuppressedNote(%q) = (%d, true), want false", in, n)
		}
	}
}
