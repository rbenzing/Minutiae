package ioswacalls

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// reasonJoinUndecided prefixes the warning of a table whose join cannot be made (4I2-P7).
const reasonJoinUndecided = "whatsapp-ios-join-undecided"

// reasonTableAbsent is the value of the note join_undecided.<table> for a table the database does not have (PF13).
const reasonTableAbsent = "table absent"

// joinGuard names the tables the mapping joins that cannot be trusted for a join at parse time: Probe defers its alias and
// column checks when a -wal or -journal companion is present (the main file proves nothing then), and the mapping joins by rowid,
// so a key that is not the rowid alias would attach the wrong row with no marker (4I review F1). Every join through such a table
// is undecided, the state of a damaged join, never "absent". Restated from ioswhatsapp/guard.go: parsers import no other parser
// package.
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

// Match is undecided for a guarded table (the mapping makes no Match: an aggregate's calls are counted once, by loadAggregates).
func (c guardedCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	if _, bad := c.g.reason(table); bad {
		return nil, sqlitedb.ErrKeyUndecidable
	}
	return c.MapContext.Match(table, col, key)
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

// joinTables are the tables the mapping joins, with the column each join needs. The key of the join is the rowid alias.
var joinTables = []struct {
	Table, Need string
}{
	{tableAgg, colIncoming},
}

const joinAlias = "Z_PK"

// dataDamage says an error is the database's damage (a table that cannot be read), not a budget, context or I/O fact.
func dataDamage(err error) bool {
	var us *sqlitedb.UnsupportedSchemaError
	return errors.As(err, &us) || errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrEngineRefuses) ||
		errors.Is(err, sqlitedb.ErrLiveUnavailable) || errors.Is(err, sqlitedb.ErrKeyUndecidable) || errors.Is(err, sqlitedb.ErrWithoutRowid)
}

// checkJoinSchema looks at the live view's schema for every table the mapping joins: the key must be the rowid alias and the
// column the join needs must exist. A table the database lacks is the note join_undecided.<table> = "table absent" (no warning:
// the absence is one fact about the file, counted per record by the mapping's own marks). Any other table that fails is guarded
// and stated: one counted warning and one note per table; the job stays complete. The primary table whose key is not the alias
// is the typed unsupported-schema error.
func checkJoinSchema(ctx context.Context, db *sqlitedb.DB, out parse.Emitter) (*joinGuard, error) {
	g := &joinGuard{}
	t, err := db.Table(ctx, tableCall, nil, nil)
	switch {
	case err == nil:
		if i := t.Col(joinAlias); i < 0 || !t.Cols()[i].RowidAlias {
			return nil, &sqlitedb.UnsupportedSchemaError{Table: tableCall, Reason: "column " + joinAlias + " is not the rowid alias"}
		}
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		// preflight has decided what an absent identifying table means
	case dataDamage(err):
		// the call's Z_PK is the Only of every aggregate count: with the table unreadable, no alias is proven, so no join is trusted
		for _, jt := range joinTables {
			g.add(jt.Table, "the call table could not be read whole: "+tableCall)
		}
	default:
		return nil, err // a budget, context or I/O fact is the job's failure, never a state
	}
	for _, jt := range joinTables {
		if _, bad := g.reason(jt.Table); bad {
			continue
		}
		t, err := db.Table(ctx, jt.Table, []string{jt.Need}, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			out.Note("join_undecided."+jt.Table, reasonTableAbsent)
			// no guard entry: Get on a missing table fails with ErrNoSuchTable, which the mapping reads as undecided
			continue
		case errors.As(err, &us):
			reason := "the table could not be read"
			if len(us.Missing) > 0 {
				reason = "the table lacks column(s) " + strings.Join(us.Missing, ", ")
			}
			g.add(jt.Table, reason)
			continue
		case dataDamage(err):
			g.add(jt.Table, "the table could not be read whole")
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

// joinState is the answer of a join: never "no rows" for what is really unknown.
type joinState int

const (
	jFound     joinState = iota // the join was made
	jNoRow                      // a clean miss: no row has the key
	jUndecided                  // the join cannot be decided: a missing or guarded table, an undecidable key
)

// joinFailure turns the errors that say "this join is unknown" into a state; every other error is fatal. A table the database
// lacks is undecided, not a miss (PF3).
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

// countingEmitter counts the records whose raw.join_undecided names a table (4I2-P7): the mapping cannot warn.
type countingEmitter struct {
	parse.Emitter
	counts map[string]int64
	seen   map[string]bool // the warnings stated so far, by locator and reason
	// readerStated counts the reader warnings (locator sqlite:file=) stated through the emitter and runDropped is the count of
	// rowrun.Run's own note warnings_dropped, held back so that Parse writes one total for the shared cap of 1000
	readerStated int
	runDropped   int
}

// Warn records the warning, so that a warning held back by preflight is not stated twice.
func (c *countingEmitter) Warn(ctx context.Context, locator, reason string) error {
	c.seen[locator+"|"+reason] = true
	if strings.HasPrefix(locator, "sqlite:file=") {
		c.readerStated++
	}
	return c.Emitter.Warn(ctx, locator, reason)
}

// Note holds back warnings_dropped of rowrun.Run: Parse states the total.
func (c *countingEmitter) Note(key, value string) {
	if key == "warnings_dropped" {
		if n, err := strconv.Atoi(value); err == nil {
			c.runDropped += n
			return
		}
	}
	c.Emitter.Note(key, value)
}

// forward states the held-back reader warnings that rowrun.Run did not state itself, up to what is left of the cap of
// maxPreflightWarnings reader warnings (preflight's and Run's count against it), and returns how many it dropped.
func (c *countingEmitter) forward(ctx context.Context, pending []readerWarning, used int) (int, error) {
	dropped := 0
	for _, w := range pending {
		if c.seen[w.Locator+"|"+w.Reason] {
			continue
		}
		if used+c.readerStated >= maxPreflightWarnings {
			dropped++
			continue
		}
		if err := c.Warn(ctx, w.Locator, w.Reason); err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

func (c *countingEmitter) Emit(ctx context.Context, r records.Record) error {
	raw, _ := r.Payload["raw"].(map[string]any)
	if list, ok := raw["join_undecided"].([]any); ok {
		for _, t := range list {
			if name, ok := t.(string); ok {
				c.counts[name]++
			}
		}
	}
	return c.Emitter.Emit(ctx, r)
}

// warn raises one warning per table with an undecided join, in table order: locator sqlite:table=<t>.
func (c *countingEmitter) warn(ctx context.Context) error {
	tables := make([]string, 0, len(c.counts))
	for t := range c.counts {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	for _, t := range tables {
		if err := c.Warn(ctx, "sqlite:table="+t, reasonJoinUndecided+": "+t+": "+strconv.FormatInt(c.counts[t], 10)+" records"); err != nil {
			return err
		}
	}
	return nil
}
