package sqlitefile_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// coldReads is the number of page images one lookup fetches on a fresh view.
func coldReads(t *testing.T, data []byte, key sqlitefile.Value, want string) int64 {
	t.Helper()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	tbl, err := v.Table(context.Background(), "wt")
	if err != nil {
		t.Fatal(err)
	}
	before := v.Stats().PageReads
	if got := getKey(t, tbl, key); got.what != want {
		t.Fatalf("%q: %s %v, want %s", key.Bytes, got.what, got.err, want)
	}
	return v.Stats().PageReads - before
}

// The pages a lookup reads are the path (one per level), plus the adjacent leaf when a miss
// ends at the end of a leaf, and the memory it charged goes back to the budget.
func TestGetByKeyDoesNotChargeBeyondThePath(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	depth := int64(tb.Depth())
	if n := coldReads(t, data, txtKey(keyName(10+2*1000)), "row"); n > depth {
		t.Errorf("a hit read %d pages on a tree of depth %d", n, depth)
	}
	if n := coldReads(t, data, txtKey(keyName(10+2*1500+1)), "absent"); n > depth {
		t.Errorf("a miss read %d pages on a tree of depth %d", n, depth)
	}
	_, l0, _ := pathPages(t, tb, data)
	p0 := pageAt(data, 512, l0)
	off := keyByteOffset(p0, len(cellOffsets(p0))-1)
	var last int
	if _, err := fmt.Sscanf(string(p0[off:off+6]), "k%d", &last); err != nil {
		t.Fatal(err)
	}
	if n := coldReads(t, data, txtKey(keyName(last+1)), "absent"); n > depth+1 {
		t.Errorf("a miss at the end of a leaf read %d pages on a tree of depth %d, want at most depth+1", n, depth)
	}
	rb := newRecBudget(256 << 20)
	_, v := openLive(t, data, sqlitefile.Options{Budget: rb})
	tbl, err := v.Table(context.Background(), "wt")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i += 37 {
		if got := getKey(t, tbl, txtKey(keyName(10+2*i))); got.what != "row" {
			t.Fatalf("%s", got.what)
		}
		if got := getKey(t, tbl, txtKey(keyName(11+2*i))); got.what != "absent" {
			t.Fatalf("%s", got.what)
		}
	}
	v.Release()
	rb.check(t)
}
