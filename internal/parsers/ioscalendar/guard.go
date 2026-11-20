package ioscalendar

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

// The join tables of the mapping (spec 8.1, 8.2).
const (
	tableLocation    = "Location"
	tableCalendar    = "Calendar"
	tableStore       = "Store"
	tableParticipant = "Participant"
	tableIdentity    = "Identity"
	tableRecurrence  = "Recurrence"
	tableExceptions  = "ExceptionDate"
	tableAlarm       = "Alarm"
)

// The reasons of the warnings and notes about joins (plan 4I3 "Notes and warnings", 4I2 PF13).
const (
	reasonJoinUndecided = "calendar-join-undecided"
	reasonTableAbsent   = "table absent"
	// reasonSchemaPartial is the join note of a join table that a schema read in part does not show: unknown, never
	// absent (coordinator ruling i of Task 5).
	reasonSchemaPartial = "unreadable: the schema could not be read whole and the table is not in what was read"
)

// joinTables are the eight tables the mapping joins (spec 8.2), with what each join needs: ROWID as the rowid alias
// where the join reads by key (Get: Location, Calendar, Store, Participant, Identity), the owner column where it
// matches (Match: Participant, Recurrence, ExceptionDate, Alarm). A fresh slice per call: the package holds no mutable
// state.
func joinTables() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableCalendar, Alias: "ROWID"},
		{Table: tableStore, Alias: "ROWID"},
		{Table: tableLocation, Alias: "ROWID"},
		{Table: tableParticipant, Need: []string{"item_id"}, Alias: "ROWID"},
		{Table: tableIdentity, Alias: "ROWID"},
		{Table: tableAlarm, Need: []string{"calendaritem_owner_id"}},
		{Table: tableRecurrence, Need: []string{"owner_id"}},
		{Table: tableExceptions, Need: []string{"owner_id"}},
	}
}

// joinGuard names the join tables that cannot be trusted for a join at parse time: Probe defers its alias checks when
// a -wal or -journal companion is present (the main file proves nothing then), a Get join is by rowid, so a ROWID
// that is not the rowid alias would attach the wrong row with no marker, and a Match join needs its owner column.
// Every join through such a table is undecided, never "absent". Restated from android.calendar: parsers import no
// other parser package.
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
// each record states its own undecided join). A table not found in a schema that was read only in part (partial, asked
// only then) is not absent: it is guarded as reasonSchemaPartial. A table that lacks a column its join needs, whose
// ROWID is not the rowid alias, or that cannot be read is guarded and stated: one note and one counted warning per
// table; the job stays complete.
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

// The row warnings of rowrun.Run that Parse counts (plan 4I3 Task 8 review M4): the non-event rows and the rows
// rowrun refuses because a join is undecidable. Any other warning, at whatever locator, is never counted.
const reasonUndecidableJoin = "a join of the row is undecidable: "

// countingEmitter is the emitter Parse hands rowrun.Run (4I3-P9, spec B9). A mapper cannot warn and still emit its
// record, so it counts the records with an undecided join (raw.join_undecided) and warns once per table after the
// run. It also counts rowrun's row warnings by their reason (Task 8 review M4) and holds back rowrun's note
// warnings_dropped (the reader warnings past its cap), so that Parse writes one total (stateDropped).
type countingEmitter struct {
	parse.Emitter
	joins      map[string]int64 // table -> records whose raw.join_undecided names it
	rowWarned  int64
	runDropped int64
}

func newCountingEmitter(out parse.Emitter) *countingEmitter {
	return &countingEmitter{Emitter: out, joins: map[string]int64{}}
}

// Emit counts the record's undecided joins and passes it on.
func (c *countingEmitter) Emit(ctx context.Context, r records.Record) error {
	raw, _ := r.Payload["raw"].(map[string]any)
	if list, ok := raw["join_undecided"].([]any); ok {
		for _, t := range list {
			if name, ok := t.(string); ok {
				c.joins[name]++
			}
		}
	}
	return c.Emitter.Emit(ctx, r)
}

// Warn counts a row warning of rowrun.Run by its reason and passes every warning on.
func (c *countingEmitter) Warn(ctx context.Context, locator, reason string) error {
	if reason == reasonNotEvent || strings.HasPrefix(reason, reasonUndecidableJoin) {
		c.rowWarned++
	}
	return c.Emitter.Warn(ctx, locator, reason)
}

// Note holds back rowrun.Run's warnings_dropped.
func (c *countingEmitter) Note(key, value string) {
	if key == "warnings_dropped" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			c.runDropped += n
			return
		}
	}
	c.Emitter.Note(key, value)
}

// warnJoins raises one counted warning per table with an undecided join (locator sqlite:table=<table>), in table
// order, through Warn: these warnings share the locator form of the row warnings and are not counted as rows (M4).
func (c *countingEmitter) warnJoins(ctx context.Context) error {
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

// stateDropped states the CalendarItem rows rowrun.Run did not warn for (its cap of 1,000 warnings per run): one
// warning naming their number (spec 8.2, 9), and the note warnings_dropped with every warning dropped.
func (c *countingEmitter) stateDropped(ctx context.Context, rep rowrun.Report) error {
	var unmapped int64
	for _, tr := range rep.Tables {
		if tr.Table == tableItem {
			unmapped += tr.Unmapped
		}
	}
	rows := max(unmapped-c.rowWarned, 0)
	if total := rows + c.runDropped; total > 0 {
		c.Emitter.Note("warnings_dropped", strconv.FormatInt(total, 10))
	}
	if rows == 0 {
		return nil
	}
	return c.Emitter.Warn(ctx, "sqlite:table="+tableItem, strconv.FormatInt(rows, 10)+" more CalendarItem row warnings dropped after the cap of 1000")
}
