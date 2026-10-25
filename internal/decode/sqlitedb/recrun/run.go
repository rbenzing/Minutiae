package recrun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// ErrNoMapping says a claim names a table that none of the mappings maps.
var ErrNoMapping = errors.New("recrun: the claimed table has no mapping")

// TableCounts counts what Run did with one claimed table. Every Delivered row ends as at least
// one record (Mapped) or one Unmapped call: Delivered == Mapped + Unmapped.
type TableCounts struct {
	Table                                string
	Delivered, Mapped, Records, Unmapped int64
	// Skipped: rows whose facts the host would refuse; not Delivered, neither mapped nor unmapped.
	Skipped int64
	// Absent: the live schema has no table of that name; the rows delivered for it, if any, are
	// all Unmapped. (ClaimMatch delivers none: a table the live schema lacks is no-live-fit and
	// stays with the generic run, counted in Report.History.Skipped.)
	Absent bool
}

// Report counts what Run did.
type Report struct {
	Tables   []TableCounts // in claim order
	Unmapped int64         // all tables
	Skipped  int64         // all tables: rows with invalid facts
	History  sqlitedb.HistoryCounts
}

const (
	defaultInvalidFactWarnings = 100  // warnings about rows whose facts are invalid
	maxRunWarnings             = 1000 // the runner's own cap on forwarded library warnings
	minRotateRows              = 1000 // rows between two rotations of the mapping Context
)

// tableState is the live state of one claimed table.
type tableState struct {
	name   string
	m      parse.TableMapping
	table  *sqlitedb.Table // nil when the live schema cannot serve the mapping
	reason string          // why table is nil
	counts int             // index into Report.Tables
	mapped bool            // a mapping for the table was found
}

type runner struct {
	ctx    context.Context
	in     *parse.Input
	out    parse.RecoveryEmitter
	db     *sqlitedb.DB
	mc     *sqlitedb.Context
	states map[string]*tableState
	rep    Report
	since  int

	factsOf        func(sqlitefile.RecoveredRow) parse.RecoveredFacts // a seam for the tests
	maxInvalidWarn int
}

// Run opens the bundle's database (roles db, wal, journal) through sqlitedb and drives the
// mappings of the claimed tables over the recovered rows of their history that
// sqlitefile.ClaimMatch accepts (sqlitedb.DB.ClaimedHistory): each row is rebuilt with
// sqlitedb.FromRecovered, mapped, and every record it yields is handed to out.EmitRecovered
// with FactsOf the row. Run never stamps a recovery fact on a record and never calls out.Emit.
//
// Every delivered row ends as a record or an out.Unmapped call with the reason: a *rowrun.RowError,
// an undecidable join (sqlitedb.ErrKeyUndecidable), a mapping that returns no record, a claimed
// table the live schema lacks or whose required columns are missing. Any other mapping error
// is fatal, as is a failure of the mapping Context (rowrun.ErrContext: a mapping reads Column() == -1
// as "the variant lacks the column", which is only true while the Context is healthy). The
// Context is checked after every row before its records are emitted and is closed and reopened
// when the joined rows have charged half the budget (at most once per 1000 rows).
//
// A row whose FactsOf the host would refuse (Validate fails) is not delivered to the mapping: it is
// counted in Skipped and warned about (the first 100), and the run continues. A caller
// that must record every row (the composed command) treats Report.Skipped > 0 as a claim it cannot honor.
//
// A claim whose table has no mapping is ErrNoMapping before any row is read; a nil Map is
// rowrun.ErrNilMap. A mapper that panics is not caught here: the host recovers it.
func Run(ctx context.Context, in *parse.Input, out parse.RecoveryEmitter, mappings []parse.TableMapping, claims []parse.TableClaim) (Report, error) {
	return run(ctx, in, out, mappings, claims, FactsOf, defaultInvalidFactWarnings)
}

func run(ctx context.Context, in *parse.Input, out parse.RecoveryEmitter, mappings []parse.TableMapping, claims []parse.TableClaim, factsOf func(sqlitefile.RecoveredRow) parse.RecoveredFacts, maxInvalidWarn int) (rep Report, err error) {
	if in == nil || out == nil {
		return rep, rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return rep, err
	}
	r := &runner{ctx: ctx, in: in, out: out, states: map[string]*tableState{}, factsOf: factsOf, maxInvalidWarn: maxInvalidWarn}
	for _, cl := range claims {
		k := asciiFold(cl.Table)
		if r.states[k] != nil {
			continue
		}
		st := &tableState{name: cl.Table, counts: len(r.rep.Tables)}
		for _, m := range mappings {
			if asciiFold(m.Table) == k {
				st.m, st.mapped = m, true
				break
			}
		}
		switch {
		case !st.mapped:
			return rep, fmt.Errorf("%w: %s", ErrNoMapping, cl.Table)
		case st.m.Map == nil:
			return rep, fmt.Errorf("%w: table %s", rowrun.ErrNilMap, cl.Table)
		}
		r.states[k] = st
		r.rep.Tables = append(r.rep.Tables, TableCounts{Table: cl.Table})
	}
	for _, role := range []string{"db", "wal", "journal"} {
		a, ok := in.Artifacts[role]
		if role == "db" {
			a, ok = in.Primary, true
		}
		if ok && a.Incomplete {
			out.Note("incomplete."+role, "true")
		}
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return rep, err
	}
	defer db.Release()
	r.db = db
	// The library's warnings are the evidence of what could not be read; they are forwarded on
	// failure too (this defer runs before db.Release).
	defer func() {
		rep = r.rep
		if werr := r.forwardWarnings(); err == nil {
			err = werr
		}
	}()
	r.newContext()
	if err = r.resolveTables(); err != nil {
		_ = r.closeContext()
		return rep, err
	}
	hc, err := db.ClaimedHistory(ctx, r.isClaimed, r.visit)
	r.rep.History = hc
	if err != nil {
		_ = r.closeContext()
		return rep, err
	}
	if err = r.closeContext(); err != nil {
		return rep, err
	}
	r.notes()
	return rep, nil
}

func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	if in.Primary.R == nil {
		return sqlitedb.Files{}, errors.New("recrun: the primary artifact has no reader")
	}
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("recrun: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("recrun: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

func (r *runner) newContext() { r.mc = sqlitedb.NewContext(r.ctx, r.db, r.in, r.out) }

// closeContext closes the current Context and reports its failure, if any.
func (r *runner) closeContext() error {
	mc := r.mc
	if mc == nil {
		return nil
	}
	r.mc = nil
	mc.Close()
	if err := mc.Err(); err != nil {
		return fmt.Errorf("%w: %w", rowrun.ErrContext, err)
	}
	return nil
}

// resolveTables looks every claimed table up in the live schema, in claim order. A table the
// schema lacks, or lacks required columns of, is not an error: its rows are unmapped with the reason.
func (r *runner) resolveTables() error {
	for i := range r.rep.Tables {
		st := r.states[asciiFold(r.rep.Tables[i].Table)]
		var need []string
		for _, c := range st.m.Columns {
			if c.Required {
				need = append(need, c.Name)
			}
		}
		t, err := r.db.Table(r.ctx, st.m.Table, need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			st.reason = "table " + st.name + " is absent from the live schema"
			r.rep.Tables[st.counts].Absent = true
		case errors.As(err, &us):
			st.reason = us.Error()
		case err != nil:
			return fmt.Errorf("recrun: table %s: %w", st.name, err)
		default:
			st.table = t
		}
	}
	return nil
}

func (r *runner) isClaimed(table string) bool { return r.states[asciiFold(table)] != nil }

// visit handles one claimed recovered row. The row is valid only during the call.
func (r *runner) visit(rr sqlitefile.RecoveredRow) error {
	st := r.states[asciiFold(rr.Table)]
	if st == nil {
		return fmt.Errorf("recrun: the history delivered a row of table %q, which is not claimed", rr.Table)
	}
	facts := r.factsOf(rr)
	if err := facts.Validate(); err != nil {
		return r.skip(st, facts, err)
	}
	tc := &r.rep.Tables[st.counts]
	tc.Delivered++
	r.since++
	if st.table == nil {
		return r.unmapped(tc, facts, st.reason)
	}
	row, err := sqlitedb.FromRecovered(st.table, rr)
	if err != nil {
		return fmt.Errorf("recrun: table %s: %w", st.name, err)
	}
	defer row.Release()
	recs, err := st.m.Map(r.mc, row)
	if cerr := r.mc.Err(); cerr != nil {
		return fmt.Errorf("%w: table %s: %w", rowrun.ErrContext, st.name, cerr)
	}
	if err != nil {
		var re *rowrun.RowError
		switch {
		case errors.As(err, &re):
			err = r.unmapped(tc, facts, re.Reason)
		case errors.Is(err, sqlitedb.ErrKeyUndecidable):
			err = r.unmapped(tc, facts, "a join of the row is undecidable: "+err.Error())
		default:
			return err
		}
		if err != nil {
			return err
		}
		return r.maybeRotate()
	}
	if len(recs) == 0 {
		if err := r.unmapped(tc, facts, "the mapping produced no record"); err != nil {
			return err
		}
		return r.maybeRotate()
	}
	for _, rec := range recs {
		if err := r.out.EmitRecovered(r.ctx, rec, facts); err != nil {
			return err
		}
		tc.Records++
	}
	tc.Mapped++
	return r.maybeRotate()
}

// skip counts a row whose facts the host would refuse (4G-C4). It is not delivered to the mapping and is
// neither mapped nor unmapped; the first 100 are warned about, the rest counted.
func (r *runner) skip(st *tableState, f parse.RecoveredFacts, cause error) error {
	r.rep.Tables[st.counts].Skipped++
	r.rep.Skipped++
	if r.rep.Skipped > int64(r.maxInvalidWarn) {
		return nil
	}
	return r.out.Warn(r.ctx, f.Locator, "row skipped: "+cause.Error())
}

func (r *runner) unmapped(tc *TableCounts, f parse.RecoveredFacts, reason string) error {
	tc.Unmapped++
	r.rep.Unmapped++
	return r.out.Unmapped(r.ctx, f, reason)
}

// maybeRotate gives back the rows and indexes the Context holds once they charge half of the budget.
func (r *runner) maybeRotate() error {
	b := r.in.Budget
	if b == nil || r.since < minRotateRows || b.Used()*2 < b.Limit() {
		return nil
	}
	if err := r.closeContext(); err != nil {
		return err
	}
	r.newContext()
	r.since = 0
	return nil
}

// notes reports the counts through the emitter, in a fixed order.
func (r *runner) notes() {
	for i := range r.rep.Tables {
		tc := r.rep.Tables[i]
		r.out.Note("recovered_rows."+tc.Table, strconv.FormatInt(tc.Delivered, 10))
		if tc.Unmapped > 0 {
			r.out.Note("unmapped_recovered."+tc.Table, strconv.FormatInt(tc.Unmapped, 10))
		}
		if tc.Skipped > 0 {
			r.out.Note("invalid_facts."+tc.Table, strconv.FormatInt(tc.Skipped, 10))
		}
		if tc.Absent {
			r.out.Note("table."+tc.Table, "absent")
		}
	}
	if over := r.rep.Skipped - int64(r.maxInvalidWarn); over > 0 {
		r.out.Note("invalid_facts_warnings_dropped", strconv.FormatInt(over, 10))
	}
	verdicts := make([]string, 0, len(r.rep.History.Skipped))
	for v := range r.rep.History.Skipped {
		verdicts = append(verdicts, v)
	}
	slices.Sort(verdicts)
	for _, v := range verdicts {
		r.out.Note("history.skipped."+v, strconv.FormatInt(r.rep.History.Skipped[v], 10))
	}
	if n := r.rep.History.Stats.DuplicateOfLive; n > 0 {
		r.out.Note("history.duplicate_of_live", strconv.FormatInt(n, 10))
	}
}

// forwardWarnings hands the library's warnings (the live view's, then the history's, each once) to
// the emitter, at most maxRunWarnings of them.
func (r *runner) forwardWarnings() error {
	seen := map[sqlitefile.Warning]bool{}
	sent, dropped := 0, 0
	for _, ws := range [][]sqlitefile.Warning{r.db.Warnings(), r.rep.History.Warnings} {
		for _, w := range ws {
			if seen[w] {
				continue
			}
			seen[w] = true
			if sent >= maxRunWarnings {
				dropped++
				continue
			}
			sent++
			loc := fmt.Sprintf("sqlite:file=%s;page=%d;offset=%d", w.File, w.Page, w.Offset)
			if err := r.out.Warn(r.ctx, loc, w.Code+": "+w.Msg); err != nil {
				return err
			}
		}
	}
	if dropped > 0 {
		r.out.Note("warnings_dropped", strconv.Itoa(dropped))
	}
	return nil
}

// asciiFold lower-cases ASCII letters only, as SQLite compares identifiers.
func asciiFold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
