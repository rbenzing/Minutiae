package records

// Test exports of the reader's unexported parts.

type Cursor = cursor

var DecodeCursor = decodeCursor

// Encode returns the wire form of c.
func (c cursor) Encode() string { return c.encode() }

// Query is one SQL statement of a listing page and its arguments (the last is the LIMIT).
type Query struct {
	SQL  string
	Args []any
}

// ListQueries returns the SQL statements List would run for the page after cur
// (nil = the first page), each with LIMIT limit; haveSuperseded says
// record_superseded is not empty.
func ListQueries(f Filter, desc bool, cur *Cursor, limit int, haveSuperseded bool) ([]Query, error) {
	qs, err := buildList(f, desc, cur, haveSuperseded)
	if err != nil {
		return nil, err
	}
	out := make([]Query, len(qs))
	for i, q := range qs {
		q.args[len(q.args)-1] = limit
		out[i] = Query{q.sql, q.args}
	}
	return out, nil
}
