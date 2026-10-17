package sqlitefile_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// taintScenario: a WITHOUT ROWID table whose history holds key 5 (no live twin) and whose
// live leaf holds keys 10..13. With doctor, two live cell pointers are swapped in the
// WAL image, so the live tree is out of order. With fullCollector the warning collector is
// already full when the scan runs (an earlier header warning, MaxWarnings 1), so the
// btree-order warning is dropped and only the flag on the row can say the live tree is damaged.
func taintScenario(t *testing.T, doctor, fullCollector bool) sqlitefile.RecoveredRow {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k integer primary key, v) without rowid", 1)
	wr.Insert(1, int64(5), "old")
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Delete(1)
	for i := range 4 {
		wr.Insert(int64(100+i), int64(10+i), "live")
	}
	if doctor {
		pg := b.PageBytes(wr.Root())
		hdr := 8
		a, c := append([]byte(nil), pg[hdr+2:hdr+4]...), append([]byte(nil), pg[hdr+4:hdr+6]...)
		off := int(wr.Root()-1)*hps + hdr + 2
		b.Patch(off, c...)
		b.Patch(off+2, a...)
	}
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	opts := sqlitefile.Options{}
	if fullCollector {
		db[21] = 0x41 // hdr-fractions: the one warning the collector keeps
		opts.Limits.MaxWarnings = 1
	}
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), opts)
	if err != nil {
		t.Fatal(err)
	}
	wal := w.Bytes()
	if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	var rows []sqlitefile.RecoveredRow
	if _, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		rows = append(rows, r)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if len(r.Values) > 0 && r.Values[0].Kind == sqlitefile.KindInt && r.Values[0].Int == 5 {
			return r
		}
	}
	t.Fatalf("the history row with key 5 was not delivered among %d rows", len(rows))
	return sqlitefile.RecoveredRow{}
}

// The control: an intact live table proves key 5 absent.
func TestWithoutRowidIntactLiveTableProvesAbsence(t *testing.T) {
	for _, full := range []bool{false, true} {
		if r := taintScenario(t, false, full); r.Relation != sqlitefile.RelAbsentFromLive {
			t.Errorf("collector full %v: relation %s (notes %v), want absent", full, r.Relation, r.Notes)
		}
	}
}

// A doctored live WITHOUT ROWID table never lets a history row be absent: the relation is
// unknown. With the collector full the warning is gone, so this is the row flag's doing.
func TestLiveWithoutRowidKeyViolationTaintsHistoryAbsence(t *testing.T) {
	for _, full := range []bool{false, true} {
		r := taintScenario(t, true, full)
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("collector full %v: relation %s (notes %v), want unknown", full, r.Relation, r.Notes)
		}
		if !hasNote(r, sqlitefile.NoteLiveUncertain) {
			t.Errorf("collector full %v: notes %v, want %s", full, r.Notes, sqlitefile.NoteLiveUncertain)
		}
	}
}
