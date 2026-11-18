package androidwhatsapp_test

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func deletedOf(r records.Record) map[string]any {
	d, _ := r.Payload["deleted"].(map[string]any)
	return d
}

func sub(t *testing.T, r records.Record, key string) map[string]any {
	t.Helper()
	m, _ := rawMap(t, r)[key].(map[string]any)
	return m
}

// Spec 6.5: a revoke is a tombstone: no body, no attachments, deleted by the app's own structure, the original kind
// gone; text a revoked row still holds is raw, never the body.
func TestWhatsAppRevokeIsTombstoneWithoutContent(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	legacy, _ := recordsOf(t, "android-transition-legacydata")
	cases := []struct {
		rec    records.Record
		source string
	}{
		{modern["message/30"], "message_revoked"}, // type 15 and a row
		{modern["message/31"], "message_type"},    // type 64 and no row
		{modern["message/32"], "message_revoked"}, // type 0, the revoke known from the table only
		{modern["message/33"], "message_type"},    // type 15 with text on the row (hostile)
		{legacy["messages/46"], "message_revoked"},
	}
	for _, c := range cases {
		r := c.rec
		if r.Locator == "" {
			t.Fatalf("missing record")
		}
		if r.Body != "" || r.Payload["attachments"] != nil || r.Payload["text_source"] != nil || !r.Deleted || deletedOf(r)["source"] != c.source {
			t.Errorf("%s: body %q attachments %v deleted %v %v, want a tombstone from %s", r.Locator, r.Body, r.Payload["attachments"], r.Deleted, deletedOf(r), c.source)
		}
		if r.Payload["kind"] != "unknown" || r.Recovery != "" || r.Confidence != nil {
			t.Errorf("%s: kind %v recovery %q confidence %v", r.Locator, r.Payload["kind"], r.Recovery, r.Confidence)
		}
		if !strings.HasSuffix(r.Summary, "[revoked, no content]") {
			t.Errorf("%s: summary %q", r.Locator, r.Summary)
		}
	}
	if raw := rawMap(t, modern["message/33"]); raw["revoked_row_text"] != "secret" {
		t.Errorf("message 33: revoked_row_text %v", raw["revoked_row_text"])
	}
	// the row of message_revoked, with the time kept raw
	rv := sub(t, modern["message/30"], "revoke")
	if rv["revoked_key_id"] != "K30R" || rv["admin_jid"] != "15550000004@s.whatsapp.net" || fmt.Sprint(rv["revoke_timestamp"]) != "1600000100000" || rv["revoke_timestamp_unit"] != "unestablished" {
		t.Errorf("message 30: raw.revoke %v", rv)
	}
	if l := rawMap(t, modern["message/31"])["label_message_type"]; l != "revoked" {
		t.Errorf("message 31: label %v", l)
	}
	if _, has := rawMap(t, modern["message/31"])["revoke"]; has {
		t.Errorf("message 31 has raw.revoke without a message_revoked row")
	}
}

// Spec 6.5: the denormalised copy of a quoted message is never a record, never part of a body and never a target.
func TestWhatsAppQuotedCopyIsNeverAMessage(t *testing.T) {
	for _, v := range []string{"android-modern-2026", "android-transition-legacydata"} {
		recs, _ := recordsOf(t, v)
		for k, r := range recs {
			if strings.Contains(r.Locator, "message_quoted") || strings.Contains(r.Locator, "messages_quotes") {
				t.Errorf("%s %s: a record from the quote table", v, k)
			}
			if r.Payload["target_guid"] != nil {
				t.Errorf("%s %s: target_guid %v (that field is for reactions)", v, k, r.Payload["target_guid"])
			}
		}
	}
	modern, _ := recordsOf(t, "android-modern-2026")
	r34 := modern["message/34"]
	q := sub(t, r34, "quoted")
	if strings.Contains(r34.Body, "hello") || r34.Body != "reply" || q["copy"] != true || q["text"] != "hello" || q["key_id"] != "K02" ||
		fmt.Sprint(q["from_me"]) != "0" || fmt.Sprint(q["timestamp"]) != "1600000000000" {
		t.Errorf("message 34: body %q quoted %v", r34.Body, q)
	}
	legacy, _ := recordsOf(t, "android-transition-legacydata")
	r45 := legacy["messages/45"]
	lq := sub(t, r45, "quoted")
	raw := rawMap(t, r45)
	if strings.Contains(r45.Body, "hello") || lq["copy"] != true || lq["text"] != "hello" || lq["key_id"] != "K02" || fmt.Sprint(raw["quoted_row_id"]) != "1" {
		t.Errorf("legacy message 45: body %q quoted %v raw %v", r45.Body, lq, raw)
	}
	// edit_version 7 alone is raw: no revoke (S1 only)
	if fmt.Sprint(raw["edit_version"]) != "7" || r45.Deleted || raw["revoke"] != nil {
		t.Errorf("legacy message 45: edit_version %v deleted %v revoke %v", raw["edit_version"], r45.Deleted, raw["revoke"])
	}
}

// D6, spec 6.4.4: a LID is mapped to a phone JID only by a row of jid_map; the participant keeps the LID.
func TestWhatsAppLIDMappedOnlyByDatabase(t *testing.T) {
	modern, res := recordsOf(t, "android-modern-2026")
	r42 := modern["message/42"]
	pn := []any{map[string]any{"lid": "100000000000001@lid", "pn": "15550000005@s.whatsapp.net", "source": "jid_map"}}
	if th := threadOf(r42); th["id"] != "100000000000001@lid" || !reflect.DeepEqual(partsOf(r42), []any{map[string]any{"role": "from", "address": "100000000000001@lid"}}) ||
		!reflect.DeepEqual(rawMap(t, r42)["lid_map"], pn) {
		t.Errorf("message 42: thread %v participants %v raw %v", threadOf(r42), partsOf(r42), rawMap(t, r42))
	}
	r43 := modern["message/43"]
	if !reflect.DeepEqual(rawMap(t, r43)["lid_unmapped"], []any{"100000000000002@lid"}) || rawMap(t, r43)["lid_map"] != nil ||
		!reflect.DeepEqual(partsOf(r43), []any{map[string]any{"role": "from", "address": "100000000000002@lid"}}) {
		t.Errorf("message 43: raw %v participants %v", rawMap(t, r43), partsOf(r43))
	}
	if _, has := res.Notes["jid_map"]; has {
		t.Errorf("note jid_map = %q on a store that has the table", res.Notes["jid_map"])
	}
	// a store with no jid_map says nothing per record, and the note says why
	old, ores := recordsOf(t, "android-modern-2021")
	for k, r := range old {
		if raw := rawMap(t, r); raw["lid_map"] != nil || raw["lid_unmapped"] != nil || raw["lid_unresolved"] != nil {
			t.Errorf("android-modern-2021 %s: LID keys %v", k, raw)
		}
	}
	if ores.Notes["jid_map"] != "absent" {
		t.Errorf("note jid_map = %q, want absent", ores.Notes["jid_map"])
	}
	// no phone JID ever sits among a participant of a LID message
	for k, r := range modern {
		for _, p := range partsOf(r) {
			if a := p.(map[string]any)["address"]; a == "15550000005@s.whatsapp.net" {
				t.Errorf("%s: the mapped phone JID is a participant", k)
			}
		}
	}
}

// R8, spec 7.6: a time whose unit no source establishes is raw only, with its unit stated; it never becomes a time.
func TestWhatsAppUnestablishedTimeUnitsStayRaw(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	for _, id := range []int{30, 35, 44} {
		r := modern["message/"+strconv.Itoa(id)]
		if r.Deleted && deletedOf(r)["source"] == "" {
			t.Errorf("message %d: deleted with no source", id)
		}
		for _, nt := range r.Times {
			if nt.Kind != "received" && nt.Kind != "delivered" {
				t.Errorf("message %d: time %q derived from a column whose unit is not established", id, nt.Kind)
			}
		}
		if len(r.Times) != 1 || r.Times[0].Kind != "received" {
			t.Errorf("message %d: times %+v, want the received time only (no edited, no deleted)", id, r.Times)
		}
	}
	rv := sub(t, modern["message/30"], "revoke")
	ed := sub(t, modern["message/35"], "edit")
	eph := sub(t, modern["message/44"], "ephemeral")
	for name, got := range map[string]any{
		"revoke_timestamp_unit": rv["revoke_timestamp_unit"], "edited_timestamp_unit": ed["edited_timestamp_unit"],
		"sender_timestamp_unit": ed["sender_timestamp_unit"], "expire_timestamp_unit": eph["expire_timestamp_unit"],
		"receipt_server_timestamp_unit": rawMap(t, modern["message/3"])["receipt_server_timestamp_unit"],
	} {
		if got != "unestablished" {
			t.Errorf("%s = %v, want unestablished", name, got)
		}
	}
	if fmt.Sprint(ed["edited_timestamp"]) != "1600000200000" || fmt.Sprint(ed["sender_timestamp"]) != "1600000199000" || ed["original_key_id"] != "K35O" || fmt.Sprint(ed["display_type"]) != "1" {
		t.Errorf("message 35: raw.edit %v", ed)
	}
	// the legacy server receipt time has a known unit (milliseconds, research 7.12): raw only, no unit key
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	if raw := rawMap(t, legacy["messages/3"]); raw["receipt_server_timestamp"] == nil || raw["receipt_server_timestamp_unit"] != nil {
		t.Errorf("legacy message 3: %v", raw)
	}
}

// R4, 4I-R18: the system action type 11 is unknown (its second source cannot be re-checked); only 18 is mapped.
func TestWhatsAppSystemAndOtherFeatures(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	r36, r37, r38 := modern["message/36"], modern["message/37"], modern["message/38"]
	raw36 := rawMap(t, r36)
	if r36.Payload["kind"] != "system" || r36.Body != "New name" || fmt.Sprint(raw36["action_type"]) != "11" || raw36["label_action_type"] != nil || raw36["old_data"] != "Old" {
		t.Errorf("message 36: kind %v body %q raw %v", r36.Payload["kind"], r36.Body, raw36)
	}
	if raw := rawMap(t, r37); fmt.Sprint(raw["action_type"]) != "18" || raw["label_action_type"] != "security_code_changed" {
		t.Errorf("message 37: raw %v", raw)
	}
	raw38 := rawMap(t, r38)
	nc := sub(t, r38, "number_change")
	if fmt.Sprint(raw38["action_type"]) != "4" || raw38["label_action_type"] != nil || nc["old_jid_row_id"] == nil || nc["new_jid_row_id"] == nil ||
		fmt.Sprint(nc["old_jid_row_id"]) == fmt.Sprint(nc["new_jid_row_id"]) {
		t.Errorf("message 38: raw %v", raw38)
	}
	// a call notice (type 10, unmapped) keeps its call_log row
	r39 := modern["message/39"]
	if r39.Payload["kind"] != "unknown" || fmt.Sprint(rawMap(t, r39)["call_log_row_id"]) != "1" || rawMap(t, r39)["label_message_type"] != nil {
		t.Errorf("message 39: %v", r39.Payload)
	}
	// locations: the columns as stored
	if loc := sub(t, modern["message/40"], "location"); fmt.Sprint(loc["latitude"]) != "48.85" || fmt.Sprint(loc["longitude"]) != "2.35" || loc["place_name"] != "Spot" {
		t.Errorf("message 40: location %v", loc)
	}
	if loc := sub(t, modern["message/41"], "location"); fmt.Sprint(loc["live_location_sequence_number"]) != "3" || fmt.Sprint(loc["live_location_final_latitude"]) != "1.75" {
		t.Errorf("message 41: location %v", loc)
	}
	// the single-source extras are raw only
	r44 := modern["message/44"]
	raw44 := rawMap(t, r44)
	if fmt.Sprint(raw44["view_once_state"]) != "1" || fmt.Sprint(raw44["starred"]) != "1" || fmt.Sprint(sub(t, r44, "forwarded")["forward_score"]) != "2" || fmt.Sprint(sub(t, r44, "ephemeral")["duration"]) != "86400" {
		t.Errorf("message 44: raw %v", raw44)
	}
}
