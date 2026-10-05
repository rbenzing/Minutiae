package sqlitefile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// swapPointers exchanges cell pointers i and j of page pgno.
func swapPointers(data []byte, ps int, pgno uint32, i, j int) {
	p := pageAt(data, ps, pgno)
	hdr := 8
	if p[0] == 0x02 || p[0] == 0x05 {
		hdr = 12
	}
	a, b := p[hdr+2*i:hdr+2*i+2], p[hdr+2*j:hdr+2*j+2]
	a[0], a[1], b[0], b[1] = b[0], b[1], a[0], a[1]
}

// TestLookupNeverAbsentOnAnUnsortedLeaf: a leaf whose cell pointers are out of
// order is not proof of absence. The reviewer's case: five rows, pointers 2
// and 3 swapped; the binary search for 3 misses although the scan delivers it.
func TestLookupNeverAbsentOnAnUnsortedLeaf(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 5)
	data := b.Bytes()
	swapPointers(data, 512, tb.Root(), 1, 2)
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	found := map[int64]bool{}
	for _, r := range scanRows(t, v, tb.Root(), sqlitefile.TableTree) {
		found[r.Rowid] = true
	}
	if !found[3] {
		t.Fatal("the scan must deliver rowid 3")
	}
	_, ok, err := v.LookupRowid(context.Background(), tb.Root(), 3)
	if ok {
		return // found: fine
	}
	if !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Fatalf("Get(3) on an unsorted leaf: ok %v err %v, want uncertain (ErrCorrupt), never absent", ok, err)
	}
}

// TestLookupNeverAbsentOnAnUnsortedInteriorPage: the same for an interior page.
// Every rowid exists, so no lookup may answer a clean absent.
func TestLookupNeverAbsentOnAnUnsortedInteriorPage(t *testing.T) {
	b, tb := bigTable(t)
	data := b.Bytes()
	pg := midInterior(t, tb, data)
	swapPointers(data, 512, pg, 1, 2)
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	uncertain := 0
	for id := int64(1); id <= 5000; id++ {
		_, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
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
		t.Error("no lookup reported the damaged interior page")
	}
}
