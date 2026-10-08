package sqlitedb

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/parse"
)

// JoinReport counts the lookups of one joined (table, column).
type JoinReport struct {
	Table, Column string
	// Lookups is the number of Match calls; Flagged those with any flag;
	// CollationDiffers and AffinityDiffers those with that flag; Unkeyed the
	// target rows the index could not key.
	Lookups, Flagged, CollationDiffers, AffinityDiffers, Unkeyed int64
}

// Notes is the sink a Context flushes its join notes to; parse.Emitter
// satisfies it.
type Notes interface{ Note(key, value string) }

// Context is the parse.MapContext of a mapping over one database: joins into
// other tables with the engine's equality. It is NOT safe for concurrent use.
// Close it: the indexes and the rows it handed out stay charged to the budget
// until then, and the join notes are emitted at Close only.
type Context struct {
	ctx     context.Context
	db      *DB
	in      *parse.Input
	notes   Notes
	tables  map[string]*Table
	indexes map[string]*Index
	reports map[string]*JoinReport
	rows    []Row
	closed  bool
}

// NewContext returns a Context over db. ctx is used for cancellation only. n
// may be nil.
func NewContext(ctx context.Context, db *DB, in *parse.Input, n Notes) *Context {
	return &Context{
		ctx: ctx, db: db, in: in, notes: n,
		tables: map[string]*Table{}, indexes: map[string]*Index{}, reports: map[string]*JoinReport{},
	}
}

var _ parse.MapContext = (*Context)(nil)

// Input returns the invocation's input.
func (c *Context) Input() *parse.Input { return c.in }

// Note records a note on the sink; it is dropped when there is none.
func (c *Context) Note(key, value string) {
	if c.notes != nil {
		c.notes.Note(key, value)
	}
}

func (c *Context) table(name string) (*Table, error) {
	if c.closed {
		return nil, ErrReleased
	}
	k := asciiFold(name)
	if t, ok := c.tables[k]; ok {
		return t, nil
	}
	t, err := c.db.Table(c.ctx, name, nil, nil)
	if err != nil {
		return nil, err
	}
	c.tables[k] = t
	return t, nil
}

// Column returns the index of column col of table, or -1 when either is
// missing.
func (c *Context) Column(table, col string) int {
	t, err := c.table(table)
	if err != nil {
		return -1
	}
	return t.Col(col)
}

// Get returns the row of table with the given rowid, owned by the Context (it
// stays charged until Close); found is false only for a clean miss. A missing
// table is ErrNoSuchTable.
func (c *Context) Get(table string, rowid int64) (parse.Row, bool, error) {
	t, err := c.table(table)
	if err != nil {
		return nil, false, err
	}
	r, ok, err := t.Get(c.ctx, rowid)
	if err != nil || !ok {
		return nil, false, err
	}
	c.rows = append(c.rows, r)
	return r, true, nil
}

// Match returns the rows of table whose column col equals key under that
// column's collation, in rowid order, owned by the Context. The index of each
// (table, column) is built once with the default cap. A rowid of the index that
// no longer resolves is an error, never a dropped row.
func (c *Context) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	t, err := c.table(table)
	if err != nil {
		return nil, err
	}
	ci := t.Col(col)
	if ci < 0 {
		return nil, &UnsupportedSchemaError{Table: t.name, Missing: []string{col}}
	}
	name := t.Cols()[ci].Name
	ik := asciiFold(t.name) + "\x00" + asciiFold(name)
	ix, ok := c.indexes[ik]
	if !ok {
		if ix, err = t.Index(c.ctx, col, 0); err != nil {
			return nil, err
		}
		c.indexes[ik] = ix
	}
	ids, flags, err := ix.Rowids(key)
	if err != nil {
		return nil, err
	}
	rep := c.reports[ik]
	if rep == nil {
		rep = &JoinReport{Table: t.name, Column: name, Unkeyed: int64(ix.Unkeyed())}
		c.reports[ik] = rep
	}
	rep.Lookups++
	if flags != 0 {
		rep.Flagged++
	}
	if flags&JoinCollationDiffers != 0 {
		rep.CollationDiffers++
	}
	if flags&JoinAffinityDiffers != 0 {
		rep.AffinityDiffers++
	}
	out := make([]parse.Row, 0, len(ids))
	for _, id := range ids {
		r, found, err := t.Get(c.ctx, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: rowid %d of %s.%s is indexed but no longer resolves", ErrCorrupt, id, t.name, name)
		}
		c.rows = append(c.rows, r)
		out = append(out, r)
	}
	return out, nil
}

// Joins returns the per-join counts sorted by (Table, Column). It is readable
// at any time, also after Close.
func (c *Context) Joins() []JoinReport {
	out := make([]JoinReport, 0, len(c.reports))
	for _, r := range c.reports {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

// Close emits at most one note per join that had a flagged lookup or an
// unkeyed target row (key join.<table>.<column>, value
// lookups:N,flagged:N,collation_differs:N,affinity_differs:N,unkeyed:N), counts
// them in Stats.JoinsFlushed and frees the indexes and rows. It is idempotent.
func (c *Context) Close() {
	if c == nil || c.closed {
		return
	}
	c.closed = true
	for _, r := range c.Joins() {
		if r.Flagged == 0 && r.Unkeyed == 0 {
			continue
		}
		c.Note("join."+r.Table+"."+r.Column, strings.Join([]string{
			fmt.Sprintf("lookups:%d", r.Lookups), fmt.Sprintf("flagged:%d", r.Flagged),
			fmt.Sprintf("collation_differs:%d", r.CollationDiffers), fmt.Sprintf("affinity_differs:%d", r.AffinityDiffers),
			fmt.Sprintf("unkeyed:%d", r.Unkeyed),
		}, ","))
		c.db.stats.JoinsFlushed++
	}
	for _, ix := range c.indexes {
		ix.Release()
	}
	for _, r := range c.rows {
		r.Release()
	}
	c.indexes, c.rows, c.tables = nil, nil, nil
}
