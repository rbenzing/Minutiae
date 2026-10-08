package sqlitefile_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestRowCarriesRecordLengthMismatch: the row delivered from a length-
// mismatched record says so itself (Row.LengthMismatch, scan and lookup;
// the note record-length-mismatch on a recovered row), so a parser can tell
// which row is damaged, not only that the file has a warning.
func TestRowCarriesRecordLengthMismatch(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.InsertRaw(1, []uint64{1}, []byte{5, 0}) // a stray byte
	tb.Insert(2, "ok", int64(2))
	db := b.Bytes()
	snap := b.Snapshot()
	db = withCount(db, snap.Pages())
	before := snap.Page(tb.Root())
	for id := int64(1); id <= 2; id++ { // the live file differs from the before-image in each row's last value
		cell, pg, off := tb.CellBytes(id)
		db[(int(pg)-1)*hps+off+len(cell)-1-int(2-id)] ^= 0x7f
	}
	_, v := openLive(t, db, sqlitefile.Options{})
	for _, r := range scanRows(t, v, tb.Root(), sqlitefile.TableTree) {
		if want := r.Rowid == 1; r.LengthMismatch != want {
			t.Errorf("scan: rowid %d LengthMismatch = %v, want %v", r.Rowid, r.LengthMismatch, want)
		}
	}
	for id, want := range map[int64]bool{1: true, 2: false} {
		r, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
		if !ok || err != nil || r.LengthMismatch != want {
			t.Errorf("lookup %d: ok %v err %v LengthMismatch %v, want %v", id, ok, err, r.LengthMismatch, want)
		}
	}
	// the same record in a journal before-image: the recovered row carries the note
	j := b.NewJournal(hps, 0x7777, snap.Pages())
	j.Record(tb.Root(), before)
	j.SuperJournal("absent-super-journal")
	_, h := openAll(t, db, nil, j.Bytes())
	rows, _ := collectRows(t, h)
	if len(rows) != 2 {
		t.Fatal("no recovered row")
	}
	for _, r := range rows {
		if has, want := hasNote(r, sqlitefile.NoteRecordLengthMismatch), *r.Rowid == 1; has != want {
			t.Errorf("recovered rowid %d: note present = %v, want %v", *r.Rowid, has, want)
		}
	}
}

// TestUnreadIsNotStored: a value past the end of a short record is not
// stored (its default; equality with the live default is legitimate), a value
// that is stored but could not be read is inconclusive, for NULL as well.
func TestUnreadIsNotStored(t *testing.T) {
	// a dead overflow chain: the integer or NULL after the blob is unread on both sides
	for _, c := range []struct {
		name string
		b    any
	}{{"null after the chain", nil}, {"integer after the chain", int64(0)}} {
		t.Run(c.name, func(t *testing.T) {
			bld := sqlitetest.New(sqlitetest.Options{PageSize: hps})
			tb := bld.CreateTable("t", "create table t(a, b)")
			tb.Insert(1, string(bytes.Repeat([]byte("x"), 12000)), c.b)
			cell, _, _ := tb.CellBytes(1)
			head := binary.BigEndian.Uint32(cell[len(cell)-4:])
			snap := bld.Snapshot()
			db := withCount(bld.Bytes(), snap.Pages())
			binary.BigEndian.PutUint32(pageAt(db, hps, head)[0:], 0xffffffff) // the chain ends in nowhere
			j := bld.NewJournal(hps, 0x4242, snap.Pages())
			j.Record(tb.Root(), snap.Page(tb.Root()))
			j.SuperJournal("absent-super-journal")
			_, h := openAll(t, db, nil, j.Bytes())
			rows, st := collectRows(t, h)
			if len(rows) != 1 || st.DuplicateOfLive != 0 {
				t.Fatalf("%d rows, stats %+v", len(rows), st)
			}
			if r := rows[0]; r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteValueUnread) {
				t.Errorf("relation %s notes %v, want unknown and value-unread", r.Relation, r.Notes)
			}
		})
	}
}
