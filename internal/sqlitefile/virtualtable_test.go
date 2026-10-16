package sqlitefile_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestTableOfAVirtualTableIsNotNotFound(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.CreateTable("real", "create table real(a)")
	b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(x)")
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	for _, name := range []string{"vt", "VT"} {
		tb, err := v.Table(t.Context(), name)
		if tb != nil || !errors.Is(err, sqlitefile.ErrVirtualTable) || errors.Is(err, sqlitefile.ErrNotFound) || errors.Is(err, sqlitefile.ErrCorrupt) {
			t.Errorf("Table(%q) = %v, %v", name, tb, err)
		}
	}
	if _, err := v.Table(t.Context(), "nothing"); !errors.Is(err, sqlitefile.ErrNotFound) || errors.Is(err, sqlitefile.ErrVirtualTable) {
		t.Errorf("a missing name: %v", err)
	}
	if _, err := v.Table(t.Context(), "real"); err != nil {
		t.Errorf("a base table: %v", err)
	}
}

// A history row labelled with a table that is virtual today has an unknown,
// never an absent, relation. PASSES ON ARRIVAL: a virtual table used to be
// ErrNotFound, which is answerless too. Mutation: remove ErrVirtualTable from
// isAnswerless and it fails.
func TestHistoryRowOfATableThatIsVirtualTodayIsUnknown(t *testing.T) {
	old := eraTable("create table t(a)", "t", false, [][]any{{"x"}, {"y"}, {"z"}})
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	b.AddSchemaRow("table", "t", "t", int64(0), "create virtual table t using fts5(a)")
	b.Free(2) // page 2 belongs to nothing today
	liveImg := b.Snapshot()
	n := liveImg.Pages()
	db := walMode(withCount(b.Bytes(), n))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, old.Page(1), n)
	w.Frame(2, old.Page(2), n)
	w.Frame(1, liveImg.Page(1), n)
	w.Frame(2, liveImg.Page(2), n)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	seen := 0
	for _, r := range rows {
		if r.WAL == nil || r.WAL.Frame != 2 || r.Table != "t" {
			continue
		}
		seen++
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("relation %s notes %v, want unknown", r.Relation, r.Notes)
		}
	}
	if seen == 0 {
		t.Fatalf("no history row of the old table t among %d rows", len(rows))
	}
}
