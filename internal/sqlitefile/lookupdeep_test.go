package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// deepTable builds a table of depth 4 (512-byte pages, 60-byte rows).
func deepTable(t *testing.T) (*sqlitetest.Builder, *sqlitetest.Table, int64) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	const n = 30000
	for i := int64(1); i <= n; i++ {
		tb.Insert(i, bytes.Repeat([]byte{byte(i)}, 60))
	}
	if tb.Depth() < 4 {
		t.Fatalf("depth %d, want at least 4", tb.Depth())
	}
	return b, tb, n
}

// TestLookupNeverAbsentOnAnUnsortedAncestor: every level of the search path
// must prove its order, not only the leaf's parent. A depth-3 table with an
// unsorted ROOT and a depth-4 table with an unsorted ROOT and with an unsorted
// MIDDLE level (a page that is neither the root nor the leaf's parent): every
// rowid exists, so no lookup may answer a clean absent. (Mutation "prove only
// the immediate parent" answered 1229 false absents for the depth-3 root.)
func TestLookupNeverAbsentOnAnUnsortedAncestor(t *testing.T) {
	check := func(t *testing.T, data []byte, root uint32, n int64, step int64) {
		t.Helper()
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		uncertain := 0
		for id := int64(1); id <= n; id += step {
			_, ok, err := v.LookupRowid(context.Background(), root, id)
			switch {
			case err == nil && !ok:
				t.Fatalf("Get(%d): a clean absent for a row that exists", id)
			case err != nil && !errors.Is(err, sqlitefile.ErrCorrupt):
				t.Fatalf("Get(%d): %v", id, err)
			case err != nil:
				uncertain++
			}
		}
		if uncertain == 0 {
			t.Error("no lookup reported the damaged page")
		}
	}
	t.Run("depth 3, unsorted root", func(t *testing.T) {
		b, tb := bigTable(t)
		data := b.Bytes()
		swapPointers(data, 512, tb.Root(), 0, 1)
		check(t, data, tb.Root(), 5000, 1)
	})
	t.Run("depth 4, unsorted root", func(t *testing.T) {
		b, tb, n := deepTable(t)
		data := b.Bytes()
		swapPointers(data, 512, tb.Root(), 0, 1)
		check(t, data, tb.Root(), n, 7)
	})
	t.Run("depth 4, unsorted middle level", func(t *testing.T) {
		b, tb, n := deepTable(t)
		data := b.Bytes()
		root := pageAt(data, 512, tb.Root())
		off := cellOffsets(root)[0]
		mid := vtU32(root[off:]) // left child of the root's first cell: a middle interior page
		if mid == 0 || pageAt(data, 512, mid)[0] != 0x05 {
			t.Fatalf("page %d is not an interior page", mid)
		}
		if len(cellOffsets(pageAt(data, 512, mid))) < 2 {
			t.Fatal("middle page too small")
		}
		swapPointers(data, 512, mid, 0, 1)
		// the leaf's parent lies below mid; the swap must be seen from there
		check(t, data, tb.Root(), n, 5)
	})
}
