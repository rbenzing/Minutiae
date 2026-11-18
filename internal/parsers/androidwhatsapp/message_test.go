package androidwhatsapp_test

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// parsedVariants are the variants Parse reads (the others are reported unparsed by Probe).
var parsedVariants = []string{
	"android-legacy-2.11", "android-transition-legacydata", "android-transition-moderndata", "android-transition-both",
	"android-transition-walonly", "android-transition-empty", "android-modern-2021", "android-modern-2026", "android-modern-business",
}

// recordsOf parses a variant and returns its records by "<table>/<rowid>".
func recordsOf(t *testing.T, variant string) (map[string]records.Record, parsertest.Result) {
	t.Helper()
	res, _ := runVariant(t, variant)
	if res.Err != nil {
		t.Fatalf("%s: Parse: %v", variant, res.Err)
	}
	out := map[string]records.Record{}
	for _, r := range res.Records {
		out[keyOf(r)] = r
	}
	return out, res
}

// keyOf is "<table>/<rowid>" of a record's locator sqlite:table=<t>;rowid=<n>;...
func keyOf(r records.Record) string {
	s, _ := strings.CutPrefix(r.Locator, "sqlite:table=")
	tb, rest, _ := strings.Cut(s, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return tb + "/" + id
}

func rawMap(t *testing.T, r records.Record) map[string]any {
	t.Helper()
	m, ok := r.Payload["raw"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no raw", r.Locator)
	}
	return m
}

func partsOf(r records.Record) []any {
	p, _ := r.Payload["participants"].([]any)
	return p
}

func threadOf(r records.Record) map[string]any {
	m, _ := r.Payload["thread"].(map[string]any)
	return m
}

// R1: a transition store with both tables populated gives every message twice, each labelled with its table.
func TestWhatsAppBothGenerationsEmitBothLabelled(t *testing.T) {
	recs, res := recordsOf(t, "android-transition-both")
	if len(recs) != 32 {
		t.Fatalf("%d records, want 32", len(recs))
	}
	for id := 2; id <= 17; id++ {
		a, b := recs["messages/"+strconv.Itoa(id)], recs["message/"+strconv.Itoa(id)]
		if rawMap(t, a)["source_table"] != "messages" || rawMap(t, b)["source_table"] != "message" {
			t.Errorf("id %d: source_table %v and %v", id, rawMap(t, a)["source_table"], rawMap(t, b)["source_table"])
		}
		if a.Payload["guid"] == nil || a.Payload["guid"] != b.Payload["guid"] ||
			!reflect.DeepEqual(threadOf(a)["id"], threadOf(b)["id"]) || a.Payload["direction"] != b.Payload["direction"] {
			t.Errorf("id %d: guid %v/%v thread %v/%v direction %v/%v", id, a.Payload["guid"], b.Payload["guid"], threadOf(a), threadOf(b), a.Payload["direction"], b.Payload["direction"])
		}
	}
	if n := len(warningsContaining(res, "whatsapp-both-generations")); n != 1 {
		t.Errorf("%d both-generations warnings, want 1", n)
	}
}

// Spec 6.2: one mapper. The same scenario in each generation gives the same records apart from raw, locator, range,
// artifact and the legacy-only delivered time (the codes both columns map; id 4's caption is Body in the modern
// generation only, spec 6.4.2).
func TestWhatsAppGenerationsMapAlike(t *testing.T) {
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	modern, _ := recordsOf(t, "android-modern-2021")
	strip := func(r records.Record) map[string]any {
		p := map[string]any{}
		for k, v := range r.Payload {
			if k != "raw" {
				p[k] = v
			}
		}
		times := map[string]any{}
		for _, nt := range r.Times {
			if nt.Kind != "delivered" {
				times[nt.Kind] = nt.Time.T
			}
		}
		return map[string]any{"type": r.Type, "time": r.Time, "times": times, "deleted": r.Deleted, "summary": r.Summary, "body": r.Body, "payload": p}
	}
	for _, id := range []int{2, 3, 5, 6, 7, 8, 9, 10, 11, 12, 13, 16, 17} {
		l, m := legacy["messages/"+strconv.Itoa(id)], modern["message/"+strconv.Itoa(id)]
		if l.Locator == "" || m.Locator == "" {
			t.Fatalf("id %d: legacy %q modern %q", id, l.Locator, m.Locator)
		}
		if !reflect.DeepEqual(strip(l), strip(m)) {
			t.Errorf("id %d differs between the generations:\n legacy %+v\n modern %+v", id, strip(l), strip(m))
		}
	}
	if l3 := legacy["messages/3"]; len(l3.Times) != 1 || l3.Times[0].Kind != "delivered" {
		t.Errorf("legacy message 3 times %+v, want the delivered time from receipt_device_timestamp", l3.Times)
	}
}

// R4: a code the research does not support is kind unknown, the stored value in raw, no label; no delivery status is
// ever mapped.
func TestWhatsAppUnmappedCodesStayUnknownWithRaw(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	m14 := modern["message/14"]
	if r := rawMap(t, m14); m14.Payload["kind"] != "unknown" || fmt.Sprint(r["message_type"]) != "999" || r["label_message_type"] != nil {
		t.Errorf("message 14: kind %v raw %v", m14.Payload["kind"], r)
	}
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	for id, want := range map[int]any{22: " 1", 23: "1.0", 24: "x", 25: map[string]any{"blob_b64": "AQ=="}} {
		r := legacy["messages/"+strconv.Itoa(id)]
		raw := rawMap(t, r)
		if r.Payload["kind"] != "unknown" || !reflect.DeepEqual(raw["media_wa_type"], want) || raw["label_media_wa_type"] != nil {
			t.Errorf("legacy message %d: kind %v raw %v, want unknown and %v with no label", id, r.Payload["kind"], raw, want)
		}
	}
	if r := legacy["messages/21"]; r.Payload["kind"] != "media" || rawMap(t, r)["label_media_wa_type"] != "image" {
		t.Errorf("legacy message 21 ('1'): %v", r.Payload)
	}
	if r := legacy["messages/14"]; r.Payload["kind"] != "unknown" || rawMap(t, r)["media_wa_type"] != "999" {
		t.Errorf("legacy message 14: %v", r.Payload)
	}
	for _, v := range parsedVariants {
		recs, _ := recordsOf(t, v)
		for k, r := range recs {
			if r.Payload["status"] != "unknown" {
				t.Errorf("%s %s: status %v, want unknown (R4)", v, k, r.Payload["status"])
			}
			if _, has := rawMap(t, r)["status"]; !has {
				t.Errorf("%s %s: the stored status is not in raw", v, k)
			}
		}
	}
}

// Spec 7.1: legacy type 0 with status 6 is not text.
func TestWhatsAppLegacySystemStatusRowIsNotText(t *testing.T) {
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	r := legacy["messages/15"]
	raw := rawMap(t, r)
	if r.Payload["kind"] != "unknown" || fmt.Sprint(raw["status"]) != "6" || fmt.Sprint(raw["media_wa_type"]) != "0" || raw["label_media_wa_type"] != nil {
		t.Errorf("legacy message 15: kind %v raw %v", r.Payload["kind"], raw)
	}
	if r.Body != "Group A" || raw["media_size"] == nil {
		t.Errorf("legacy message 15: body %q media_size %v (the data is the body, media_size stays raw)", r.Body, raw["media_size"])
	}
	// the modern system type is kind system
	modern, _ := recordsOf(t, "android-modern-2021")
	if k := modern["message/15"].Payload["kind"]; k != "system" {
		t.Errorf("modern message 15: kind %v, want system", k)
	}
}

// R3: an incoming status update has no attributed author; an outgoing one keeps where it was posted.
func TestWhatsAppStatusUpdateAuthorUnknown(t *testing.T) {
	for variant, table := range map[string]string{"android-modern-2026": "message", "android-legacy-2.11": "messages"} {
		recs, _ := recordsOf(t, variant)
		r12, r13 := recs[table+"/12"], recs[table+"/13"]
		if r12.Payload["participants_unknown"] != true || len(partsOf(r12)) != 0 {
			t.Errorf("%s message 12: %v", variant, r12.Payload)
		}
		raw := rawMap(t, r12)
		if table == "message" && raw["sender_jid_row_id"] == nil || table == "messages" && raw["remote_resource"] != "15550000003@s.whatsapp.net" {
			t.Errorf("%s message 12: the stored sender column is not in raw: %v", variant, raw)
		}
		if raw["chat_class"] != "status" {
			t.Errorf("%s message 12: chat_class %v", variant, raw["chat_class"])
		}
		want := []any{map[string]any{"role": "to", "address": "status@broadcast"}}
		if !reflect.DeepEqual(partsOf(r13), want) {
			t.Errorf("%s message 13: participants %v", variant, partsOf(r13))
		}
	}
}

// Spec 6.4.3: a group sender is never the group.
func TestWhatsAppGroupSenderNeverTheGroup(t *testing.T) {
	for _, v := range parsedVariants {
		recs, _ := recordsOf(t, v)
		for k, r := range recs {
			th := threadOf(r)
			if th["group"] != true {
				continue
			}
			for _, p := range partsOf(r) {
				if pm := p.(map[string]any); pm["role"] == "from" && pm["address"] == th["id"] {
					t.Errorf("%s %s: the group %v is its own sender", v, k, th["id"])
				}
			}
		}
	}
	modern, _ := recordsOf(t, "android-modern-2026")
	r10 := modern["message/10"]
	if r10.Payload["participants_unknown"] != true || rawMap(t, r10)["sender_state"] != "absent" {
		t.Errorf("message 10: %v", r10.Payload)
	}
	if r9 := modern["message/9"]; !reflect.DeepEqual(partsOf(r9), []any{map[string]any{"role": "from", "address": "15550000002@s.whatsapp.net"}}) {
		t.Errorf("message 9 participants %v", partsOf(r9))
	}
}

// 4I-R13: a legacy row's title comes from chat_list only; without that table the thread has an id and no title,
// although the store's chat.subject is set.
func TestWhatsAppLegacyTitleOnlyFromChatList(t *testing.T) {
	recs, res := recordsOf(t, "android-transition-legacydata")
	if res.Notes["table.chat_list"] != "absent" {
		t.Errorf("note table.chat_list = %q, want absent", res.Notes["table.chat_list"])
	}
	for _, id := range []int{9, 10, 11, 15} {
		r := recs["messages/"+strconv.Itoa(id)]
		th := threadOf(r)
		if th["id"] != "120363000000000001@g.us" || th["group"] != true || th["title"] != nil {
			t.Errorf("legacy message %d: thread %v, want the group id and no title", id, th)
		}
	}
	// the same group's modern rows (in the both-tables store) keep chat.subject
	both, _ := recordsOf(t, "android-transition-both")
	if th := threadOf(both["message/9"]); th["title"] != "Group A" {
		t.Errorf("modern message 9: thread %v", th)
	}
	if th := threadOf(both["messages/9"]); th["title"] != nil {
		t.Errorf("legacy message 9 of a store with no chat_list: thread %v", th)
	}
	// with chat_list the title is chat_list.subject
	leg, _ := recordsOf(t, "android-legacy-2.11")
	if th := threadOf(leg["messages/9"]); th["title"] != "Group A" {
		t.Errorf("legacy message 9: thread %v", th)
	}
}

// A contact card's Body is the vCard; an image's caption is Body in the modern generation only.
func TestWhatsAppBodyAndCaptionRules(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2021")
	if r := modern["message/7"]; r.Body != "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n" || r.Payload["text_source"] != "part" {
		t.Errorf("modern message 7: body %q source %v", r.Body, r.Payload["text_source"])
	}
	if r := modern["message/4"]; r.Body != "pic" || r.Payload["text_source"] != "text" {
		t.Errorf("modern message 4: body %q", r.Body)
	}
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	if r := legacy["messages/4"]; r.Body != "" || rawMap(t, r)["media_caption"] != "pic" {
		t.Errorf("legacy message 4: body %q raw %v", r.Body, rawMap(t, r))
	}
	if r := legacy["messages/7"]; r.Body != "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n" || r.Payload["text_source"] != "part" {
		t.Errorf("legacy message 7: body %q source %v", r.Body, r.Payload["text_source"])
	}
}

// Spec 6.4.2 and 4I-R10: a seconds value in a millisecond column is invalid, a zero is a sentinel; neither is a time.
func TestWhatsAppSentinelAndInvalidTimesAreNotTimes(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2021")
	r16, r17 := modern["message/16"], modern["message/17"]
	if r16.Time != nil || rawMap(t, r16)["timestamp_status"] != "invalid" || r17.Time != nil || rawMap(t, r17)["timestamp_status"] != "sentinel" {
		t.Errorf("message 16: %v %v; 17: %v %v", r16.Time, rawMap(t, r16), r17.Time, rawMap(t, r17))
	}
	if len(r17.Times) != 0 || rawMap(t, r17)["received_timestamp_status"] != "sentinel" {
		t.Errorf("message 17: times %v raw %v", r17.Times, rawMap(t, r17))
	}
}

// 4I-R3: a dangling chat or sender key is stated, never read as a chat or sender with no JID.
func TestWhatsAppDanglingJoinsAreStated(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	// a chat whose jid row has no raw_string: no address, the parts in raw
	r19 := modern["message/19"]
	if raw := rawMap(t, r19); !reflect.DeepEqual(raw["chat_jid_parts"], map[string]any{"user": "15550000009", "server": "s.whatsapp.net"}) || raw["chat_jid"] != nil {
		t.Errorf("message 19: raw %v", raw)
	}
	if th := threadOf(r19); th["id"] != nil || th["group"] != false {
		t.Errorf("message 19: thread %v", th)
	}
}
