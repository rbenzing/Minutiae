package androidcalendar

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// The reasons of the warnings and notes about joins (plan 4I3 "Notes and warnings", 4I2 PF13).
const (
	reasonJoinUndecided = "calendar-join-undecided"
	reasonFlagUndecided = "calendar-flag-undecided"
	reasonTableAbsent   = "table absent"
)

// joinTables are the tables the mapping joins (spec 7.2, 7.3), with the columns each join needs, as Probe checks
// them: Calendars by its rowid alias, the children by event_id.
func joinTables() []rowrun.Req {
	var out []rowrun.Req
	for _, rq := range reqs() {
		switch rq.Table {
		case "Calendars", "Attendees", "Reminders", "CalendarAlerts", "ExtendedProperties", "EventsRawTimes":
			out = append(out, rq)
		}
	}
	return out
}

// joinGuard names the tables the mapping joins that cannot be trusted for a join at parse time: Probe defers its
// alias and column checks when a -wal or -journal companion is present (the main file proves nothing then), and the
// Calendars join is by rowid, so a key that is not the rowid alias would attach the wrong row with no marker. Every
// join through such a table is undecided, never "absent". Restated from the WhatsApp parsers: parsers import no other
// parser package.
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

// checkJoinSchema looks at the live view's schema for every table the mapping joins. A table the database lacks is
// the note join_undecided.<table> = "table absent" and no warning (4I2 PF13: the absence is one fact about the file;
// each record states its own undecided join). A table that lacks a column its join needs, whose key is not the rowid
// alias, or that cannot be read is guarded and stated: one note and one counted warning per table; the job stays
// complete.
func checkJoinSchema(ctx context.Context, db *sqlitedb.DB, out parse.Emitter) (*joinGuard, error) {
	g := &joinGuard{}
	for _, jt := range joinTables() {
		t, err := db.Table(ctx, jt.Table, jt.Need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			out.Note("join_undecided."+jt.Table, reasonTableAbsent)
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

// counts are the records Deleted by Events.deleted (soft deletes) and by Events.lastSynced (last-synced copies), for
// the count notes Task 5 writes (F08).
type counts struct{ SoftDeleted, LastSynced int64 }

// flagColumns are the flags whose undecided states are counted (4I3-P9), in the order of their warnings, with the
// raw state key that marks a record.
var flagColumns = []struct{ Column, Key string }{
	{srcEventsDeleted, "deleted_state"},
	{srcLastSynced, "lastSynced_state"},
	{"Events.dirty", "dirty_state"},
	{srcCalendarDeleted, "deleted_via_state"},
}

// countingEmitter counts what the mapping cannot warn about (4I3-P9, spec B9): a mapper cannot warn and still emit
// its record, so the records with an undecided flag or join are counted here and warned about once per column or
// table after rowrun.Run.
type countingEmitter struct {
	parse.Emitter
	flags map[string]int64 // "<Table>.<col>" -> records with that flag undecided
	joins map[string]int64 // table -> records whose raw.join_undecided names it
	c     counts
}

func newCountingEmitter(out parse.Emitter) *countingEmitter {
	return &countingEmitter{Emitter: out, flags: map[string]int64{}, joins: map[string]int64{}}
}

func (c *countingEmitter) Emit(ctx context.Context, r records.Record) error {
	raw, _ := r.Payload["raw"].(map[string]any)
	for _, f := range flagColumns {
		if raw[f.Key] == flagUndecided {
			c.flags[f.Column]++
		}
	}
	if list, ok := raw["join_undecided"].([]any); ok {
		for _, t := range list {
			if name, ok := t.(string); ok {
				c.joins[name]++
			}
		}
	}
	if del, ok := r.Payload["deleted"].(map[string]any); ok {
		switch del["source"] {
		case srcEventsDeleted:
			c.c.SoftDeleted++
		case srcLastSynced:
			c.c.LastSynced++
		}
	}
	return c.Emitter.Emit(ctx, r)
}

// warn raises one counted warning per undecided flag column (locator sqlite:table=<Table>) and then one per table
// with an undecided join, in table order.
func (c *countingEmitter) warn(ctx context.Context) error {
	for _, f := range flagColumns {
		n := c.flags[f.Column]
		if n == 0 {
			continue
		}
		table, _, _ := strings.Cut(f.Column, ".")
		if err := c.Warn(ctx, "sqlite:table="+table, reasonFlagUndecided+": "+f.Column+": "+strconv.FormatInt(n, 10)+" records"); err != nil {
			return err
		}
	}
	tables := make([]string, 0, len(c.joins))
	for t := range c.joins {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	for _, t := range tables {
		if err := c.Warn(ctx, "sqlite:table="+t, reasonJoinUndecided+": "+t+": "+strconv.FormatInt(c.joins[t], 10)+" records"); err != nil {
			return err
		}
	}
	return nil
}
