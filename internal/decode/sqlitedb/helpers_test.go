package sqlitedb_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// testBudget is a Budget with a limit that records its peak. It refuses with
// parse.ErrBudget and never goes negative.
type testBudget struct {
	limit, used, peak int64
}

func (b *testBudget) Alloc(n int64) error {
	if n < 0 || b.used+n > b.limit {
		return fmt.Errorf("%w: %d bytes requested, %d of %d in use", parse.ErrBudget, n, b.used, b.limit)
	}
	b.used += n
	b.peak = max(b.peak, b.used)
	return nil
}

func (b *testBudget) Free(n int64) {
	b.used = max(0, b.used-n)
}

// newBuilderDB builds a database with the test builder: define fills it.
func newBuilderDB(t testing.TB, opts sqlitetest.Options, define func(b *sqlitetest.Builder)) []byte {
	t.Helper()
	b := sqlitetest.New(opts)
	define(b)
	return b.Bytes()
}

// bigBudget is a budget no test of the helpers can exhaust.
func bigBudget() *testBudget { return &testBudget{limit: 1 << 40} }

// openBytes opens a database held in memory; a nil companion is not supplied
// (an empty, non-nil one is supplied with size 0). The database is released
// when the test ends.
func openBytes(t testing.TB, db, wal, journal []byte, b sqlitedb.Budget) *sqlitedb.DB {
	t.Helper()
	d, err := sqlitedb.Open(t.Context(), filesOf(db, wal, journal), b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(d.Release)
	return d
}

func filesOf(db, wal, journal []byte) sqlitedb.Files {
	f := sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}
	if wal != nil {
		f.WAL, f.WALSize = bytes.NewReader(wal), int64(len(wal))
	}
	if journal != nil {
		f.Journal, f.JournalSize = bytes.NewReader(journal), int64(len(journal))
	}
	return f
}
