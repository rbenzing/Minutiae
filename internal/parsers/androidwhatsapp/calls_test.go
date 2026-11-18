package androidwhatsapp_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

func callRecord(t *testing.T, recs map[string]records.Record, id int) records.Record {
	t.Helper()
	r, ok := recs[fmt.Sprintf("call_log/%d", id)]
	if !ok || r.Type != "call" {
		t.Fatalf("no call record for call_log row %d", id)
	}
	return r
}

// Spec 6.6 and 7.4: one call per call_log row; the direction from from_me, the kind from video_call, the outcome from
// call_result (5 answered; 2 missed when incoming, unknown with the label not_answered when outgoing; 3 unknown with the
// label unavailable; a value in a research conflict row unknown with no label), the end derived from the duration.
func TestWhatsAppCallOutcomes(t *testing.T) {
	recs, _ := recordsOf(t, "android-modern-2026")
	type want struct {
		dir, outcome, kind, label string
	}
	for id, w := range map[int]want{
		1: {"in", "answered", "voice", ""},
		2: {"in", "missed", "voice", ""},
		3: {"out", "unknown", "voice", "not_answered"},
		4: {"in", "unknown", "voice", "unavailable"},
		5: {"in", "unknown", "voice", ""}, // call_result 4 is in a research conflict row
		6: {"in", "answered", "video", ""},
		8: {"in", "answered", "voice", ""},
	} {
		r := callRecord(t, recs, id)
		raw := rawMap(t, r)
		label, _ := raw["label_call_result"].(string)
		if r.Payload["direction"] != w.dir || r.Payload["outcome"] != w.outcome || r.Payload["kind"] != w.kind || label != w.label || r.Payload["service"] != "whatsapp" {
			t.Errorf("call %d: direction %v outcome %v kind %v label %q service %v, want %+v", id, r.Payload["direction"], r.Payload["outcome"], r.Payload["kind"], label, r.Payload["service"], w)
		}
		if raw["source_table"] != "call_log" || raw["app"] != "whatsapp" || fmt.Sprint(raw["call_id"]) != fmt.Sprintf("C%02d", id) {
			t.Errorf("call %d: raw %v", id, raw)
		}
		if r.Payload["address"] != "15550000001@s.whatsapp.net" && id != 8 {
			t.Errorf("call %d: address %v", id, r.Payload["address"])
		}
	}
	r1 := callRecord(t, recs, 1)
	if r1.Time == nil || r1.TimeEnd == nil || r1.TimeEnd.T.Sub(r1.Time.T) != 60*time.Second || fmt.Sprint(r1.Payload["duration_s"]) != "60" || rawMap(t, r1)["time_end_basis"] != "start+duration" {
		t.Errorf("call 1: time %v end %v duration %v basis %v", r1.Time, r1.TimeEnd, r1.Payload["duration_s"], rawMap(t, r1)["time_end_basis"])
	}
	if r2 := callRecord(t, recs, 2); r2.TimeEnd != nil || rawMap(t, r2)["time_end_basis"] != nil {
		t.Errorf("call 2 (no duration): end %v", r2.TimeEnd)
	}

	// 6: a video call to a group, with two participants whose results are S6-only: raw
	raw6 := rawMap(t, callRecord(t, recs, 6))
	wantParts := []any{
		map[string]any{"jid": "15550000002@s.whatsapp.net", "call_result": float64(5)},
		map[string]any{"jid": "15550000003@s.whatsapp.net", "call_result": float64(2)},
	}
	if raw6["group_jid"] != "120363000000000001@g.us" || !reflect.DeepEqual(normNums(raw6["participants"]), normNums(wantParts)) {
		t.Errorf("call 6: group_jid %v participants %v", raw6["group_jid"], raw6["participants"])
	}
	if _, has := rawMap(t, callRecord(t, recs, 1))["group_jid"]; has {
		t.Errorf("call 1 has a group_jid, its group_jid_row_id is 0")
	}

	// 8: a LID, mapped by jid_map only
	if got := rawMap(t, callRecord(t, recs, 8))["lid_map"]; got == nil || callRecord(t, recs, 8).Payload["address"] != "100000000000001@lid" {
		t.Errorf("call 8: lid_map %v", got)
	}

	// the transition store has the table too
	legacy, _ := recordsOf(t, "android-transition-legacydata")
	if r := callRecord(t, legacy, 1); r.Payload["outcome"] != "answered" {
		t.Errorf("transition call 1: %v", r.Payload)
	}
}

// normNums makes every number in a decoded value comparable (the payload may carry int64 or float64).
func normNums(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normNums(x[i])
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = normNums(e)
		}
		return out
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return v
}

// R10 (4I-R10): call_log.timestamp has no 1e11 floor, as android.calllog's ruling T7: seconds in the millisecond column
// are a 1970 date stated as stored, never rescaled and never invalid.
func TestWhatsAppCallTimeFollowsCallLogRule(t *testing.T) {
	recs, _ := recordsOf(t, "android-modern-2026")
	r := callRecord(t, recs, 7)
	raw := rawMap(t, r)
	if r.Time == nil || r.Time.T.UTC().Format(time.RFC3339) != "1970-01-19T12:26:40Z" {
		t.Errorf("call 7: time %v, want 1970-01-19T12:26:40Z", r.Time)
	}
	if fmt.Sprint(raw["timestamp"]) != "1600000000" || raw["timestamp_status"] != nil {
		t.Errorf("call 7: raw.timestamp %v status %v, want 1600000000 and none", raw["timestamp"], raw["timestamp_status"])
	}
	// a message_add_on or message with the same value is invalid (the floor of 4I-R10): the two rules differ on purpose
	for _, r := range recs {
		if r.Type == "message" && r.Time != nil && r.Time.T.Year() < 1973 {
			t.Errorf("%s: a message time before 1973 (%v)", r.Locator, r.Time)
		}
	}
}

// 4I-R22 vocabulary of a call, pinned on rows the test wrote: a counterparty that is absent, names no jid row or has no
// raw_string; a group key that is negative; a participant with no jid; a duration that is not a number is no duration.
func TestWhatsAppCallStatesArePinned(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		jidRow(tb, 2, "15550000002", "s.whatsapp.net", nil)
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "one")
		tb["call_log"].Insert(1, nil, nil, int64(0), "A", goodTS, int64(0), int64(5), int64(5), int64(0))
		tb["call_log"].Insert(2, nil, int64(99), int64(1), "B", goodTS, int64(1), "x", int64(2), int64(-1))
		tb["call_log"].Insert(3, nil, int64(2), int64(1), "C", goodTS, int64(1), int64(5), int64(7), int64(99))
		tb["call_log_participant_v2"].Insert(1, nil, int64(3), nil, int64(5))
		tb["call_log_participant_v2"].Insert(2, nil, int64(3), int64(2), nil)
		tb["call_log_participant_v2"].Insert(3, nil, int64(3), int64(98), int64(2))
	})
	recs := byRowid(t, runBytes(t, db, nil, remoteLive))
	r1, r2, r3 := rawMap(t, recs["call_log/1"]), rawMap(t, recs["call_log/2"]), rawMap(t, recs["call_log/3"])
	if r1["address_state"] != "absent" || recs["call_log/1"].Payload["address"] != nil {
		t.Errorf("call 1: raw %v", r1)
	}
	if r2["address_state"] != "no-jid-row" || r2["group_jid_state"] != "absent" || r2["duration"] != nil {
		t.Errorf("call 2: raw %v", r2)
	}
	if recs["call_log/2"].Payload["duration_s"] != nil || !reflect.DeepEqual(r2["unknown_columns"], map[string]any{"duration": "type-mismatch"}) {
		t.Errorf("call 2: duration_s %v unknown %v", recs["call_log/2"].Payload["duration_s"], r2["unknown_columns"])
	}
	if r3["address_state"] != "no-raw-string" || !reflect.DeepEqual(r3["address_parts"], map[string]any{"user": "15550000002", "server": "s.whatsapp.net"}) || r3["group_jid_state"] != "no-jid-row" {
		t.Errorf("call 3: raw %v", r3)
	}
	want := []any{
		map[string]any{"jid_state": "absent", "call_result": float64(5)},
		map[string]any{"jid_state": "no-raw-string"},
		map[string]any{"jid_state": "no-jid-row", "call_result": float64(2)},
	}
	if !reflect.DeepEqual(normNums(r3["participants"]), normNums(want)) {
		t.Errorf("call 3: participants %v", r3["participants"])
	}
	if recs["call_log/3"].Payload["outcome"] != "unknown" || r3["label_call_result"] != nil {
		t.Errorf("call 3: call_result 7 is in a research conflict row: outcome %v label %v", recs["call_log/3"].Payload["outcome"], r3["label_call_result"])
	}
}
