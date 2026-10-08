package sqlitefile_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestFitRowOfAnUncommittedImageIsUnknownWithUncommittedOrigin (ruling C47):
// "uncommitted" is a fact about the ORIGIN of the bytes. A row labelled by a
// shape fit has the relation unknown, whatever its origin.
func TestFitRowOfAnUncommittedImageIsUnknownWithUncommittedOrigin(t *testing.T) {
	for _, loose := range []bool{false, true} {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		liveCols := "create table t(a, b)"
		if loose {
			liveCols = "create table t(a, b, c not null)" // only the loose fit names t
		}
		live := b.CreateTable("t", liveCols)
		for id := int64(1); id <= 3; id++ {
			if loose {
				live.Insert(id, id, "live", "c")
			} else {
				live.Insert(id, id, "live")
			}
		}
		gone := b.CreateTable("gone", "create table gone(a, b)")
		for id := int64(1); id <= 3; id++ {
			gone.Insert(id, id*100, "gone-text")
		}
		goneSnap := b.Snapshot()
		gonePage := gone.Pages()[0]
		b.DropTable("gone")
		pages := b.Snapshot().Pages()
		w := b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(gonePage, goneSnap.Page(gonePage), 0) // uncommitted: the commit size is 0
		_, h := openAll(t, walMode(withCount(b.Bytes(), pages)), w.Bytes(), nil)
		rows, _ := collectRows(t, h)
		n := 0
		for _, r := range rows {
			if r.Origin != sqlitefile.OriginWALUncommitted {
				continue
			}
			n++
			if r.TableBasis != sqlitefile.BasisFit && r.TableBasis != sqlitefile.BasisGuess {
				t.Fatalf("loose=%v: basis %s, want a fit or guess: %+v", loose, r.TableBasis, r)
			}
			if r.Relation != sqlitefile.RelUnknown {
				t.Errorf("loose=%v: relation %q, want exactly unknown", loose, r.Relation)
			}
			if !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
				t.Errorf("loose=%v: no identity-by-fit-only note", loose)
			}
		}
		if n != 3 {
			t.Fatalf("loose=%v: %d uncommitted-origin rows, want 3: %v", loose, n, methods(rows))
		}
	}
}
