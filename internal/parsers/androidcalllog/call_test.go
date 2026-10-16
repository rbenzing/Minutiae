package androidcalllog_test

import (
	"math"
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
	if len(res.Records) != 30 {
		t.Fatalf("%d records, want 30", len(res.Records))
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
	// T61: a BLOB subscription_id is unknown, not dropped silently
	if r := call(t, res, "26"); field(r, "subscription") != nil || unknownColumns(r)["subscription_id"] != "type-mismatch" {
		t.Errorf("row 26: %v", r.Payload)
	}
	// T62: 2^53+1 is not exactly a float64
	if r := call(t, res, "27"); field(r, "duration_s") != nil || unknownColumns(r)["duration"] != "precision" ||
		raw(r, "duration") != int64(1<<53+1) || r.TimeEnd != nil {
		t.Errorf("row 27: %v", r.Payload)
	}
	// T63: only ASCII decimal text is a subscription (Go's ParseInt), and at most 32 bytes
	for _, id := range []string{"28", "29", "30"} {
		if r := call(t, res, id); field(r, "subscription") != nil || raw(r, "subscription_id") == nil {
			t.Errorf("row %s: %v", id, r.Payload)
		}
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

// T61: presentation, features and subscription_id of another storage class, and a number that is
// an integer, are unknown and named in raw.unknown_columns; nothing is invented from them.
func TestOtherStorageClassesOfTheOptionalColumnsAreUnknown(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("calls", "create table calls(_id integer primary key, number text, date integer, duration integer, type integer, presentation integer, features integer, subscription_id text)")
	base := int64(1682935200000)
	tbl.Insert(1, int64(1), "+15550001", base, int64(5), int64(1), 2.5, "video", 1.5)
	tbl.Insert(2, int64(2), "+15550002", base, int64(5), int64(1), int64(1), int64(0), []byte{1, 2, 3})
	tbl.Insert(3, int64(3), int64(5550003), base, int64(5), int64(1), int64(1), int64(0), int64(3))
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "other", Files: map[string][]byte{"db": b.Bytes()}}.Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	res := h.RunLenient(p, bundle.Input(h, p, false))
	if res.Err != nil || len(res.Records) != 3 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	r1 := call(t, res, "1")
	u1 := unknownColumns(r1)
	if u1["presentation"] != "type-mismatch" || u1["features"] != "type-mismatch" || u1["subscription_id"] != "type-mismatch" {
		t.Errorf("row 1: unknown_columns %v", u1)
	}
	if _, has := r1.Payload["address_presentation"]; has || field(r1, "kind") != nil || field(r1, "subscription") != nil {
		t.Errorf("row 1 invented a value: %v", r1.Payload)
	}
	r2 := call(t, res, "2")
	if u := unknownColumns(r2); u["subscription_id"] != "type-mismatch" || field(r2, "subscription") != nil {
		t.Errorf("row 2 (BLOB subscription_id): %v", r2.Payload)
	}
	r3 := call(t, res, "3")
	if u := unknownColumns(r3); u["number"] != "type-mismatch" {
		t.Errorf("row 3 (INTEGER number): unknown_columns %v", u)
	}
	if _, has := r3.Payload["address"]; has || r3.Summary != "call in answered" {
		t.Errorf("row 3 invented an address: %v summary %q", r3.Payload, r3.Summary)
	}
	if field(r3, "subscription") != int64(3) {
		t.Errorf("row 3: an INTEGER subscription_id is a subscription, got %v", field(r3, "subscription"))
	}
}

// T62: a duration beyond 2^53 is not exactly a float64, so duration_s is not stated: the column
// is unknown ("precision"), its integer stays in raw.duration, and there is no end time.
func TestDurationBeyondFloatPrecisionIsUnknownNotRounded(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("calls", "create table calls(_id integer primary key, number text, date integer, duration integer, type integer)")
	base := int64(1682935200000)
	const exact = int64(1) << 53
	tbl.Insert(1, int64(1), "+1", base, exact, int64(1))
	tbl.Insert(2, int64(2), "+2", base, exact+1, int64(1))
	tbl.Insert(3, int64(3), "+3", base, -(exact + 1), int64(2))
	tbl.Insert(4, int64(4), "+4", base, int64(math.MaxInt64), int64(2))
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "prec", Files: map[string][]byte{"db": b.Bytes()}}.Bundle(h, remoteCalllog)
	p := androidcalllog.Parser{}
	res := h.RunLenient(p, bundle.Input(h, p, false))
	if res.Err != nil || len(res.Records) != 4 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	if r := call(t, res, "1"); field(r, "duration_s") != float64(exact) || raw(r, "unknown_columns") != nil {
		t.Errorf("2^53 is exact and stated: %v", r.Payload)
	}
	for id, want := range map[string]int64{"2": exact + 1, "3": -(exact + 1), "4": math.MaxInt64} {
		r := call(t, res, id)
		u := unknownColumns(r)
		if field(r, "duration_s") != nil || u["duration"] != "precision" || raw(r, "duration") != want || r.TimeEnd != nil {
			t.Errorf("row %s: payload %v end %v", id, r.Payload, r.TimeEnd)
		}
	}
	if r := call(t, res, "2"); field(r, "outcome") != "answered" || r.Time == nil {
		t.Errorf("row 2: the direction and outcome do not depend on the magnitude: %v", r.Payload)
	}
}

// unknownColumns is raw.unknown_columns, or an empty map when the record has none.
func unknownColumns(r records.Record) map[string]any {
	u, _ := raw(r, "unknown_columns").(map[string]any)
	return u
}
