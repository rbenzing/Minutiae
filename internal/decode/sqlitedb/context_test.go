package sqlitedb_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fakeNotes records every Note call.
type fakeNotes struct {
	calls int
	m     map[string]string
}

func (f *fakeNotes) Note(key, value string) {
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.calls++
	f.m[key] = value
}

// chatDB holds thread(id, title) and message(id, thread_id, body).
func chatDB(t testing.TB) (*sqlitedb.DB, *testBudget) {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		th := b.CreateTable("thread", "create table thread(id integer primary key, title text)")
		th.Insert(1, 1, "alpha")
		th.Insert(2, 2, "beta")
		m := b.CreateTable("message", "create table message(id integer primary key, thread_id integer, body text)")
		m.Insert(1, 1, 1, "hi")
		m.Insert(2, 2, 2, "yo")
		m.Insert(3, 3, 1, "again")
	})
	b := bigBudget()
	return openBytes(t, data, nil, nil, b), b
}

func newCtx(t testing.TB, d *sqlitedb.DB, n sqlitedb.Notes) *sqlitedb.Context {
	t.Helper()
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, n)
	t.Cleanup(c.Close)
	return c
}

var _ parse.MapContext = (*sqlitedb.Context)(nil)

func TestContextGetMatchAndColumn(t *testing.T) {
	d, _ := chatDB(t)
	in := &parse.Input{}
	c := sqlitedb.NewContext(t.Context(), d, in, &fakeNotes{})
	defer c.Close()
	if c.Input() != in {
		t.Error("Input")
	}
	if c.Column("MESSAGE", "Thread_Id") != 1 || c.Column("message", "nope") != -1 || c.Column("nope", "id") != -1 {
		t.Errorf("Column = %d %d %d", c.Column("MESSAGE", "Thread_Id"), c.Column("message", "nope"), c.Column("nope", "id"))
	}
	row, ok, err := c.Get("thread", 2)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if s, _ := row.Text(1); string(s) != "beta" {
		t.Errorf("title = %q", s)
	}
	if _, ok, err := c.Get("thread", 99); ok || err != nil {
		t.Errorf("miss: %v %v", ok, err)
	}
	rows, err := c.Match("message", "thread_id", sqlitedb.LiteralIntKey(1))
	if err != nil || len(rows) != 2 {
		t.Fatalf("Match = %d rows, %v", len(rows), err)
	}
	var ids []int64
	for _, r := range rows {
		id, _ := r.Rowid()
		ids = append(ids, id)
	}
	if !slices.Equal(ids, []int64{1, 3}) {
		t.Errorf("rowids = %v", ids)
	}
	if rows, err := c.Match("message", "thread_id", sqlitedb.LiteralIntKey(77)); err != nil || len(rows) != 0 {
		t.Errorf("miss: %v %v", rows, err)
	}
}

func TestMatchInsideScanKeepsOuterRowValid(t *testing.T) {
	d, _ := chatDB(t)
	c := newCtx(t, d, &fakeNotes{})
	tb, err := d.Table(t.Context(), "thread", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var titles, bodies []string
	err = tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		key, ok, kerr := sqlitedb.KeyOf(r, 0)
		if kerr != nil || !ok {
			t.Fatal("no key")
		}
		msgs, err := c.Match("message", "thread_id", key)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			s, _ := m.Text(2)
			bodies = append(bodies, string(s))
		}
		s, ok := r.Text(1) // the outer row is still valid after the Match
		if !ok {
			t.Error("outer row lost its text")
		}
		titles = append(titles, string(s))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(titles, []string{"alpha", "beta"}) || !slices.Equal(bodies, []string{"hi", "again", "yo"}) {
		t.Errorf("titles %v bodies %v", titles, bodies)
	}
}

func TestContextMissingTableIsAnErrorNotEmpty(t *testing.T) {
	d, _ := chatDB(t)
	c := newCtx(t, d, &fakeNotes{})
	if _, _, err := c.Get("nope", 1); !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("Get: %v", err)
	}
	if rows, err := c.Match("nope", "id", sqlitedb.LiteralIntKey(1)); !errors.Is(err, sqlitedb.ErrNoSuchTable) || rows != nil {
		t.Errorf("Match: %v %v", rows, err)
	}
	if c.Column("nope", "id") != -1 {
		t.Error("Column of a missing table")
	}
	if _, err := c.Match("message", "nope", sqlitedb.LiteralIntKey(1)); !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Errorf("Match on a missing column: %v", err)
	}
}

// joinFixture holds src(a text nocase) with 50 rows and tgt(n integer).
func joinFixture(t testing.TB) (*sqlitedb.DB, *testBudget) {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		s := b.CreateTable("src", "create table src(id integer primary key, a text collate nocase, n integer)")
		g := b.CreateTable("tgt", "create table tgt(id integer primary key, n integer, a text collate nocase)")
		for i := int64(1); i <= 50; i++ {
			s.Insert(i, i, fmt.Sprintf("%d", i), i)
			g.Insert(i, i, i, fmt.Sprintf("%d", i))
		}
	})
	b := bigBudget()
	return openBytes(t, data, nil, nil, b), b
}

func TestJoinMismatchIsFlaggedWithCount(t *testing.T) {
	d, _ := joinFixture(t)
	notes := &fakeNotes{}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, notes)
	src, err := d.Table(t.Context(), "src", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Scan(t.Context(), func(r sqlitedb.Row) error {
		k, ok, kerr := sqlitedb.KeyOf(r, 1) // text, NOCASE
		if kerr != nil || !ok {
			t.Fatal("no key")
		}
		rows, err := c.Match("tgt", "n", k) // integer, BINARY
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("text matched integer: %d rows", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := sqlitedb.JoinReport{Table: "tgt", Column: "n", Lookups: 50, Flagged: 50, CollationDiffers: 50, AffinityDiffers: 50, Unkeyed: 0}
	if j := c.Joins(); len(j) != 1 || j[0] != want {
		t.Fatalf("Joins = %+v, want [%+v]", j, want)
	}
	if notes.calls != 0 {
		t.Errorf("%d notes before Close", notes.calls)
	}
	c.Close()
	c.Close() // idempotent
	if notes.calls != 1 || notes.m["join.tgt.n"] != "lookups:50,flagged:50,collation_differs:50,affinity_differs:50,unkeyed:0,undecidable:0" {
		t.Errorf("notes = %d %v", notes.calls, notes.m)
	}
	if d.Stats().JoinsFlushed != 1 {
		t.Errorf("JoinsFlushed = %d", d.Stats().JoinsFlushed)
	}

	// A clean join (same collation and class) leaves no note.
	clean := &fakeNotes{}
	c2 := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, clean)
	if err := src.Scan(t.Context(), func(r sqlitedb.Row) error {
		k, _, _ := sqlitedb.KeyOf(r, 1)
		rows, err := c2.Match("tgt", "a", k)
		if err != nil || len(rows) != 1 {
			t.Errorf("clean Match = %d rows, %v", len(rows), err)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if j := c2.Joins(); len(j) != 1 || j[0].Flagged != 0 || j[0].Lookups != 50 || j[0].Unkeyed != 0 {
		t.Errorf("clean Joins = %+v", j)
	}
	c2.Close()
	if clean.calls != 0 {
		t.Errorf("a clean join left %d notes: %v", clean.calls, clean.m)
	}
}

// An unflagged join that skipped target rows still tells the examiner.
func TestContextNotesUnkeyedTargetRows(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("tgt", "create table tgt(id integer primary key, n integer)")
		g.Insert(1, 1, 5)
		g.Insert(2, 2, nil)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	notes := &fakeNotes{}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, notes)
	if _, err := c.Match("tgt", "n", sqlitedb.LiteralIntKey(5)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if notes.calls != 1 || notes.m["join.tgt.n"] != "lookups:1,flagged:0,collation_differs:0,affinity_differs:0,unkeyed:1,undecidable:0" {
		t.Errorf("notes = %d %v", notes.calls, notes.m)
	}
}

func TestSkippedCloseIsVisible(t *testing.T) {
	d, _ := joinFixture(t)
	notes := &fakeNotes{}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, notes)
	k := parse.JoinKey{Kind: parse.JoinText, S: "7", Collation: "NOCASE", Class: parse.ClassText}
	if _, err := c.Match("tgt", "n", k); err != nil {
		t.Fatal(err)
	}
	if j := c.Joins(); len(j) != 1 || j[0].Flagged != 1 {
		t.Fatalf("Joins = %+v", j)
	}
	if d.Stats().JoinsFlushed != 0 || notes.calls != 0 {
		t.Errorf("JoinsFlushed %d notes %d without Close", d.Stats().JoinsFlushed, notes.calls)
	}
	c.Close()
}

func TestContextCloseFreesCharges(t *testing.T) {
	d, b := joinFixture(t)
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, &fakeNotes{})
	if _, ok, err := c.Get("src", 1); err != nil || !ok { // warm the reader's pages
		t.Fatal(ok, err)
	}
	if _, err := c.Match("tgt", "n", sqlitedb.LiteralIntKey(1)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	warm := b.used
	c2 := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, &fakeNotes{})
	if _, err := c2.Match("tgt", "n", sqlitedb.LiteralIntKey(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.Get("src", 2); err != nil {
		t.Fatal(err)
	}
	if b.used <= warm {
		t.Fatalf("a context holding an index and rows charged nothing (%d vs %d)", b.used, warm)
	}
	c2.Close()
	if b.used != warm {
		t.Errorf("used %d after Close, want %d", b.used, warm)
	}
	if _, _, err := c2.Get("src", 1); !errors.Is(err, sqlitedb.ErrReleased) {
		t.Errorf("Get after Close: %v", err)
	}
}

func TestMatchOfALiteralKeyIsNeverFlagged(t *testing.T) {
	d, _ := joinFixture(t)
	notes := &fakeNotes{}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, notes)
	for _, col := range []string{"n", "a"} {
		for _, k := range []parse.JoinKey{
			sqlitedb.LiteralIntKey(3), sqlitedb.LiteralTextKey([]byte("3")), sqlitedb.LiteralBlobKey([]byte("3")),
		} {
			if _, err := c.Match("tgt", col, k); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, j := range c.Joins() {
		if j.Flagged != 0 || j.CollationDiffers != 0 || j.AffinityDiffers != 0 || j.Lookups != 3 {
			t.Errorf("join %+v: a literal key was flagged", j)
		}
	}
	if j := c.Joins(); len(j) != 2 || j[0].Column != "a" || j[1].Column != "n" {
		t.Errorf("Joins not sorted by (table, column): %+v", j)
	}
	c.Close()
}
