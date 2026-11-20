package ioscalendar_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
)

// Plan 4I3 Task 9 (4I3-P4): the typed pass's mapping is the one CalendarItem mapping with its five identifying
// columns required (spec 8.1).
func TestMappingsAreTheCalendarItemTable(t *testing.T) {
	m := ioscalendar.Parser{}.Mappings()
	if len(m) != 1 || m[0].Table != "CalendarItem" || m[0].Map == nil {
		t.Fatalf("Mappings = %+v, want the one CalendarItem mapping", m)
	}
	var req []string
	for _, c := range m[0].Columns {
		if c.Required {
			req = append(req, c.Name)
		}
	}
	if fmt.Sprint(req) != "[ROWID start_date start_tz all_day calendar_id]" {
		t.Errorf("required columns %v (spec 8.1)", req)
	}
}

// Spec 6.4: no iOS deletion marker is established, so every record says that its deletion state was not evaluated and
// none is Deleted; hidden is raw only (row 25).
func TestIOSCalendarDeletionStateIsNotEvaluated(t *testing.T) {
	n := 0
	for _, v := range recordedVariants() {
		for _, r := range parseVariant(t, v).Records {
			n++
			if rawOf(r)["deletion_state"] != "not-evaluated" || r.Deleted || r.Payload["deleted"] != nil {
				t.Errorf("%s %s: deletion_state %#v, Deleted %v, deleted %v", v, r.Locator, rawOf(r)["deletion_state"], r.Deleted, r.Payload["deleted"])
			}
		}
	}
	if n == 0 {
		t.Fatal("no record")
	}
	r25 := byRowid(t, parseVariant(t, "calendar-ios"))[25]
	if rawOf(r25)["hidden"] != int64(1) || r25.Deleted {
		t.Errorf("row 25: raw.hidden %#v, Deleted %v; want 1 and not Deleted", rawOf(r25)["hidden"], r25.Deleted)
	}
}

// labelKeys lists every map key under v that starts with "label_".
func labelKeys(v any, path string, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if strings.HasPrefix(k, "label_") {
				*out = append(*out, path+"."+k)
			}
			labelKeys(e, path+"."+k, out)
		}
	case []any:
		for i, e := range x {
			labelKeys(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// Q1 (coordinator ruling 1): no iOS code is mapped: status, availability and privacy, and an attendee's role, type
// and status, are "unknown" with the stored code in raw; NULL states nothing; no raw.label_* key anywhere.
func TestIOSCalendarCodesStayRaw(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	r1 := recs[1]
	for _, k := range []string{"status", "availability", "privacy"} {
		if payloadStr(r1, k) != "unknown" {
			t.Errorf("row 1: %s %q, want unknown", k, payloadStr(r1, k))
		}
	}
	raw := rawOf(r1)
	if raw["status"] != int64(1) || raw["availability"] != int64(0) || raw["privacy_level"] != int64(0) {
		t.Errorf("row 1: raw codes %#v %#v %#v, want 1, 0, 0 as stored", raw["status"], raw["availability"], raw["privacy_level"])
	}
	for _, id := range []int64{1, 9} {
		for i, a := range listOf(recs[id].Payload["attendees"]) {
			for _, k := range []string{"role", "type", "status"} {
				if objOf(a)[k] != "unknown" {
					t.Errorf("row %d attendee %d: %s %v, want unknown", id, i, k, objOf(a)[k])
				}
			}
		}
	}
	if recs[2].Payload["status"] != nil || rawOf(recs[2])["status"] != nil {
		t.Errorf("row 2 (status NULL): status %v, raw %v", recs[2].Payload["status"], rawOf(recs[2])["status"])
	}
	var labels []string
	for id, r := range recs {
		labelKeys(r.Payload, fmt.Sprintf("row %d", id), &labels)
	}
	if len(labels) != 0 {
		t.Errorf("label keys %v: no iOS code is labelled (Q1)", labels)
	}

	s := newBuilt()
	s.event(1, map[string]any{"status": "1", "availability": float64(2.5), "privacy_level": int64(99)})
	r := byRowid(t, parseBuilt(t, s.bytes(), false))[1]
	if payloadStr(r, "status") != "unknown" || rawOf(r)["status"] != "1" || unknownOf(r)["CalendarItem.status"] != "type-mismatch" {
		t.Errorf("TEXT status: status %q, raw %#v, unknown %v", payloadStr(r, "status"), rawOf(r)["status"], unknownOf(r))
	}
	if payloadStr(r, "availability") != "unknown" || rawOf(r)["availability"] != 2.5 || payloadStr(r, "privacy") != "unknown" || rawOf(r)["privacy_level"] != int64(99) {
		t.Errorf("REAL availability, another privacy code: raw %v", rawOf(r))
	}
}

// Spec 9 (iOS half): an identifier over its cap is never cut: absent, with its state, the length of the stored bytes
// and their SHA-256; the cap holds for the cleaned text (an invalid byte grows to the three bytes of U+FFFD).
func TestCalendarIdentifiersAreNeverCut(t *testing.T) {
	zone := "Zone/" + strings.Repeat("x", 295)
	r22 := byRowid(t, parseVariant(t, "calendar-ios"))[22]
	raw := rawOf(r22)
	if r22.Payload["start_timezone"] != nil || raw["start_tz_state"] != "too-long" || raw["start_tz_len"] != int64(300) || raw["start_tz_sha256"] != sha(zone) ||
		payloadStr(r22, "timing") != "unknown" || raw["timing_state"] != "start-tz-too-long" {
		t.Errorf("row 22: start_timezone %v, raw %v", r22.Payload["start_timezone"], raw)
	}

	uid := "uid-" + strings.Repeat("u", 1996)
	url := "https://example.test/" + strings.Repeat("p", 1004) // 1,025 bytes
	okURL := url[:1024]
	grown := strings.Repeat("\xff", 400) // 400 stored bytes, 1,200 once cleaned
	endZone := "End/" + strings.Repeat("e", 300)
	s := newBuilt()
	s.event(1, map[string]any{"unique_identifier": uid, "url": url, "external_id": grown})
	s.event(2, map[string]any{"url": okURL})
	s.event(3, map[string]any{"end_tz": endZone})
	got := byRowid(t, parseBuilt(t, s.bytes(), false))
	raw = rawOf(got[1])
	for _, c := range []struct{ field, col, stored string }{{"uid", "unique_identifier", uid}, {"url", "url", url}, {"sync_id", "external_id", grown}} {
		if got[1].Payload[c.field] != nil || raw[c.col+"_state"] != "too-long" || raw[c.col+"_len"] != int64(len(c.stored)) || raw[c.col+"_sha256"] != sha(c.stored) {
			t.Errorf("%s: %v, raw %v %v %v", c.col, got[1].Payload[c.field], raw[c.col+"_state"], raw[c.col+"_len"], raw[c.col+"_sha256"])
		}
	}
	if payloadStr(got[2], "url") != okURL || rawOf(got[2])["url_state"] != nil {
		t.Errorf("a 1,024-byte url is kept: url %d bytes, url_state %v", len(payloadStr(got[2], "url")), rawOf(got[2])["url_state"])
	}
	// B10: an over-long end zone is not stated and its end is undecided, never a mismatch
	raw = rawOf(got[3])
	if got[3].Payload["end_timezone"] != nil || raw["end_tz_state"] != "too-long" || raw["end_tz_len"] != int64(len(endZone)) || raw["end_tz_sha256"] != sha(endZone) ||
		raw["end_state"] != "end-zone-undecided" || got[3].TimeEnd != nil {
		t.Errorf("an end zone over 256 bytes: end_timezone %v, TimeEnd %v, raw %v", got[3].Payload["end_timezone"], got[3].TimeEnd, raw)
	}
	if _, listed := unknownOf(got[3])["CalendarItem.end_tz"]; listed {
		t.Errorf("an over-long end zone is stated by its length and hash, not in unknown_columns: %v", unknownOf(got[3]))
	}
}

// Spec 4.1, 9, C6 (iOS half): participants, alarms, recurrence rules and exception dates keep at most 1,000 per event,
// with <list>_total and <list>_truncated; recurrence.stored_rules counts every rule; a title over 4 KiB is cut and
// named; a description over 256 KiB is cut with body_truncated and body_total_bytes (row 20).
func TestCalendarCapsAreCountedNeverSilent(t *testing.T) {
	s := newBuilt()
	s.event(1, map[string]any{"summary": strings.Repeat("t", 5<<10)})
	for i := int64(1); i <= 1500; i++ {
		s.add("Participant", i, map[string]any{"item_id": int64(1), "email": "a@example.test", "role": int64(1)})
	}
	for i := int64(1); i <= 1200; i++ {
		s.add("Alarm", i, map[string]any{"calendaritem_owner_id": int64(1), "trigger_interval": -60 * i})
	}
	for i := int64(1); i <= 1100; i++ {
		s.add("Recurrence", i, map[string]any{"owner_id": int64(1), "frequency": int64(1), "interval": i})
		s.add("ExceptionDate", i, map[string]any{"owner_id": int64(1), "date": float64(cD + i*cDay)})
	}
	r := byRowid(t, parseBuilt(t, s.bytes(), false))[1]
	raw := rawOf(r)
	for _, l := range []struct {
		label string
		m     map[string]any
		key   string
		total int64
	}{
		{"payload", r.Payload, "attendees", 1500},
		{"raw", raw, "alarms", 1200},
		{"raw", raw, "recurrence", 1100},
		{"raw", raw, "exception_dates", 1100},
	} {
		if got := listOf(l.m[l.key]); len(got) != 1000 || l.m[l.key+"_total"] != l.total || l.m[l.key+"_truncated"] != true {
			t.Errorf("%s.%s: %d kept, total %#v, truncated %#v; want 1000, %d, true", l.label, l.key, len(got), l.m[l.key+"_total"], l.m[l.key+"_truncated"], l.total)
		}
	}
	if len(listOf(raw["attendees"])) != 1000 {
		t.Errorf("raw.attendees has %d entries, want 1000 (aligned with the kept list)", len(listOf(raw["attendees"])))
	}
	if !sameJSON(t, r.Payload["recurrence"], map[string]any{"stored_rules": 1100}) {
		t.Errorf("recurrence %v: stored_rules counts every rule", r.Payload["recurrence"])
	}
	if len(payloadStr(r, "title")) > 4<<10 || !sameJSON(t, r.Payload["truncated_fields"], []any{"title"}) {
		t.Errorf("title %d bytes, truncated_fields %v", len(payloadStr(r, "title")), r.Payload["truncated_fields"])
	}
	r20 := byRowid(t, parseVariant(t, "calendar-ios"))[20]
	if r20.Payload["body_truncated"] != true || r20.Payload["body_total_bytes"] != int64(300<<10) || len(r20.Body) > 256<<10 {
		t.Errorf("row 20: body_truncated %v, body_total_bytes %v, Body %d bytes", r20.Payload["body_truncated"], r20.Payload["body_total_bytes"], len(r20.Body))
	}
}

// Plan 4I3 Task 9 (deferred from Task 8): with a companion Probe defers, so Parse refuses a CalendarItem whose ROWID
// is not the rowid alias (the source ids and every join would name other rows), with no record.
func TestIOSCalendarParseChecksTheRowidAliasWhenDeferred(t *testing.T) {
	noAlias := goStoreDDL("CalendarItem", `CREATE TABLE CalendarItem (ROWID INTEGER, start_date REAL, start_tz TEXT, all_day INTEGER, calendar_id INTEGER)`)
	res := parseAt(t, map[string][]byte{"db": noAlias, "wal": {}}, true)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "rowid alias") || len(res.Records) != 0 {
		t.Errorf("CalendarItem without the alias, deferred: Parse err %v, %d records", res.Err, len(res.Records))
	}
}
