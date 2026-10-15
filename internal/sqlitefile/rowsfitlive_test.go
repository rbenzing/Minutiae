package sqlitefile_test

// C45: a row named by the column fit alone (BasisFit) whose rowid and values
// equal a live row of that table is annotated equals-live-row. The annotation
// is a note only: the relation stays unknown, the confidence is unchanged, the
// row is delivered and it is never counted in DuplicateOfLive.

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fitLiveFixture: live table t holds rows 1..6 as (id, "live"). The dropped
// table "gone" has the shape of t only, so its leaf is named t by the fit. Its
// rows 1..3 equal the live rows, rows 4 and 5 have the live rowid and another
// value, row 7 has no live rowid.
func fitLiveFixture(t *testing.T) ([]sqlitefile.RecoveredRow, sqlitefile.RowStats) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 6; id++ {
		live.Insert(id, id, "live")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b)")
	for id := int64(1); id <= 3; id++ {
		gone.Insert(id, id, "live")
	}
	gone.Insert(4, int64(400), "live")
	gone.Insert(5, int64(5), "other")
	gone.Insert(7, int64(7), "live")
	b.DropTable("pad")
	b.DropTable("gone")
	_, h := openAll(t, b.Bytes(), nil, nil)
	rows, st := collectRows(t, h)
	fr := freelistRows(rows)
	if len(fr) != 6 {
		t.Fatalf("%d freelist rows: %v", len(fr), methods(rows))
	}
	return fr, st
}

func TestFitOnlyRowEqualToLiveRowIsAnnotated(t *testing.T) {
	fr, st := fitLiveFixture(t)
	n := 0
	for _, r := range fr {
		if r.TableBasis != sqlitefile.BasisFit || r.Table != "t" {
			t.Fatalf("basis %s table %q, want fit t: %+v", r.TableBasis, r.Table, r)
		}
		want := *r.Rowid <= 3
		if got := hasNote(r, sqlitefile.NoteEqualsLiveRow); got != want {
			t.Errorf("rowid %d: equals-live-row %v, want %v (notes %v)", *r.Rowid, got, want, r.Notes)
		}
		if !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
			t.Errorf("rowid %d: lost identity-by-fit-only: %v", *r.Rowid, r.Notes)
		}
		if want {
			n++
		}
	}
	if st.EqualsLiveFit != int64(n) || n != 3 {
		t.Errorf("EqualsLiveFit %d, annotated rows %d, want 3", st.EqualsLiveFit, n)
	}
}

func TestFitOnlyRowDifferingFromLiveIsNotAnnotated(t *testing.T) {
	fr, _ := fitLiveFixture(t)
	for _, r := range fr {
		switch *r.Rowid {
		case 4, 5, 7: // another value on a live rowid, and a rowid with no live row
			if hasNote(r, sqlitefile.NoteEqualsLiveRow) {
				t.Errorf("rowid %d differs from the live state yet carries equals-live-row", *r.Rowid)
			}
		}
	}
}

func TestFitOnlyRowIsNeverCountedAsDuplicateOfLive(t *testing.T) {
	fr, st := fitLiveFixture(t)
	if st.DuplicateOfLive != 0 {
		t.Errorf("DuplicateOfLive %d: a fit-only row is delivered, never dropped as a duplicate", st.DuplicateOfLive)
	}
	if len(fr) != 6 {
		t.Errorf("%d rows delivered, want all 6", len(fr))
	}
	if st.RowsByMethod[sqlitefile.MethodFreelist] != 6 || st.Unknown != 6 {
		t.Errorf("stats %+v", st)
	}
}

func TestEqualsLiveRowNeverUpgradesRelationOrConfidence(t *testing.T) {
	fr, _ := fitLiveFixture(t)
	for _, r := range fr {
		if r.Relation != sqlitefile.RelUnknown || r.TableBasis != sqlitefile.BasisFit {
			t.Errorf("rowid %d: relation %s basis %s", *r.Rowid, r.Relation, r.TableBasis)
		}
		plain := r
		plain.Notes = nil
		if a, b := sqlitefile.Confidence(r), sqlitefile.Confidence(plain); a != b || a > 60 {
			t.Errorf("rowid %d: confidence %d with the notes, %d without (fit caps at 60)", *r.Rowid, a, b)
		}
	}
	// the annotated and the plain rows rank the same
	var annotated, plain int
	for _, r := range fr {
		if hasNote(r, sqlitefile.NoteEqualsLiveRow) {
			annotated = sqlitefile.Confidence(r)
		} else {
			plain = sqlitefile.Confidence(r)
		}
	}
	if annotated != plain {
		t.Errorf("annotated rows rank %d, the others %d", annotated, plain)
	}
}

// TestGuessRowEqualToLiveRowIsOnlyNoted (J81): a row named by the loose fit
// (BasisGuess) whose rowid and values equal a live row gets the note, and
// nothing else changes: the relation stays unknown and the confidence is the
// one of the same row without the note.
func TestGuessRowEqualToLiveRowIsOnlyNoted(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b, c not null)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live", nil) // the builder does not enforce NOT NULL: only the loose fit accepts a NULL c
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b, c)")
	for id := int64(1); id <= 3; id++ {
		gone.Insert(id, id, "live", nil)
	}
	gone.Insert(4, int64(4), "live", nil) // no live rowid
	b.DropTable("pad")
	b.DropTable("gone")
	_, h := openAll(t, b.Bytes(), nil, nil)
	rows, st := collectRows(t, h)
	fr := freelistRows(rows)
	if len(fr) != 4 {
		t.Fatalf("%d freelist rows: %v", len(fr), methods(rows))
	}
	for _, r := range fr {
		if r.TableBasis != sqlitefile.BasisGuess || r.Table != "t" {
			t.Fatalf("basis %s table %q, want a guess naming t: %+v", r.TableBasis, r.Table, r)
		}
		want := *r.Rowid <= 3
		if got := hasNote(r, sqlitefile.NoteEqualsLiveRow); got != want {
			t.Errorf("rowid %d: equals-live-row %v, want %v (notes %v)", *r.Rowid, got, want, r.Notes)
		}
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
			t.Errorf("rowid %d: relation %s notes %v", *r.Rowid, r.Relation, r.Notes)
		}
		plain := r
		plain.Notes = []string{sqlitefile.NoteIdentityByFitOnly}
		if a, c := sqlitefile.Confidence(r), sqlitefile.Confidence(plain); a != c {
			t.Errorf("rowid %d: confidence %d with the notes, %d with the fit-only note alone", *r.Rowid, a, c)
		}
	}
	if st.EqualsLiveFit != 3 || st.DuplicateOfLive != 0 {
		t.Errorf("EqualsLiveFit %d DuplicateOfLive %d, want 3 and 0", st.EqualsLiveFit, st.DuplicateOfLive)
	}
}
