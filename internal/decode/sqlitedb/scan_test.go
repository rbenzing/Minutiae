package sqlitedb_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// rowids scans tb and returns the rowids in delivery order.
func rowids(t testing.TB, tb *sqlitedb.Table) []int64 {
	t.Helper()
	var out []int64
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		id, _ := r.Rowid()
		out = append(out, id)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return out
}

func TestScanDeliversRowsInRowidOrderWithStop(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		for _, id := range []int64{5, 1, 3, -2} {
			tt.Insert(id, id)
		}
	})
	if got := rowids(t, tb); !slices.Equal(got, []int64{-2, 1, 3, 5}) {
		t.Fatalf("order = %v", got)
	}
	// ErrStop ends the scan with a nil result.
	var seen []int64
	err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		id, _ := r.Rowid()
		seen = append(seen, id)
		if len(seen) == 2 {
			return sqlitedb.ErrStop
		}
		return nil
	})
	if err != nil || !slices.Equal(seen, []int64{-2, 1}) {
		t.Errorf("stop: err %v seen %v", err, seen)
	}
	// Any other error ends it and is returned unchanged.
	boom := errors.New("boom")
	seen = nil
	err = tb.Scan(t.Context(), func(_ sqlitedb.Row) error {
		seen = append(seen, 1)
		return boom
	})
	if err != boom || len(seen) != 1 {
		t.Errorf("error: err %v seen %d", err, len(seen))
	}
}

// bigTable has more rows than one poll interval (the reader polls the context
// itself every few dozen rows, so the layer's own poll every parse.TickEvery rows
// is a bound this test cannot tell apart from it).
func bigTable(t testing.TB) *sqlitedb.Table {
	return tableOf(t, sqlitetest.Options{PageSize: 65536}, "create table t(a)", func(tt *sqlitetest.Table) {
		for i := int64(1); i <= 6000; i++ {
			tt.Insert(i, nil)
		}
	})
}

func TestScanCancelledMidwayIsAnError(t *testing.T) {
	tb := bigTable(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	n := 0
	err := tb.Scan(ctx, func(sqlitedb.Row) error {
		n++
		if n == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v after %d rows", err, n)
	}
	if n >= 6000 {
		t.Errorf("the scan did not stop: %d rows", n)
	}
}

func TestScanPollsContextEvery4096(t *testing.T) {
	tb := bigTable(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	n := 0
	err := tb.Scan(ctx, func(sqlitedb.Row) error {
		n++
		if n == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || n < 2 || n > parse.TickEvery {
		t.Errorf("err %v after %d rows: want a cancel within %d rows", err, n, parse.TickEvery)
	}
	// A context that is already done delivers nothing.
	n = 0
	err = tb.Scan(ctx, func(sqlitedb.Row) error { n++; return nil })
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Errorf("cancelled before the scan: err %v rows %d", err, n)
	}
}

func TestScanWithoutRowidTable(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		w := b.CreateTableWithoutRowid("w", "create table w(a primary key, b) without rowid", 1)
		w.Insert(1, "k2", "v2")
		w.Insert(2, "k1", "v1")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "w", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if _, ok := r.Rowid(); ok {
			t.Error("a WITHOUT ROWID row has a rowid")
		}
		a, _ := r.Text(0)
		b, _ := r.Text(1)
		got = append(got, string(a)+"="+string(b))
		if r.State(0) != sqlitedb.StatePresent || r.State(1) != sqlitedb.StatePresent {
			t.Errorf("states %v %v", r.State(0), r.State(1))
		}
	})
	if !slices.Equal(got, []string{"k1=v1", "k2=v2"}) {
		t.Errorf("rows = %v", got)
	}
}

func TestScanNeverContainsIndexEntries(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a)")
		b.CreateIndex("ia", "t", "create index ia on t(a)", 0)
		for i := int64(1); i <= 3; i++ {
			tt.Insert(i, fmt.Sprint("v", i))
		}
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowids(t, tb); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("rows = %v", got)
	}
	if _, err := d.Table(t.Context(), "ia", nil, nil); !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("an index resolved as a table: %v", err)
	}
}

func TestLiveParseSeesCommittedWALRows(t *testing.T) {
	db, wal := walDB(t)
	d := openBytes(t, db, wal(), nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		id, _ := r.Rowid()
		ids = append(ids, id)
		if _, role := r.Range(); role != parse.RoleWAL {
			t.Errorf("rowid %d: role %q, want %q", id, role, parse.RoleWAL)
		}
	})
	if !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Errorf("rows = %v", ids)
	}
}

func TestWithoutTheWALOldRowsShow(t *testing.T) {
	db, _ := walDB(t)
	d := openBytes(t, db, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if _, role := r.Range(); role != parse.RoleDB {
			t.Errorf("role %q", role)
		}
	})
	if got := rowids(t, tb); !slices.Equal(got, []int64{1}) {
		t.Errorf("rows = %v", got)
	}
}

func TestSuperJournalRowsAreTheDatabaseAsStored(t *testing.T) {
	db, journal := hotJournalDB(t, 1024, "x")
	d := openBytes(t, db, nil, journal, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The journal is not applied, so row 2 (which it would roll back) is there.
	if got := rowids(t, tb); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("rows = %v", got)
	}
}

func TestStatsCountsFlaggedRowsAndWarnings(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		tt.InsertRaw(1, []uint64{0}, []byte{0}) // length mismatch
		tt.Insert(2, "ok", int64(2))
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Stats(); s != (sqlitedb.Stats{}) {
		t.Fatalf("stats before the scan = %+v", s)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	s := d.Stats()
	if s.Scans != 1 || s.Rows != 2 || s.RowsFlagged < 1 || s.NewWarningsAtLeast < 1 {
		t.Errorf("stats = %+v", s)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	if s := d.Stats(); s.Scans != 2 || s.Rows != 4 {
		t.Errorf("stats after two scans = %+v", s)
	}
}

// chargeBudget is a Budget that refuses one request size and records them.
type chargeBudget struct {
	testBudget
	refuse int64
	allocs []int64
}

func (b *chargeBudget) Alloc(n int64) error {
	b.allocs = append(b.allocs, n)
	if n == b.refuse {
		return fmt.Errorf("%w: refused %d", parse.ErrBudget, n)
	}
	return b.testBudget.Alloc(n)
}

func TestUtf16TextBudgetRefusalEndsTheScan(t *testing.T) {
	long := strings.Repeat("x", 1000) // 2000 bytes in UTF-16: charge 2000*3/2+4
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a)")
		tt.Insert(1, "ab")
		tt.Insert(2, long)
		tt.Insert(3, "cd")
	})
	b := &chargeBudget{testBudget: testBudget{limit: 1 << 40}, refuse: 2000*3/2 + 4}
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []int64
	err = tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		id, _ := r.Rowid()
		seen = append(seen, id)
		return nil
	})
	if !errors.Is(err, parse.ErrBudget) || !slices.Equal(seen, []int64{1}) {
		t.Fatalf("err %v seen %v", err, seen)
	}
	if !slices.Contains(b.allocs, int64(sqlitedb.RowValueCost)) || !slices.Contains(b.allocs, int64(4*3/2+4)) {
		t.Errorf("charges %v lack the row charge %d or the decode charge of the first row", b.allocs, sqlitedb.RowValueCost)
	}
}

func TestScanFreesItsCharges(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= 20; i++ {
			tt.Insert(i, strings.Repeat("y", 100), i)
		}
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	first := b.used
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	if b.used != first {
		t.Errorf("used grew from %d to %d across scans", first, b.used)
	}
	// An early end frees as well.
	_ = tb.Scan(t.Context(), func(sqlitedb.Row) error { return sqlitedb.ErrStop })
	_ = tb.Scan(t.Context(), func(sqlitedb.Row) error { return errors.New("x") })
	if b.used != first {
		t.Errorf("used after early ends = %d, want %d", b.used, first)
	}
}

func TestScanAfterReleaseIsErrReleased(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	called := false
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error { called = true; return nil })
	if !errors.Is(err, sqlitedb.ErrReleased) || called || b.used != 0 {
		t.Errorf("Scan after Release: err %v called %v used %d", err, called, b.used)
	}
}
