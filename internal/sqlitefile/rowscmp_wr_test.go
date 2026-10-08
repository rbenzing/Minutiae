package sqlitefile_test

// WITHOUT ROWID comparison pins (Task 13 review I-2 and I-3): primary keys of
// different storage kinds never collide, and a live scan that omitted a value
// is never taken for a complete one.

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestWithoutRowidKeysOfOtherKindsNeverCollide: the live row has the integer key
// 1; the history rows carry the keys '1' (text) and x'31' (blob),
// which are different keys: absent from live, not a superseded version of the
// live row. The integer key itself, with another value, is the superseded one.
func TestWithoutRowidKeysOfOtherKindsNeverCollide(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k primary key, v) without rowid", 1)
	wr.Insert(1, "1", "text-key")
	wr.Insert(2, []byte("1"), "blob-key")
	wr.Insert(3, int64(1), "old-int-key")
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Delete(1)
	wr.Delete(2)
	wr.Update(3, int64(1), "live-int-key")
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != 3 || st.DuplicateOfLive != 0 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	for _, r := range rows {
		v := txtOf(t, r.Values[1])
		want := sqlitefile.RelAbsentFromLive
		if v == "old-int-key" {
			want = sqlitefile.RelSupersededVersion
		}
		if r.Relation != want {
			t.Errorf("key of %s: relation %s, want %s", v, r.Relation, want)
		}
	}
}

// TestWithoutRowidOmittedLiveValueIsNeverAbsent: the live value of key1 is longer
// than the text cap (4 MiB), so the live scan omitted it. The history row of key1
// cannot be compared with it (compare-incomplete); the history row of key2,
// whose key is not found, cannot be called absent either, the scan may have
// missed it (live-lookup-uncertain).
func TestWithoutRowidOmittedLiveValueIsNeverAbsent(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
	wr.Insert(1, "key1", longText(4500000, 3))
	wr.Insert(2, "key2", "small")
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Delete(2)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != 2 || st.DuplicateOfLive != 0 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	for _, r := range rows {
		k := txtOf(t, r.Values[0])
		note := sqlitefile.NoteCompareIncomplete
		if k == "key2" {
			note = sqlitefile.NoteLiveUncertain
		}
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, note) {
			t.Errorf("%s: relation %s notes %v, want unknown with %s", k, r.Relation, r.Notes, note)
		}
	}
}

// wrDefaultTable is a WITHOUT ROWID table whose column w has a non-literal
// default: a record written before the column existed is short and its w is
// omitted (not materialized), so such a row is never provably equal to a full one.
const wrDefaultSQL = "create table wr(k text primary key, v, w default (1+1)) without rowid"

// TestWithoutRowidIncompleteRowsAreNeverEqualOrAbsent: a live row with an omitted
// value makes the digest set incomplete (a missing key is uncertain, never
// absent), and an incomplete row on either side is never reported equal or
// different.
func TestWithoutRowidIncompleteRowsAreNeverEqualOrAbsent(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", wrDefaultSQL, 1)
	wr.Insert(1, "ka", "va", int64(5)) // full today? no: shortened below
	wr.Insert(2, "kb", "vb")           // short in the old era
	wr.Insert(3, "kc", "vc", int64(7)) // unchanged
	wr.Insert(4, "kg", "vg", int64(9)) // gone
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Update(1, "ka", "va") // live: short, w omitted
	wr.Update(2, "kb", "vb", int64(6))
	wr.Delete(4)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != 3 || st.DuplicateOfLive != 1 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	want := map[string]string{"ka": sqlitefile.NoteCompareIncomplete, "kb": sqlitefile.NoteCompareIncomplete, "kg": sqlitefile.NoteLiveUncertain}
	for _, r := range rows {
		k := txtOf(t, r.Values[0])
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, want[k]) {
			t.Errorf("%s: relation %s notes %v, want unknown with %s", k, r.Relation, r.Notes, want[k])
		}
	}
}

// TestWithoutRowidNonBinaryKeyIncompleteRowIsUnknown: when the key does not
// compare as BINARY the whole-row digest is the key, so an incomplete history row
// cannot be looked up at all.
func TestWithoutRowidNonBinaryKeyIncompleteRowIsUnknown(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k text collate nocase primary key, v, w default (1+1)) without rowid", 1)
	wr.Insert(1, "ka", "va") // short: w omitted
	wr.Insert(2, "kb", "vb", int64(2))
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Delete(1)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if txtOf(t, r.Values[0]) != "ka" {
			continue
		}
		n++
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteCompareIncomplete) {
			t.Errorf("relation %s notes %v, want unknown with compare-incomplete", r.Relation, r.Notes)
		}
	}
	if n != 1 {
		t.Fatalf("%d rows for ka in %v", n, methods(rows))
	}
}

// TestWithoutRowidClippedKeyIsUnknown: a history key longer than the recovered
// value cap is clipped, so it cannot be looked up in the live set.
func TestWithoutRowidClippedKeyIsUnknown(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
	wr.Insert(1, "a-long-key-1", "va")
	wr.Insert(2, "a-long-key-2", "vb")
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Delete(1)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxRecoveredValueBytes: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))); err != nil {
		t.Fatal(err)
	}
	hh := d.History()
	t.Cleanup(hh.Release)
	rows, _ := collectRows(t, hh)
	if len(rows) == 0 {
		t.Fatal("no row delivered")
	}
	for _, r := range rows {
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteCompareIncomplete) {
			t.Errorf("clipped key: relation %s notes %v, want unknown with compare-incomplete", r.Relation, r.Notes)
		}
	}
}

// TestHistoryRowWithRowidAliasIsComparedByRowid: the rowid-alias column of a
// stored record is NULL; the history row and the live row are compared with the
// alias resolved to the rowid on both sides, so an unchanged row of an older
// page image is a duplicate of live and a changed one is a superseded version.
func TestHistoryRowWithRowidAliasIsComparedByRowid(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(id integer primary key, v)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, nil, "value")
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	tb.Update(2, nil, "changed")
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != 1 || st.DuplicateOfLive != 2 || *rows[0].Rowid != 2 || rows[0].Relation != sqlitefile.RelSupersededVersion {
		t.Fatalf("%d rows, stats %+v: %+v", len(rows), st, rows)
	}
}
