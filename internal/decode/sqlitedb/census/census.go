package census

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// MaxSQL is the number of bytes of an object's SQL that a Report keeps; longer is cut and flagged.
const MaxSQL = 64 << 10

// MaxSpecs is the most CountSpecs one Take accepts.
const MaxSpecs = 8

// ErrTooManySpecs is returned when more than MaxSpecs count specs are given.
var ErrTooManySpecs = errors.New("census: too many count specs")

// SchemaDamagedNote is the Unreadable text of a table that is not listed while the schema read
// lost entries: Present is false but only because the listing is incomplete, so presence is unknown.
const SchemaDamagedNote = "schema damaged, presence unknown"

// Object is one row of the schema table. Type is table, index, view or trigger.
type Object struct {
	Type, Name, TblName, SQL string
	SQLTruncated             bool
}

// CountSpec names a table to count. Column "" asks for no match count. Otherwise Matched counts
// the rows whose Column cell is TEXT and equal to Equals.
type CountSpec struct{ Table, Column, Equals string }

// Count is the census of one spec.
type Count struct {
	Table                string
	Present              bool  // a base table of that name exists
	Rows, Matched        int64 // Matched: rows whose Column cell is TEXT and equal to Equals
	LostPages, LostCells int64
	Unreadable           string // "" when the scan was whole; else why not: the data error that ended it, or the pages and cells it lost
}

// Report is the census of one database.
type Report struct {
	Objects       []Object
	Counts        []Count
	SchemaSkipped int // damage events met while reading the schema table
}

// adapter hands the caller's Budget to the library and gives back exactly what it granted.
type adapter struct {
	b           sqlitedb.Budget
	outstanding int64
}

func (a *adapter) Alloc(n int64) error {
	if err := a.b.Alloc(n); err != nil {
		return err
	}
	a.outstanding += n
	return nil
}

func (a *adapter) Free(n int64) {
	n = min(n, a.outstanding)
	if n <= 0 {
		return
	}
	a.outstanding -= n
	a.b.Free(n)
}

func (a *adapter) releaseAll() {
	if a.outstanding > 0 {
		a.b.Free(a.outstanding)
		a.outstanding = 0
	}
}

// passThrough reports the errors that end the census instead of becoming a state of a table:
// budget, cancellation and the library's resource and I/O errors.
func passThrough(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, parse.ErrBudget) || errors.Is(err, sqlitefile.ErrBudget) || errors.Is(err, sqlitefile.ErrLimit)
}

func wrapOpen(err error) error {
	switch {
	case err == nil:
		return nil
	case passThrough(err):
		return err
	case errors.Is(err, sqlitefile.ErrNotSQLite):
		if errors.Is(err, sqlitefile.ErrLooksEncrypted) {
			return fmt.Errorf("%w: %w: %w", sqlitedb.ErrEncrypted, sqlitedb.ErrNotSQLite, err)
		}
		return fmt.Errorf("%w: %w", sqlitedb.ErrNotSQLite, err)
	}
	return err
}

// Take reads the census of the database in f. A file that is not SQLite is sqlitedb.ErrNotSQLite
// (with sqlitedb.ErrEncrypted when it looks encrypted); budget, context and I/O errors pass through
// unchanged; a scan that meets a data error states it in Count.Unreadable and does not fail Take.
// Everything charged to b is freed before Take returns.
func Take(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, specs []CountSpec) (rep Report, err error) {
	if b == nil {
		return Report{}, sqlitedb.ErrNoBudget
	}
	if f.DB == nil || f.DBSize < 0 || (f.WAL == nil && f.WALSize != 0) || (f.Journal == nil && f.JournalSize != 0) {
		return Report{}, fmt.Errorf("%w: unusable file set", sqlitedb.ErrBadFiles)
	}
	if len(specs) > MaxSpecs {
		return Report{}, ErrTooManySpecs
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	ad := &adapter{b: b}
	var view *sqlitefile.View
	defer func() {
		if r := recover(); r != nil {
			rep, err = Report{}, fmt.Errorf("%w: %v", sqlitedb.ErrInternal, r)
		}
		if view != nil {
			view.Release()
		}
		ad.releaseAll()
	}()
	lib, err := sqlitefile.Open(f.DB, f.DBSize, sqlitefile.Options{Budget: ad})
	if err != nil {
		return Report{}, wrapOpen(err)
	}
	if f.WAL != nil {
		if _, err := lib.AttachWAL(f.WAL, f.WALSize); err != nil {
			return Report{}, wrapOpen(err)
		}
	}
	if f.Journal != nil {
		if _, err := lib.AttachJournal(f.Journal, f.JournalSize); err != nil {
			return Report{}, wrapOpen(err)
		}
	}
	view = lib.Live()
	schema, err := view.Schema(ctx)
	if err != nil {
		return Report{}, wrapOpen(err)
	}
	rep.SchemaSkipped = schema.Skipped
	rep.Objects = make([]Object, 0, len(schema.Objects))
	for _, o := range schema.Objects {
		sql, cut := o.SQL, false
		if len(sql) > MaxSQL {
			n := MaxSQL
			for n > 0 && !utf8.RuneStart(sql[n]) {
				n--
			}
			sql, cut = sql[:n], true
		}
		if err := ad.Alloc(int64(len(sql))); err != nil {
			return Report{}, err
		}
		rep.Objects = append(rep.Objects, Object{Type: o.Type, Name: o.Name, TblName: o.TblName, SQL: strings.Clone(sql), SQLTruncated: cut})
	}
	for _, sp := range specs {
		c, err := count(ctx, view, schema, sp)
		if err != nil {
			return Report{}, err
		}
		rep.Counts = append(rep.Counts, c)
	}
	return rep, nil
}

// count walks one table of the live view.
func count(ctx context.Context, view *sqlitefile.View, schema *sqlitefile.Schema, sp CountSpec) (Count, error) {
	c := Count{Table: sp.Table}
	for i := range schema.Objects {
		o := &schema.Objects[i]
		if o.Type == "table" && !o.Virtual && strings.EqualFold(o.Name, sp.Table) {
			c.Present = true
			break
		}
	}
	if !c.Present {
		if schema.Skipped > 0 {
			// The schema read lost entries: the table may be among them. Unknown is never absent.
			c.Unreadable = SchemaDamagedNote
		}
		return c, nil
	}
	t, err := view.Table(ctx, sp.Table)
	if err != nil {
		if passThrough(err) {
			return c, err
		}
		c.Unreadable = err.Error()
		return c, nil
	}
	col := -1
	if sp.Column != "" {
		def := t.Def()
		for i, cd := range def.Columns {
			if def.ParseOK && strings.EqualFold(cd.Name, sp.Column) {
				col = i
				break
			}
		}
		if col < 0 {
			c.Unreadable = "the match column " + sp.Column + " is not a declared column"
		}
	}
	var ctxErr error
	var unknownCells int64 // match cells that are stored but could not be read whole
	loss, err := t.RowsLoss(ctx, func(r sqlitefile.Row) bool {
		if ctxErr = parse.Tick(ctx, int(c.Rows)); ctxErr != nil {
			return false
		}
		c.Rows++
		if col >= 0 {
			if v := t.Resolve(r); col < len(v) {
				if x := v[col]; x.Unread || x.Clipped || x.Omitted {
					unknownCells++
				} else if s, ok := x.Text(); ok && s == sp.Equals {
					c.Matched++
				}
			}
		}
		return true
	})
	c.LostPages, c.LostCells = loss.Pages, loss.Cells
	switch {
	case ctxErr != nil:
		return c, ctxErr
	case err != nil && passThrough(err):
		return c, err
	case err != nil:
		c.Unreadable = err.Error()
	case loss.Pages > 0 || loss.Cells > 0:
		c.Unreadable = fmt.Sprintf("the scan lost %d pages and %d cells", loss.Pages, loss.Cells)
	}
	if unknownCells > 0 && c.Unreadable == "" {
		c.Unreadable = fmt.Sprintf("%d match cells could not be read whole, so Matched is a lower bound", unknownCells)
	}
	return c, nil
}
