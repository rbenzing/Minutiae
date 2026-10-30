package ioscalls_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// t0 is 2023-05-01T10:00:00Z, the instant of the scenario's row 0; the rows are an hour apart.
var t0 = time.Date(2023, 5, 1, 10, 0, 0, 0, time.UTC)

func rawOf(r parsertest.ExpRecord) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func byRowid(t *testing.T, variant string) map[int64]parsertest.ExpRecord {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		if r.Type != "call" {
			t.Errorf("%s rowid %d: type %q", variant, r.Rowid, r.Type)
		}
		out[r.Rowid] = r
	}
	return out
}

func num(t *testing.T, v any) string {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("%v is not a JSON number", v)
	}
	return n.String()
}

func wantTime(t *testing.T, label string, got *string, want time.Time) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: no time, want %s", label, want.Format(time.RFC3339Nano))
		return
	}
	g, err := time.Parse(time.RFC3339Nano, *got)
	if err != nil || !g.Equal(want) {
		t.Errorf("%s: time %q, want %s", label, *got, want.Format(time.RFC3339Nano))
	}
}

// The spec facts of the Core Data scenario, written from the rules (4E-R22, R35 and the contract table),
// not read back from the generator.
func TestOracleStatesTheSpecFacts(t *testing.T) {
	rows := byRowid(t, "coredata-current")
	if len(rows) != 20 {
		t.Fatalf("%d records, the scenario writes 20 rows", len(rows))
	}
	// 4E-R35: an outgoing call with ZANSWERED 0 is "unknown" (busy, failed, not answered), never "cancelled".
	for _, id := range []int64{4, 5} {
		r := rows[id]
		if r.Payload["direction"] != "out" || r.Payload["outcome"] != "unknown" {
			t.Errorf("row %d: direction %v outcome %v, want out and unknown", id, r.Payload["direction"], r.Payload["outcome"])
		}
		if num(t, rawOf(r)["ZANSWERED"]) != "0" {
			t.Errorf("row %d: raw.ZANSWERED %v, want 0 kept", id, rawOf(r)["ZANSWERED"])
		}
		if _, ok := rawOf(r)["ZDURATION"]; !ok {
			t.Errorf("row %d: raw.ZDURATION is not kept", id)
		}
	}
	if num(t, rawOf(rows[4])["ZDISCONNECTED_CAUSE"]) != "7" {
		t.Errorf("row 4: raw %v", rawOf(rows[4]))
	}
	// the other direction and outcome rules
	for _, c := range []struct {
		id           int64
		dir, outcome string
	}{{1, "in", "answered"}, {2, "in", "missed"}, {3, "out", "answered"}, {6, "unknown", "unknown"}} {
		r := rows[c.id]
		if r.Payload["direction"] != c.dir || r.Payload["outcome"] != c.outcome {
			t.Errorf("row %d: %v/%v, want %s/%s", c.id, r.Payload["direction"], r.Payload["outcome"], c.dir, c.outcome)
		}
	}
	if num(t, rawOf(rows[6])["ZORIGINATED"]) != "2" {
		t.Errorf("row 6: raw.ZORIGINATED %v, want 2 kept", rawOf(rows[6])["ZORIGINATED"])
	}
	// ZCALLTYPE 1, 8, 16, 99
	for id, kind := range map[int64]string{1: "voice", 7: "facetime_video", 8: "facetime_audio", 9: "unknown"} {
		if rows[id].Payload["kind"] != kind {
			t.Errorf("row %d: kind %v, want %s", id, rows[id].Payload["kind"], kind)
		}
	}
	if rows[7].Payload["service"] != "com.apple.FaceTime" || rows[9].Payload["service"] != "com.example.voip" || rows[1].Payload["service"] != "com.apple.Telephony" {
		t.Errorf("service values: %v %v %v", rows[1].Payload["service"], rows[7].Payload["service"], rows[9].Payload["service"])
	}
	// the BLOB address rows, with and without garbage bytes
	if rawOf(rows[3])["address_storage"] != "blob" || rows[3].Payload["address"] != "+15551230003" || rawOf(rows[3])["address_flags"] != nil {
		t.Errorf("row 3: address %v raw %v", rows[3].Payload["address"], rawOf(rows[3]))
	}
	r10 := rawOf(rows[10])
	if r10["address_storage"] != "blob" || r10["address_raw_b64"] != "KzE1NTUxMjMwMDEw//4=" || rows[10].Payload["address"] != "+15551230010��" {
		t.Errorf("row 10: address %q raw %v", rows[10].Payload["address"], r10)
	}
	if fl, _ := r10["address_flags"].([]any); len(fl) != 1 || fl[0] != "invalid_utf8" {
		t.Errorf("row 10: address_flags %v", r10["address_flags"])
	}
	// NULL and empty addresses are absent; only the empty one says so
	for _, id := range []int64{11, 12} {
		if _, has := rows[id].Payload["address"]; has {
			t.Errorf("row %d has an address", id)
		}
	}
	if _, has := rawOf(rows[11])["address_state"]; has {
		t.Errorf("row 11 (NULL address) states an address_state: %v", rawOf(rows[11]))
	}
	if rawOf(rows[12])["address_state"] != "empty" {
		t.Errorf("row 12: raw %v, want address_state empty", rawOf(rows[12]))
	}
	// durations: -3 is negative (no duration_s), +Inf is invalid stated as IEEE bits (P-4-1), 2e9 is a value without an end
	r16 := rows[16]
	if _, has := r16.Payload["duration_s"]; has || rawOf(r16)["ZDURATION_status"] != "negative" || num(t, rawOf(r16)["ZDURATION"]) != "-3" {
		t.Errorf("row 16: duration_s %v raw %v", r16.Payload["duration_s"], rawOf(r16))
	}
	r17 := rawOf(rows[17])
	if _, has := rows[17].Payload["duration_s"]; has || r17["ZDURATION_ieee"] != "7ff0000000000000" || r17["ZDURATION_status"] != "invalid" {
		t.Errorf("row 17: raw %v", r17)
	}
	if _, has := r17["ZDURATION"]; has {
		t.Errorf("row 17: a non-finite REAL has no raw.ZDURATION key: %v", r17)
	}
	if num(t, rows[18].Payload["duration_s"]) != "2000000000" || rows[18].TimeEnd != nil {
		t.Errorf("row 18: duration_s %v time_end %v, want the value and no end (over 1e9)", rows[18].Payload["duration_s"], rows[18].TimeEnd)
	}
	if rows[2].TimeEnd != nil {
		t.Errorf("row 2 (duration 0) has a time_end %q", *rows[2].TimeEnd)
	}
	if _, has := rows[20].Payload["duration_s"]; has || rows[20].TimeEnd != nil {
		t.Errorf("row 20 (NULL duration): duration_s %v", rows[20].Payload["duration_s"])
	}
	// times: REAL, integral REAL stored as INTEGER, 0 and -1 sentinels, year 2100 invalid
	wantTime(t, "row 1", rows[1].Time, t0.Add(1*time.Hour+500*time.Millisecond))
	wantTime(t, "row 1 end", rows[1].TimeEnd, t0.Add(1*time.Hour+500*time.Millisecond+125*time.Second))
	wantTime(t, "row 5 end", rows[5].TimeEnd, t0.Add(5*time.Hour+500*time.Millisecond+12500*time.Millisecond))
	wantTime(t, "row 13", rows[13].Time, t0.Add(13*time.Hour))
	if s := num(t, rawOf(rows[13])["ZDATE"]); strings.ContainsAny(s, ".eE") {
		t.Errorf("row 13: raw.ZDATE %s, want an INTEGER (NUMERIC affinity)", s)
	}
	for id, status := range map[int64]string{14: "sentinel", 19: "sentinel", 15: "invalid"} {
		if rows[id].Time != nil || rows[id].TimeEnd != nil || rawOf(rows[id])["ZDATE_status"] != status {
			t.Errorf("row %d: time %v raw %v, want no time and status %s", id, rows[id].Time, rawOf(rows[id]), status)
		}
	}
	// ZREAD 1, 0 and NULL
	if rows[1].Payload["read"] != true || rows[2].Payload["read"] != false {
		t.Errorf("read: %v %v", rows[1].Payload["read"], rows[2].Payload["read"])
	}
	if _, has := rows[3].Payload["read"]; has {
		t.Errorf("row 3 (NULL ZREAD) has read %v", rows[3].Payload["read"])
	}
	// Z_ENT, Z_OPT (and the one column the contract table does not map) stay unmapped; the other table is absent
	fx := parsertest.LoadFixture(t, "testdata", "coredata-current")
	if got := fx.Expect.Notes["unmapped_columns.ZCALLRECORD"]; got != "Z_ENT,Z_OPT,ZDEVICE_ID" {
		t.Errorf("unmapped_columns.ZCALLRECORD = %q", got)
	}
	if fx.Expect.Notes["table.call"] != "absent" {
		t.Errorf("notes %v", fx.Expect.Notes)
	}
}

// The WAL variant: the newest calls exist only in the -wal and say so.
func TestOracleStatesTheWALOnlyCalls(t *testing.T) {
	rows := byRowid(t, "coredata-wal")
	if len(rows) != 11 {
		t.Fatalf("%d records, want 8 in the database and 3 only in the WAL", len(rows))
	}
	for id, r := range rows {
		inWAL := id >= 9
		if inWAL != (r.File == "wal") || inWAL != strings.HasSuffix(r.SourcePath, "-wal") {
			t.Errorf("row %d: file %q source_path %q", id, r.File, r.SourcePath)
		}
	}
	if rows[9].Payload["outcome"] != "missed" || rows[10].Payload["direction"] != "out" || rows[11].Payload["kind"] != "facetime_video" {
		t.Errorf("wal rows: %v %v %v", rows[9].Payload, rows[10].Payload, rows[11].Payload)
	}
}

// 4E-R21: the legacy flags bits are not interpreted, so direction and outcome are unknown on every row.
func TestOracleStatesTheLegacyFlagsAreNotInterpreted(t *testing.T) {
	rows := byRowid(t, "legacy-call-history")
	if len(rows) != 8 {
		t.Fatalf("%d records, the scenario writes 8 rows", len(rows))
	}
	flags := map[int64]string{1: "4", 2: "5", 3: "8", 4: "1769472"}
	for id, r := range rows {
		if r.Payload["direction"] != "unknown" || r.Payload["outcome"] != "unknown" {
			t.Errorf("row %d: %v/%v, want unknown/unknown", id, r.Payload["direction"], r.Payload["outcome"])
		}
		if _, has := r.Payload["kind"]; has {
			t.Errorf("row %d has a kind", id)
		}
		if want, ok := flags[id]; ok && num(t, rawOf(r)["flags"]) != want {
			t.Errorf("row %d: raw.flags %v, want %s kept", id, rawOf(r)["flags"], want)
		}
	}
	wantTime(t, "row 1", rows[1].Time, t0.Add(1*time.Hour))
	wantTime(t, "row 1 end", rows[1].TimeEnd, t0.Add(1*time.Hour+125*time.Second))
	if rows[5].Time != nil || rawOf(rows[5])["date_status"] != "invalid" || num(t, rawOf(rows[5])["date"]) != "1682953200.5" {
		t.Errorf("row 5 (fractional REAL date): time %v raw %v", rows[5].Time, rawOf(rows[5]))
	}
	if rows[7].Time != nil || rawOf(rows[7])["date_status"] != "sentinel" {
		t.Errorf("row 7 (date 0): time %v raw %v", rows[7].Time, rawOf(rows[7]))
	}
	if _, has := rows[6].Payload["duration_s"]; has || rawOf(rows[6])["duration_status"] != "negative" {
		t.Errorf("row 6 (duration -4): %v", rows[6].Payload)
	}
	if num(t, rawOf(rows[4])["face_time_data_bytes"]) != "3" {
		t.Errorf("row 4: raw %v", rawOf(rows[4]))
	}
	if rawOf(rows[8])["address_state"] != "empty" {
		t.Errorf("row 8: raw %v", rawOf(rows[8]))
	}
}
