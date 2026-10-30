package ioscalls_test

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The Core Data layout in the order of the DDL snapshot, and a table built from it.
var coreCols = []string{
	"Z_PK", "Z_ENT", "Z_OPT", "ZANSWERED", "ZCALLTYPE", "ZDISCONNECTED_CAUSE", "ZFACE_TIME_DATA",
	"ZNUMBER_AVAILABILITY", "ZORIGINATED", "ZREAD", "ZJUNKCONFIDENCE", "ZDATE", "ZDURATION", "ZADDRESS", "ZDEVICE_ID",
	"ZISO_COUNTRY_CODE", "ZLOCATION", "ZNAME", "ZSERVICE_PROVIDER", "ZUNIQUE_ID",
}

const coreDDL = "create table ZCALLRECORD(Z_PK integer primary key, Z_ENT integer, Z_OPT integer, ZANSWERED integer, ZCALLTYPE integer, " +
	"ZDISCONNECTED_CAUSE integer, ZFACE_TIME_DATA integer, ZNUMBER_AVAILABILITY integer, ZORIGINATED integer, ZREAD integer, " +
	"ZJUNKCONFIDENCE integer, ZDATE timestamp, ZDURATION float, ZADDRESS varchar, ZDEVICE_ID varchar, ZISO_COUNTRY_CODE varchar, " +
	"ZLOCATION varchar, ZNAME varchar, ZSERVICE_PROVIDER varchar, ZUNIQUE_ID varchar)"

var legacyCols = []string{
	"ROWID", "address", "date", "duration", "flags", "id", "name", "country_code", "network_code", "read",
	"assisted", "face_time_data", "originalAddress",
}

const legacyDDL = "create table call(ROWID integer primary key autoincrement, address text, date integer, duration integer, flags integer, " +
	"id integer, name text, country_code text, network_code text, read integer, assisted integer, face_time_data blob, originalAddress text)"

type memDB struct {
	b   *sqlitetest.Builder
	tbl *sqlitetest.Table
	cs  []string
}

func newMem(table, ddl string, cols []string) *memDB {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	return &memDB{b: b, tbl: b.CreateTable(table, ddl), cs: cols}
}

func newCore() *memDB   { return newMem("ZCALLRECORD", coreDDL, coreCols) }
func newLegacy() *memDB { return newMem("call", legacyDDL, legacyCols) }

// add inserts row id with the named columns; every other column is NULL.
func (d *memDB) add(id int64, over map[string]any) {
	vals := make([]any, len(d.cs))
	vals[0] = id
	for i, c := range d.cs[1:] {
		vals[i+1] = over[c]
	}
	d.tbl.Insert(id, vals...)
}

func runMem(t *testing.T, data []byte, remote, logical string) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "mem", Files: map[string][]byte{"db": data}}.BundleIOS(h, remote, logical)
	p := ioscalls.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	for _, v := range res.Violations {
		t.Errorf("contract violation: %v", v)
	}
	return res
}

func runCore(t *testing.T, d *memDB) parsertest.Result {
	t.Helper()
	return runMem(t, d.b.Bytes(), remoteCore, logicalCore)
}

func runLegacy(t *testing.T, d *memDB) parsertest.Result {
	t.Helper()
	return runMem(t, d.b.Bytes(), remoteLegacy, logicalLegacy)
}

func callAt(t *testing.T, res parsertest.Result, table string, rowid int64) records.Record {
	t.Helper()
	loc := "sqlite:table=" + table + ";rowid=" + strconv.FormatInt(rowid, 10)
	for _, r := range res.Records {
		if r.Locator == loc {
			return r
		}
	}
	t.Fatalf("no record %s in %d records (warnings %+v)", loc, len(res.Records), res.Warnings)
	return records.Record{}
}

func core(t *testing.T, res parsertest.Result, rowid int64) records.Record {
	t.Helper()
	return callAt(t, res, "ZCALLRECORD", rowid)
}

func legacy(t *testing.T, res parsertest.Result, rowid int64) records.Record {
	t.Helper()
	return callAt(t, res, "call", rowid)
}

func rawField(r records.Record, key string) any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m[key]
}

func unknownOf(r records.Record) map[string]any {
	u, _ := rawField(r, "unknown_columns").(map[string]any)
	return u
}

func has(r records.Record, key string) bool { _, ok := r.Payload[key]; return ok }

var cocoaEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func wantTimeIs(t *testing.T, label string, got *records.Time, want time.Time) {
	t.Helper()
	if got == nil || !got.T.Equal(want) {
		t.Errorf("%s: %v, want %s", label, got, want.Format(time.RFC3339Nano))
	}
}

// The contract table of "Mapping contract ios.calls", row by row, on one fully populated Core Data row.
func TestCallMappingFromTheSpec(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{
		"Z_ENT": int64(2), "Z_OPT": int64(1), "ZANSWERED": int64(1), "ZCALLTYPE": int64(16), "ZDISCONNECTED_CAUSE": int64(3),
		"ZFACE_TIME_DATA": int64(1), "ZNUMBER_AVAILABILITY": int64(0), "ZORIGINATED": int64(1), "ZREAD": int64(1), "ZJUNKCONFIDENCE": int64(2),
		"ZDATE": 700000000.5, "ZDURATION": 125.0, "ZADDRESS": "+15551230001", "ZDEVICE_ID": "DEV-1", "ZISO_COUNTRY_CODE": "us",
		"ZLOCATION": "California", "ZNAME": "Alice", "ZSERVICE_PROVIDER": "com.apple.FaceTime", "ZUNIQUE_ID": "U-1",
	})
	res := runCore(t, d)
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records, warnings %+v", res.Err, len(res.Records), res.Warnings)
	}
	r := core(t, res, 1)
	for k, want := range map[string]any{
		"direction": "out", "outcome": "answered", "address": "+15551230001", "name_cached": "Alice", "country_iso": "us",
		"geo_description": "California", "duration_s": 125.0, "kind": "facetime_audio", "service": "com.apple.FaceTime", "read": true,
	} {
		if r.Payload[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, r.Payload[k], want)
		}
	}
	start := cocoaEpoch.Add(700000000*time.Second + 500*time.Millisecond)
	wantTimeIs(t, "time", r.Time, start)
	wantTimeIs(t, "time_end", r.TimeEnd, start.Add(125*time.Second))
	if r.Summary != "call out answered +15551230001" || r.Type != "call" || r.Body != "" {
		t.Errorf("summary %q type %q body %q", r.Summary, r.Type, r.Body)
	}
	if r.SourcePath != remoteCore || r.ArtifactID == "" || r.Range == nil {
		t.Errorf("place: source_path %q artifact %q range %v", r.SourcePath, r.ArtifactID, r.Range)
	}
	// the raw columns of the contract; ZREAD and a plain ZDURATION are in payload.read / duration_s only
	for k, want := range map[string]any{
		"ZORIGINATED": int64(1), "ZANSWERED": int64(1), "ZCALLTYPE": int64(16), "ZDISCONNECTED_CAUSE": int64(3), "ZNUMBER_AVAILABILITY": int64(0),
		"ZFACE_TIME_DATA": int64(1), "ZJUNKCONFIDENCE": int64(2), "ZUNIQUE_ID": "U-1", "ZSERVICE_PROVIDER": "com.apple.FaceTime", "ZDATE": 700000000.5,
	} {
		if rawField(r, k) != want {
			t.Errorf("raw[%s] = %v, want %v", k, rawField(r, k), want)
		}
	}
	for _, k := range []string{"ZREAD", "ZDURATION", "Z_ENT", "Z_OPT", "ZDEVICE_ID", "ZDATE_status", "unknown_columns", "row_flags", "text_flags"} {
		if rawField(r, k) != nil {
			t.Errorf("raw[%s] = %v, want absent", k, rawField(r, k))
		}
	}
}

func TestCoreDirectionAndOutcome(t *testing.T) {
	cases := []struct {
		id            int64
		orig, ans     any
		direction, oc string
	}{
		{1, int64(0), int64(1), "in", "answered"},
		{2, int64(0), int64(0), "in", "missed"},
		{3, int64(1), int64(1), "out", "answered"},
		{4, int64(1), int64(0), "out", "unknown"},
		{5, int64(2), int64(1), "unknown", "unknown"},
		{6, int64(0), int64(2), "in", "unknown"},
		{7, nil, int64(1), "unknown", "unknown"},
		{8, int64(0), nil, "in", "unknown"},
		{9, int64(1), nil, "out", "unknown"},
	}
	d := newCore()
	for _, c := range cases {
		d.add(c.id, map[string]any{"ZORIGINATED": c.orig, "ZANSWERED": c.ans, "ZDATE": 700000000.0})
	}
	res := runCore(t, d)
	for _, c := range cases {
		r := core(t, res, c.id)
		if r.Payload["direction"] != c.direction || r.Payload["outcome"] != c.oc {
			t.Errorf("row %d: %v/%v, want %s/%s", c.id, r.Payload["direction"], r.Payload["outcome"], c.direction, c.oc)
		}
	}
}

func TestCoreCallTypeKinds(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZCALLTYPE": int64(1)})
	d.add(2, map[string]any{"ZCALLTYPE": int64(8)})
	d.add(3, map[string]any{"ZCALLTYPE": int64(16)})
	d.add(4, map[string]any{"ZCALLTYPE": int64(99)})
	d.add(5, map[string]any{"ZCALLTYPE": nil})
	d.add(6, map[string]any{"ZCALLTYPE": "voice"})
	res := runCore(t, d)
	for id, want := range map[int64]any{1: "voice", 2: "facetime_video", 3: "facetime_audio", 4: "unknown"} {
		if got := core(t, res, id).Payload["kind"]; got != want {
			t.Errorf("row %d: kind %v, want %v", id, got, want)
		}
	}
	if r := core(t, res, 5); has(r, "kind") || unknownOf(r) != nil {
		t.Errorf("a NULL ZCALLTYPE has no kind and no flag: %v", r.Payload)
	}
	if r := core(t, res, 6); has(r, "kind") || unknownOf(r)["ZCALLTYPE"] != "type-mismatch" {
		t.Errorf("a TEXT ZCALLTYPE is unknown, never a kind: %v", r.Payload)
	}
}

// 4E-R35: an outgoing call with ZANSWERED 0 covers busy, failed and not answered; it is unknown, never cancelled,
// and the values that would decide it are kept.
func TestOutgoingUnansweredCallIsUnknownNotCancelled(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZORIGINATED": int64(1), "ZANSWERED": int64(0), "ZDURATION": 0.0, "ZDATE": 700000000.0})
	d.add(2, map[string]any{"ZORIGINATED": int64(1), "ZANSWERED": int64(0), "ZDURATION": 0.0, "ZDISCONNECTED_CAUSE": int64(7), "ZDATE": 700000000.0})
	d.add(3, map[string]any{"ZORIGINATED": int64(1), "ZANSWERED": int64(0), "ZDURATION": 12.5, "ZDATE": 700000000.0})
	d.add(4, map[string]any{"ZORIGINATED": int64(1), "ZANSWERED": int64(0), "ZDATE": 700000000.0})
	res := runCore(t, d)
	if len(res.Records) != 4 {
		t.Fatalf("%d records", len(res.Records))
	}
	for _, r := range res.Records {
		if r.Payload["outcome"] != "unknown" || r.Payload["direction"] != "out" {
			t.Errorf("%s: %v/%v, want out/unknown", r.Locator, r.Payload["direction"], r.Payload["outcome"])
		}
		if rawField(r, "ZANSWERED") != int64(0) {
			t.Errorf("%s: raw.ZANSWERED %v", r.Locator, rawField(r, "ZANSWERED"))
		}
	}
	if rawField(core(t, res, 1), "ZDURATION") != 0.0 || rawField(core(t, res, 2), "ZDISCONNECTED_CAUSE") != int64(7) ||
		rawField(core(t, res, 3), "ZDURATION") != 12.5 {
		t.Errorf("the deciding values were not kept: %v %v %v", core(t, res, 1).Payload, core(t, res, 2).Payload, core(t, res, 3).Payload)
	}
	if rawField(core(t, res, 4), "ZDURATION") != nil || has(core(t, res, 4), "duration_s") {
		t.Errorf("a NULL duration was invented: %v", core(t, res, 4).Payload)
	}
}

func TestCoreDurationRules(t *testing.T) {
	const date = 700000000.0
	start := cocoaEpoch.Add(700000000 * time.Second)
	d := newCore()
	cases := []struct {
		id  int64
		dur any
	}{
		{1, 0.0},
		{2, 12.5},
		{3, 1e9},
		{4, 1e9 + 1},
		{5, -3.0},
		{6, math.Inf(1)},
		{7, math.Inf(-1)},
		{8, int64(7)},
		{10, "ten"},
		{11, nil},
		{12, -7.0},
		{13, math.Copysign(0, -1)},
	}
	for _, c := range cases {
		d.add(c.id, map[string]any{"ZDATE": date, "ZDURATION": c.dur})
	}
	res := runCore(t, d)
	// finite and non-negative: the value, and an end only for 0 < duration <= 1e9
	for id, want := range map[int64]float64{1: 0, 2: 12.5, 3: 1e9, 4: 1e9 + 1, 8: 7, 13: 0} {
		r := core(t, res, id)
		if r.Payload["duration_s"] != want || rawField(r, "ZDURATION") != nil {
			t.Errorf("row %d: duration_s %v raw %v, want %v", id, r.Payload["duration_s"], r.Payload["raw"], want)
		}
	}
	wantTimeIs(t, "row 2 end", core(t, res, 2).TimeEnd, start.Add(12*time.Second+500*time.Millisecond))
	wantTimeIs(t, "row 3 end (the 1e9 bound is inclusive)", core(t, res, 3).TimeEnd, start.Add(1e9*time.Second))
	wantTimeIs(t, "row 8 end", core(t, res, 8).TimeEnd, start.Add(7*time.Second))
	for _, id := range []int64{1, 4, 13} {
		if core(t, res, id).TimeEnd != nil {
			t.Errorf("row %d has an end time", id)
		}
	}
	// negative: no duration_s, the value kept with a status
	for id, want := range map[int64]any{5: -3.0, 12: -7.0} {
		r := core(t, res, id)
		if has(r, "duration_s") || rawField(r, "ZDURATION") != want || rawField(r, "ZDURATION_status") != "negative" || r.TimeEnd != nil {
			t.Errorf("row %d: %v", id, r.Payload)
		}
	}
	// non-finite (P-4-1): IEEE bits and a status, never a ZDURATION key
	for id, bits := range map[int64]string{6: "7ff0000000000000", 7: "fff0000000000000"} {
		r := core(t, res, id)
		if has(r, "duration_s") || rawField(r, "ZDURATION_ieee") != bits || rawField(r, "ZDURATION_status") != "invalid" || rawField(r, "ZDURATION") != nil {
			t.Errorf("row %d: %v", id, r.Payload)
		}
	}
	if r := core(t, res, 10); has(r, "duration_s") || unknownOf(r)["ZDURATION"] != "type-mismatch" || rawField(r, "ZDURATION") != nil {
		t.Errorf("row 10: %v", r.Payload)
	}
	if r := core(t, res, 11); has(r, "duration_s") || unknownOf(r) != nil || rawField(r, "ZDURATION") != nil {
		t.Errorf("row 11: %v", r.Payload)
	}
}

func TestCoreTimes(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZDATE": 700000000.5})
	d.add(2, map[string]any{"ZDATE": int64(700000000)})
	d.add(3, map[string]any{"ZDATE": int64(0)})
	d.add(4, map[string]any{"ZDATE": int64(-1)})
	d.add(5, map[string]any{"ZDATE": 3137184000.0})
	d.add(6, map[string]any{"ZDATE": "yesterday"})
	d.add(7, map[string]any{"ZDATE": nil})
	d.add(8, map[string]any{"ZDATE": math.Inf(1), "ZDURATION": 30.0})
	d.add(9, map[string]any{"ZDATE": int64(0), "ZDURATION": 30.0})
	res := runCore(t, d)
	wantTimeIs(t, "row 1", core(t, res, 1).Time, cocoaEpoch.Add(700000000*time.Second+500*time.Millisecond))
	wantTimeIs(t, "row 2", core(t, res, 2).Time, cocoaEpoch.Add(700000000*time.Second))
	for id, st := range map[int64]string{3: "sentinel", 4: "sentinel", 5: "invalid"} {
		r := core(t, res, id)
		if r.Time != nil || r.TimeEnd != nil || rawField(r, "ZDATE_status") != st || rawField(r, "ZDATE") == nil {
			t.Errorf("row %d: time %v raw %v", id, r.Time, r.Payload["raw"])
		}
	}
	if r := core(t, res, 6); r.Time != nil || rawField(r, "ZDATE") != nil || unknownOf(r)["ZDATE"] != "type-mismatch" {
		t.Errorf("row 6 (TEXT date): %v", r.Payload)
	}
	if r := core(t, res, 7); r.Time != nil || rawField(r, "ZDATE") != nil || unknownOf(r) != nil {
		t.Errorf("row 7 (NULL date): %v", r.Payload)
	}
	if r := core(t, res, 8); r.Time != nil || r.TimeEnd != nil || rawField(r, "ZDATE_ieee") != "7ff0000000000000" ||
		rawField(r, "ZDATE_status") != "invalid" || rawField(r, "ZDATE") != nil {
		t.Errorf("row 8 (+Inf date): %v", r.Payload)
	}
	if r := core(t, res, 9); r.Time != nil || r.TimeEnd != nil || r.Payload["duration_s"] != 30.0 {
		t.Errorf("row 9: a sentinel date has no end time but keeps the duration: %v", r.Payload)
	}
}

func TestCoreAddressForms(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZADDRESS": "+15551230001"})
	d.add(2, map[string]any{"ZADDRESS": []byte("+15551230002")})
	d.add(3, map[string]any{"ZADDRESS": []byte("+1555\xff\xfe")})
	d.add(4, map[string]any{"ZADDRESS": nil})
	d.add(5, map[string]any{"ZADDRESS": ""})
	d.add(6, map[string]any{"ZADDRESS": []byte{}})
	d.add(7, map[string]any{"ZADDRESS": int64(5550007)})
	d.add(8, map[string]any{"ZADDRESS": "a\x00b"})
	res := runCore(t, d)
	if r := core(t, res, 1); r.Payload["address"] != "+15551230001" || rawField(r, "address_storage") != nil || rawField(r, "address_state") != nil {
		t.Errorf("row 1: %v", r.Payload)
	}
	if r := core(t, res, 2); r.Payload["address"] != "+15551230002" || rawField(r, "address_storage") != "blob" {
		t.Errorf("row 2: %v", r.Payload)
	}
	r3 := core(t, res, 3)
	if r3.Payload["address"] != "+1555��" || rawField(r3, "address_storage") != "blob" || rawField(r3, "address_raw_b64") != "KzE1NTX//g==" {
		t.Errorf("row 3: %v", r3.Payload)
	}
	if fl, _ := rawField(r3, "address_flags").([]any); len(fl) != 1 || fl[0] != "invalid_utf8" {
		t.Errorf("row 3 flags %v", rawField(r3, "address_flags"))
	}
	if r := core(t, res, 4); has(r, "address") || rawField(r, "address_state") != nil || unknownOf(r) != nil {
		t.Errorf("row 4 (NULL): %v", r.Payload)
	}
	if r := core(t, res, 5); has(r, "address") || rawField(r, "address_state") != "empty" {
		t.Errorf("row 5 (empty text): %v", r.Payload)
	}
	if r := core(t, res, 6); has(r, "address") || rawField(r, "address_state") != "empty" || rawField(r, "address_storage") != "blob" {
		t.Errorf("row 6 (empty blob): %v", r.Payload)
	}
	if r := core(t, res, 7); has(r, "address") || unknownOf(r)["ZADDRESS"] != "type-mismatch" {
		t.Errorf("row 7 (INTEGER): %v", r.Payload)
	}
	if fl, _ := rawField(core(t, res, 8), "address_flags").([]any); core(t, res, 8).Payload["address"] != "a�b" || len(fl) == 0 {
		t.Errorf("row 8 (NUL): %v", core(t, res, 8).Payload)
	}
}

func TestCoreReadAndTextColumns(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZREAD": int64(0)})
	d.add(2, map[string]any{"ZREAD": int64(1)})
	d.add(3, map[string]any{"ZREAD": int64(2)})
	d.add(4, map[string]any{"ZREAD": nil})
	d.add(5, map[string]any{"ZREAD": "1", "ZNAME": int64(5), "ZISO_COUNTRY_CODE": []byte{1}, "ZSERVICE_PROVIDER": 1.5, "ZUNIQUE_ID": []byte("x")})
	d.add(6, map[string]any{"ZNAME": "Bob\xff", "ZLOCATION": "", "ZUNIQUE_ID": "id\xfe", "ZSERVICE_PROVIDER": "s\xff"})
	res := runCore(t, d)
	if r := core(t, res, 1); r.Payload["read"] != false {
		t.Errorf("row 1: %v", r.Payload)
	}
	if r := core(t, res, 2); r.Payload["read"] != true {
		t.Errorf("row 2: %v", r.Payload)
	}
	if r := core(t, res, 3); has(r, "read") || rawField(r, "ZREAD") != int64(2) {
		t.Errorf("row 3 (ZREAD 2 is neither read nor unread): %v", r.Payload)
	}
	if r := core(t, res, 4); has(r, "read") {
		t.Errorf("row 4: a NULL ZREAD became %v", r.Payload["read"])
	}
	r5 := core(t, res, 5)
	u := unknownOf(r5)
	for _, k := range []string{"ZREAD", "ZNAME", "ZISO_COUNTRY_CODE", "ZSERVICE_PROVIDER", "ZUNIQUE_ID"} {
		if u[k] != "type-mismatch" {
			t.Errorf("row 5: unknown_columns[%s] = %v", k, u[k])
		}
	}
	for _, k := range []string{"name_cached", "country_iso", "service", "read"} {
		if has(r5, k) {
			t.Errorf("row 5 invented %s = %v", k, r5.Payload[k])
		}
	}
	r6 := core(t, res, 6)
	if r6.Payload["name_cached"] != "Bob�" || has(r6, "geo_description") {
		t.Errorf("row 6: %v", r6.Payload)
	}
	fl, _ := rawField(r6, "text_flags").([]any)
	if len(fl) != 3 || fl[0] != "ZUNIQUE_ID" || fl[1] != "name_cached" || fl[2] != "service" {
		t.Errorf("row 6: text_flags %v, want the altered fields sorted", rawField(r6, "text_flags"))
	}
}

// A cell of the wrong storage class is unknown, never a zero, a default or an absent column.
func TestWrongStorageClassIsUnknownNotZero(t *testing.T) {
	d := newCore()
	d.add(1, map[string]any{"ZDATE": "yesterday", "ZDURATION": "ten", "ZORIGINATED": 1.5, "ZANSWERED": "yes", "ZCALLTYPE": []byte{1}})
	res := runCore(t, d)
	r := core(t, res, 1)
	if r.Time != nil || has(r, "duration_s") || has(r, "kind") || r.Payload["direction"] != "unknown" || r.Payload["outcome"] != "unknown" {
		t.Errorf("invented values: %v", r.Payload)
	}
	u := unknownOf(r)
	for _, k := range []string{"ZDATE", "ZDURATION", "ZORIGINATED", "ZANSWERED", "ZCALLTYPE"} {
		if u[k] != "type-mismatch" {
			t.Errorf("unknown_columns[%s] = %v, want type-mismatch", k, u[k])
		}
		if rawField(r, k) != nil {
			t.Errorf("raw[%s] = %v: a value of the wrong class is not stated as one", k, rawField(r, k))
		}
	}
}

func TestLegacyMappingFromTheSpec(t *testing.T) {
	d := newLegacy()
	d.add(1, map[string]any{
		"address": "+15557770001", "date": int64(1682935200), "duration": int64(125), "flags": int64(1769472), "id": int64(9), "name": "Alice",
		"country_code": "us", "network_code": "310", "read": int64(1), "assisted": int64(0), "face_time_data": []byte{1, 2, 3}, "originalAddress": "5557770001",
	})
	d.add(2, map[string]any{"date": int64(1682935200), "duration": int64(-4), "read": int64(0)})
	d.add(3, map[string]any{"date": 1682935200.5, "duration": 30.5})
	d.add(4, map[string]any{"date": int64(0), "duration": int64(30), "address": "", "read": int64(3)})
	d.add(5, map[string]any{"date": "x", "duration": "y", "flags": "z", "face_time_data": int64(1), "address": int64(5)})
	res := runLegacy(t, d)
	r := legacy(t, res, 1)
	for k, want := range map[string]any{
		"direction": "unknown", "outcome": "unknown", "address": "+15557770001", "name_cached": "Alice", "country_iso": "us", "duration_s": 125.0, "read": true,
	} {
		if r.Payload[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, r.Payload[k], want)
		}
	}
	if has(r, "kind") || has(r, "service") {
		t.Errorf("the legacy table has no kind or service: %v", r.Payload)
	}
	start := time.Unix(1682935200, 0).UTC()
	wantTimeIs(t, "time", r.Time, start)
	wantTimeIs(t, "time_end", r.TimeEnd, start.Add(125*time.Second))
	if r.Summary != "call unknown unknown +15557770001" || r.SourcePath != remoteLegacy {
		t.Errorf("summary %q source_path %q", r.Summary, r.SourcePath)
	}
	for k, want := range map[string]any{
		"flags": int64(1769472), "id": int64(9), "assisted": int64(0), "network_code": "310", "originalAddress": "5557770001",
		"face_time_data_bytes": int64(3), "date": int64(1682935200),
	} {
		if rawField(r, k) != want {
			t.Errorf("raw[%s] = %v, want %v", k, rawField(r, k), want)
		}
	}
	for _, k := range []string{"read", "ROWID", "duration", "name", "country_code", "face_time_data", "date_status"} {
		if rawField(r, k) != nil {
			t.Errorf("raw[%s] = %v, want absent", k, rawField(r, k))
		}
	}
	if r2 := legacy(t, res, 2); has(r2, "duration_s") || rawField(r2, "duration") != int64(-4) || rawField(r2, "duration_status") != "negative" || r2.TimeEnd != nil || r2.Payload["read"] != false {
		t.Errorf("row 2: %v", r2.Payload)
	}
	if r3 := legacy(t, res, 3); r3.Time != nil || rawField(r3, "date_status") != "invalid" || rawField(r3, "date") != 1682935200.5 || r3.Payload["duration_s"] != 30.5 {
		t.Errorf("row 3 (fractional REAL date): %v", r3.Payload)
	}
	if r4 := legacy(t, res, 4); r4.Time != nil || rawField(r4, "date_status") != "sentinel" || has(r4, "address") || rawField(r4, "address_state") != "empty" || has(r4, "read") || rawField(r4, "read") != int64(3) {
		t.Errorf("row 4: %v", r4.Payload)
	}
	r5 := legacy(t, res, 5)
	u := unknownOf(r5)
	for _, k := range []string{"date", "duration", "flags", "face_time_data", "address"} {
		if u[k] != "type-mismatch" {
			t.Errorf("row 5: unknown_columns[%s] = %v", k, u[k])
		}
	}
	if r5.Time != nil || has(r5, "duration_s") || has(r5, "address") || r5.Payload["direction"] != "unknown" {
		t.Errorf("row 5 invented values: %v", r5.Payload)
	}
}

// A database that holds both tables (hostile or a migration) is read in both, each under its own rules.
func TestBothTablesAreReadEachUnderItsOwnRules(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("ZCALLRECORD", coreDDL).Insert(1, append([]any{int64(1)}, make([]any, len(coreCols)-1)...)...)
	b.CreateTable("call", legacyDDL).Insert(1, append([]any{int64(1)}, make([]any, len(legacyCols)-1)...)...)
	res := runMem(t, b.Bytes(), remoteCore, logicalCore)
	if len(res.Records) != 2 {
		t.Fatalf("%d records", len(res.Records))
	}
	c, l := core(t, res, 1), legacy(t, res, 1)
	if c.Payload["direction"] != "unknown" || l.Payload["direction"] != "unknown" {
		t.Errorf("%v %v", c.Payload, l.Payload)
	}
	if res.Notes["table.call"] != "" || res.Notes["table.ZCALLRECORD"] != "" {
		t.Errorf("notes %v: both tables exist", res.Notes)
	}
}

// 4E-R30: a WAL is present, so Probe deferred; Parse finds neither table and ends like the other
// identifying parsers: one warning, the note, no record, no error.
func TestCallWithNeitherTableAfterADeferredProbeIsNotApplicable(t *testing.T) {
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("other", "create table other(id integer primary key, x text)")
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "none", Files: map[string][]byte{"db": bld.Bytes(), "wal": {}}}.BundleIOS(h, remoteCore, logicalCore)
	p := ioscalls.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.Applicable {
		t.Fatalf("Probe = %+v, %v; with a WAL the decision is deferred to Parse", got, err)
	}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil || len(res.Records) != 0 || res.Notes["not_applicable"] == "" {
		t.Fatalf("err %v, %d records, notes %v", res.Err, len(res.Records), res.Notes)
	}
	var warned int
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "not applicable at parse time") {
			warned++
		}
	}
	if warned != 1 || len(res.Warnings) != 1 {
		t.Errorf("want exactly one audited warning: %+v", res.Warnings)
	}
}

// With one table present and the other absent, the absent one is a note, never a warning or an error.
func TestAbsentAlternativeTableIsANoteNotAnError(t *testing.T) {
	d := newLegacy()
	d.add(1, map[string]any{"date": int64(1682935200)})
	res := runLegacy(t, d)
	if res.Err != nil || len(res.Warnings) != 0 || res.Notes["table.ZCALLRECORD"] != "absent" || res.Notes["not_applicable"] != "" {
		t.Errorf("err %v warnings %+v notes %v", res.Err, res.Warnings, res.Notes)
	}
}

// An INTEGER duration beyond 2^53 is not a float64: duration_s is not stated (unknown "precision"), the integer
// stays in raw, and there is no end time; 2^53 itself is exact.
func TestIntegerDurationBeyondFloatPrecisionIsUnknownNotRounded(t *testing.T) {
	const exact = int64(1) << 53
	d := newLegacy()
	for id, dur := range map[int64]int64{1: exact, 2: exact + 1, 3: -(exact + 1), 4: math.MaxInt64} {
		d.add(id, map[string]any{"date": int64(1682935200), "duration": dur})
	}
	res := runLegacy(t, d)
	if r := legacy(t, res, 1); r.Payload["duration_s"] != float64(exact) || unknownOf(r) != nil {
		t.Errorf("2^53 is exact and stated: %v", r.Payload)
	}
	for id, want := range map[int64]int64{2: exact + 1, 4: math.MaxInt64} {
		r := legacy(t, res, id)
		if has(r, "duration_s") || unknownOf(r)["duration"] != "precision" || rawField(r, "duration") != want || r.TimeEnd != nil {
			t.Errorf("row %d: %v", id, r.Payload)
		}
	}
	if r := legacy(t, res, 3); has(r, "duration_s") || rawField(r, "duration_status") != "negative" || rawField(r, "duration") != -(exact+1) {
		t.Errorf("row 3: %v", r.Payload)
	}
}
