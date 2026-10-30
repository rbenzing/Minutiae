package iossms_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// t0 is 2023-05-01T10:00:00Z, the instant of the scenario's row 0; the date of row i is t0 + i hours
// unless a row says otherwise.
var t0 = time.Date(2023, 5, 1, 10, 0, 0, 0, time.UTC)

const (
	guidP  = "E8F3C1A2-4B5D-4E6F-8A9B-0C1D2E3F4A5B" // the target of the reaction written with "p:0/"
	guidBP = "0A1B2C3D-4E5F-4A6B-9C7D-8E9F0A1B2C3D" // the target of the reaction written with "bp:"
	attDir = "ios:MediaDomain/Library/SMS/Attachments/"
)

func rawOf(r parsertest.ExpRecord) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func byRowid(t *testing.T, variant string) map[int64]parsertest.ExpRecord {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		if r.Type != "message" || r.Table != "message" {
			t.Errorf("%s rowid %d: type %q table %q", variant, r.Rowid, r.Type, r.Table)
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

func wantTimes(t *testing.T, label string, got map[string]string, want map[string]time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: times %v, want %d entries", label, got, len(want))
	}
	for k, w := range want {
		s, ok := got[k]
		if g, err := time.Parse(time.RFC3339Nano, s); !ok || err != nil || !g.Equal(w) {
			t.Errorf("%s: times[%s] = %q, want %s", label, k, s, w.Format(time.RFC3339Nano))
		}
	}
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

func participantAddrs(r parsertest.ExpRecord) (roles, addrs []string) {
	l, _ := r.Payload["participants"].([]any)
	for _, e := range l {
		m, _ := e.(map[string]any)
		roles = append(roles, m["role"].(string))
		addrs = append(addrs, m["address"].(string))
	}
	return roles, addrs
}

func objOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// The spec facts of the ios17-nanos scenario, written from the rules (the contract table and rulings 4E-R14 to
// R20, R31 and R41), not read back from the generator. The row numbers are the scenario's message ROWIDs.
func TestOracleStatesTheSpecFacts(t *testing.T) {
	rows := byRowid(t, "ios17-nanos")
	if len(rows) != 45 {
		t.Fatalf("%d records, the scenario writes 45 messages", len(rows))
	}
	fx := parsertest.LoadFixture(t, "testdata", "ios17-nanos")

	// 4E-R17: the reaction target is the GUID after "p:0/" or "bp:"; a malformed value has none and stays in raw
	if rows[11].Payload["target_guid"] != guidP || rawOf(rows[11])["associated_message_guid"] != "p:0/"+guidP || rows[11].Payload["kind"] != "reaction" {
		t.Errorf("row 11: %v", rows[11].Payload)
	}
	if rows[12].Payload["target_guid"] != guidBP || rawOf(rows[12])["associated_message_guid"] != "bp:"+guidBP || rows[12].Payload["kind"] != "reaction" {
		t.Errorf("row 12: %v", rows[12].Payload)
	}
	if _, has := rows[13].Payload["target_guid"]; has || rawOf(rows[13])["associated_message_guid"] != "x:abc" || rows[13].Payload["kind"] != "reaction" {
		t.Errorf("row 13: %v", rows[13].Payload)
	}
	// 4E-R15: date 0 is a sentinel, a nanosecond value keeps its exact nanoseconds, seconds stay seconds, 2100 is invalid
	if rows[15].Time != nil || rawOf(rows[15])["date_status"] != "sentinel" {
		t.Errorf("row 15 (date 0): time %v raw %v", rows[15].Time, rawOf(rows[15]))
	}
	if _, has := rawOf(rows[15])["date_unit"]; has {
		t.Errorf("row 15: a sentinel has no unit: %v", rawOf(rows[15]))
	}
	wantTime(t, "row 17", rows[17].Time, t0.Add(17*time.Hour+987654321*time.Nanosecond))
	if rawOf(rows[17])["date_unit"] != "nanoseconds" || num(t, rawOf(rows[17])["date"]) != "704689200987654321" {
		t.Errorf("row 17: raw %v", rawOf(rows[17]))
	}
	wantTime(t, "row 2", rows[2].Time, t0.Add(2*time.Hour+123456789*time.Nanosecond))
	wantTime(t, "row 16", rows[16].Time, time.Date(2023, 3, 8, 20, 26, 40, 0, time.UTC))
	if rawOf(rows[16])["date_unit"] != "seconds" || num(t, rawOf(rows[16])["date"]) != "700000000" {
		t.Errorf("row 16: raw %v", rawOf(rows[16]))
	}
	if rows[18].Time != nil || rawOf(rows[18])["date_status"] != "invalid" || rawOf(rows[18])["date_unit"] != "seconds" {
		t.Errorf("row 18 (year 2100): time %v raw %v", rows[18].Time, rawOf(rows[18]))
	}
	// 4E-R16: the channel is the service, exactly
	for id, ch := range map[int64]string{1: "sms", 3: "imessage", 30: "rcs", 31: "unknown"} {
		if rows[id].Payload["channel"] != ch {
			t.Errorf("row %d: channel %v, want %s", id, rows[id].Payload["channel"], ch)
		}
	}
	if rows[30].Payload["service"] != "RCS" || rawOf(rows[30])["service"] != "RCS" {
		t.Errorf("row 30: %v", rows[30].Payload)
	}
	if _, has := rows[31].Payload["service"]; has {
		t.Errorf("row 31 (NULL service) states a service: %v", rows[31].Payload["service"])
	}
	if _, has := rawOf(rows[31])["service"]; has {
		t.Errorf("row 31 (NULL service): raw %v", rawOf(rows[31]))
	}
	// the recently-deleted message is the app's own marker, with the time it was deleted
	r22 := rows[22]
	if !r22.Deleted || objOf(r22.Payload["deleted"])["source"] != "chat_recoverable_message_join" {
		t.Errorf("row 22: deleted %v payload %v", r22.Deleted, r22.Payload["deleted"])
	}
	wantTimes(t, "row 22", r22.Times, map[string]time.Time{"deleted": t0.Add(22*time.Hour + 24*time.Hour)})
	if num(t, rawOf(r22)["recoverable_chat_id"]) != "1" || rawOf(r22)["thread_state"] != "no-chat-join" {
		t.Errorf("row 22: raw %v", rawOf(r22))
	}
	for id, r := range rows {
		if id != 22 && (r.Deleted || r.Payload["deleted"] != nil) {
			t.Errorf("row %d is marked deleted", id)
		}
	}
	// is_from_me = 2 is no direction
	if rows[28].Payload["direction"] != "unknown" || rows[28].Payload["status"] != "unknown" || num(t, rawOf(rows[28])["is_from_me"]) != "2" {
		t.Errorf("row 28: %v", rows[28].Payload)
	}
	if _, has := rows[28].Payload["read"]; has {
		t.Errorf("row 28 (unknown direction) has read")
	}
	// status: error beats delivered beats sent
	for id, st := range map[int64]string{1: "received", 2: "delivered", 29: "failed", 43: "sent", 44: "unknown"} {
		if rows[id].Payload["status"] != st {
			t.Errorf("row %d: status %v, want %s", id, rows[id].Payload["status"], st)
		}
	}
	// 4E-R31: read only for incoming messages
	if rows[1].Payload["read"] != true || rows[20].Payload["read"] != false {
		t.Errorf("read: row 1 %v, row 20 %v", rows[1].Payload["read"], rows[20].Payload["read"])
	}
	if _, has := rows[2].Payload["read"]; has {
		t.Errorf("row 2 (outgoing) has read %v", rows[2].Payload["read"])
	}
	// every time column: valid values become Times; date_read 0 on an unread incoming message is no time
	wantTimes(t, "row 19", rows[19].Times, map[string]time.Time{
		"delivered": t0.Add(19*time.Hour + 2*time.Second), "read": t0.Add(19*time.Hour + time.Minute),
		"played": t0.Add(19*time.Hour + 90*time.Second), "edited": t0.Add(19*time.Hour + 10*time.Minute),
		"retracted": t0.Add(19*time.Hour + time.Hour),
	})
	if len(rows[20].Times) != 0 || rawOf(rows[20])["date_read_status"] != "sentinel" || num(t, rawOf(rows[20])["date_read"]) != "0" {
		t.Errorf("row 20: times %v raw %v", rows[20].Times, rawOf(rows[20]))
	}
	// edit history: two edits under ec; the keys of the summary are sorted
	r21 := rawOf(rows[21])
	if num(t, r21["edit_count"]) != "2" || strings.Join(strs(objOf(r21["summary_info"])["keys"]), ",") != "amc,ec,ep,otr" {
		t.Errorf("row 21: raw %v", r21)
	}
	wantTimes(t, "row 21", rows[21].Times, map[string]time.Time{"edited": t0.Add(21*time.Hour + 5*time.Minute)})
	// payload_data: the root class of the archive; a corrupt one is flagged, not hidden
	if objOf(rawOf(rows[39])["payload_data"])["root_class"] != "LPLinkMetadata" {
		t.Errorf("row 39: raw %v", rawOf(rows[39]))
	}
	if rawOf(rows[40])["payload_data_status"] != "undecodable" || rawOf(rows[40])["payload_data"] != nil {
		t.Errorf("row 40: raw %v", rawOf(rows[40]))
	}
	if len(fx.Expect.Warnings) != 1 || fx.Expect.Warnings[0].Table != "message" || fx.Expect.Warnings[0].Rowid != 40 {
		t.Errorf("warnings %+v, want exactly one for message 40", fx.Expect.Warnings)
	}
	// 4E-R14: attributedBody
	if rows[3].Payload["text_source"] != "attributedBody" || rows[3].Payload["kind"] != "text" || rows[3].Body == nil ||
		*rows[3].Body != "Great job \U0001F44D\U0001F3FD family \U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466 flag \U0001F1E8\U0001F1E6 done" {
		t.Errorf("row 3: %v body %v", rows[3].Payload, rows[3].Body)
	}
	if rows[4].Payload["text_source"] != "text" || rawOf(rows[4])["attributed_body_differs"] != nil || rawOf(rows[4])["attributed_body_status"] != nil {
		t.Errorf("row 4 (equal): %v", rows[4].Payload)
	}
	if rows[5].Payload["text_source"] != "text" || rows[5].Body == nil || *rows[5].Body != "text column wins" || rawOf(rows[5])["attributed_body_differs"] != true {
		t.Errorf("row 5 (differs): %v body %v", rows[5].Payload, rows[5].Body)
	}
	if fx.Expect.Notes["attributed_body_differs"] != "1" {
		t.Errorf("notes %v, want attributed_body_differs = 1", fx.Expect.Notes)
	}
	r6 := rows[6]
	if fl := strs(r6.Payload["body_flags"]); len(fl) != 1 || fl[0] != "invalid_utf8" || r6.Payload["body_raw_b64"] != "YmFkIP/+IGJ5dGVz" || r6.Payload["text_source"] != "attributedBody" {
		t.Errorf("row 6: %v", r6.Payload)
	}
	r7 := rows[7]
	if r7.Body != nil || rawOf(r7)["attributed_body_status"] != "undecodable" || r7.Payload["kind"] != "unknown" {
		t.Errorf("row 7 (truncated attributedBody, no text): %v body %v", r7.Payload, r7.Body)
	}
	if _, has := r7.Payload["text_source"]; has {
		t.Errorf("row 7 has a text_source")
	}
	if rows[8].Payload["text_source"] != "text" || rawOf(rows[8])["attributed_body_status"] != "undecodable" || rows[8].Payload["kind"] != "text" {
		t.Errorf("row 8 (text and a truncated attributedBody): %v", rows[8].Payload)
	}
	// text hygiene
	if fl := strs(rows[32].Payload["body_flags"]); len(fl) != 1 || fl[0] != "nul" || rows[32].Payload["body_raw_b64"] != "bnVsAGluIHRleHQ=" {
		t.Errorf("row 32: %v", rows[32].Payload)
	}
	if rows[33].Payload["body_truncated"] != true || num(t, rows[33].Payload["body_total_bytes"]) != "307200" || rows[33].Body == nil || len(*rows[33].Body) != 256<<10 {
		t.Errorf("row 33: payload %v", rows[33].Payload)
	}
	if got := strs(rawOf(rows[45])["text_flags"]); len(got) != 1 || got[0] != "subject" || rows[45].Payload["subject"] != "bad � subject" {
		t.Errorf("row 45: payload %v", rows[45].Payload)
	}
	// 4E-R17/R41: system items; an unreadable item_type or associated_message_type is "unknown", never "text"
	if rows[14].Payload["kind"] != "system" || rawOf(rows[14])["group_title"] != "Weekend plans" {
		t.Errorf("row 14: %v", rows[14].Payload)
	}
	r37, r38 := rows[37], rows[38]
	if r37.Payload["kind"] != "unknown" || r37.Body == nil || objOf(rawOf(r37)["unknown_columns"])["item_type"] != "null" {
		t.Errorf("row 37 (NULL item_type): %v", r37.Payload)
	}
	if r38.Payload["kind"] != "unknown" || r38.Body == nil || objOf(rawOf(r38)["unknown_columns"])["associated_message_type"] != "type-mismatch" {
		t.Errorf("row 38 (TEXT associated_message_type): %v", r38.Payload)
	}
	// 4E-R20: participants and threads
	if roles, addrs := participantAddrs(rows[2]); strings.Join(roles, ",") != "to,to,to" ||
		strings.Join(addrs, ",") != "+15551230001,alice@example.test,+15551230004" || rows[2].Payload["participants_unknown"] != nil {
		t.Errorf("row 2: participants %v %v", roles, addrs)
	}
	th := objOf(rows[2].Payload["thread"])
	if th["id"] != "3" || th["title"] != "Weekend plans" || th["group"] != true || len(strs(th["members"])) != 3 {
		t.Errorf("row 2: thread %v", th)
	}
	if roles, addrs := participantAddrs(rows[1]); strings.Join(roles, ",") != "from" || strings.Join(addrs, ",") != "+15551230001" || objOf(rows[1].Payload["thread"])["group"] != false {
		t.Errorf("row 1: participants %v %v thread %v", roles, addrs, rows[1].Payload["thread"])
	}
	if rows[23].Payload["thread"] != nil || rawOf(rows[23])["thread_state"] != "several-chats" {
		t.Errorf("row 23: %v", rows[23].Payload)
	}
	if ids, _ := rawOf(rows[23])["chat_ids"].([]any); len(ids) != 2 || num(t, ids[0]) != "1" || num(t, ids[1]) != "4" {
		t.Errorf("row 23: chat_ids %v", rawOf(rows[23])["chat_ids"])
	}
	if roles, addrs := participantAddrs(rows[23]); strings.Join(roles, ",") != "to" || strings.Join(addrs, ",") != "alice@example.test" {
		t.Errorf("row 23: participants %v %v", roles, addrs)
	}
	if rows[24].Payload["thread"] != nil || rawOf(rows[24])["thread_state"] != "no-chat-join" {
		t.Errorf("row 24: %v", rows[24].Payload)
	}
	if roles, addrs := participantAddrs(rows[24]); strings.Join(roles, ",") != "to" || strings.Join(addrs, ",") != "+15551230001" {
		t.Errorf("row 24: participants %v %v", roles, addrs)
	}
	if rows[25].Payload["participants_unknown"] != true || len(rows[25].Payload["participants"].([]any)) != 0 || rawOf(rows[25])["handle_state"] != nil {
		t.Errorf("row 25 (handle_id 0): %v", rows[25].Payload)
	}
	if rows[26].Payload["participants_unknown"] != true || rawOf(rows[26])["handle_state"] != "no-handle-row" {
		t.Errorf("row 26 (dangling handle_id): %v", rows[26].Payload)
	}
	if th := objOf(rows[30].Payload["thread"]); th["id"] != "6" {
		t.Errorf("row 30: thread %v", th)
	}
	// one number under two handles stays two messages with the same address
	_, a1 := participantAddrs(rows[1])
	_, a3 := participantAddrs(rows[3])
	if a1[0] != a3[0] || rows[1].Payload["channel"] == rows[3].Payload["channel"] {
		t.Errorf("rows 1 and 3: %v %v", a1, a3)
	}
	// 4E-R19: attachments link only the file of the message's own backup
	atts := func(id int64) []map[string]any {
		var out []map[string]any
		l, _ := rows[id].Payload["attachments"].([]any)
		for _, e := range l {
			out = append(out, objOf(e))
		}
		return out
	}
	a10 := atts(10)
	if len(a10) != 2 || a10[0]["artifact_id"] != "@id:"+attDir+"ab/12/IMG_0001.jpeg" || a10[0]["artifact_sha256"] != "@sha256:"+attDir+"ab/12/IMG_0001.jpeg" ||
		a10[0]["source_ref"] != "~/Library/SMS/Attachments/ab/12/IMG_0001.jpeg" || a10[0]["name"] != "IMG_0001.jpeg" || a10[0]["mime"] != "image/jpeg" ||
		a10[1]["artifact_id"] != nil || a10[1]["source_ref"] != "~/Library/SMS/Attachments/bb/22/missing.jpeg" || rows[10].Payload["kind"] != "media" {
		t.Errorf("row 10: attachments %v kind %v", a10, rows[10].Payload["kind"])
	}
	if num(t, rawOf(rows[10])["attachments_resolved"]) != "1" || num(t, rawOf(rows[10])["attachments_unresolved"]) != "1" {
		t.Errorf("row 10: raw %v", rawOf(rows[10]))
	}
	if a := atts(27); len(a) != 1 || a[0]["artifact_id"] != "@id:"+attDir+"cd/34/IMG_0002.heic" || a[0]["source_ref"] != "/var/mobile/Library/SMS/Attachments/cd/34/IMG_0002.heic" || rows[27].Payload["kind"] != "text" {
		t.Errorf("row 27: attachments %v", atts(27))
	}
	if a := atts(35); len(a) != 1 || a[0]["artifact_id"] != nil || num(t, rawOf(rows[35])["attachments_other_source"]) != "1" || num(t, rawOf(rows[35])["attachments_unresolved"]) != "0" {
		t.Errorf("row 35 (the file exists only in the backup of UDID2): %v raw %v", atts(35), rawOf(rows[35]))
	}
	if a := atts(36); len(a) != 1 || a[0]["artifact_id"] != nil || strings.Count(a[0]["source_ref"].(string), "*") != 1 || num(t, rawOf(rows[36])["attachments_unresolved"]) != "1" {
		t.Errorf("row 36 (a * in the name, the file exists): %v raw %v", atts(36), rawOf(rows[36]))
	}
	// two identical messages are two records
	if rows[41].Body == nil || rows[42].Body == nil || *rows[41].Body != *rows[42].Body || rows[41].Time == nil || rows[42].Time == nil || *rows[41].Time != *rows[42].Time {
		t.Errorf("rows 41 and 42 are not identical twins")
	}
	// the provenance of every cell: the backup path of the one file, T48 with 4E-R10
	for id, r := range rows {
		if r.SourcePath != backupPath {
			t.Errorf("row %d: source_path %q, want %q", id, r.SourcePath, backupPath)
		}
		if r.TimeEnd != nil {
			t.Errorf("row %d: a message has no time_end", id)
		}
	}
	// the schema note: the optional mapped columns the snapshot has
	if sch := fx.Expect.Notes["schema.message"]; !strings.HasPrefix(sch, "columns:") || !strings.Contains(sch, ";optional:") || !strings.Contains(sch, "date_edited") {
		t.Errorf("notes %v", fx.Expect.Notes)
	}
}

// The WAL variant: the newest messages and one edit exist only in the -wal and say so.
func TestOracleStatesTheWALOnlyMessages(t *testing.T) {
	rows := byRowid(t, "ios17-wal")
	if len(rows) != 11 {
		t.Fatalf("%d records, want 8 in the database and 3 only in the WAL", len(rows))
	}
	for id, r := range rows {
		inWAL := id == 5 || id >= 9
		if inWAL && (r.File != "wal" || !strings.HasPrefix(r.SourcePath, "UDID1/") || r.SourcePath == backupPath) {
			t.Errorf("row %d: file %q source_path %q, want the -wal", id, r.File, r.SourcePath)
		}
		if !inWAL && r.File != "any" {
			t.Errorf("row %d: file %q", id, r.File)
		}
	}
	wantTimes(t, "row 5", rows[5].Times, map[string]time.Time{"edited": t0.Add(5*time.Hour + 5*time.Minute)})
	if rows[5].Body == nil || *rows[5].Body != "edited in the wal" || num(t, rawOf(rows[5])["edit_count"]) != "1" {
		t.Errorf("row 5: body %v raw %v", rows[5].Body, rawOf(rows[5]))
	}
	for id := int64(9); id <= 11; id++ {
		if rows[id].Body == nil || !strings.HasPrefix(*rows[id].Body, "only in the wal") {
			t.Errorf("row %d: body %v", id, rows[id].Body)
		}
	}
}

// ios14: the attributedBody era; no edit, retraction or recently-deleted columns, so none of those times exist.
func TestOracleStatesTheIOS14Facts(t *testing.T) {
	rows := byRowid(t, "ios14-attributed")
	if len(rows) != 12 {
		t.Fatalf("%d records, the scenario writes 12 messages", len(rows))
	}
	fx := parsertest.LoadFixture(t, "testdata", "ios14-attributed")
	if strings.Contains(fx.Expect.Notes["schema.message"], "date_edited") || strings.Contains(fx.Expect.Notes["schema.message"], "date_retracted") {
		t.Errorf("notes %v", fx.Expect.Notes)
	}
	fromBlob := 0
	for id, r := range rows {
		for _, k := range []string{"edited", "retracted", "deleted"} {
			if _, has := r.Times[k]; has {
				t.Errorf("row %d has a %s time", id, k)
			}
		}
		if r.Deleted {
			t.Errorf("row %d is deleted", id)
		}
		if r.Payload["text_source"] == "attributedBody" {
			fromBlob++
		}
	}
	if fromBlob != 6 {
		t.Errorf("%d bodies come from attributedBody, want 6", fromBlob)
	}
}

// ios10: seconds only, no attributedBody, payload or reaction columns; three short records were written before an
// ALTER TABLE ADD COLUMN.
func TestOracleStatesTheIOS10Facts(t *testing.T) {
	rows := byRowid(t, "ios10-seconds")
	if len(rows) != 10 {
		t.Fatalf("%d records, the scenario writes 10 messages", len(rows))
	}
	fx := parsertest.LoadFixture(t, "testdata", "ios10-seconds")
	sch := fx.Expect.Notes["schema.message"]
	for _, col := range []string{"attributedBody", "payload_data", "message_summary_info", "associated_message_type", "balloon_bundle_id"} {
		if strings.Contains(sch, col) {
			t.Errorf("notes %v mention %s", fx.Expect.Notes, col)
		}
	}
	for id, r := range rows {
		if u, has := rawOf(r)["date_unit"]; has && u != "seconds" {
			t.Errorf("row %d: date_unit %v", id, u)
		}
		if r.Payload["kind"] != "text" {
			t.Errorf("row %d: kind %v, a variant without associated_message_type is no reaction", id, r.Payload["kind"])
		}
		if r.Payload["text_source"] != "text" {
			t.Errorf("row %d: text_source %v", id, r.Payload["text_source"])
		}
	}
	wantTime(t, "row 1", rows[1].Time, t0.Add(1*time.Hour))
	wantTime(t, "row 6", rows[6].Time, t0.Add(6*time.Hour+500*time.Millisecond))
	if num(t, rawOf(rows[6])["date"]) != "704649600.5" {
		t.Errorf("row 6 (a REAL date): %v", rawOf(rows[6]))
	}
}
