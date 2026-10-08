package sqlitefile_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestFitOnlyIdentityNeverCompares (ruling C4): a dropped table whose column
// affinities match a live table is named by the fit alone (BasisFit). Its rows
// share rowids with live rows of the other table, but a rowid comparison would
// present them as modified rows of that table; the relation must be unknown.
func TestFitOnlyIdentityNeverCompares(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b)")
	for id := int64(1); id <= 3; id++ {
		gone.Insert(id, id*100, "gone-text") // same rowids as the live rows, other values
	}
	b.DropTable("pad")
	b.DropTable("gone")
	_, h := openAll(t, b.Bytes(), nil, nil)
	rows, st := collectRows(t, h)
	fr := freelistRows(rows)
	if len(fr) != 3 {
		t.Fatalf("%d freelist rows: %v", len(fr), methods(rows))
	}
	for _, r := range fr {
		if r.TableBasis != sqlitefile.BasisFit {
			t.Fatalf("basis %s, want fit: %+v", r.TableBasis, r)
		}
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("rowid %d: relation %q, want unknown (identity by fit only)", *r.Rowid, r.Relation)
		}
		if !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
			t.Errorf("rowid %d: no identity-by-fit-only note: %v", *r.Rowid, r.Notes)
		}
	}
	if st.Unknown != 3 || st.DuplicateOfLive != 0 {
		t.Errorf("stats %+v", st)
	}
}

// TestGuessIdentityCarriesTheFitOnlyNote (rulings C4, C44, C46): a label that
// comes from the loose fit (BasisGuess) is identity by fit alone as much as a
// strict one, so it carries the same note and its relation is unknown.
func TestGuessIdentityCarriesTheFitOnlyNote(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b, c not null)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live", "c")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b)")
	for id := int64(1); id <= 3; id++ {
		gone.Insert(id, id*100, "gone-text") // two values: t needs three, so only the loose fit names t
	}
	b.DropTable("pad")
	b.DropTable("gone")
	_, h := openAll(t, b.Bytes(), nil, nil)
	rows, _ := collectRows(t, h)
	fr := freelistRows(rows)
	if len(fr) != 3 {
		t.Fatalf("%d freelist rows: %v", len(fr), methods(rows))
	}
	for _, r := range fr {
		if r.TableBasis != sqlitefile.BasisGuess || r.Table != "t" {
			t.Fatalf("basis %s table %q, want a guess naming t: %+v", r.TableBasis, r.Table, r)
		}
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("rowid %d: relation %q, want unknown", *r.Rowid, r.Relation)
		}
		if !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
			t.Errorf("rowid %d: a guess carries no identity-by-fit-only note: %v", *r.Rowid, r.Notes)
		}
	}
}
