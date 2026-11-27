package androidtelegram

import (
	"bytes"
	"strconv"
	"testing"
	"time"
)

// Ruling T5a: a stored name with no bytes is "empty"; a stored name of bytes the cleaning reduces to nothing is
// "unprintable" (it is never stated as absent or empty). Cleaning at maxName never empties a non-empty name, so the
// state is pinned on its decision function.
func TestNoNameStateSeparatesEmptyFromUnprintable(t *testing.T) {
	for _, c := range []struct {
		stored []byte
		want   string
	}{
		{nil, nameEmpty},
		{[]byte{}, nameEmpty},
		{[]byte{0}, nameUnprintable},
		{[]byte("\xff\xfe"), nameUnprintable},
		// no printable name here: noNameState is called only after cleaning gave "" (final-review item 1)
	} {
		if got := noNameState(c.stored); got != c.want {
			t.Errorf("noNameState(%q) = %q, want %q", c.stored, got, c.want)
		}
	}
}

// Ruling T5b: the distinct-name check is linear: 100000 filename attributes (50000 distinct) keep wire order and finish
// far inside the bound an O(n^2) scan (about 5e9 comparisons) cannot meet.
func TestDistinctNamesAreLinear(t *testing.T) {
	var values [][]byte
	for i := range 100000 {
		values = append(values, []byte("n"+strconv.Itoa(i%50000)))
	}
	start := time.Now()
	got := distinctBytes(values)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("distinctBytes took %v for 100000 names; want linear", d)
	}
	if len(got) != 50000 || !bytes.Equal(got[0], []byte("n0")) || !bytes.Equal(got[49999], []byte("n49999")) {
		t.Errorf("distinctBytes kept %d names, first %q last %q; want 50000 in first-occurrence order", len(got), got[0], got[len(got)-1])
	}
}
