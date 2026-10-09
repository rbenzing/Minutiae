package sqlitedb_test

// Final-review fix wave (B76-B79): a lossy scan never answers a clean miss, the
// Context keeps the first hard failure, a possibly short answer is flagged, and
// a data-caused internal error is not rescanned.

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

// emptiedLeafDB is the reviewer's reproduction: 300 rows of t(a) (a = rowid),
// 512-byte pages, the second leaf given a cell count of 0. lost holds the
// rowids of that leaf, which no scan can deliver.
func emptiedLeafDB(t testing.TB) (data []byte, lost map[int64]bool) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a integer)")
	for i := int64(1); i <= 300; i++ {
		tb.Insert(i, i)
	}
	data = b.Bytes()
	leaves := tb.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := tb.CellBytes(i); pg == leaves[1] {
			lost[i] = true
		}
	}
	page := data[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0
	if len(lost) == 0 {
		t.Fatal("the damaged leaf holds no rows")
	}
	return data, lost
}

// B76: an index built from a scan that lost rows answers every miss with
// ErrKeyUndecidable naming the loss, never with a clean empty. The table is
// scanned first, so the damage is already warned about (a warnings delta would
// see nothing new).
func TestMatchOnATableThatLostRowsIsNeverACleanEmpty(t *testing.T) {
	data, lost := emptiedLeafDB(t)
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings()) == 0 {
		t.Fatal("the first scan raised no warning")
	}
	c := newCtx(t, d, nil)
	undecided, hits := 0, 0
	for id := int64(1); id <= 300; id++ {
		rows, err := c.Match("t", "a", sqlitedb.LiteralIntKey(id))
		switch {
		case err == nil && len(rows) == 0:
			t.Fatalf("Match(%d) answered a clean empty (lost row: %v)", id, lost[id])
		case lost[id]:
			if !errors.Is(err, sqlitedb.ErrKeyUndecidable) || !strings.Contains(err.Error(), "lost") {
				t.Fatalf("Match(%d) of a lost row = %v, want ErrKeyUndecidable naming the loss", id, err)
			}
			undecided++
		default:
			if err != nil || len(rows) != 1 {
				t.Fatalf("Match(%d) = %d rows, %v", id, len(rows), err)
			}
			hits++
		}
	}
	if undecided != len(lost) || hits != 300-len(lost) {
		t.Errorf("undecided %d of %d lost, hits %d", undecided, len(lost), hits)
	}
	j := c.Joins()
	if len(j) != 1 || j[0].Undecidable != int64(len(lost)) || j[0].UndecidedHits != int64(hits) {
		t.Errorf("Joins = %+v, want %d undecidable and %d undecided hits", j, len(lost), hits)
	}
}

func TestIndexOfALossyScanIsLossyAndItsHitsAreFlagged(t *testing.T) {
	data, lost := emptiedLeafDB(t)
	tb := tableFrom(t, data, "t")
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Lossy() {
		t.Fatal("an index over an emptied leaf is not Lossy")
	}
	var survivor, gone int64
	for id := int64(1); id <= 300; id++ {
		if lost[id] {
			gone = id
		} else {
			survivor = id
		}
	}
	ids, flags, err := ix.Rowids(sqlitedb.LiteralIntKey(survivor))
	if err != nil || !slices.Equal(ids, []int64{survivor}) || flags&sqlitedb.JoinUndecided == 0 {
		t.Errorf("hit = %v, %v, %v; want the row with JoinUndecided", ids, flags, err)
	}
	if ids, _, err := ix.Rowids(sqlitedb.LiteralIntKey(gone)); ids != nil || !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("miss = %v, %v; want ErrKeyUndecidable", ids, err)
	}
	tb2, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, 1)
		tt.Insert(2, 2)
	})
	ix2, err := tb2.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ix2.Lossy() {
		t.Error("a clean index is Lossy")
	}
	if _, flags, err := ix2.Rowids(sqlitedb.LiteralIntKey(1)); err != nil || flags != 0 {
		t.Errorf("clean hit flags %v, %v", flags, err)
	}
}

// B78: a hit returned while target rows have undecided keys may be short, so it
// carries JoinUndecided; a hit on a fully decided index does not.
func TestRowidsHitWithUndecidedTargetRowsCarriesJoinUndecided(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k text)")
		g.InsertRaw(1, []uint64{0, 19}, []byte{0x41, 0x00, 0x42}) // odd-length UTF-16: undecodable
		g.Insert(2, 2, "abc")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tb.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	ids, flags, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("abc")))
	if err != nil || !slices.Equal(ids, []int64{2}) || flags&sqlitedb.JoinUndecided == 0 {
		t.Fatalf("hit = %v, %v, %v; want [2] with JoinUndecided", ids, flags, err)
	}
	c := newCtx(t, d, nil)
	if rows, err := c.Match("g", "k", sqlitedb.LiteralTextKey([]byte("abc"))); err != nil || len(rows) != 1 {
		t.Fatalf("Match = %d, %v", len(rows), err)
	}
	j := c.Joins()
	if len(j) != 1 || j[0].UndecidedHits != 1 || j[0].Flagged != 1 {
		t.Errorf("Joins = %+v, want one flagged undecided hit", j)
	}
}

// B77: Column returns -1 only for a missing table or column. Any other failure
// is kept (the first one wins), shown by Err, and returned first by Get and Match.
func TestContextColumnKeepsAHardFailureInErr(t *testing.T) {
	d, _ := chatDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	c := sqlitedb.NewContext(ctx, d, &parse.Input{}, nil)
	t.Cleanup(c.Close)

	if c.Column("nope", "id") != -1 || c.Column("thread", "nope") != -1 || c.Err() != nil {
		t.Fatalf("a missing table or column set Err = %v", c.Err())
	}
	if _, ok, err := c.Get("thread", 1); err != nil || !ok { // loads and caches the thread table
		t.Fatal(ok, err)
	}
	cancel()
	if got := c.Column("message", "id"); got != -1 {
		t.Fatalf("Column = %d, want -1", got)
	}
	if !errors.Is(c.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want the cancellation", c.Err())
	}
	first := c.Err()
	d.Release() // a second, different failure must not replace the first
	_ = c.Column("message", "id")
	if c.Err() != first {
		t.Errorf("Err = %v, want the first failure %v", c.Err(), first)
	}
	if _, _, err := c.Get("thread", 2); !errors.Is(err, context.Canceled) {
		t.Errorf("Get after a sticky failure = %v, want the first failure", err)
	}
	if _, err := c.Match("thread", "id", sqlitedb.LiteralIntKey(1)); !errors.Is(err, context.Canceled) {
		t.Errorf("Match after a sticky failure = %v, want the first failure", err)
	}
}

// B79: a data-caused internal error (a recovered panic) is remembered, so a
// panicking table costs one scan, not one per Match.
func TestContextCachesADataCausedInternalError(t *testing.T) {
	d, _ := chatDB(t)
	c := newCtx(t, d, nil)
	calls := 0
	c.SetBuildIndex(func(context.Context, *sqlitedb.Table, string) (*sqlitedb.Index, error) {
		calls++
		return nil, fmt.Errorf("%w: boom", sqlitedb.ErrInternal)
	})
	for range 3 {
		if _, err := c.Match("message", "thread_id", sqlitedb.LiteralIntKey(1)); !errors.Is(err, sqlitedb.ErrInternal) {
			t.Fatalf("Match = %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("builds = %d, want 1", calls)
	}
}

// B92: the budget is exact at the limit. Each operation is measured once
// (warm), then must succeed with the limit set to exactly its peak cost and be
// refused one byte below it; after the owned memory is released the budget is
// back at its base. Refusal alone would not catch an early refusal (an
// off-by-one charge).
func TestBudgetExactlyAtTheCostSucceeds(t *testing.T) {
	tb, _, b := joinTable(t, sqlitetest.Options{}, "create table t(a, s)", func(tt *sqlitetest.Table) {
		for i := int64(1); i <= 20; i++ {
			tt.Insert(i, i, fmt.Sprintf("row-%d", i))
		}
	})
	scan := func() error { return tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil }) }
	ops := []struct {
		name string
		run  func() error
	}{
		{"index", func() error {
			ix, err := tb.Index(t.Context(), "a", 0)
			ix.Release()
			return err
		}},
		{"scan", scan},
		{"get", func() error {
			r, ok, err := tb.Get(t.Context(), 7)
			if err == nil && !ok {
				return errors.New("row 7 not found")
			}
			r.Release()
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			b.limit = 1 << 40
			if err := op.run(); err != nil { // warm: the reader's page cache stays charged
				t.Fatal(err)
			}
			base := b.used
			b.peak = base
			if err := op.run(); err != nil {
				t.Fatal(err)
			}
			cost := b.peak - base
			if cost <= 0 {
				t.Fatalf("%s charged nothing (cost %d)", op.name, cost)
			}
			b.limit = base + cost
			if err := op.run(); err != nil {
				t.Errorf("%s with the limit at exactly its cost %d: %v", op.name, cost, err)
			}
			if b.used != base {
				t.Errorf("%s left %d bytes charged", op.name, b.used-base)
			}
			b.limit = base + cost - 1
			if err := op.run(); !errors.Is(err, parse.ErrBudget) {
				t.Errorf("%s one byte under its cost = %v, want ErrBudget", op.name, err)
			}
			if b.used != base {
				t.Errorf("%s refused but left %d bytes charged", op.name, b.used-base)
			}
		})
	}
	t.Run("clone", func(t *testing.T) {
		b.limit = 1 << 40
		cloneAt := func(slack int64) (cost int64, err error) { // slack < 0: measure
			serr := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
				before := b.used
				if slack >= 0 {
					b.limit = before + slack
				}
				c, cerr := r.Clone()
				cost, err = b.used-before, cerr
				c.Release()
				b.limit = 1 << 40
				return sqlitedb.ErrStop
			})
			if serr != nil {
				t.Fatal(serr)
			}
			return cost, err
		}
		cost, err := cloneAt(-1)
		if err != nil || cost <= 0 {
			t.Fatalf("measure: cost %d, %v", cost, err)
		}
		if _, err := cloneAt(cost); err != nil {
			t.Errorf("Clone with room for exactly its cost %d: %v", cost, err)
		}
		if _, err := cloneAt(cost - 1); !errors.Is(err, parse.ErrBudget) {
			t.Errorf("Clone one byte under its cost = %v, want ErrBudget", err)
		}
	})
}
