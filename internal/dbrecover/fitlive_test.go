package dbrecover

// C45 on the mapper side: the library's equals-live-row note is carried into
// the record unchanged, and neither the record's confidence nor the run's
// duplicate count changes because of it.

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestMapperCarriesEqualsLiveRowNote(t *testing.T) {
	m := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1 << 20})
	r := baseRow()
	r.TableBasis = sqlitefile.BasisFit
	r.Notes = []string{sqlitefile.NoteIdentityByFitOnly}
	plain := mustRecord(t, m, r)
	r.Notes = append(r.Notes, sqlitefile.NoteEqualsLiveRow)
	rec := mustRecord(t, m, r)
	notes, _ := rec.Payload["notes"].([]any)
	if !slices.Contains(notes, any(sqlitefile.NoteEqualsLiveRow)) {
		t.Errorf("notes %v lack %q", notes, sqlitefile.NoteEqualsLiveRow)
	}
	if rec.Payload["relation"] != "unknown" || rec.Payload["table_basis"] != "fit" {
		t.Errorf("relation %v basis %v: the note must not reclassify the row", rec.Payload["relation"], rec.Payload["table_basis"])
	}
	if rec.Confidence == nil || plain.Confidence == nil || *rec.Confidence != *plain.Confidence {
		t.Errorf("confidence %v with the note, %v without", rec.Confidence, plain.Confidence)
	}
	if !rec.Deleted || rec.Recovery == "" {
		t.Errorf("the record lost its recovered flags: %+v", rec)
	}
}

// TestRunCountsFitOnlyRowsEqualToLive: a dropped table with the shape of the
// live table t holds copies of live rows. Every one is delivered and counted
// as fit-only, none is a duplicate of live, and the annotation does not raise
// the confidence above the fit cap.
func TestRunCountsFitOnlyRowsEqualToLive(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: testPageSize})
	live := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	b.CreateTable("pad", "create table pad(z)").Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b)")
	for id := int64(1); id <= 3; id++ {
		gone.Insert(id, id, "live") // equal to the live rows
	}
	b.DropTable("pad")
	b.DropTable("gone")
	data := b.Bytes()
	c := recordstest.NewCase(t)
	art := addFile(t, c, "/data/x.db", data)
	res := mustRun(t, c, Inputs{DB: art.ID}, Options{})
	if res.Counts.FitOnly != 3 || res.Counts.Written != 3 || res.Counts.DuplicateOfLive != 0 || res.Counts.RelationUnknown != 3 {
		t.Fatalf("counts %+v", res.Counts)
	}
	for _, row := range visibleRecords(t, c) {
		if row.Confidence == nil || *row.Confidence > 60 {
			t.Errorf("confidence %v above the fit cap of 60", row.Confidence)
		}
	}
}
