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

// rowidKeyed says why a join table read by key (Get, by rowid) is not keyed by its rowid, "" when it is: a WITHOUT
// ROWID table has no rowid; a column named alias (ROWID) that is not the rowid alias shadows it, so the stored keys
// may name that column and not the rowid. A table that declares no column of that name is keyed by its implicit rowid,
// which is what alias names in SQL and what a Get reads (the real store's Identity, plan 4I3 Task 12).
func rowidKeyed(t *sqlitedb.Table, alias string) string {
	if t.WithoutRowid() {
		return "the table is WITHOUT ROWID: it has no rowid to join by"
	}
	if i := t.Col(alias); i >= 0 && !t.Cols()[i].RowidAlias {
		return "column " + alias + " is not the rowid alias"
	}
	return ""
}

// joinKind is the kind of a join through a table: by ROWID (Get) or by an owner column (Match). The guard holds each
// kind apart (fix round 1, m5): a Participant without item_id leaves the organizer's Get by ROWID decided.
type joinKind uint8

const (
	joinGet joinKind = 1 << iota
	joinMatch
	joinBoth = joinGet | joinMatch
)

// joinGuard names the joins that cannot be trusted at parse time: Probe defers its alias checks when a -wal or
// -journal companion is present (the main file proves nothing then), a Get join is by rowid, so a ROWID that is not
// the rowid alias would attach the wrong row with no marker, and a Match join needs its owner column. Every such join
// is undecided, never "absent". Restated from android.calendar (parsers import no other parser package), per join kind.
type joinGuard struct {
	bad   map[string]joinKind // lower-case table name -> the guarded kinds
	why   map[string][]string // lower-case table name -> the reasons, in the order found
	order []string            // the tables in the order they were found
}

// guarded says the join of kind k through table is guarded.
func (g *joinGuard) guarded(table string, k joinKind) bool {
	return g != nil && g.bad[strings.ToLower(table)]&k != 0
}

func (g *joinGuard) add(table string, k joinKind, reason string) {
	if g.bad == nil {
		g.bad, g.why = map[string]joinKind{}, map[string][]string{}
	}
	key := strings.ToLower(table)
	if _, seen := g.bad[key]; !seen {
		g.order = append(g.order, table)
	}
	g.bad[key] |= k
	g.why[key] = append(g.why[key], reason)
}

// undecidedHits is what the host context offers beside parse.MapContext (rowrun.Match reads it).
type undecidedHits interface {
	UndecidedHits(table, col string) int64
}

// guardedCtx is the MapContext of a mapping whose guarded joins are undecided.
type guardedCtx struct {
	parse.MapContext
	g *joinGuard
}

// Get is undecided for a table whose Get joins are guarded.
func (c guardedCtx) Get(table string, rowid int64) (parse.Row, bool, error) {
	if c.g.guarded(table, joinGet) {
		return nil, false, sqlitedb.ErrKeyUndecidable
	}
	return c.MapContext.Get(table, rowid)
}

// Match is undecided for a table whose Match joins are guarded.
func (c guardedCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	if c.g.guarded(table, joinMatch) {
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
// only then) is not absent: both kinds of join are guarded as reasonSchemaPartial; so are they for a table that cannot
// be read. A table that lacks the owner column of its Match join has that join guarded, one whose ROWID is not the
// rowid alias its Get join (m5). Each guarded table is stated once: a note (its reasons joined by "; ") and one counted
// warning; the job stays complete.
func checkJoinSchema(ctx context.Context, db *sqlitedb.DB, out parse.Emitter, partial func() (bool, error)) (*joinGuard, error) {
	g := &joinGuard{}
	var part *bool
	for _, jt := range joinTables() {
		t, err := db.Table(ctx, jt.Table, nil, nil)
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
				g.add(jt.Table, joinBoth, reasonSchemaPartial)
			} else {
				out.Note("join_undecided."+jt.Table, reasonTableAbsent)
			}
			continue
		case dataDamage(err):
			g.add(jt.Table, joinBoth, "the table could not be read")
			continue
		case err != nil:
			return nil, err
		}
		var missing []string
		for _, n := range jt.Need {
			if t.Col(n) < 0 {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			g.add(jt.Table, joinMatch, "the table lacks column(s) "+strings.Join(missing, ", ")+" of its join by owner")
		}
		if jt.Alias == "" {
			continue
		}
		if why := rowidKeyed(t, jt.Alias); why != "" {
			g.add(jt.Table, joinGet, why)
		}
	}
	for _, table := range g.order {
		key := strings.ToLower(table)
		reason := strings.Join(g.why[key], "; ")
		out.Note("join_undecided."+table, reason)
		scope := "every join through it"
		switch g.bad[key] {
		case joinGet:
			scope = "every join through it by ROWID"
		case joinMatch:
			scope = "every join through it by owner"
		}
		if err := out.Warn(ctx, "sqlite:schema", reasonJoinUndecided+": table "+table+": "+reason+"; "+scope+" is undecided"); err != nil {
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
