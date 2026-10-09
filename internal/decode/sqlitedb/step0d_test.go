package sqlitedb_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func hundredRowDB(t testing.TB) []byte {
	return newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, n integer)")
		for i := int64(1); i <= 100; i++ {
			g.Insert(i, i, i)
		}
	})
}

// B51: ErrIndexLimit is a property of the data, so it is remembered.
func TestContextCachesIndexLimitError(t *testing.T) {
	d := openBytes(t, hundredRowDB(t), nil, nil, bigBudget())
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	c.SetIndexLimit(10)
	_, err1 := c.Match("g", "n", sqlitedb.LiteralIntKey(1))
	if !errors.Is(err1, sqlitedb.ErrIndexLimit) {
		t.Fatalf("first Match = %v, want ErrIndexLimit", err1)
	}
	scans := d.Stats().Scans
	for range 5 {
		if _, err := c.Match("g", "n", sqlitedb.LiteralIntKey(2)); !errors.Is(err, sqlitedb.ErrIndexLimit) {
			t.Fatalf("repeat Match = %v, want ErrIndexLimit", err)
		}
	}
	if got := d.Stats().Scans; got != scans {
		t.Errorf("Scans %d -> %d: the failed build was retried", scans, got)
	}
}

// B51: a corrupt-structure build error is remembered too.
func TestContextCachesCorruptBuildError(t *testing.T) {
	d := openBytes(t, hundredRowDB(t), nil, nil, bigBudget())
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	calls := 0
	c.SetBuildIndex(func(context.Context, *sqlitedb.Table, string) (*sqlitedb.Index, error) {
		calls++
		return nil, fmt.Errorf("%w: page 7 is not a b-tree page", sqlitedb.ErrCorrupt)
	})
	for range 4 {
		if _, err := c.Match("g", "n", sqlitedb.LiteralIntKey(1)); !errors.Is(err, sqlitedb.ErrCorrupt) {
			t.Fatalf("Match = %v, want ErrCorrupt", err)
		}
	}
	if calls != 1 {
		t.Errorf("the build ran %d times, want 1", calls)
	}
}

// B51: a cancellation or a deadline says nothing about the data and is retried.
func TestContextDoesNotCacheCancellationOrDeadline(t *testing.T) {
	for _, first := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(first.Error(), func(t *testing.T) {
			d := openBytes(t, hundredRowDB(t), nil, nil, bigBudget())
			c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
			t.Cleanup(c.Close)
			calls := 0
			c.SetBuildIndex(func(ctx context.Context, tb *sqlitedb.Table, col string) (*sqlitedb.Index, error) {
				calls++
				if calls == 1 {
					return nil, fmt.Errorf("scan: %w", first)
				}
				return tb.Index(ctx, col, 0)
			})
			if _, err := c.Match("g", "n", sqlitedb.LiteralIntKey(1)); !errors.Is(err, first) {
				t.Fatalf("first Match = %v, want %v", err, first)
			}
			rows, err := c.Match("g", "n", sqlitedb.LiteralIntKey(5))
			if err != nil || len(rows) != 1 {
				t.Fatalf("second Match = %d rows, %v; want the build retried", len(rows), err)
			}
			if calls != 2 {
				t.Errorf("builds = %d, want 2", calls)
			}
		})
	}
}

// B52: the rows a Match returns are charged, so a hostile fan-out ends in
// ErrBudget and never in a truncated result.
func TestContextMatchFanOutEndsInBudgetError(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k integer, pad text)")
		for i := int64(1); i <= 400; i++ {
			g.Insert(i, i, 7, "padding-padding-padding")
		}
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	if _, err := c.Match("g", "id", sqlitedb.LiteralIntKey(1)); err != nil { // warm: builds the id index
		t.Fatal(err)
	}
	if _, err := c.Match("g", "k", sqlitedb.LiteralIntKey(99)); err != nil { // builds the k index (miss)
		t.Fatal(err)
	}
	b.limit = b.used + 4000 // room for a handful of rows, not for 400
	rows, err := c.Match("g", "k", sqlitedb.LiteralIntKey(7))
	if !errors.Is(err, parse.ErrBudget) || rows != nil {
		t.Fatalf("Match of 400 rows = %d rows, %v; want nil and ErrBudget", len(rows), err)
	}
	if _, err := c.Match("g", "k", sqlitedb.LiteralIntKey(7)); !errors.Is(err, parse.ErrBudget) {
		t.Errorf("repeat = %v, want ErrBudget again", err)
	}
}

// B53: undecidable lookups are counted in the report and in the Close note.
func TestUndecidableLookupsAreCounted(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k text)")
		g.InsertRaw(1, []uint64{0, 19}, []byte{0x41, 0x00, 0x42}) // odd-length UTF-16: undecidable
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	notes := &fakeNotes{}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, notes)
	for _, s := range []string{"x", "y", "z"} {
		if _, err := c.Match("g", "k", sqlitedb.LiteralTextKey([]byte(s))); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
			t.Fatalf("Match = %v, want ErrKeyUndecidable", err)
		}
	}
	want := sqlitedb.JoinReport{Table: "g", Column: "k", Lookups: 3, Undecidable: 3, Unkeyed: 1}
	if j := c.Joins(); len(j) != 1 || j[0] != want {
		t.Fatalf("Joins = %+v, want [%+v]", j, want)
	}
	c.Close()
	if got := notes.m["join.g.k"]; got != "lookups:3,flagged:0,collation_differs:0,affinity_differs:0,unkeyed:1,undecidable:3" {
		t.Errorf("note = %q", got)
	}
}

// flakyReader fails every read with errFlaky while armed.
var errFlaky = errors.New("flaky: transient read failure")

type flakyReader struct {
	r        *bytes.Reader
	armed    bool
	failFrom int64 // while armed, only reads at or past this offset fail (0: all)
}

func (f *flakyReader) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off+int64(len(p)) > f.failFrom {
		return 0, errFlaky
	}
	return f.r.ReadAt(p, off)
}

// B59: a plain I/O error says nothing about the data (it may be transient), so
// the failed build is NOT remembered and the next Match retries and succeeds.
func TestContextDoesNotCacheIOError(t *testing.T) {
	data := hundredRowDB(t)
	fr := &flakyReader{r: bytes.NewReader(data)}
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: fr, DBSize: int64(len(data))}, bigBudget())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	calls := 0
	c.SetBuildIndex(func(ctx context.Context, tb *sqlitedb.Table, col string) (*sqlitedb.Index, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("scan: %w", errFlaky) // the reader failed during the build
		}
		return tb.Index(ctx, col, 0)
	})
	fr.armed = true
	if _, err := c.Match("g", "n", sqlitedb.LiteralIntKey(1)); !errors.Is(err, errFlaky) {
		t.Fatalf("first Match = %v, want the I/O error", err)
	}
	fr.armed = false
	rows, err := c.Match("g", "n", sqlitedb.LiteralIntKey(5))
	if err != nil || len(rows) != 1 {
		t.Fatalf("second Match = %d rows, %v; want the build retried and answered", len(rows), err)
	}
	if calls != 2 {
		t.Errorf("builds = %d, want 2", calls)
	}
}

// B67: the I/O failure comes through the real read path (a failing
// io.ReaderAt under the default index build). It is reported as the I/O error
// it is, never as ErrCorrupt (that would misstate the evidence), and it is not
// remembered: the next Match, with the reader recovered, answers.
func TestContextIOErrorThroughReaderIsNotCorruptAndNotCached(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, n integer, pad text)")
		for i := int64(1); i <= 400; i++ {
			g.Insert(i, i, i, "padding-padding-padding-padding")
		}
	})
	fr := &flakyReader{r: bytes.NewReader(data)}
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: fr, DBSize: int64(len(data))}, bigBudget())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	// Resolve the table (its root page) while the reader works, then fail the
	// reads of every other page: the leaves the index build has not read yet.
	tb, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fr.failFrom = int64(tb.RootPage()) * 1024
	fr.armed = true
	_, err = c.Match("g", "n", sqlitedb.LiteralIntKey(1))
	if !errors.Is(err, errFlaky) {
		t.Fatalf("Match with a failing reader = %v, want the reader's error", err)
	}
	if errors.Is(err, sqlitedb.ErrCorrupt) {
		t.Errorf("an I/O failure was reported as ErrCorrupt: %v", err)
	}
	fr.armed = false
	rows, err := c.Match("g", "n", sqlitedb.LiteralIntKey(5))
	if err != nil || len(rows) != 1 {
		t.Fatalf("Match after the reader recovered = %d rows, %v; the error was cached", len(rows), err)
	}
}
