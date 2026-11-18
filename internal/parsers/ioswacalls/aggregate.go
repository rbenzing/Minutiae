package ioswacalls

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The aggregate set (4I2-R4, PF4): which calls link to which aggregate (ZWACDCALLEVENT.Z1CALLEVENTS is the aggregate's key, a
// name from memory, docs/limitations.md). A call takes its direction only from an aggregate that links exactly this one call,
// so the mapping needs, for every key, how many calls name it, and for a single-call key which call. One scan of the call table
// gives both, holding a count and a Z_PK per distinct key and no row, so the cost is the number of distinct keys, charged to the
// budget and freed by the caller. A call is never counted per aggregate by a per-call join (that would be quadratic).

const (
	// maxAggregates is the most distinct aggregate keys the set holds (about 64 MiB of budget at aggCharge each).
	maxAggregates = 1_000_000
	// maxAggCount is the count of calls per key that is kept: the mapping states at most 1000 and says when there are more
	// (raw.aggregate_calls_truncated), so 1001 is enough to know that.
	maxAggCount = 1001
	// aggCharge is what one key of the set is charged to the budget: its map entry with the overhead of the map.
	aggCharge = 64
)

// aggEntry is what is known of one aggregate key: how many calls name it (at most maxAggCount) and, when exactly one does, its
// Z_PK.
type aggEntry struct {
	Count int
	Only  int64
}

// aggSet is the result of the scan. ByAgg holds the keys read. Complete says every key and every count is exact. Lossy says the
// scan did not see every call (a damaged page or cell, a budget refusal): then no count can be trusted, because the call that
// was not seen may name any aggregate, and no direction is decided from the set. A set that is not Complete and not Lossy hit
// the cap: the keys it holds are counted exactly (the scan went on counting them), the rows whose key is not in it are
// undecided. NoColumn says the table has no Z1CALLEVENTS (every call states no-aggregate-column). Read is the number of keys
// held, Total the number of rows seen, Why the reason the set is not complete. charged is what the set holds of the budget.
type aggSet struct {
	ByAgg    map[int64]aggEntry
	Complete bool
	Lossy    bool
	NoColumn bool
	Read     int64
	Total    int64
	Why      string
	charged  int64
}

// loadAggregates scans ZWACDCALLEVENT of db once. Only a cancelled context and a failure that is not the database's damage (I/O,
// the context) are errors; damage and the limits are states of the set. The memory the set holds is charged to b and is given
// back by the caller (b.Free(as.charged)), also on the error path of the caller.
func loadAggregates(ctx context.Context, db *sqlitedb.DB, b *parse.BudgetView, maxKeys int) (*aggSet, error) {
	as := &aggSet{ByAgg: map[int64]aggEntry{}, Complete: true}
	lossy := func(why string) {
		as.Complete, as.Lossy = false, true
		if as.Why == "" {
			as.Why = why
		}
	}
	t, err := db.Table(ctx, tableCall, nil, nil)
	switch {
	case err == nil:
	case dataDamage(err):
		lossy("the call table could not be read")
		return as, nil
	default:
		return nil, err
	}
	col := t.Col("Z1CALLEVENTS")
	if col < 0 {
		as.NoColumn = true
		return as, nil
	}
	warned := len(db.Warnings())
	budgetHit := false
	err = t.Scan(ctx, func(r sqlitedb.Row) error {
		as.Total++
		if rowrun.DamageFlags(r) != nil {
			lossy("a call row is damaged")
			return nil
		}
		key, k := rowrun.IntCell(r, col)
		switch k {
		case rowrun.CellNull, rowrun.CellOther: // names no aggregate (the row's own state says undecided for a non-integer)
			return nil
		case rowrun.CellValue:
		default:
			lossy("a Z1CALLEVENTS cell could not be read")
			return nil
		}
		if key == 0 {
			return nil
		}
		id, ok := r.Rowid()
		if !ok {
			lossy("a call row has no rowid")
			return nil
		}
		if e, have := as.ByAgg[key]; have {
			if e.Count < maxAggCount {
				e.Count++
				as.ByAgg[key] = e
			}
			return nil
		}
		if len(as.ByAgg) >= maxKeys {
			if as.Why == "" {
				as.Why = fmt.Sprintf("more than %d distinct aggregates", maxKeys)
			}
			as.Complete = false // the scan goes on: the counts of the keys held stay exact
			return nil
		}
		if err := b.Alloc(aggCharge); err != nil {
			budgetHit = true
			return sqlitedb.ErrStop
		}
		as.charged += aggCharge
		as.ByAgg[key] = aggEntry{Count: 1, Only: id}
		return nil
	})
	as.Read = int64(len(as.ByAgg))
	switch {
	case err != nil && errors.Is(err, parse.ErrBudget):
		budgetHit = true
	case err != nil:
		if cerr := ctx.Err(); cerr != nil {
			return as, cerr
		}
		if !dataDamage(err) && !errors.Is(err, sqlitedb.ErrCorrupt) {
			return as, err
		}
		lossy("the call table could not be read whole")
	}
	if budgetHit {
		lossy("the memory limit was reached")
	}
	if len(db.Warnings()) > warned {
		lossy("the reader skipped damaged pages")
	}
	return as, nil
}
