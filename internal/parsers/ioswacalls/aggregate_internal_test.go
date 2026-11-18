package ioswacalls

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// callDB is a CallHistory-like store of the call table only: Z_PK, Z1CALLEVENTS (the values, one call each).
func callDB(t *testing.T, ddl string, keys ...any) *sqlitedb.DB {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	c := b.CreateTable(tableCall, ddl)
	for i, k := range keys {
		c.Insert(int64(i+1), nil, k)
	}
	data := b.Bytes()
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(256<<20).View())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Release)
	return db
}

const callDDL1 = "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z1CALLEVENTS INTEGER)"

// loadAggregates counts, per integer key, the calls that name it, capped at maxAggCount, and keeps the Z_PK of the one call of a
// single-call aggregate. A NULL, a 0, a TEXT, a BLOB and a REAL name no aggregate; none is held beyond a count (PF4).
func TestLoadAggregatesCountsCallsPerKey(t *testing.T) {
	keys := []any{int64(5), int64(5), int64(7), nil, int64(0), "x", []byte{1}, 2.5, int64(9), int64(-3)}
	for i := 0; i < maxAggCount+5; i++ {
		keys = append(keys, int64(11))
	}
	db := callDB(t, callDDL1, keys...)
	b := parse.NewBudget(1 << 20).View()
	as, err := loadAggregates(context.Background(), db, b, maxAggregates)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Free(as.charged)
	if !as.Complete || as.Lossy || as.NoColumn || as.Why != "" || as.Total != int64(len(keys)) {
		t.Errorf("set %+v, want complete over %d rows", as, len(keys))
	}
	want := map[int64]aggEntry{5: {Count: 2, Only: 1}, 7: {Count: 1, Only: 3}, 9: {Count: 1, Only: 9}, -3: {Count: 1, Only: 10}, 11: {Count: maxAggCount, Only: 11}}
	if len(as.ByAgg) != len(want) || as.Read != int64(len(want)) {
		t.Fatalf("ByAgg %v read %d, want %d keys", as.ByAgg, as.Read, len(want))
	}
	for k, w := range want {
		got := as.ByAgg[k]
		if got.Count != w.Count || (w.Count == 1 && got.Only != w.Only) {
			t.Errorf("key %d: %+v, want %+v", k, got, w)
		}
	}
	if as.charged <= 0 || b.Used() != as.charged {
		t.Errorf("charged %d, budget used %d: the set is charged to the budget", as.charged, b.Used())
	}
}

func TestLoadAggregatesStatesWhatItCouldNotDo(t *testing.T) {
	// no column: the state of the call, not a count of zero
	db := callDB(t, "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z2CALLEVENTS INTEGER)", int64(1))
	as, err := loadAggregates(context.Background(), db, parse.NewBudget(1<<20).View(), maxAggregates)
	if err != nil || !as.NoColumn || len(as.ByAgg) != 0 {
		t.Errorf("no column: %+v, %v", as, err)
	}
	// more distinct keys than the cap: the keys read are kept and counted exactly, the rest is unread, the set is incomplete
	db = callDB(t, callDDL1, int64(1), int64(2), int64(3), int64(1), int64(4), int64(1))
	as, err = loadAggregates(context.Background(), db, parse.NewBudget(1<<20).View(), 3)
	if err != nil || as.Complete || as.Lossy || as.Why == "" || as.Total != 6 || as.Read != 3 || as.ByAgg[1].Count != 3 || len(as.ByAgg) != 3 {
		t.Errorf("capped: %+v, %v", as, err)
	}
	if _, has := as.ByAgg[4]; has {
		t.Error("a key past the cap is in the set")
	}
	// a budget too small for the set: the scan stops, so the counts are unreliable: lossy, never a decided count
	db = callDB(t, callDDL1, int64(1), int64(2), int64(3), int64(1))
	tiny := parse.NewBudget(aggCharge + aggCharge/2).View()
	as, err = loadAggregates(context.Background(), db, tiny, maxAggregates)
	if err != nil || as.Complete || !as.Lossy || as.Why == "" {
		t.Errorf("budget: %+v, %v", as, err)
	}
	tiny.Free(as.charged)
	if tiny.Used() != 0 {
		t.Errorf("budget holds %d after the free", tiny.Used())
	}
	// a cancelled context is the caller's error, never a state
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadAggregates(ctx, callDB(t, callDDL1, int64(1)), parse.NewBudget(1<<20).View(), maxAggregates); err == nil {
		t.Error("a cancelled context gave no error")
	}
}
