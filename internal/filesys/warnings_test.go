package filesys_test

import (
	"fmt"
	"slices"
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
	if got[filesys.MaxWarnings] != filesys.SuppressedWarning {
		t.Errorf("last = %q, want the suppressed line", got[filesys.MaxWarnings])
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
	if len(got) != filesys.MaxWarnings+1 || got[len(got)-1] != filesys.SuppressedWarning {
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
