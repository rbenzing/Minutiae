package ioswhatsapp

import (
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// joinState is the answer of a join: never "no rows" for what is really unknown.
type joinState int

const (
	jFound     joinState = iota // the join was made (a Match may return no rows)
	jNoRow                      // a clean miss: no row has the key
	jUndecided                  // the join cannot be decided: a missing or guarded table, an undecidable key, a lossy index
)

// joinFailure turns the errors that say "this join is unknown" into a state; every other error is fatal. A table the
// database lacks is undecided, not a miss (PF3).
func joinFailure(err error) (joinState, error) {
	var us *sqlitedb.UnsupportedSchemaError
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable), errors.As(err, &us), errors.Is(err, sqlitedb.ErrKeyUndecidable):
		return jUndecided, nil
	}
	return jFound, err
}

// getRow returns the row of table with the rowid id (the join column is the rowid alias: checkJoinSchema guards it).
func getRow(m parse.MapContext, table string, id int64) (parse.Row, joinState, error) {
	r, found, err := m.Get(table, id)
	if err != nil {
		st, ferr := joinFailure(err)
		return nil, st, ferr
	}
	if !found {
		return nil, jNoRow, nil
	}
	return r, jFound, nil
}

// matchInt returns the rows of table whose column col equals id.
func matchInt(m parse.MapContext, table, col string, id int64) ([]parse.Row, joinState, error) {
	rows, undecided, err := rowrun.Match(m, table, col, id)
	if err != nil {
		st, ferr := joinFailure(err)
		return nil, st, ferr
	}
	if undecided {
		return nil, jUndecided, nil
	}
	return rows, jFound, nil
}
