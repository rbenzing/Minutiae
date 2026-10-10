package rowrun

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Step is one table to map. An Optional table that the database lacks is a note,
// not an error; a required one that is missing fails the job.
type Step struct {
	Mapping  parse.TableMapping
	Optional bool
	// Identifying marks the table whose absence means the file is a database of another
	// kind (sms, calls, raw_contacts): Run then ends cleanly with Report.NotApplicable set,
	// one analysis.warning and the note not_applicable, instead of failing the job (D13).
	// It must be the first step.
	Identifying bool
}

// TableReport counts what Run did with one table.
type TableReport struct {
	Table                   string
	Rows, Records, Unmapped int64
	Absent                  bool
}

// Report counts what Run did.
type Report struct {
	Tables    []TableReport
	Rotations int // times the mapping Context was closed and reopened to give back joined rows
	// NotApplicable says why an Identifying table was absent ("" otherwise); no record was emitted.
	NotApplicable string
}

// errNotApplicable ends the step loop when an Identifying table is absent.
var errNotApplicable = errors.New("rowrun: not applicable")

const (
	maxRunWarnings = 1000 // the runner's own cap; the host's is parse.MaxWarnings
	minRotateRows  = 1000 // rows between two rotations, so an index is not rebuilt per row
)

type seams struct{ afterOpen func(*sqlitedb.DB) }

// Run opens the bundle's database (roles db, wal, journal) through sqlitedb,
// maps the steps' tables in order and hands every record to out.
//
// The mapping Context is checked after every row, before the row's records are
// emitted, and again when it is closed: a Context failure that Column could not
// report fails the job with ErrContext (a mapping reads Column() == -1 as "the
// variant lacks the column", which is only true while the Context is healthy).
// A *RowError (and an ErrKeyUndecidable a mapping let through) is a counted
// analysis.warning for that row; every other mapping error is fatal. When the
// joined rows have charged half of the budget, the Context is closed and
// reopened (at most once per 1000 rows) so a large database cannot exhaust the
// budget through Get and Match rows.
func Run(ctx context.Context, in *parse.Input, out parse.Emitter, steps []Step) (Report, error) {
	return run(ctx, in, out, steps, nil)
}

type runner struct {
	ctx    context.Context
	in     *parse.Input
	out    parse.Emitter
	db     *sqlitedb.DB
	mc     *sqlitedb.Context
	warned int
	since  int
	rep    Report
}

func run(ctx context.Context, in *parse.Input, out parse.Emitter, steps []Step, sm *seams) (rep Report, err error) {
	files, err := filesOf(in)
	if err != nil {
		return rep, err
	}
	for _, role := range []string{"db", "wal", "journal"} {
		a := in.Primary
		if role != "db" {
			var ok bool
			if a, ok = in.Artifacts[role]; !ok {
				continue
			}
		}
		if a.Incomplete {
			out.Note("incomplete."+role, "true")
		}
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return rep, err
	}
	defer db.Release()
	r := &runner{ctx: ctx, in: in, out: out, db: db}
	// The library's warnings are the evidence of what could not be read; they are
	// forwarded on failure too (this defer runs before db.Release).
	defer func() {
		rep = r.rep
		if werr := r.forwardWarnings(); err == nil {
			err = werr
		}
	}()
	if sm != nil && sm.afterOpen != nil {
		sm.afterOpen(db)
	}
	r.newContext()
	for _, st := range steps {
		if err = r.step(st); errors.Is(err, errNotApplicable) {
			_ = r.closeContext()
			r.warned++
			r.out.Note("not_applicable", r.rep.NotApplicable)
			return rep, r.out.Warn(ctx, "sqlite:schema", "not applicable at parse time: "+r.rep.NotApplicable)
		} else if err != nil {
			_ = r.closeContext()
			return rep, err
		}
	}
	return rep, r.closeContext()
}

func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	if in == nil || in.Primary.R == nil {
		return sqlitedb.Files{}, errors.New("rowrun: the primary artifact has no reader")
	}
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("rowrun: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("rowrun: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

func (r *runner) newContext() { r.mc = sqlitedb.NewContext(r.ctx, r.db, r.in, r.out) }

// closeContext closes the current Context and reports its failure, if any.
func (r *runner) closeContext() error {
	mc := r.mc
	r.mc = nil
	mc.Close()
	if err := mc.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrContext, err)
	}
	return nil
}

func (r *runner) step(st Step) error {
	m := st.Mapping
	var need []string
	for _, c := range m.Columns {
		if c.Required {
			need = append(need, c.Name)
		}
	}
	t, err := r.db.Table(r.ctx, m.Table, need, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable) && st.Identifying:
		r.rep.NotApplicable = "table " + m.Table + " is absent"
		return errNotApplicable
	case errors.Is(err, sqlitedb.ErrNoSuchTable) && st.Optional:
		r.out.Note("table."+m.Table, "absent")
		r.rep.Tables = append(r.rep.Tables, TableReport{Table: m.Table, Absent: true})
		return nil
	case err != nil:
		return fmt.Errorf("rowrun: table %s: %w", m.Table, err)
	}
	r.noteSchema(t, m)
	tr := TableReport{Table: m.Table}
	err = t.Scan(r.ctx, func(row sqlitedb.Row) error { return r.row(&tr, m, row) })
	r.rep.Tables = append(r.rep.Tables, tr)
	if tr.Unmapped > 0 {
		r.out.Note("unmapped_rows."+m.Table, strconv.FormatInt(tr.Unmapped, 10))
	}
	return err
}

func (r *runner) row(tr *TableReport, m parse.TableMapping, row sqlitedb.Row) error {
	tr.Rows++
	r.since++
	recs, err := m.Map(r.mc, row)
	if cerr := r.mc.Err(); cerr != nil {
		return fmt.Errorf("%w: table %s: %w", ErrContext, m.Table, cerr)
	}
	if err != nil {
		var re *RowError
		switch {
		case errors.As(err, &re):
			if err := r.unmapped(tr, row, re.Reason); err != nil {
				return err
			}
		case errors.Is(err, sqlitedb.ErrKeyUndecidable):
			if err := r.unmapped(tr, row, "a join of the row is undecidable: "+err.Error()); err != nil {
				return err
			}
		default:
			return err
		}
		return r.maybeRotate()
	}
	for _, rec := range recs {
		if err := r.out.Emit(r.ctx, rec); err != nil {
			return err
		}
		tr.Records++
	}
	return r.maybeRotate()
}

func (r *runner) unmapped(tr *TableReport, row sqlitedb.Row, reason string) error {
	tr.Unmapped++
	if r.warned >= maxRunWarnings {
		return nil
	}
	r.warned++
	loc, ok := row.Locator()
	if !ok {
		loc = "sqlite:table=" + escapeComponent(row.Table())
		if id, ok := row.Rowid(); ok {
			loc += ";rowid=" + strconv.FormatInt(id, 10)
		}
	}
	return r.out.Warn(r.ctx, loc, reason)
}

// maybeRotate gives back the rows and indexes the Context holds once they charge
// half of the budget.
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
	r.rep.Rotations++
	return nil
}

func (r *runner) noteSchema(t *sqlitedb.Table, m parse.TableMapping) {
	declared := map[string]bool{}
	var optional []string
	for _, c := range m.Columns {
		declared[asciiFold(c.Name)] = true
		if !c.Required && t.Col(c.Name) >= 0 {
			optional = append(optional, c.Name)
		}
	}
	cols := t.Cols()
	var unmapped []string
	for _, c := range cols {
		if !declared[asciiFold(c.Name)] && !c.RowidAlias {
			unmapped = append(unmapped, c.Name)
		}
	}
	v := "columns:" + strconv.Itoa(len(cols))
	if len(optional) > 0 {
		v += ";optional:" + strings.Join(optional, ",")
	}
	r.out.Note("schema."+m.Table, v)
	if len(unmapped) > 0 {
		r.out.Note("unmapped_columns."+m.Table, strings.Join(unmapped, ","))
	}
}

func (r *runner) forwardWarnings() error {
	for _, w := range r.db.Warnings() {
		if r.warned >= maxRunWarnings {
			break
		}
		r.warned++
		loc := fmt.Sprintf("sqlite:file=%s;page=%d;offset=%d", w.File, w.Page, w.Offset)
		if err := r.out.Warn(r.ctx, loc, w.Code+": "+w.Msg); err != nil {
			return err
		}
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

const hexDigits = "0123456789ABCDEF"

// escapeComponent percent-encodes every byte outside [A-Za-z0-9._~-] (the
// locator grammar of decode/sqlitedb).
func escapeComponent(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '~', c == '-':
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&15])
		}
	}
	return sb.String()
}
