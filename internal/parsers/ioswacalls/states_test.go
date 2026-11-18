package ioswacalls_test

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The in-test databases of the state vocabulary (4I2-R7) and of the other unit tests of this package: built with the test
// builder, so every state is pinned on a row the test itself wrote, never on the parser's own output. The DDL is the subset of
// columns the mapping reads, in the Core Data form of tools/fixtures/schemas/whatsapp-ios/callhistory.sql.

type col []string // a table's declared columns, Z_PK first

var callDDL = map[string]struct {
	sql  string
	cols col
}{
	"ZWACDCALLEVENT": {
		"CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z1CALLEVENTS INTEGER, ZBYTESRECEIVED INTEGER, ZBYTESSENT INTEGER, ZOUTCOME INTEGER, ZVIDEO INTEGER, ZDATE TIMESTAMP, ZDURATION FLOAT, ZCALLIDSTRING VARCHAR, ZGROUPCALLCREATORUSERJIDSTRING VARCHAR, ZGROUPJIDSTRING VARCHAR)",
		col{"Z_PK", "Z_ENT", "Z1CALLEVENTS", "ZBYTESRECEIVED", "ZBYTESSENT", "ZOUTCOME", "ZVIDEO", "ZDATE", "ZDURATION", "ZCALLIDSTRING", "ZGROUPCALLCREATORUSERJIDSTRING", "ZGROUPJIDSTRING"},
	},
	"ZWAAGGREGATECALLEVENT": {
		"CREATE TABLE ZWAAGGREGATECALLEVENT (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, ZINCOMING INTEGER, ZMISSED INTEGER, ZMISSEDREASON INTEGER, ZVIDEO INTEGER)",
		col{"Z_PK", "Z_ENT", "ZINCOMING", "ZMISSED", "ZMISSEDREASON", "ZVIDEO"},
	},
	"ZWACDCALLEVENTPARTICIPANT": {
		"CREATE TABLE ZWACDCALLEVENTPARTICIPANT (Z_PK INTEGER PRIMARY KEY, Z1PARTICIPANTS INTEGER, ZJIDSTRING VARCHAR)",
		col{"Z_PK", "Z1PARTICIPANTS", "ZJIDSTRING"},
	},
}

var callOrder = []string{"ZWACDCALLEVENT", "ZWAAGGREGATECALLEVENT"}

// callTable puts rows by column name.
type callTable struct {
	t    *sqlitetest.Table
	cols col
}

func (c callTable) vals(kv map[string]any) []any {
	vals := make([]any, len(c.cols))
	for i, n := range c.cols {
		if n != "Z_PK" {
			vals[i] = kv[n]
		}
	}
	return vals
}

func (c callTable) put(pk int64, kv map[string]any)    { c.t.Insert(pk, c.vals(kv)...) }
func (c callTable) update(pk int64, kv map[string]any) { c.t.Update(pk, c.vals(kv)...) }

type callTables map[string]callTable

// buildCalls builds a CallHistory-like store from callDDL. over replaces a table's DDL (an empty string drops the table); extra
// names more tables of callDDL to create. fill adds the rows. The second result is a WAL of the pages (a companion that
// defers the schema checks to Parse).
func buildCalls(over map[string]string, fill func(tb callTables), extra ...string) (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := callTables{}
	for _, name := range append(append([]string{}, callOrder...), extra...) {
		d := callDDL[name]
		sql := d.sql
		if o, ok := over[name]; ok {
			sql = o
		}
		if sql != "" {
			tb[name] = callTable{t: b.CreateTable(name, sql), cols: d.cols}
		}
	}
	fill(tb)
	return b.Bytes(), b.NewWAL(false, 1, 2, 0).Bytes()
}

const goodDate = float64(600000100)

// callRow is a ZWACDCALLEVENT row: an answered voice call of no aggregate (Z1CALLEVENTS NULL) unless kv says otherwise.
func callRow(kv map[string]any) map[string]any {
	r := map[string]any{"Z_ENT": int64(1), "ZOUTCOME": int64(0), "ZVIDEO": int64(0), "ZDATE": goodDate, "ZDURATION": float64(0)}
	for k, v := range kv {
		r[k] = v
	}
	return r
}

func aggRow(incoming any, kv map[string]any) map[string]any {
	r := map[string]any{"Z_ENT": int64(2), "ZINCOMING": incoming}
	for k, v := range kv {
		r[k] = v
	}
	return r
}

// runCalls parses a Go-built store at the consumer backup domain (with a -wal when wal is not nil) and returns the records by
// Z_PK.
func runCalls(t *testing.T, db, wal []byte, allowed ...string) (map[int64]records.Record, parsertest.Result) {
	t.Helper()
	res := runCallsErr(t, db, wal, allowed...)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	return byRowid(res), res
}

func byRowid(res parsertest.Result) map[int64]records.Record {
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		out[rowidOf(r.Locator)] = r
	}
	return out
}

func runCallsErr(t *testing.T, db, wal []byte, allowed ...string) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswacalls.Parser{}
	return runLenient(t, h, p, b.Input(h, p, false), allowed...)
}

func rowidOf(loc string) int64 {
	_, rest, _ := strings.Cut(loc, ";rowid=")
	num, _, _ := strings.Cut(rest, ";")
	n, _ := strconv.ParseInt(num, 10, 64)
	return n
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// 4I2-R7: every raw state key and value of the calls parser, pinned on a row of a database written here with hard-coded
// expectations. The vocabulary is the coordinator's; this test is its oracle. Each aggregate is linked by the calls its case
// names, so a case never changes the count of another.
func TestIOSWhatsAppCallsStateVocabularyIsPinned(t *testing.T) {
	cases := map[string]int64{} // the Z_PK of the row built for each case
	const floodAgg, floodCalls = 6, 1001
	var floodFirst int64
	db, _ := buildCalls(nil, func(tb callTables) {
		a := tb["ZWAAGGREGATECALLEVENT"]
		a.put(1, aggRow(int64(1), map[string]any{"ZVIDEO": int64(0), "ZMISSED": int64(0), "ZMISSEDREASON": int64(3)}))
		a.put(2, aggRow(int64(0), nil))
		a.put(3, aggRow(nil, nil))
		a.put(4, aggRow(int64(7), nil))
		a.put(5, aggRow(int64(1), nil))
		a.put(6, aggRow(int64(1), nil))
		a.put(7, aggRow("1", nil)) // a TEXT ZINCOMING: a stored value, not 0 or 1
		a.put(8, aggRow(int64(1), nil))
		a.put(9, aggRow(int64(0), nil))
		c := tb["ZWACDCALLEVENT"]
		pk := int64(0)
		add := func(name string, kv map[string]any) {
			pk++
			cases[name] = pk
			c.put(pk, callRow(kv))
		}
		add("agg NULL", nil)
		add("agg 0", map[string]any{"Z1CALLEVENTS": int64(0)})
		add("single in", map[string]any{"Z1CALLEVENTS": int64(1)})
		add("single out", map[string]any{"Z1CALLEVENTS": int64(2)})
		add("incoming NULL", map[string]any{"Z1CALLEVENTS": int64(3)})
		add("incoming 7", map[string]any{"Z1CALLEVENTS": int64(4)})
		add("no aggregate row", map[string]any{"Z1CALLEVENTS": int64(99)})
		add("multi a", map[string]any{"Z1CALLEVENTS": int64(5)})
		add("multi b", map[string]any{"Z1CALLEVENTS": int64(5)})
		add("key text", map[string]any{"Z1CALLEVENTS": "x"})
		add("key blob", map[string]any{"Z1CALLEVENTS": []byte{1}})
		add("incoming text", map[string]any{"Z1CALLEVENTS": int64(7)})
		add("missed in", map[string]any{"Z1CALLEVENTS": int64(8), "ZOUTCOME": int64(1)})
		add("missed out", map[string]any{"Z1CALLEVENTS": int64(9), "ZOUTCOME": int64(1)})
		add("missed no aggregate", map[string]any{"ZOUTCOME": int64(1)})
		add("outcome 5", map[string]any{"ZOUTCOME": int64(5)})
		add("duration", map[string]any{"ZDURATION": float64(10.5)})
		add("duration negative", map[string]any{"ZDURATION": float64(-3)})
		add("duration nan", map[string]any{"ZDURATION": math.NaN()})
		add("duration inf", map[string]any{"ZDURATION": math.Inf(1)})
		add("duration text", map[string]any{"ZDURATION": "long"})
		add("duration int", map[string]any{"ZDURATION": int64(7)})
		add("duration huge", map[string]any{"ZDURATION": 2e9})
		add("date null", map[string]any{"ZDATE": nil, "ZDURATION": float64(5)})
		add("bytes", map[string]any{"ZBYTESSENT": int64(10), "ZBYTESRECEIVED": int64(20), "ZCALLIDSTRING": "CID", "ZGROUPJIDSTRING": "120363000000000001@g.us", "ZGROUPCALLCREATORUSERJIDSTRING": "15550000002@s.whatsapp.net"})
		floodFirst = pk + 1
		for i := 0; i < floodCalls; i++ {
			pk++
			c.put(pk, callRow(map[string]any{"Z1CALLEVENTS": int64(floodAgg)}))
		}
	})
	recs, res := runCalls(t, db, nil)
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %v", res.Warnings)
	}
	type want struct {
		key string
		val any
	}
	check := func(name string, wants ...want) {
		t.Helper()
		r, ok := recs[cases[name]]
		if !ok {
			t.Fatalf("%s: no record", name)
		}
		raw := rawOf(r)
		for _, w := range wants {
			if got := raw[w.key]; !reflect.DeepEqual(got, w.val) {
				t.Errorf("%s: raw.%s = %#v, want %#v (raw %v)", name, w.key, got, w.val, raw)
			}
		}
	}
	dir := func(name, direction string) {
		t.Helper()
		if got := recs[cases[name]].Payload["direction"]; got != direction {
			t.Errorf("%s: direction %v, want %s", name, got, direction)
		}
	}
	// direction_state, one value at a time
	check("agg NULL", want{"direction_state", "no-aggregate"}, want{"aggregate", nil}, want{"Z1CALLEVENTS", nil})
	check("agg 0", want{"direction_state", "no-aggregate"}, want{"Z1CALLEVENTS", int64(0)})
	check("single in", want{"direction_state", nil}, want{"aggregate", map[string]any{"Z_PK": int64(1), "ZINCOMING": int64(1), "ZVIDEO": int64(0), "ZMISSED": int64(0), "ZMISSEDREASON": int64(3)}})
	check("single out", want{"direction_state", nil}, want{"aggregate", map[string]any{"Z_PK": int64(2), "ZINCOMING": int64(0)}})
	dir("single in", "in")
	dir("single out", "out")
	check("incoming NULL", want{"direction_state", "incoming-unknown"}, want{"aggregate", map[string]any{"Z_PK": int64(3)}})
	check("incoming 7", want{"direction_state", "incoming-unknown"}, want{"aggregate", map[string]any{"Z_PK": int64(4), "ZINCOMING": int64(7)}})
	check("no aggregate row", want{"direction_state", "no-ZWAAGGREGATECALLEVENT-row"}, want{"aggregate", nil}, want{"Z1CALLEVENTS", int64(99)})
	check("multi a", want{"direction_state", "multi-call-aggregate"}, want{"aggregate_calls", int64(2)}, want{"aggregate_calls_truncated", nil},
		want{"aggregate", map[string]any{"Z_PK": int64(5), "ZINCOMING": int64(1)}})
	check("key text", want{"direction_state", "undecided"}, want{"Z1CALLEVENTS", "x"}, want{"aggregate", nil},
		want{"unknown_columns", map[string]any{"ZWACDCALLEVENT.Z1CALLEVENTS": "type-mismatch"}})
	check("key blob", want{"direction_state", "undecided"}, want{"Z1CALLEVENTS", map[string]any{"blob_b64": "AQ=="}}, want{"aggregate", nil},
		want{"unknown_columns", map[string]any{"ZWACDCALLEVENT.Z1CALLEVENTS": "type-mismatch"}})
	check("incoming text", want{"direction_state", "incoming-unknown"}, want{"aggregate", map[string]any{"Z_PK": int64(7), "ZINCOMING": "1"}},
		want{"unknown_columns", map[string]any{"ZWAAGGREGATECALLEVENT.ZINCOMING": "type-mismatch"}})
	for _, n := range []string{"agg NULL", "agg 0", "incoming NULL", "incoming 7", "no aggregate row", "multi a", "multi b", "key text", "key blob", "incoming text"} {
		dir(n, "unknown")
	}
	// 1001 calls on one aggregate: the count is capped at 1000 and says so
	flood := recs[floodFirst]
	if rf := rawOf(flood); flood.Payload["direction"] != "unknown" || rf["direction_state"] != "multi-call-aggregate" || rf["aggregate_calls"] != int64(1000) || rf["aggregate_calls_truncated"] != true {
		t.Errorf("1001 calls on one aggregate: %v", rf)
	}
	// outcome and label_ZOUTCOME: a missed code is "missed" only on a call known incoming; otherwise it is kept as a label
	check("missed in", want{"label_ZOUTCOME", nil}, want{"ZOUTCOME", int64(1)})
	dir("missed in", "in")
	check("missed out", want{"label_ZOUTCOME", "missed"})
	check("missed no aggregate", want{"label_ZOUTCOME", "missed"})
	check("outcome 5", want{"label_ZOUTCOME", nil}, want{"ZOUTCOME", int64(5)})
	for name, outcome := range map[string]string{"missed in": "missed", "missed out": "unknown", "missed no aggregate": "unknown", "outcome 5": "unknown", "single in": "answered"} {
		if got := recs[cases[name]].Payload["outcome"]; got != outcome {
			t.Errorf("%s: outcome %v, want %s", name, got, outcome)
		}
	}
	// time_end_basis and the ZDURATION states
	check("duration", want{"time_end_basis", "start+duration"}, want{"ZDURATION", nil})
	if r := recs[cases["duration"]]; r.TimeEnd == nil || r.Payload["duration_s"] != 10.5 {
		t.Errorf("duration: time end %v payload %v", r.TimeEnd, r.Payload)
	}
	check("duration negative", want{"ZDURATION", float64(-3)}, want{"ZDURATION_status", "negative"}, want{"time_end_basis", nil})
	check("duration nan", want{"ZDURATION_status", "invalid"}, want{"time_end_basis", nil})
	check("duration inf", want{"ZDURATION_status", "invalid"}, want{"ZDURATION_ieee", "7ff0000000000000"}, want{"time_end_basis", nil})
	check("duration text", want{"unknown_columns", map[string]any{"ZWACDCALLEVENT.ZDURATION": "type-mismatch"}})
	check("duration int", want{"time_end_basis", "start+duration"})
	check("duration huge", want{"time_end_basis", nil})
	if r := recs[cases["duration huge"]]; r.TimeEnd != nil || r.Payload["duration_s"] != 2e9 {
		t.Errorf("duration huge: %v %v", r.TimeEnd, r.Payload)
	}
	for _, n := range []string{"duration negative", "duration nan", "duration inf", "duration text"} {
		if r := recs[cases[n]]; r.TimeEnd != nil || r.Payload["duration_s"] != nil {
			t.Errorf("%s: duration %v end %v", n, r.Payload["duration_s"], r.TimeEnd)
		}
	}
	if r := recs[cases["date null"]]; r.Time != nil || r.TimeEnd != nil || rawOf(r)["ZDATE"] != nil || rawOf(r)["ZDATE_status"] != nil {
		t.Errorf("date null: %v %v", r.Time, rawOf(r))
	}
	// the row's own columns, as stored, and the label of the app (the participant table is not joined)
	check("bytes", want{"ZBYTESSENT", int64(10)}, want{"ZBYTESRECEIVED", int64(20)}, want{"ZCALLIDSTRING", "CID"}, want{"ZGROUPJIDSTRING", "120363000000000001@g.us"},
		want{"ZGROUPCALLCREATORUSERJIDSTRING", "15550000002@s.whatsapp.net"}, want{"Z_ENT", int64(1)}, want{"ZOUTCOME", int64(0)}, want{"ZVIDEO", int64(0)},
		want{"app", "whatsapp"}, want{"app_basis", "backup_domain"}, want{"app_state", nil})
}

// a store with no Z1CALLEVENTS column (another Z<n>CALLEVENTS name; spec 6.11 names Z1CALLEVENTS): every call states
// no-aggregate-column, never a direction and never no-aggregate (4I2-R4, PF9 c).
func TestIOSWhatsAppCallsWithoutTheAggregateColumnStatesIt(t *testing.T) {
	db, _ := buildCalls(map[string]string{
		"ZWACDCALLEVENT": "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z2CALLEVENTS INTEGER, ZOUTCOME INTEGER, ZVIDEO INTEGER, ZDATE TIMESTAMP, ZDURATION FLOAT)",
	}, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		c := tb["ZWACDCALLEVENT"]
		for i := int64(1); i <= 3; i++ {
			c.t.Insert(i, int64(1), int64(1), int64(0), int64(0), goodDate, float64(0)) // Z2CALLEVENTS = 1: it would be a direction if it were read
		}
	})
	recs, res := runCalls(t, db, nil)
	if len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
	for id, r := range recs {
		raw := rawOf(r)
		if r.Payload["direction"] != "unknown" || raw["direction_state"] != "no-aggregate-column" || raw["aggregate"] != nil {
			t.Errorf("call %d: %v", id, r.Payload)
		}
	}
	if _, has := res.Notes["aggregates"]; has {
		t.Errorf("an aggregates note without the column: %v", res.Notes)
	}
}
