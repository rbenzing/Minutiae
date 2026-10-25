package recrun_test

import (
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func baseRow() sqlitefile.RecoveredRow {
	id := int64(7)
	return sqlitefile.RecoveredRow{
		Method: sqlitefile.MethodFreelist, Origin: sqlitefile.OriginFreelistLeaf, Table: "people",
		TableBasis: sqlitefile.BasisFit, Rowid: &id, Relation: sqlitefile.RelUnknown,
		Loc:   sqlitefile.Loc{File: sqlitefile.FileDB, Page: 5, Cell: 3, Offset: 20449, Length: 31, PageOffset: 20000},
		Notes: []string{sqlitefile.NoteIdentityByFitOnly},
	}
}

func TestFactsOfDatabaseRow(t *testing.T) {
	rr := baseRow()
	got := recrun.FactsOf(rr)
	want := parse.RecoveredFacts{
		Table: "people", Method: sqlitefile.MethodFreelist, Basis: "fit", Relation: "unknown", Origin: sqlitefile.OriginFreelistLeaf.String(),
		Confidence: sqlitefile.Confidence(rr), Role: parse.RoleDB, Range: records.Range{Offset: 20449, Length: 31},
		Locator: sqlitefile.RecoveredLocator(rr), Page: 5, Cell: 3,
		Notes: []string{sqlitefile.NoteIdentityByFitOnly, recrun.NoteJoinsFromLiveView},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("facts %+v\nwant  %+v", got, want)
	}
	if got.Confidence != 60 || got.Origin != "freelist-leaf" || got.Locator != "sqlite:table=people;page=5;cell=3;freelist_page=5;file=db" {
		t.Errorf("confidence %d origin %q locator %q", got.Confidence, got.Origin, got.Locator)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
	// the row's own notes are not written into (the result owns its slice)
	for i := range got.Notes {
		got.Notes[i] = "changed"
	}
	if rr.Notes[0] != sqlitefile.NoteIdentityByFitOnly || len(rr.Notes) != 1 {
		t.Errorf("FactsOf aliased the row's notes: %v", rr.Notes)
	}
}

func TestFactsOfWALRow(t *testing.T) {
	rr := baseRow()
	rr.Method, rr.Origin, rr.TableBasis, rr.Relation = sqlitefile.MethodWALUncommitted, sqlitefile.OriginWALUncommitted, sqlitefile.BasisSchema, sqlitefile.RelUncommitted
	rr.Loc = sqlitefile.Loc{File: sqlitefile.FileWAL, Page: 3, Cell: -1, Offset: 1628, Length: 21, PageOffset: 1104, Frame: 2}
	rr.WAL = &sqlitefile.WALProv{Frame: 2, Salt1: 11, Salt2: 22, Committed: false}
	rr.Notes = nil
	got := recrun.FactsOf(rr)
	if got.Role != parse.RoleWAL || got.WAL == nil || *got.WAL != (parse.RecoveredWAL{Frame: 2, Salt1: 11, Salt2: 22}) || got.Journal != nil {
		t.Errorf("facts %+v wal %+v", got, got.WAL)
	}
	if got.Cell != -1 || got.Basis != "schema" || got.Relation != "uncommitted" || got.Confidence != 45 {
		t.Errorf("facts %+v", got)
	}
	if !reflect.DeepEqual(got.Notes, []string{recrun.NoteJoinsFromLiveView}) {
		t.Errorf("notes %v", got.Notes)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
	rr.WAL.Committed = true
	if g := recrun.FactsOf(rr); g.WAL == nil || !g.WAL.Committed {
		t.Error("Committed lost")
	}
}

func TestFactsOfJournalRow(t *testing.T) {
	rr := baseRow()
	rr.Method, rr.Origin, rr.TableBasis, rr.Relation = sqlitefile.MethodJournalPersist, sqlitefile.OriginJournalBefore, sqlitefile.BasisSchema, sqlitefile.RelSupersededVersion
	rr.Loc = sqlitefile.Loc{File: sqlitefile.FileJournal, Page: 3, Cell: 0, Offset: 4599, Length: 13, PageOffset: 4096, Record: 4}
	rr.Journal = &sqlitefile.JournalProv{Record: 4, Segment: 1, Hot: true, Applied: true}
	got := recrun.FactsOf(rr)
	if got.Role != parse.RoleJournal || got.Journal == nil || *got.Journal != (parse.RecoveredJournal{Record: 4, Segment: 1, Hot: true, Applied: true}) || got.WAL != nil {
		t.Errorf("facts %+v journal %+v", got, got.Journal)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
}

// A page the journal hides, or a database page under a WAL, lies in the DATABASE file: the facts
// name no journal or WAL position for it (they would not validate), and stay valid.
func TestFactsOfDatabaseRowWithCompanionProvenance(t *testing.T) {
	rr := baseRow()
	rr.Method, rr.Origin, rr.TableBasis, rr.Relation = sqlitefile.MethodJournalRolledBack, sqlitefile.OriginDBRolledBack, sqlitefile.BasisSchema, sqlitefile.RelUncommitted
	rr.Journal = &sqlitefile.JournalProv{Record: 1, Hot: true, Applied: true}
	rr.WAL = &sqlitefile.WALProv{Frame: 1}
	got := recrun.FactsOf(rr)
	if got.Role != parse.RoleDB || got.Journal != nil || got.WAL != nil {
		t.Errorf("facts %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Error(err)
	}
}

func TestFactsOfCarriesTruncation(t *testing.T) {
	rr := baseRow()
	rr.Truncated = true
	if !recrun.FactsOf(rr).Truncated {
		t.Error("Truncated lost")
	}
	rr.Truncated = false
	if recrun.FactsOf(rr).Truncated {
		t.Error("Truncated invented")
	}
}
