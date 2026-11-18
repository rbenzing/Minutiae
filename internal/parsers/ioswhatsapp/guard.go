package ioswhatsapp

import (
	"context"
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// reasonJoinUndecided prefixes the warning of a table whose join cannot be made (4I2-P7).
const reasonJoinUndecided = "whatsapp-ios-join-undecided"

// reasonTableAbsent is the value of the note join_undecided.<table> for a table the database does not have (PF13).
const reasonTableAbsent = "table absent"

// joinGuard names the tables the mapping joins that cannot be trusted for a join at parse time: Probe defers its alias
// and column checks when a -wal or -journal companion is present (the main file proves nothing then), and the mapping
// joins by rowid, so a key that is not the rowid alias would attach the wrong row with no marker (4I review F1). Every
// join through such a table is undecided, the state of a damaged join, never "absent". Restated from
// androidwhatsapp/guard.go: parsers import no other parser package.
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

// UndecidedHits forwards the host context's count (rowrun.Match reads it from the context it is given).
func (c guardedCtx) UndecidedHits(table, col string) int64 {
	if j, ok := c.MapContext.(undecidedHits); ok {
		return j.UndecidedHits(table, col)
	}
	return 0
}

// undecidedHits is what *sqlitedb.Context offers beside parse.MapContext.
type undecidedHits interface {
	UndecidedHits(table, col string) int64
}

// withGuard makes the mapping read through the guard.
func withGuard(mp parse.TableMapping, g *joinGuard) parse.TableMapping {
	if g == nil || len(g.bad) == 0 {
		return mp
	}
	inner := mp.Map
	mp.Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return inner(guardedCtx{MapContext: m, g: g}, row)
	}
	return mp
}

// joinTables are the tables the mapping joins, with the column each join needs and the key it joins by (the rowid alias).
var joinTables = []struct {
	Table, Need string
}{
	{"ZWACHATSESSION", "ZCONTACTJID"},
	{"ZWAGROUPMEMBER", "ZMEMBERJID"},
	{"ZWAMEDIAITEM", "ZMESSAGE"},
	{"ZWAMESSAGEINFO", "ZRECEIPTINFO"},
}

const joinAlias = "Z_PK"

// checkJoinSchema looks at the live view's schema for every table the mapping joins: the key must be the rowid alias and
// the column the join needs must exist. A table the database lacks is the note join_undecided.<table> = "table absent"
// (no warning: the absence is one fact about the file, counted per record by the mapping's own marks). Any other table
// that fails is guarded and stated: one counted warning and one note per table; the job stays complete. The primary
// table whose key is not the alias is the typed unsupported-schema error.
func checkJoinSchema(ctx context.Context, db *sqlitedb.DB, out parse.Emitter) (*joinGuard, error) {
	if t, err := db.Table(ctx, tableMessage, nil, nil); err == nil {
		if i := t.Col(joinAlias); i < 0 || !t.Cols()[i].RowidAlias {
			return nil, &sqlitedb.UnsupportedSchemaError{Table: tableMessage, Reason: "column " + joinAlias + " is not the rowid alias"}
		}
	}
	g := &joinGuard{}
	for _, jt := range joinTables {
		t, err := db.Table(ctx, jt.Table, []string{jt.Need}, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			out.Note("join_undecided."+jt.Table, reasonTableAbsent)
			// no guard entry: Get and Match on a missing table fail with ErrNoSuchTable, which the mapping reads as undecided
			continue
		case errors.As(err, &us):
			reason := "the table could not be read"
			if len(us.Missing) > 0 {
				reason = "the table lacks column(s) " + strings.Join(us.Missing, ", ")
			}
			g.add(jt.Table, reason)
			continue
		case err != nil:
			return nil, err
		}
		if i := t.Col(joinAlias); i < 0 || !t.Cols()[i].RowidAlias {
			g.add(jt.Table, "column "+joinAlias+" is not the rowid alias")
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
