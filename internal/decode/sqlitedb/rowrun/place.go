package rowrun

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// Place says where a row's cell lies, in the terms a record needs (spec C9):
// the artifact that holds the cell (the database, its WAL or its journal), the
// cell's byte range in that artifact and the row's locator.
type Place struct {
	ArtifactID string
	Role       parse.FileRole
	Range      records.Range
	Locator    string // "" for a recovered row: the recovery host stamps its own (plan 4G)
	SourcePath string // the remote path of the file that holds the cell: the db, db+"-wal" or db+"-journal" (ruling T48)
}

type locatorRow interface{ Locator() (string, bool) }

type recoveredRow interface {
	Recovered() *sqlitedb.RecoveredInfo
}

// Where attributes a row to the artifact of the bundle that holds its cell. It
// refuses (Unmappable) a cell in a file that is not in the bundle, a range that
// is not inside that artifact, and a live row that has no locator (a WITHOUT
// ROWID row): an unattributable row is a warning, never a record with a guessed
// artifact.
func Where(m parse.MapContext, row parse.Row) (Place, error) {
	in := m.Input()
	if in == nil {
		return Place{}, Unmappable("the mapping context has no input")
	}
	rng, role := row.Range()
	if role == "" {
		return Place{}, Unmappable("the cell of %s lies in no known file", row.Table())
	}
	art, ok := in.Artifacts[string(role)]
	if !ok {
		return Place{}, Unmappable("the cell of %s lies in the %s file, which is not in the bundle", row.Table(), role)
	}
	if rng.Offset < 0 || rng.Length <= 0 || rng.Offset > art.Size || rng.Length > art.Size-rng.Offset {
		return Place{}, Unmappable("the cell of %s lies outside the %s artifact", row.Table(), role)
	}
	p := Place{ArtifactID: art.ID, Role: role, Range: rng, SourcePath: art.Source.RemotePath}
	if l, ok := row.(locatorRow); ok {
		if loc, ok := l.Locator(); ok {
			p.Locator = loc
			return p, nil
		}
	}
	if r, ok := row.(recoveredRow); ok && r.Recovered() != nil {
		return p, nil
	}
	return Place{}, Unmappable("the %s row has no rowid locator", row.Table())
}
