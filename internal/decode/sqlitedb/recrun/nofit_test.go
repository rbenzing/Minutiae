package recrun_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// threeRows is a 512-byte-page database with table t (the given definition) and rows 1..3 of vals(i).
func threeRows(sql string, vals func(i int64) []any) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", sql)
	for i := int64(1); i <= 3; i++ {
		tb.Insert(i, vals(i)...)
	}
	return b
}

// A WAL that commits a redefinition of t (three columns in the database file, two in the live
// state) leaves the old rows with more values than the live definition has columns: they must
// never be read through today's columns. The library already refuses them the table label
// (owner-changed), so they are not claimed and stay with the generic run; a WAL that keeps the
// definition delivers the same pages' old rows. Both are decided by the counts, not by a record.
func TestRecoverLeavesNonFittingRowsToTheGenericRun(t *testing.T) {
	old := threeRows("create table t(a, b, c)", func(i int64) []any { return []any{i, "old", "extra"} })
	run := func(t *testing.T, live *sqlitetest.Builder, cols int) (*recEmitter, recrun.Report) {
		w := live.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(1, live.PageBytes(1), 0)
		w.Frame(2, live.PageBytes(2), 2)
		em := newEmitter(t)
		rep, err := recrun.Run(t.Context(), input(old.Bytes(), w.Bytes(), nil), em, []parse.TableMapping{fakeMapping("t", cols)}, claim("t"))
		if err != nil {
			t.Fatal(err)
		}
		return em, rep
	}
	t.Run("redefined table", func(t *testing.T) {
		live := threeRows("create table t(a, b)", func(i int64) []any { return []any{i, "new"} })
		em, rep := run(t, live, 3)
		// the schema page and the three old rows of page 2: none is a claimed row of t
		if len(em.recs) != 0 || len(em.unmapped) != 0 || rep.History.Delivered != 0 || rep.History.Skipped["not-claimed"] != 4 {
			t.Errorf("%d records, %d unmapped, history %+v", len(em.recs), len(em.unmapped), rep.History)
		}
	})
	t.Run("unchanged definition", func(t *testing.T) {
		live := threeRows("create table t(a, b, c)", func(i int64) []any { return []any{i, "new", "extra"} })
		em, rep := run(t, live, 3)
		if len(em.recs) != 3 || rep.History.Delivered != 3 || rep.Tables[0].Mapped != 3 {
			t.Fatalf("%d records, history %+v", len(em.recs), rep.History)
		}
		for _, e := range em.recs {
			if e.Facts.Basis != "schema" || e.Facts.Method != "sqlite-wal-prior" || e.Rec.Payload["c1"] != "told" {
				t.Errorf("facts %+v payload %v", e.Facts, e.Rec.Payload)
			}
		}
	})
}
