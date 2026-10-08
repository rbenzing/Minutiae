package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
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
