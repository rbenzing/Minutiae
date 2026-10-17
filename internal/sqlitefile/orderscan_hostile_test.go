package sqlitefile_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// With Status counters: scanning n entries parses at most n + interiors cells and makes
// exactly n - 1 comparisons.
func TestOrderCheckCostIsLinear(t *testing.T) {
	const n = 300
	b, tb := woTable(n)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	if _, err := v.Table(context.Background(), "w"); err != nil { // reads the schema first
		t.Fatal(err)
	}
	before, compares := v.Stats(), v.OrderCompares()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != n {
		t.Fatalf("%d rows", len(got.rows))
	}
	parsed := v.Stats().CellsParsed - before.CellsParsed
	if limit := int64(n + len(tb.Interiors())); parsed > limit {
		t.Errorf("%d cells parsed for %d entries and %d interior pages, want at most %d", parsed, n, len(tb.Interiors()), limit)
	}
	if c := v.OrderCompares() - compares; c < n-1 || c > 3*n {
		t.Errorf("%d comparisons for %d entries, want %d to %d (the previous entry, and at most two bounds)", c, n, n-1, 3*n)
	}
}

// bigKeys: a WITHOUT ROWID table with the given text keys (inserted in the order given; the
// builder sorts them).
func bigKeyTable(keys ...string) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := b.CreateTableWithoutRowid("w", "create table w(k text primary key, v) without rowid", 1)
	for i, k := range keys {
		tb.Insert(int64(i+1), k, "v")
	}
	return b
}

func TestOrderCheckHostile(t *testing.T) {
	t.Run("4 MiB keys are compared within the budget and the charge returns to zero", func(t *testing.T) {
		const n = 4<<20 - 64
		prefix := strings.Repeat("a", n)
		b := bigKeyTable(prefix+"b", prefix+"c", prefix+"d")
		rb := newRecBudget(256 << 20)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		got := scanTableRows(t, v, "w")
		if len(got.rows) != 3 || len(flaggedIdx(got.rows)) != 0 {
			t.Fatalf("%d rows, flagged %v", len(got.rows), flaggedIdx(got.rows))
		}
		if _, found := uncheckedCount(t, v); found {
			t.Error("4 MiB keys are decidable and were counted unchecked")
		}
		if rb.peak > 128<<20 {
			t.Errorf("peak charge %d MiB for three 4 MiB keys", rb.peak>>20)
		}
		v.Release()
		rb.check(t)
	})
	t.Run("equal 4 MiB keys are one violation", func(t *testing.T) {
		// two rows (k, 1) and (k, 2) read under a key of the first column alone
		k := strings.Repeat("q", 4<<20-64)
		b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
		tb := b.CreateTableWithoutRowid("w", "create table w(k text, b int, primary key(k, b)) without rowid", 2)
		tb.Insert(1, k, int64(1))
		tb.Insert(2, k, int64(2))
		rb := newRecBudget(256 << 20)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		var flagged []int
		total := 0
		if _, err := v.ScanTreeOrdered(context.Background(), tb.Root(), sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{}}, Unique: true}, func(r sqlitefile.Row) bool {
			if r.KeyRangeViolation {
				flagged = append(flagged, total)
			}
			total++
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if total != 2 || len(flagged) != 1 || flagged[0] != 1 {
			t.Fatalf("%d rows, flagged %v, want the second flagged", total, flagged)
		}
		v.Release()
		rb.check(t)
	})
	t.Run("a cancelled context is the context error and nothing stays charged", func(t *testing.T) {
		b, _ := woTable(60)
		rb := newRecBudget(64 << 20)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		tbl, err := v.Table(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seen := 0
		err = tbl.Rows(ctx, func(sqlitefile.Row) bool {
			if seen++; seen == 5 {
				cancel()
			}
			return true
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		v.Release()
		rb.check(t)
	})
	t.Run("a panic in the check is recovered into an error", func(t *testing.T) {
		b, _ := woTable(60)
		data := b.Bytes()
		var armed atomic.Bool
		rb := newRecBudget(64 << 20)
		db, err := sqlitefile.OpenWithHook(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Budget: rb}, func(site string) {
			if site == "scan.order" && armed.CompareAndSwap(true, false) {
				panic("injected")
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		base := rb.level()
		v := db.Live()
		tbl, err := v.Table(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		armed.Store(true)
		wantPanicError(t, "Rows", tbl.Rows(context.Background(), func(sqlitefile.Row) bool { return true }))
		n := 0
		if err := tbl.Rows(context.Background(), func(sqlitefile.Row) bool { n++; return true }); err != nil || n != 60 {
			t.Errorf("Rows again: %d rows, %v", n, err)
		}
		v.Release()
		if got := rb.level(); got != base {
			t.Errorf("budget level %d after the panic, a second scan and the release, want %d", got, base)
		}
	})
	t.Run("100000 violations make a bounded number of warnings", func(t *testing.T) {
		const n = 100000
		// written by the engine: the builder inserts 100000 rows in over ten seconds
		path := filepath.Join(t.TempDir(), "big.db")
		edb, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		edb.SetMaxOpenConns(1)
		for _, q := range []string{
			"create table w(k integer primary key, v) without rowid",
			"with recursive c(i) as (select 1 union all select i+1 from c where i<100000) insert into w select i, 'v' from c",
		} {
			if _, err := edb.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if err := edb.Close(); err != nil {
			t.Fatal(err)
		}
		rb := newRecBudget(256 << 20)
		_, v := openLive(t, readFileRetry(t, path), sqlitefile.Options{Budget: rb})
		tbl, err := v.Table(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		flagged, total := 0, 0
		// read ascending as descending: every entry after the first is out of order
		_, err = v.ScanTreeOrdered(context.Background(), tbl.RootPage(), sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{Desc: true}}}, func(r sqlitefile.Row) bool {
			total++
			if r.KeyRangeViolation {
				flagged++
			}
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != n || flagged < n-1 || flagged > n {
			t.Errorf("%d entries, %d flagged, want %d and %d or %d (the first can also lie outside its separator)", total, flagged, n, n-1, n)
		}
		lim := sqlitefile.ResolveLimits(sqlitefile.Limits{}).MaxWarnings
		if w := len(v.Warnings()); w > lim+1 {
			t.Errorf("%d warnings, the collector's cap is %d", w, lim)
		}
		v.Release()
		rb.check(t)
	})
}
