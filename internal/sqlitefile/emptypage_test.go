package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestEmptyNonRootPageIsDamage: the engine calls an empty non-root b-tree
// page (and an empty interior root) corrupt. A scan warns and a lookup that
// passes through such a page is never a clean absent (final review A, F1).
func TestEmptyNonRootPageIsDamage(t *testing.T) {
	zeroCount := func(f *treeFixture, data []byte, pg uint32) {
		binary.BigEndian.PutUint16(pageAt(data, f.ps, pg)[3:], 0)
	}
	cases := []struct {
		name string
		page func(f *treeFixture) uint32
	}{
		{"interior root", func(f *treeFixture) uint32 { return f.tb.Root() }},
		{"leaf", func(f *treeFixture) uint32 { return f.tb.Leaves()[1] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTreeFixture(t, 300, false)
			data := f.b.Bytes()
			pg := c.page(f)
			zeroCount(f, data, pg)
			_, v := openLive(t, data, sqlitefile.Options{})
			scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
			if !viewWarns(v, sqlitefile.WarnBTreeShape, pg) {
				t.Errorf("no %s warning for page %d: %v", sqlitefile.WarnBTreeShape, pg, v.Warnings())
			}
			// Every rowid: found, or an uncertain error; never a clean absent
			// while the engine-valid row was on the damaged path.
			for id := int64(1); id <= int64(f.n); id++ {
				row, ok, err := v.LookupRowid(context.Background(), f.tb.Root(), id)
				if err == nil && !ok {
					t.Fatalf("rowid %d: clean absent through the empty page %d", id, pg)
				}
				if err != nil && !errors.Is(err, sqlitefile.ErrCorrupt) {
					t.Fatalf("rowid %d: %v", id, err)
				}
				if ok && row.Rowid != id {
					t.Fatalf("rowid %d: got %d", id, row.Rowid)
				}
			}
		})
	}
	t.Run("an empty leaf root is a valid empty table", func(t *testing.T) {
		f := newTreeFixture(t, 0, false)
		_, v := openLive(t, f.b.Bytes(), sqlitefile.Options{})
		scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		if viewWarns(v, sqlitefile.WarnBTreeShape, 0) {
			t.Errorf("warnings %v", v.Warnings())
		}
		if _, ok, err := v.LookupRowid(context.Background(), f.tb.Root(), 1); ok || err != nil {
			t.Errorf("ok %v err %v", ok, err)
		}
	})
}

// TestEmptyAdjacentLeafMakesAMissUncertain: a miss at the edge of a leaf is
// proved against the neighbouring leaf; when that leaf is empty (corrupt for the
// engine) the answer is uncertain, not a clean absent. Rowids are 10 apart, so
// the missing rowid lies between two leaves.
func TestEmptyAdjacentLeafMakesAMissUncertain(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 300; i++ {
		tb.Insert(i*10, "padpadpadpadpadpadpadpad")
	}
	data := b.Bytes()
	leaves := tb.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	var maxMid int64
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := tb.CellBytes(i * 10); pg == leaves[1] {
			maxMid = i * 10
		}
	}
	binary.BigEndian.PutUint16(pageAt(data, 512, leaves[1])[3:], 0)
	_, v := openLive(t, data, sqlitefile.Options{})
	// maxMid+5 is above the separator of the emptied leaf: the search ends at
	// the start of the next leaf, whose lower neighbour is the empty one
	if _, ok, err := v.LookupRowid(context.Background(), tb.Root(), maxMid+5); ok || !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("miss next to an empty leaf: ok %v err %v, want ErrCorrupt", ok, err)
	}
}
