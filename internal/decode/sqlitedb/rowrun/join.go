package rowrun

import (
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// joinReporter is what *sqlitedb.Context offers beside parse.MapContext.
type joinReporter interface{ Joins() []sqlitedb.JoinReport }

func undecidedHits(j joinReporter, table, col string) int64 {
	if j == nil {
		return 0
	}
	for _, r := range j.Joins() {
		if asciiFold(r.Table) == asciiFold(table) && asciiFold(r.Column) == asciiFold(col) {
			return r.UndecidedHits
		}
	}
	return 0
}

// Match returns the rows of table whose column col equals id (the engine's equality,
// under the column's collation) and whether the answer is undecided: an
// ErrKeyUndecidable (rows nil) or a hit on an index that lost rows, whose rows may be a
// short answer. An undecided answer is never "no rows" and never "all the rows": the
// caller records the join as undecided in the record (spec 4B, D7).
func Match(m parse.MapContext, table, col string, id int64) ([]parse.Row, bool, error) {
	j, _ := m.(joinReporter)
	before := undecidedHits(j, table, col)
	rows, err := m.Match(table, col, sqlitedb.LiteralIntKey(id))
	switch {
	case errors.Is(err, sqlitedb.ErrKeyUndecidable):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return rows, undecidedHits(j, table, col) > before, nil
}
