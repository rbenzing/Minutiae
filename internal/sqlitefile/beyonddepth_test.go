package sqlitefile_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// chainLeafDepth is the depth of the leaf holding rowid r in chainTree(n): leaf
// r (1..n) is the left child of interior r, the last leaf (rowid n+1) is the
// right child of interior n.
func chainLeafDepth(n int, r int64) int { return int(min(r+1, int64(n+1))) }

// engineRefusesRow asks the independent engine for one row by rowid.
func engineRefusesRow(t *testing.T, db *sql.DB, rowid int64) bool {
	t.Helper()
	var a sql.NullString
	err := db.QueryRow("select a from t where rowid = ?", rowid).Scan(&a)
	return err != nil && !errors.Is(err, sql.ErrNoRows)
}

// TestRowsBeyondEngineDepthAreFlagged: a row whose leaf lies deeper than the
// engine cursor (20 levels) carries BeyondEngineDepth; the engine, asked row
// by row, is the oracle (L108 item 6, ruling U4).
func TestRowsBeyondEngineDepthAreFlagged(t *testing.T) {
	for _, n := range []int{17, 18, 19, 20, 21, 25, 30} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			data, rows := chainTree(t, n)
			eng := openEngine(t, writeTemp(t, data))
			_, v := openLive(t, data, sqlitefile.Options{})
			got := scanRows(t, v, 2, sqlitefile.TableTree)
			if len(got) != rows {
				t.Fatalf("scan delivers %d rows, want %d", len(got), rows)
			}
			flagged := 0
			for _, r := range got {
				refused := engineRefusesRow(t, eng, r.Rowid)
				want := chainLeafDepth(n, r.Rowid) > 20
				if r.BeyondEngineDepth != want || r.BeyondEngineDepth != refused {
					t.Errorf("rowid %d (leaf depth %d): flagged %v, want %v, engine refuses %v", r.Rowid, chainLeafDepth(n, r.Rowid), r.BeyondEngineDepth, want, refused)
				}
				if r.BeyondEngineDepth {
					flagged++
				}
			}
			if n >= 20 && flagged == 0 {
				t.Errorf("a tree of %d levels flagged no row", n+1)
			}
		})
	}
}

// TestLookupFoundBeyondEngineDepthIsFlagged: the found row of a lookup through
// a path deeper than the engine follows is flagged; a miss stays uncertain.
func TestLookupFoundBeyondEngineDepthIsFlagged(t *testing.T) {
	data, _ := chainTree(t, 25)
	_, v := openLive(t, data, sqlitefile.Options{})
	row, ok, err := v.LookupRowid(context.Background(), 2, 21) // leaf depth 22
	if err != nil || !ok || row.Rowid != 21 || !row.BeyondEngineDepth {
		t.Fatalf("LookupRowid(21) = %+v, %v, %v", row, ok, err)
	}
	row, ok, err = v.LookupRowid(context.Background(), 2, 3) // leaf depth 4
	if err != nil || !ok || row.BeyondEngineDepth {
		t.Fatalf("LookupRowid(3) = %+v, %v, %v", row, ok, err)
	}
	if _, ok, err = v.LookupRowid(context.Background(), 2, 1000); ok || !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("a miss through a deep path: ok %v err %v, want ErrCorrupt (uncertain)", ok, err)
	}
}

// chainIndex returns a database whose page 2 is a degenerate index tree:
// interior i (page i+1) has one entry of value 2i whose left child is the leaf
// holding 2i-1 and whose right child is the next interior; the last interior
// right child is a leaf holding 2n+1. Interior i lies at depth i, leaf i at i+1.
func chainIndex(t testing.TB, n int) []byte {
	t.Helper()
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.CreateTable("t", "create table t(a)")
	data := b.Bytes()[:ps]
	entry := func(v int) []byte { return []byte{3, 2, 1, byte(v)} } // payload length 3, then header 2 and an int8
	leaf := func(v int) []byte {
		p := make([]byte, ps)
		cell := entry(v)
		off := ps - len(cell)
		copy(p[off:], cell)
		p[0] = 0x0a
		binary.BigEndian.PutUint16(p[3:], 1)
		binary.BigEndian.PutUint16(p[5:], uint16(off))
		binary.BigEndian.PutUint16(p[8:], uint16(off))
		return p
	}
	interior := func(left, right uint32, v int) []byte {
		p := make([]byte, ps)
		cell := binary.BigEndian.AppendUint32(nil, left)
		cell = append(cell, entry(v)...)
		off := ps - len(cell)
		copy(p[off:], cell)
		p[0] = 0x02
		binary.BigEndian.PutUint16(p[3:], 1)
		binary.BigEndian.PutUint16(p[5:], uint16(off))
		binary.BigEndian.PutUint32(p[8:], right)
		binary.BigEndian.PutUint16(p[12:], uint16(off))
		return p
	}
	for i := 1; i <= n; i++ {
		right := uint32(i + 2)
		if i == n {
			right = uint32(2*n + 2)
		}
		data = append(data, interior(uint32(n+1+i), right, 2*i)...)
	}
	for i := 1; i <= n; i++ {
		data = append(data, leaf(2*i-1)...)
	}
	data = append(data, leaf(2*n+1)...)
	binary.BigEndian.PutUint32(data[28:], uint32(len(data)/ps))
	copy(data[92:96], data[24:28])
	return data
}

// TestDeepIndexEntriesAreFlagged: entries of an index tree deeper than the
// engine follows, interior and leaf, carry the flag; shallower ones do not.
func TestDeepIndexEntriesAreFlagged(t *testing.T) {
	const n = 24
	data := chainIndex(t, n)
	_, v := openLive(t, data, sqlitefile.Options{})
	got := scanRows(t, v, 2, sqlitefile.IndexTree)
	if len(got) != 2*n+1 {
		t.Fatalf("scan delivers %d entries, want %d", len(got), 2*n+1)
	}
	flagged := 0
	for _, r := range got {
		val := int(r.Values[0].Int)
		var depth int
		switch {
		case val == 2*n+1:
			depth = n + 1
		case val%2 == 0:
			depth = val / 2 // interior i
		default:
			depth = (val+1)/2 + 1 // leaf i
		}
		if want := depth > 20; r.BeyondEngineDepth != want {
			t.Errorf("entry %d (depth %d): flagged %v, want %v", val, depth, r.BeyondEngineDepth, want)
		}
		if r.BeyondEngineDepth {
			flagged++
		}
	}
	if flagged == 0 {
		t.Error("no entry was flagged")
	}
}

// PASSES ON ARRIVAL: no row of a tree that fits the engine cursor carries the
// flag. chainTree(19) has its deepest leaf at depth 20, the boundary; mutation:
// flag at depth >= engineMaxBTreeDepth.
func TestShallowTreesCarryNoDepthFlag(t *testing.T) {
	for _, n := range []int{1, 3, 10, 18, 19} {
		data, _ := chainTree(t, n)
		_, v := openLive(t, data, sqlitefile.Options{})
		for _, r := range scanRows(t, v, 2, sqlitefile.TableTree) {
			if r.BeyondEngineDepth {
				t.Errorf("tree of %d interiors: rowid %d is flagged", n, r.Rowid)
			}
		}
		if row, ok, err := v.LookupRowid(context.Background(), 2, 1); err != nil || !ok || row.BeyondEngineDepth {
			t.Errorf("tree of %d interiors: LookupRowid = %+v, %v, %v", n, row, ok, err)
		}
	}
}

// TestDepthCapStillHolds: the reader cap of 32 levels is unchanged. A tree at
// the cap is delivered in full; one level more skips the deepest pages with a
// warning, never panics, and the rows above are still delivered.
func TestDepthCapStillHolds(t *testing.T) {
	t.Run("at the cap", func(t *testing.T) {
		data, rows := chainTree(t, 31) // deepest leaf at depth 32
		_, v := openLive(t, data, sqlitefile.Options{})
		got := scanRows(t, v, 2, sqlitefile.TableTree)
		if len(got) != rows {
			t.Fatalf("delivers %d rows, want %d", len(got), rows)
		}
	})
	t.Run("one level past it", func(t *testing.T) {
		data, _ := chainTree(t, 32) // leaves 32 and 33 lie at depth 33
		_, v := openLive(t, data, sqlitefile.Options{})
		got := scanRows(t, v, 2, sqlitefile.TableTree)
		if len(got) != 31 {
			t.Fatalf("delivers %d rows, want the 31 above the cap", len(got))
		}
		if !viewWarns(v, sqlitefile.WarnBTreeDepth, 0) {
			t.Errorf("no btree-depth warning: %v", v.Warnings())
		}
		for _, r := range got {
			if want := chainLeafDepth(32, r.Rowid) > 20; r.BeyondEngineDepth != want {
				t.Errorf("rowid %d: flagged %v, want %v", r.Rowid, r.BeyondEngineDepth, want)
			}
		}
	})
}
