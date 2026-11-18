package androidwhatsapp

import (
	"context"
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// reasonJoinUndecided prefixes the warning of a table whose join cannot be made.
const reasonJoinUndecided = "whatsapp-join-undecided"

// joinGuard names the tables the parser joins that cannot be trusted for a join at parse time: Probe defers its alias
// and column checks when a -wal or -journal companion is present (the main file proves nothing then), and Parse joins
// by rowid, so a key that is not the rowid alias would attach the wrong row with no marker (4I review F1). Every join
// through such a table is undecided, the state of a damaged join, never "absent".
type joinGuard struct {
	bad   map[string]string // lower-case table name -> why
	order []string          // the tables in the order they were found
}

func (g *joinGuard) reason(table string) (string, bool) {
	if g == nil {
		return "", false
	}
	r, ok := g.bad[strings.ToLower(table)]
	return r, ok
}

func (g *joinGuard) add(table, reason string) {
	if g.bad == nil {
		g.bad = map[string]string{}
	}
	k := strings.ToLower(table)
	if _, dup := g.bad[k]; !dup {
		g.bad[k] = reason
		g.order = append(g.order, table)
	}
}

// guardedCtx is the MapContext of a mapping whose joins through the tables of the guard are undecided.
type guardedCtx struct {
	parse.MapContext
	g *joinGuard
}

// Get is undecided for a guarded table.
func (c guardedCtx) Get(table string, rowid int64) (parse.Row, bool, error) {
	if _, bad := c.g.reason(table); bad {
		return nil, false, sqlitedb.ErrKeyUndecidable
	}
	return c.MapContext.Get(table, rowid)
}

// Match is undecided for a guarded table.
func (c guardedCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	if _, bad := c.g.reason(table); bad {
		return nil, sqlitedb.ErrKeyUndecidable
	}
	return c.MapContext.Match(table, col, key)
}

// UndecidedHits forwards the host context's count.
func (c guardedCtx) UndecidedHits(table, col string) int64 {
	if j, ok := c.MapContext.(undecidedHits); ok {
		return j.UndecidedHits(table, col)
	}
	return 0
}

// badTable says whether the joins through table are undecided.
func badTable(m parse.MapContext, table string) bool {
	c, ok := m.(guardedCtx)
	if !ok {
		return false
	}
	_, bad := c.g.reason(table)
	return bad
}

// withGuard makes the mapping read through the guard.
func withGuard(mp parse.TableMapping, g *joinGuard) parse.TableMapping {
	if len(g.bad) == 0 {
		return mp
	}
	inner := mp.Map
	mp.Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return inner(guardedCtx{MapContext: m, g: g}, row)
	}
	return mp
}

// checkJoinSchema looks at the live view's schema for every table the mappings join: where the join key is the rowid it
// must be the rowid alias, and a join table must have the column the join needs. The primary message table's key that
// is not the alias is the typed unsupported-schema error. Any other table that fails is guarded and stated: one counted
// warning and one note per table; the job stays complete.
func checkJoinSchema(ctx context.Context, in *parse.Input, d detection, out parse.Emitter) (*joinGuard, error) {
	files, err := filesOf(in)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, err
	}
	defer db.Release()
	g := &joinGuard{}
	for _, rq := range reqsFor(true, true) {
		primary := (rq.Table == tblMessages && d.hasMessages) || (rq.Table == tblMessage && d.hasMessage)
		t, err := db.Table(ctx, rq.Table, rq.Need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			continue
		case errors.As(err, &us):
			if primary {
				continue // the run reports a message table that lacks its columns
			}
			reason := "the table could not be read"
			if len(us.Missing) > 0 {
				reason = "the table lacks column(s) " + strings.Join(us.Missing, ", ")
			}
			g.add(rq.Table, reason)
			continue
		case err != nil:
			return nil, err
		}
		if rq.Alias == "" {
			continue
		}
		if i := t.Col(rq.Alias); i < 0 || !t.Cols()[i].RowidAlias {
			reason := "column " + rq.Alias + " is not the rowid alias"
			if primary {
				return nil, &sqlitedb.UnsupportedSchemaError{Table: rq.Table, Reason: reason}
			}
			g.add(rq.Table, reason)
		}
	}
	for _, table := range g.order {
		reason, _ := g.reason(table)
		out.Note("join_undecided."+table, reason)
		if err := out.Warn(ctx, "sqlite:schema", reasonJoinUndecided+": table "+table+": "+reason+"; every join through it is undecided"); err != nil {
			return nil, err
		}
	}
	return g, nil
}
