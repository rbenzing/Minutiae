package ioswacalls_test

import (
	"math"
	"reflect"
	"testing"
)

// Final review I1: a stored REAL NaN, which the reader gives as NULL with its serial type 7, in a key, code or text column
// is a value of another class: undecided and listed in unknown_columns, never "no-aggregate" (amendment 4I2-A4: a key that
// is not an integer is undecided).
func TestIOSWhatsAppCallsNaNInAKeyCodeOrTextColumnIsUndecided(t *testing.T) {
	nan := math.NaN()
	cases := map[string]int64{}
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(nan, nil))
		c := tb["ZWACDCALLEVENT"]
		pk := int64(0)
		add := func(name string, kv map[string]any) {
			pk++
			cases[name] = pk
			c.put(pk, callRow(kv))
		}
		add("key", map[string]any{"Z1CALLEVENTS": nan})
		add("outcome", map[string]any{"ZOUTCOME": nan})
		add("call id", map[string]any{"ZCALLIDSTRING": nan})
		add("bytes", map[string]any{"ZBYTESSENT": nan})
		add("incoming", map[string]any{"Z1CALLEVENTS": int64(1)})
	})
	recs, _ := runCalls(t, db, nil)
	flagged := func(name, col string) {
		t.Helper()
		uc, _ := rawOf(recs[cases[name]])["unknown_columns"].(map[string]any)
		if uc[col] != "type-mismatch" {
			t.Errorf("%s: unknown_columns[%s] = %v, want type-mismatch (raw %v)", name, col, uc[col], rawOf(recs[cases[name]]))
		}
	}
	if got := rawOf(recs[cases["key"]])["direction_state"]; got != "undecided" {
		t.Errorf("a NaN Z1CALLEVENTS: direction_state %v, want undecided", got)
	}
	flagged("key", "ZWACDCALLEVENT.Z1CALLEVENTS")
	flagged("outcome", "ZWACDCALLEVENT.ZOUTCOME")
	if got := recs[cases["outcome"]].Payload["outcome"]; got != "unknown" {
		t.Errorf("a NaN ZOUTCOME: outcome %v", got)
	}
	flagged("call id", "ZWACDCALLEVENT.ZCALLIDSTRING")
	flagged("bytes", "ZWACDCALLEVENT.ZBYTESSENT")
	if got := rawOf(recs[cases["incoming"]])["direction_state"]; got != "incoming-unknown" {
		t.Errorf("a NaN ZINCOMING: direction_state %v", got)
	}
	flagged("incoming", "ZWAAGGREGATECALLEVENT.ZINCOMING")
}

// Final review: a text cell of the aggregate row that cleaning changed is listed in raw.text_flags under "aggregate.<column>"
// (raw.aggregate holds it), never under the bare name of a column of the call row.
func TestIOSWhatsAppCallsAggregateTextFlagIsPrefixed(t *testing.T) {
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow("1\x00", nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
	})
	recs, _ := runCalls(t, db, nil)
	raw := rawOf(recs[1])
	if got := raw["text_flags"]; !reflect.DeepEqual(got, []any{"aggregate.ZINCOMING"}) {
		t.Errorf("text_flags = %v, want [aggregate.ZINCOMING] (raw %v)", got, raw)
	}
}
