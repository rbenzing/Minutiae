package sqlitedb

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Files are the evidence files of one database: the database itself and,
// when the host holds them, its write-ahead log and rollback journal. A
// companion is attached when its reader is non-nil, whatever its size; the
// library decides what an empty one means. The host owns bundle completeness:
// a companion that is not supplied is not looked for and not warned about.
type Files struct {
	DB          io.ReaderAt
	DBSize      int64
	WAL         io.ReaderAt
	WALSize     int64
	Journal     io.ReaderAt
	JournalSize int64
}

// Warning is a library warning, passed through unchanged.
type Warning = sqlitefile.Warning

// Info is what the decoder knows about the files it opened.
type Info struct {
	Header  sqlitefile.Info         // the LIVE view's header information (after any rollback)
	Stored  sqlitefile.Info         // the database file's header as stored
	WAL     *sqlitefile.WALInfo     // nil when no WAL was supplied
	Journal *sqlitefile.JournalInfo // nil when no journal was supplied
}

// Stats counts the layer's work. NewWarningsAtLeast is a LOWER BOUND of damage
// events: the reader de-duplicates and caps its warnings, so never present it
// as an exact count.
type Stats struct {
	Scans, Rows, RowsFlagged, NewWarningsAtLeast int64
	IndexUnkeyed                                 int64 // target rows the join indexes did not key (NULL, NaN, unknown state)
	JoinsFlushed                                 int64 // join notes the contexts OFFERED to their sink at Close (the sink may drop a note: key collision, note cap)
}

// DB is an opened database. It is NOT safe for concurrent use: it belongs to
// one goroutine, and the layer has no lock.
type DB struct {
	lib      *sqlitefile.DB
	view     *sqlitefile.View
	schema   *sqlitefile.Schema
	budget   *budgetAdapter
	released bool
	baseWarn int // warnings the live view had when Open returned
	stats    Stats
}

// Open attaches the supplied companions, takes the live view and reads the
// schema once. A hot journal that names a super-journal is not applied (the
// warning says so): the layer never tells the library that the file exists.
func Open(ctx context.Context, f Files, b Budget) (*DB, error) { return open(ctx, f, b, nil) }

// open is Open with a test seam: after is called once the library calls have
// returned, so a test can make the layer's own code panic.
func open(ctx context.Context, f Files, b Budget, after func()) (db *DB, err error) {
	if b == nil {
		return nil, ErrNoBudget
	}
	if err := checkFiles(f); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ad := &budgetAdapter{b: b}
	var view *sqlitefile.View
	defer func() {
		if r := recover(); r != nil {
			db, err = nil, fmt.Errorf("%w: %v", ErrInternal, r)
		}
		if db == nil {
			if view != nil {
				view.Release()
			}
			ad.releaseAll()
		}
	}()
	lib, err := sqlitefile.Open(f.DB, f.DBSize, sqlitefile.Options{Budget: ad})
	if err != nil {
		return nil, wrapErr(err)
	}
	if f.WAL != nil {
		if _, err := lib.AttachWAL(f.WAL, f.WALSize); err != nil {
			return nil, wrapErr(err)
		}
	}
	if f.Journal != nil {
		if _, err := lib.AttachJournal(f.Journal, f.JournalSize); err != nil {
			return nil, wrapErr(err)
		}
	}
	view = lib.Live()
	schema, err := view.Schema(ctx)
	if err != nil {
		return nil, wrapErr(err)
	}
	if after != nil {
		after()
	}
	db = &DB{lib: lib, view: view, schema: schema, budget: ad, baseWarn: len(view.Warnings())}
	return db, nil
}

func checkFiles(f Files) error {
	switch {
	case f.DB == nil:
		return fmt.Errorf("%w: no database reader", ErrBadFiles)
	case f.DBSize < 0:
		return fmt.Errorf("%w: negative database size", ErrBadFiles)
	case f.WAL == nil && f.WALSize != 0:
		return fmt.Errorf("%w: WAL size without a WAL reader", ErrBadFiles)
	case f.WAL != nil && f.WALSize < 0:
		return fmt.Errorf("%w: negative WAL size", ErrBadFiles)
	case f.Journal == nil && f.JournalSize != 0:
		return fmt.Errorf("%w: journal size without a journal reader", ErrBadFiles)
	case f.Journal != nil && f.JournalSize < 0:
		return fmt.Errorf("%w: negative journal size", ErrBadFiles)
	}
	return nil
}

// wrapErr maps a library error to the layer sentinels. The library error
// stays in the chain. Budget, limit, page-unavailable, context and I/O errors
// pass through unchanged.
func wrapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sqlitefile.ErrBudget), errors.Is(err, parse.ErrBudget),
		errors.Is(err, sqlitefile.ErrLimit), errors.Is(err, sqlitefile.ErrPageUnavailable),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, sqlitefile.ErrNotSQLite):
		if errors.Is(err, sqlitefile.ErrLooksEncrypted) {
			return fmt.Errorf("%w: %w: %w", ErrEncrypted, ErrNotSQLite, err)
		}
		return fmt.Errorf("%w: %w", ErrNotSQLite, err)
	case errors.Is(err, sqlitefile.ErrCorrupt):
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	case errors.Is(err, sqlitefile.ErrLiveUnavailable):
		return fmt.Errorf("%w: %w", ErrLiveUnavailable, err)
	case errors.Is(err, sqlitefile.ErrEngineRefuses):
		return fmt.Errorf("%w: %w", ErrEngineRefuses, err)
	case errors.Is(err, sqlitefile.ErrWithoutRowid):
		return fmt.Errorf("%w: %w", ErrWithoutRowid, err)
	case errors.Is(err, sqlitefile.ErrInternal):
		return fmt.Errorf("%w: %w", ErrInternal, err)
	}
	return err
}

// Info returns the header information of the live view and of the stored
// file, and the state of each supplied companion.
func (db *DB) Info() Info {
	st := db.lib.Status()
	return Info{Header: db.view.Info(), Stored: st.Info, WAL: st.WAL, Journal: st.Journal}
}

// Warnings returns the warnings of the live view so far, in the order first
// seen.
func (db *DB) Warnings() []Warning { return db.view.Warnings() }

// Stats returns the layer counters. NewWarningsAtLeast counts the live
// view warnings added since Open returned; the reader caps and
// de-duplicates them, so it is a lower bound.
func (db *DB) Stats() Stats {
	s := db.stats
	s.NewWarningsAtLeast = int64(max(0, len(db.view.Warnings())-db.baseWarn))
	return s
}

// Tables lists the base tables of the schema (virtual tables excluded) in the
// order the reader gives them.
func (db *DB) Tables(ctx context.Context) (names []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			names, err = nil, fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names = []string{}
	for _, o := range db.schema.Objects {
		if o.Type == "table" && !o.Virtual {
			names = append(names, o.Name)
		}
	}
	return names, nil
}

// Release frees the view and the memory charged to the budget. It only returns
// memory: Tables, Info, Warnings and Stats keep working afterwards from what
// Open read (the schema is kept). Data access must not be used after Release. It is
// idempotent.
func (db *DB) Release() {
	if db == nil || db.released {
		return
	}
	db.released = true
	defer func() { _ = recover() }() // a panic in Release must not escape; the charges are still freed below
	defer db.budget.releaseAll()
	db.view.Release()
}
