package recrun

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// NoteJoinsFromLiveView is the note every typed recovered row carries (4G-R8): the joins a mapping
// makes for a recovered row read the LIVE tables of the bundle, which may belong to a later
// state or to another row that reused an id.
const NoteJoinsFromLiveView = "joins-from-live-view"

// FactsOf computes, from the recovered row alone, the facts the host stamps on the record a
// mapping built from it. Nothing is read from the mapping or invented: the confidence is the
// library's (a function of method and table basis), the locator the shared one, the range the
// cell's, and a WAL or journal position is stated only for a row whose cell lies in that file, or
// for a database row of origin db-under-wal (the frame) or db-rolled-back (the journal record).
// The result owns its Notes: the row's notes, then NoteJoinsFromLiveView.
func FactsOf(rr sqlitefile.RecoveredRow) parse.RecoveredFacts {
	f := parse.RecoveredFacts{
		Table:      rr.Table,
		Method:     rr.Method,
		Basis:      string(rr.TableBasis),
		Relation:   string(rr.Relation),
		Origin:     rr.Origin.String(),
		Confidence: sqlitefile.Confidence(rr),
		Range:      records.Range{Offset: rr.Loc.Offset, Length: rr.Loc.Length},
		Locator:    sqlitefile.RecoveredLocator(rr),
		Page:       rr.Loc.Page,
		Cell:       rr.Loc.Cell,
		Truncated:  rr.Truncated,
		Notes:      make([]string, 0, len(rr.Notes)+1),
	}
	f.Notes = append(f.Notes, rr.Notes...)
	f.Notes = append(f.Notes, NoteJoinsFromLiveView)
	switch rr.Loc.File {
	case sqlitefile.FileDB:
		// The cell lies in the database, so Role stays db. A page a committed WAL frame overlays, or
		// a rollback hides, additionally states the companion record that explains it (4G-C3).
		f.Role = parse.RoleDB
		switch {
		case rr.Origin == sqlitefile.OriginDBUnderWAL && rr.WAL != nil:
			f.WAL = &parse.RecoveredWAL{Frame: rr.WAL.Frame, Salt1: rr.WAL.Salt1, Salt2: rr.WAL.Salt2, Committed: rr.WAL.Committed}
		case rr.Origin == sqlitefile.OriginDBRolledBack && rr.Journal != nil:
			j := rr.Journal
			f.Journal = &parse.RecoveredJournal{Record: j.Record, Segment: j.Segment, Hot: j.Hot, Applied: j.Applied}
		}
	case sqlitefile.FileWAL:
		f.Role = parse.RoleWAL
		if w := rr.WAL; w != nil {
			f.WAL = &parse.RecoveredWAL{Frame: w.Frame, Salt1: w.Salt1, Salt2: w.Salt2, Committed: w.Committed}
		}
	case sqlitefile.FileJournal:
		f.Role = parse.RoleJournal
		if j := rr.Journal; j != nil {
			f.Journal = &parse.RecoveredJournal{Record: j.Record, Segment: j.Segment, Hot: j.Hot, Applied: j.Applied}
		}
	}
	return f
}
