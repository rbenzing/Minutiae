package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// chainTree returns a database whose table t is a degenerate tree: interior
// pages 2..interiors+1, each with one cell (the left child a one-row leaf, the
// right child the next interior page, the last one a leaf too). The deepest
// leaf lies at depth interiors+1.
func chainTree(t *testing.T, interiors int) (data []byte, rows int) {
	t.Helper()
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.CreateTable("t", "create table t(a)")
	data = b.Bytes()[:ps] // page 1 only: the schema names root page 2
	leafPage := func(rowid int) []byte {
		p := make([]byte, ps)
		cell := []byte{2, byte(rowid), 2, 0} // payload 2 bytes, rowid, record (header 2, one NULL)
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
	// pages: 2..interiors+1 interior, then the leaves: leaf i (1..interiors) at
	// interiors+1+i, the last leaf at 2*interiors+2
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
	n := uint32(len(data) / ps)
	binary.BigEndian.PutUint32(data[28:], n)
	copy(data[92:96], data[24:28]) // the page count is valid
	return data, interiors + 1
}

// TestTreeDeeperThanTheEngineAllowsIsWarned: the engine calls a tree deeper
// than its cursor depth corrupt; the library reads such a tree (its own cap is
// 32) and says so. The engine decides where the limit lies (final review A,
// F8).
func TestTreeDeeperThanTheEngineAllowsIsWarned(t *testing.T) {
	for _, interiors := range []int{3, 17, 18, 19, 20, 21, 25, 30} {
		t.Run(fmt.Sprint(interiors), func(t *testing.T) {
			data, rows := chainTree(t, interiors)
			var n int
			engineErr := openEngine(t, writeTemp(t, data)).QueryRow("select count(*) from t").Scan(&n)
			if engineErr == nil && n != rows {
				t.Fatalf("engine counts %d rows, the tree holds %d", n, rows)
			}
			_, v := openLive(t, data, sqlitefile.Options{})
			got := scanRows(t, v, 2, sqlitefile.TableTree)
			if len(got) != rows {
				t.Fatalf("library reads %d rows, want %d: %v", len(got), rows, v.Warnings())
			}
			warned := false
			for _, w := range v.Warnings() {
				warned = warned || w.Code == sqlitefile.WarnBTreeDepth
			}
			t.Logf("depth %d: engine error %v", interiors+1, engineErr)
			if warned != (engineErr != nil) {
				t.Errorf("tree of %d levels: engine fails = %v, btree-depth warning = %v", interiors+1, engineErr != nil, warned)
			}
			// an absent answer through a tree the engine calls corrupt is uncertain
			_, ok, err := v.LookupRowid(context.Background(), 2, 1000)
			if ok || (err != nil) != (engineErr != nil) {
				t.Errorf("lookup of a missing rowid: ok %v err %v, engine fails = %v", ok, err, engineErr != nil)
			}
		})
	}
}
