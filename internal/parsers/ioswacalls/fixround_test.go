package ioswacalls_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// Amendment 4I2-A4 (fix round 1, M4): raw.aggregate states every stored column of the aggregate row as stored, whatever the
// storage class (a BLOB over 256 bytes by its length only); the direction still needs a 0 or 1.
func TestIOSWhatsAppCallsAggregateColumnsAreStatedAsStored(t *testing.T) {
	blob := make([]byte, 300)
	db, _ := buildCalls(nil, func(tb callTables) {
		a := tb["ZWAAGGREGATECALLEVENT"]
		a.put(1, aggRow("1", map[string]any{"ZMISSEDREASON": 2.5, "ZMISSED": blob, "ZVIDEO": []byte{1}}))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
	})
	recs, _ := runCalls(t, db, nil)
	raw := rawOf(recs[1])
	want := map[string]any{"Z_PK": int64(1), "ZINCOMING": "1", "ZMISSEDREASON": 2.5, "ZMISSED": map[string]any{"blob_len": int64(300)}, "ZVIDEO": map[string]any{"blob_b64": "AQ=="}}
	if !reflect.DeepEqual(raw["aggregate"], want) || raw["direction_state"] != "incoming-unknown" || recs[1].Payload["direction"] != "unknown" {
		t.Errorf("aggregate %v state %v, want %v and incoming-unknown", raw["aggregate"], raw["direction_state"], want)
	}
	uc, _ := raw["unknown_columns"].(map[string]any)
	if uc["ZWAAGGREGATECALLEVENT.ZINCOMING"] != "type-mismatch" || uc["ZWAAGGREGATECALLEVENT.ZMISSEDREASON"] != "type-mismatch" {
		t.Errorf("unknown_columns %v", uc)
	}
}

// refusing is an emitter that refuses every reader warning and drops everything else.
type refusing struct{}

func (refusing) Emit(context.Context, records.Record) error { return nil }
func (refusing) Warn(_ context.Context, locator, _ string) error {
	if strings.HasPrefix(locator, "sqlite:file=") {
		return errors.New("warning refused")
	}
	return nil
}
func (refusing) Note(string, string)   {}
func (refusing) Progress(int64, int64) {}

// Fix round 1, M5b: a reader warning the host refuses is the job's failure, never swallowed (the held-back scan warnings of a
// damaged call page go through the same emitter).
func TestIOSWhatsAppCallsARefusedReaderWarningFailsTheJob(t *testing.T) {
	data := damagedCallStore(t)
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "refuse", Files: map[string][]byte{"db": data}}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswacalls.Parser{}
	err := ioswacalls.ParseWith(t.Context(), b.Input(h, p, false), refusing{}, 1000)
	if err == nil || !strings.Contains(err.Error(), "warning refused") {
		t.Errorf("err %v, want the refused warning", err)
	}
}

// Fix round 1, M3: a database that only a deferred Probe lets through (a -wal companion) with a required column missing is a
// typed schema error of rowrun.Run, never ErrAggregatesIncomplete, even when the set is incomplete (Run reports not-applicable
// only for an absent identifying table, which preflight has already decided: then there is no set at all).
func TestIOSWhatsAppCallsAnotherAppsDatabaseIsNotAnIncompleteSet(t *testing.T) {
	db, wal := buildCalls(map[string]string{
		"ZWACDCALLEVENT": "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z1CALLEVENTS INTEGER, ZOUTCOME INTEGER, ZDATE TIMESTAMP)",
	}, func(tb callTables) {
		for i := int64(1); i <= 3; i++ {
			tb["ZWACDCALLEVENT"].t.Insert(i, nil, i, int64(0), goodDate)
		}
	})
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "other", Files: map[string][]byte{"db": db, "wal": wal}}.BundleIOS(h, extractRemote, extractLogical)
	p := cappedParser{max: 1}
	res := runLenientWith(t, h, p, b.Input(h, p, false), "not applicable at parse time", "wal-frames-not-applied", "aggregate-set-incomplete")
	if !errors.Is(res.Err, sqlitedb.ErrUnsupportedSchema) || errors.Is(res.Err, ioswacalls.ErrAggregatesIncomplete) || len(res.Records) != 0 {
		t.Errorf("err %v, %d records, want the typed schema error", res.Err, len(res.Records))
	}
}

// Fix round 1, mutation gap: the aggregate set is charged to the budget. The smallest budget that lets the parse run with an
// empty set (cap 0: nothing is charged) is found; with that budget a set that needs a charge is refused and states the memory
// limit, and with a little more room it is read whole and the budget is empty again afterwards.
func TestIOSWhatsAppCallsAggregateSetIsChargedToTheBudget(t *testing.T) {
	db, _ := buildCalls(nil, func(tb callTables) {
		for i := int64(1); i <= 20; i++ {
			tb["ZWAAGGREGATECALLEVENT"].put(i, aggRow(int64(1), nil))
			tb["ZWACDCALLEVENT"].put(i, callRow(map[string]any{"Z1CALLEVENTS": i}))
		}
	})
	run := func(limit int64, keys int) (parsertest.Result, *parse.Budget) {
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: "budget", Files: map[string][]byte{"db": db}}.BundleIOSBackup(h, domainConsumer, dbName)
		p := cappedParser{max: keys}
		in := b.Input(h, p, false)
		bud := parse.NewBudget(limit)
		in.Budget = bud.View()
		return h.RunLenient(p, in), bud
	}
	runs := func(limit int64) bool { // the parse runs to its end with nothing charged for the set
		res, _ := run(limit, 0)
		return len(res.Records) == 20 && errors.Is(res.Err, ioswacalls.ErrAggregatesIncomplete)
	}
	lo, hi := int64(1), int64(64<<20)
	if !runs(hi) {
		t.Fatal("the parse does not run at 64 MiB")
	}
	for lo < hi {
		mid := (lo + hi) / 2
		if runs(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	res, _ := run(lo, 20)
	var why string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "aggregate-set-incomplete") {
			why = w.Reason
		}
	}
	if ok := len(res.Records) == 20 && (res.Err == nil || errors.Is(res.Err, ioswacalls.ErrAggregatesIncomplete)); ok {
		t.Errorf("at the smallest budget (%d) that runs with nothing charged, a set that is charged ran to its end: err %v, warning %q", lo, res.Err, why)
	}
	res, bud := run(lo+64*20*4, 20)
	if res.Err != nil || bud.Used() != 0 {
		t.Errorf("with room: err %v, budget holds %d afterwards", res.Err, bud.Used())
	}
}
