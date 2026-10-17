package sqlitedb_test

import (
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// chainTree is a copy of the sqlitefile test builder (test-only duplication is
// accepted): a database whose table t is a degenerate tree with interior pages
// 2..interiors+1, each holding one cell (the left child a one-row leaf, the
// right child the next interior page). Leaf r (1..interiors) is the left child
// of interior r, the last leaf (rowid interiors+1) the right child of the last
// interior, so the leaf of rowid r lies at depth min(r+1, interiors+1).
func chainTree(t *testing.T, interiors int) (data []byte, rows int) {
	t.Helper()
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.CreateTable("t", "create table t(a)")
	data = b.Bytes()[:ps]
	leafPage := func(rowid int) []byte {
		p := make([]byte, ps)
		cell := []byte{2, byte(rowid), 2, 0}
		off := ps - len(cell)
		copy(p[off:], cell)
		p[0] = 0x0d
		binary.BigEndian.PutUint16(p[3:], 1)
		binary.BigEndian.PutUint16(p[5:], uint16(off))
		binary.BigEndian.PutUint16(p[8:], uint16(off))
		return p
	}
	interior := func(left, right uint32, key int) []byte {
		p := make([]byte, ps)
		cell := binary.BigEndian.AppendUint32(nil, left)
		cell = append(cell, byte(key))
		off := ps - len(cell)
		copy(p[off:], cell)
		p[0] = 0x05
		binary.BigEndian.PutUint16(p[3:], 1)
		binary.BigEndian.PutUint16(p[5:], uint16(off))
		binary.BigEndian.PutUint32(p[8:], right)
		binary.BigEndian.PutUint16(p[12:], uint16(off))
		return p
	}
	for i := 1; i <= interiors; i++ {
		right := uint32(i + 2)
		if i == interiors {
			right = uint32(2*interiors + 2)
		}
		data = append(data, interior(uint32(interiors+1+i), right, i)...)
	}
	for i := 1; i <= interiors+1; i++ {
		data = append(data, leafPage(i)...)
	}
	binary.BigEndian.PutUint32(data[28:], uint32(len(data)/ps))
	copy(data[92:96], data[24:28])
	return data, interiors + 1
}

func chainLeafDepth(n int, r int64) int { return int(min(r+1, int64(n+1))) }

// TestScanFlagsRowsBeyondEngineDepth: FlagBeyondEngineDepth is set on exactly
// the rows read from a tree level the engine would refuse, a point read agrees,
// and Stats.RowsFlagged counts them (L108 item 6, ruling U4).
func TestScanFlagsRowsBeyondEngineDepth(t *testing.T) {
	const n = 25
	data, rows := chainTree(t, n)
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantFlagged := 0
	seen := 0
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		id, _ := r.Rowid()
		seen++
		want := chainLeafDepth(n, id) > 20
		if got := r.Flags()&sqlitedb.FlagBeyondEngineDepth != 0; got != want {
			t.Errorf("rowid %d: FlagBeyondEngineDepth = %v, want %v", id, got, want)
		}
		if want {
			wantFlagged++
		}
	})
	if seen != rows || wantFlagged == 0 {
		t.Fatalf("scan delivered %d rows (want %d), %d expected flagged", seen, rows, wantFlagged)
	}
	if s := d.Stats(); s.RowsFlagged != int64(wantFlagged) {
		t.Errorf("RowsFlagged = %d, want %d", s.RowsFlagged, wantFlagged)
	}
	for _, c := range []struct {
		rowid int64
		want  bool
	}{{3, false}, {21, true}} {
		r, ok, err := tb.Get(t.Context(), c.rowid)
		if err != nil || !ok {
			t.Fatalf("Get(%d) = %v, %v", c.rowid, ok, err)
		}
		if got := r.Flags()&sqlitedb.FlagBeyondEngineDepth != 0; got != c.want {
			t.Errorf("Get(%d): FlagBeyondEngineDepth = %v, want %v", c.rowid, got, c.want)
		}
		r.Release()
	}
}

// TestBeyondEngineDepthFlagIsAppendedLast: the existing flags keep their values.
func TestBeyondEngineDepthFlagIsAppendedLast(t *testing.T) {
	if sqlitedb.FlagUnknownValues != 1<<5 || sqlitedb.FlagBeyondEngineDepth != 1<<6 {
		t.Errorf("FlagUnknownValues %d, FlagBeyondEngineDepth %d", sqlitedb.FlagUnknownValues, sqlitedb.FlagBeyondEngineDepth)
	}
}

// PASSES ON ARRIVAL: a tree whose deepest leaf lies at depth 20 flags nothing.
func TestShallowTreesCarryNoDepthFlag(t *testing.T) {
	data, _ := chainTree(t, 19)
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.Flags() != 0 {
			id, _ := r.Rowid()
			t.Errorf("rowid %d: flags %b", id, r.Flags())
		}
	})
	if s := d.Stats(); s.RowsFlagged != 0 {
		t.Errorf("RowsFlagged = %d", s.RowsFlagged)
	}
}
