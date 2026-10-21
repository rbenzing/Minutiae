package sqlitefile_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestHistoryRowWithAnUndecidedDefaultIsNeverSameOrDuplicate: a recovered short
// row whose column default is undecided cannot be proven equal to the live row
// (both would read as "not stored"), so it is delivered with relation unknown,
// never dropped as a duplicate of the live row. For an expression default the
// table is still named by the schema, so the reason is the comparison itself
// (compare-incomplete). For a literal the reader cannot convert exactly the
// short record no longer fits the table strictly (ruling U22), so the table is
// named by the fit alone and the relation is unknown for that reason.
func TestHistoryRowWithAnUndecidedDefaultIsNeverSameOrDuplicate(t *testing.T) {
	cases := []struct{ def, note string }{
		{"integer default (1+1)", sqlitefile.NoteCompareIncomplete},
		{"integer default '12345678901234567890'", sqlitefile.NoteIdentityByFitOnly},
		{"integer default 0.1234567890123456789", sqlitefile.NoteIdentityByFitOnly},
	}
	for _, k := range cases {
		t.Run(k.def, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
			tb := b.CreateTable("t", "create table t(a, c "+k.def+")")
			tb.Insert(1, "ka") // short: c is the default
			tb.Insert(2, "kb")
			v0 := b.Snapshot()
			db := walMode(withCount(b.Bytes(), v0.Pages()))
			tb.Update(2, "kb2")
			w := b.NewWAL(false, 0x1000, 0x1001, 0)
			b.CommitTo(w, v0)
			_, h := openAll(t, db, w.Bytes(), nil)
			rows, st := collectRows(t, h)
			if st.DuplicateOfLive != 0 {
				t.Errorf("DuplicateOfLive = %d: an undecided default was read as equal to the live one", st.DuplicateOfLive)
			}
			found := false
			for _, r := range rows {
				if txtOf(t, r.Values[0]) != "ka" {
					continue
				}
				found = true
				if r.Relation != sqlitefile.RelUnknown || !hasNote(r, k.note) {
					t.Errorf("ka: relation %s notes %v; want unknown with %s", r.Relation, r.Notes, k.note)
				}
			}
			if !found {
				t.Errorf("the history row ka was not delivered (%d rows)", len(rows))
			}
		})
	}
}
