package androidcalllog_test

import (
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func featuresRun(t *testing.T) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "features-subid").Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

func call(t *testing.T, res parsertest.Result, rowid string) records.Record {
	t.Helper()
	loc := "sqlite:table=calls;rowid=" + rowid
	for _, r := range res.Records {
		if r.Locator == loc {
			return r
		}
	}
	t.Fatalf("no record %s", loc)
	return records.Record{}
}

func field(r records.Record, key string) any { return r.Payload[key] }

func raw(r records.Record, key string) any { return r.Payload["raw"].(map[string]any)[key] }

func TestCallMappingFromTheSpec(t *testing.T) {
	res := featuresRun(t)
	if len(res.Records) != 25 {
		t.Fatalf("%d records, want 25", len(res.Records))
	}
	one := call(t, res, "1")
	start := time.Unix(1682935200+3600, 0).UTC()
	if one.Time == nil || !one.Time.T.Equal(start) || one.TimeEnd == nil || !one.TimeEnd.T.Equal(start.Add(125*time.Second)) {
		t.Errorf("row 1 times: %v %v", one.Time, one.TimeEnd)
	}
	if field(one, "direction") != "in" || field(one, "outcome") != "answered" || field(one, "address") != "+15551230001" ||
		field(one, "name_cached") != "Alice" || field(one, "country_iso") != "US" || field(one, "geo_description") != "California" ||
		field(one, "duration_s") != float64(125) || field(one, "kind") != "voice" || field(one, "read") != true {
		t.Errorf("row 1 payload: %v", one.Payload)
	}
	if one.Summary != "call in answered +15551230001" || one.Body != "" {
		t.Errorf("row 1 summary %q body %q", one.Summary, one.Body)
	}
	// T48: the record names the file that holds its bytes
	for _, r := range res.Records {
		if r.SourcePath != remoteCalllog || r.ArtifactID == "" {
			t.Fatalf("%s: SourcePath %q artifact %q", r.Locator, r.SourcePath, r.ArtifactID)
		}
	}
	// outgoing: answered only when it lasted, cancelled at duration 0, unknown without a duration
	if field(call(t, res, "2"), "outcome") != "answered" || field(call(t, res, "3"), "outcome") != "cancelled" ||
		field(call(t, res, "17"), "outcome") != "unknown" {
		t.Error("outgoing outcomes")
	}
	if call(t, res, "3").TimeEnd != nil {
		t.Error("a zero-duration call has an end time")
	}
	for id, want := range map[string]string{"4": "missed", "5": "voicemail", "6": "rejected", "7": "blocked", "8": "answered"} {
		if got := field(call(t, res, id), "outcome"); got != want || field(call(t, res, id), "direction") != "in" {
			t.Errorf("type of row %s: outcome %v", id, got)
		}
	}
	if raw(call(t, res, "8"), "type") != int64(7) {
		t.Error("the raw type 7 (answered externally) was not kept")
	}
	// unknown is never absent, zero or a default
	if r := call(t, res, "19"); field(r, "direction") != "unknown" || field(r, "outcome") != "unknown" || raw(r, "type") != int64(99) {
		t.Errorf("row 19 (type 99): %v", r.Payload)
	}
	if r := call(t, res, "17"); field(r, "duration_s") != nil || raw(r, "duration") != nil {
		t.Errorf("row 17: a NULL duration became %v", r.Payload)
	}
	// hidden numbers: no invented address
	for _, id := range []string{"9", "10"} {
		if _, has := call(t, res, id).Payload["address"]; has {
			t.Errorf("row %s has an address", id)
		}
	}
	if field(call(t, res, "9"), "address_presentation") != "restricted" || field(call(t, res, "11"), "address_presentation") != "payphone" {
		t.Error("presentation")
	}
	// times through ts: sentinels and invalid values have no time, the raw value stays
	for id, st := range map[string]string{"12": "invalid", "13": "sentinel", "14": "sentinel", "15": "sentinel"} {
		r := call(t, res, id)
		if r.Time != nil || r.TimeEnd != nil || raw(r, "date_status") != st {
			t.Errorf("row %s: time %v status %v", id, r.Time, raw(r, "date_status"))
		}
	}
	if r := call(t, res, "16"); field(r, "duration_s") != nil || raw(r, "duration") != int64(-5) {
		t.Errorf("a negative duration became %v", field(r, "duration_s"))
	}
	if field(call(t, res, "18"), "kind") != "video" {
		t.Error("features&1 is video")
	}
	if r := call(t, res, "20"); field(r, "subscription") != nil || raw(r, "subscription_id") != "89014103211118510720" {
		t.Errorf("row 20 subscription: %v", r.Payload)
	}
	if field(call(t, res, "21"), "subscription") != int64(2) {
		t.Error("a decimal subscription_id text is a subscription")
	}
	if r := call(t, res, "22"); raw(r, "number_flags") == nil || raw(r, "number_raw_b64") == nil {
		t.Errorf("row 22: invalid UTF-8 in the number was not flagged: %v", r.Payload)
	}
	if r := call(t, res, "24"); r.Time == nil || r.TimeEnd != nil {
		t.Errorf("row 24: an end time beyond 2100 must be omitted, got %v", r.TimeEnd)
	}
	if r := call(t, res, "23"); field(r, "duration_s") != float64(4_000_000_000) || r.TimeEnd != nil {
		t.Errorf("row 23: a 4e9 s call is kept but has no end time: %v", r.TimeEnd)
	}
}

// Cells of the wrong storage class are unknown, never a zero, a default or an absent column:
// a NULL or text type has direction and outcome unknown, a text duration has no duration_s and
// a non-integer date has no time, each named in raw.unknown_columns.
func TestWrongStorageClassIsUnknownNotZero(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("calls", "create table calls(_id integer primary key, number text, date integer, duration integer, type integer)")
	tbl.Insert(1, int64(1), "+15550001", int64(1682935200000), "ten", "two")
	tbl.Insert(2, int64(2), "+15550002", "yesterday", nil, nil)
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "wrong", Files: map[string][]byte{"db": b.Bytes()}}.Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	res := h.RunLenient(p, bundle.Input(h, p, false))
	if res.Err != nil || len(res.Records) != 2 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	for _, id := range []string{"1", "2"} {
		r := call(t, res, id)
		if field(r, "direction") != "unknown" || field(r, "outcome") != "unknown" || field(r, "duration_s") != nil {
			t.Errorf("row %s: %v", id, r.Payload)
		}
	}
	u1 := raw(call(t, res, "1"), "unknown_columns").(map[string]any)
	if u1["duration"] != "type-mismatch" || u1["type"] != "type-mismatch" || call(t, res, "1").Time == nil {
		t.Errorf("row 1: unknown_columns %v", u1)
	}
	r2 := call(t, res, "2")
	u2 := raw(r2, "unknown_columns").(map[string]any)
	if u2["date"] != "type-mismatch" || r2.Time != nil || raw(r2, "date") != nil {
		t.Errorf("row 2: unknown_columns %v time %v", u2, r2.Time)
	}
}
