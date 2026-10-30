package iossms

import (
	"testing"
	"time"
)

// The chat ids of one message are deduplicated in about linear time: a hostile join with a million rows of
// distinct chat ids must not cost a quadratic scan.
func TestUniqueSortedIsNotQuadratic(t *testing.T) {
	const n = 1_000_000
	ids := make([]int64, 0, 2*n)
	for i := int64(n); i >= 1; i-- {
		ids = append(ids, i, i)
	}
	start := time.Now()
	got := uniqueSorted(ids)
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("uniqueSorted of %d ids took %v", len(ids), el)
	}
	if len(got) != n || got[0] != 1 || got[n-1] != n {
		t.Errorf("uniqueSorted: %d ids, first %v last %v", len(got), got[0], got[len(got)-1])
	}
	if got := uniqueSorted(nil); len(got) != 0 {
		t.Errorf("uniqueSorted(nil) = %v", got)
	}
}
