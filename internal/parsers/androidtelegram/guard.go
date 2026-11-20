package androidtelegram

import (
	"context"
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The reasons of the warnings and notes about joins (the plan's "Notes and warnings", 4I2 PF13).
const (
	reasonJoinUndecided = "telegram-join-undecided"
	reasonTableAbsent   = "table absent"
	// reasonSchemaPartial is the join note of a join table that a schema read in part does not show: unknown, never
	// absent.
	reasonSchemaPartial = "unreadable: the schema could not be read whole and the table is not in what was read"
)

// joinTables are the tables the mappings join (spec 6.3), with the columns each join needs, as Probe checks them:
// chats, contacts, users_data and enc_chats, each by its uid rowid alias. users is a mapped table, not a join table.
func joinTables() []rowrun.Req {
	var out []rowrun.Req
	for _, rq := range reqs() {
		switch rq.Table {
		case tableChats, tableContacts, tableUsersData, tableEncChats:
			out = append(out, rq)
		}
	}
	return out
}

// joinGuard names the tables the mappings join that cannot be trusted for a join at parse time: Probe defers its
// alias and column checks when a -wal or -journal companion is present (the main file proves nothing then), and the
// joins are by rowid, so a key that is not the rowid alias would attach the wrong row with no marker. Every join
// through such a table is undecided, never "absent". Restated from androidcalendar/guard.go (F29: parsers import no
// other parser package).
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

// undecidedHits is what the host context offers beside parse.MapContext (rowrun.Match reads it).
type undecidedHits interface {
	UndecidedHits(table, col string) int64
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

// dataDamage says an error is the database's damage (a table that cannot be read), not a budget, context or I/O fact.
func dataDamage(err error) bool {
	var us *sqlitedb.UnsupportedSchemaError
	return errors.As(err, &us) || errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrEngineRefuses) ||
		errors.Is(err, sqlitedb.ErrLiveUnavailable) || errors.Is(err, sqlitedb.ErrKeyUndecidable) || errors.Is(err, sqlitedb.ErrWithoutRowid)
}

// checkJoinSchema looks at the live view's schema for every table the mappings join. A table the database lacks is
// the note join_undecided.<table> = "table absent" and no warning (4I2 PF13: the absence is one fact about the file;
// each record states its own undecided join). A table not found in a schema that was read only in part (partial, asked
// only then) is not absent: it is guarded as reasonSchemaPartial. A table that lacks a column its join needs, whose key
// is not the rowid alias, or that cannot be read is guarded and stated: one note and one counted warning per table;
// the job stays complete.
func checkJoinSchema(ctx context.Context, db *sqlitedb.DB, out parse.Emitter, partial func() (bool, error)) (*joinGuard, error) {
	g := &joinGuard{}
	var part *bool
	for _, jt := range joinTables() {
		t, err := db.Table(ctx, jt.Table, jt.Need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			if part == nil {
				p, perr := partial()
				if perr != nil {
					return nil, perr
				}
				part = &p
			}
			if *part {
				g.add(jt.Table, reasonSchemaPartial)
			} else {
				out.Note("join_undecided."+jt.Table, reasonTableAbsent)
			}
			continue
		case errors.As(err, &us) && len(us.Missing) > 0:
			g.add(jt.Table, "the table lacks column(s) "+strings.Join(us.Missing, ", "))
			continue
		case dataDamage(err):
			g.add(jt.Table, "the table could not be read")
			continue
		case err != nil:
			return nil, err
		}
		if jt.Alias == "" {
			continue
		}
		if i := t.Col(jt.Alias); i < 0 || !t.Cols()[i].RowidAlias {
			g.add(jt.Table, "column "+jt.Alias+" is not the rowid alias")
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
