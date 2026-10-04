package sqlitefile_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestEngineReservedSerialTypesReadAsNull: serial types 10 and 11 are read by
// the engine as zero-width NULL (the row and its other columns are returned).
// The reader does the same and warns once for the record. Measured, not
// assumed; the first half of the test is the probe.
func TestEngineReservedSerialTypesReadAsNull(t *testing.T) {
	for _, s := range []uint64{10, 11} {
		t.Run(fmt.Sprint(s), func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{})
			tb := b.CreateTable("t", "CREATE TABLE t(a, b, c, d)")
			tb.InsertRaw(1, []uint64{s, 1, s, 13 + 2*3}, []byte{42, 'a', 'b', 'c'})
			tb.Insert(2, "ok", int64(2), nil, "two") // a record next to it: not touched
			data := b.Bytes()

			db := openEngine(t, writeTemp(t, data))
			var a, c any
			var ta, tc string
			var bb int64
			var d string
			if err := db.QueryRow("select a, typeof(a), b, c, typeof(c), d from t where rowid = 1").Scan(&a, &ta, &bb, &c, &tc, &d); err != nil {
				t.Fatalf("the engine does not read the row: %v", err)
			}
			if a != nil || ta != "null" || bb != 42 || c != nil || tc != "null" || d != "abc" {
				t.Fatalf("engine reads (%v %s %d %v %s %q), want (nil null 42 nil null abc)", a, ta, bb, c, tc, d)
			}

			_, v := openLive(t, data, sqlitefile.Options{})
			rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
			if len(rows) != 2 {
				t.Fatalf("%d rows: a reserved serial type must never hide the row", len(rows))
			}
			r := rows[0]
			want := []any{nil, int64(42), nil, "abc"}
			if err := rowIs(r, want); err != nil {
				t.Errorf("row 1: %v", err)
			}
			if r.Values[0].Serial != s || r.Values[0].Omitted || r.Values[0].Len != 0 {
				t.Errorf("column 0 must be a plain NULL that remembers its serial type: %+v", r.Values[0])
			}
			if err := rowIs(rows[1], []any{"ok", int64(2), nil, "two"}); err != nil {
				t.Errorf("row 2: %v", err)
			}
			n := 0
			for _, w := range v.Warnings() {
				if w.Code == sqlitefile.WarnRecordInvalid && strings.Contains(w.Msg, "reserved serial type") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d reserved-serial-type warnings, want one for the record: %v", n, v.Warnings())
			}
		})
	}
}

// TestEngineReadsCellsBelowTheStoredContentStart: a cell pointer that lies
// below the stored content-start field but inside the page, after the header
// and the pointer array, is read by the engine. The reader reads it too and
// warns once for the page. A pointer into the pointer array or the header
// stays bad.
func TestEngineReadsCellsBelowTheStoredContentStart(t *testing.T) {
	for _, delta := range []int{4, 8, 20, 40} {
		t.Run(fmt.Sprintf("content start +%d", delta), func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
			tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
			for i := int64(1); i <= 12; i++ {
				tb.Insert(i, fmt.Sprintf("row-%d", i), i*10)
			}
			if tb.Depth() != 1 {
				t.Fatalf("depth %d", tb.Depth())
			}
			data := b.Bytes()
			page := pageAt(data, 512, tb.Root())
			lowest := 512
			for _, o := range cellOffsets(page) {
				lowest = min(lowest, o)
			}
			binary.BigEndian.PutUint16(page[5:], uint16(lowest+delta))

			db := openEngine(t, writeTemp(t, data))
			var n int
			var sum int64
			if err := db.QueryRow("select count(*), sum(b) from t").Scan(&n, &sum); err != nil || n != 12 || sum != 780 {
				t.Fatalf("the engine reads %d rows, sum %d, err %v: want all 12", n, sum, err)
			}

			_, v := openLive(t, data, sqlitefile.Options{})
			rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
			if len(rows) != 12 {
				t.Fatalf("%d rows, want 12", len(rows))
			}
			for _, r := range rows {
				if err := rowIs(r, []any{fmt.Sprintf("row-%d", r.Rowid), r.Rowid * 10}); err != nil {
					t.Errorf("rowid %d: %v", r.Rowid, err)
				}
			}
			warned := 0
			for _, w := range v.Warnings() {
				if w.Code == sqlitefile.WarnCellPointer && w.Page == tb.Root() {
					warned++
				}
			}
			if warned != 1 {
				t.Errorf("%d cell-pointer warnings for the page, want one: %v", warned, v.Warnings())
			}
			if v.Stats().PagesSkipped != 0 {
				t.Errorf("PagesSkipped = %d", v.Stats().PagesSkipped)
			}
		})
	}
	t.Run("a pointer into the pointer array stays bad", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
		for i := int64(1); i <= 12; i++ {
			tb.Insert(i, fmt.Sprintf("row-%d", i), i*10)
		}
		data := b.Bytes()
		page := pageAt(data, 512, tb.Root())
		binary.BigEndian.PutUint16(page[8+2*3:], 10) // cell 3 now points into the pointer array
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if len(rows) != 11 || slices.Contains(rowids(rows), 4) {
			t.Errorf("rowids %v: cell 3 (rowid 4) is lost, the others read", rowids(rows))
		}
		// Observation only: what the engine makes of the bytes of its own pointer
		// array read as a cell is not something the reader copies; the pointer
		// stays bad (the ruling keeps the true bounds).
		db := openEngine(t, writeTemp(t, data))
		var n int
		var sum any
		err := db.QueryRow("select count(*), sum(b) from t").Scan(&n, &sum)
		t.Logf("engine with a pointer into its own pointer array: count %d, sum %v, err %v", n, sum, err)
	})
}

// TestLookupReadsCellBelowTheStoredContentStart: the point lookup reads such a cell too.
func TestLookupReadsCellBelowTheStoredContentStart(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	for i := int64(1); i <= 12; i++ {
		tb.Insert(i, fmt.Sprintf("row-%d", i))
	}
	data := b.Bytes()
	page := pageAt(data, 512, tb.Root())
	lowest := 512
	for _, o := range cellOffsets(page) {
		lowest = min(lowest, o)
	}
	binary.BigEndian.PutUint16(page[5:], uint16(lowest+20))
	_, v := openLive(t, data, sqlitefile.Options{})
	for i := int64(1); i <= 12; i++ {
		r, ok, err := v.LookupRowid(context.Background(), tb.Root(), i)
		if err != nil || !ok || !bytes.Equal(r.Values[0].Bytes, []byte(fmt.Sprintf("row-%d", i))) {
			t.Fatalf("lookup %d: %v %v", i, ok, err)
		}
	}
}
