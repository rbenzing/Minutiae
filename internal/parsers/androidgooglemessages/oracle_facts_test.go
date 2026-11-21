package androidgooglemessages_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// rec is one expected record of an oracle, read through the names the spec's facts use.
type rec struct {
	r   parsertest.ExpRecord
	p   map[string]any
	raw map[string]any
}

func (e rec) str(k string) string {
	s, _ := e.p[k].(string)
	return s
}

func (e rec) obj(k string) map[string]any {
	m, _ := e.p[k].(map[string]any)
	return m
}

func (e rec) rawObj(k string) map[string]any {
	m, _ := e.raw[k].(map[string]any)
	return m
}

func (e rec) rawStr(k string) string {
	s, _ := e.raw[k].(string)
	return s
}

func (e rec) body() string {
	if e.r.Body == nil {
		return ""
	}
	return *e.r.Body
}

// parties is the participants as "<role> <address>", names and kinds apart.
func (e rec) parties() []string {
	list, _ := e.p["participants"].([]any)
	out := []string{}
	for _, x := range list {
		m, _ := x.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v", m["role"], m["address"]))
	}
	return out
}

// party0 is the first participant.
func (e rec) party0() map[string]any {
	list, _ := e.p["participants"].([]any)
	if len(list) == 0 {
		return nil
	}
	m, _ := list[0].(map[string]any)
	return m
}

func (e rec) unknownParts() bool { return e.p["participants_unknown"] == true }

func (e rec) threadStr(k string) string { s, _ := e.obj("thread")[k].(string); return s }

func (e rec) members() []string {
	list, _ := e.obj("thread")["members"].([]any)
	out := []string{}
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func (e rec) attachments() []map[string]any {
	list, _ := e.p["attachments"].([]any)
	var out []map[string]any
	for _, x := range list {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out
}

// num renders a JSON number of the oracle (decoded with UseNumber) as its decimal text; "" when absent.
func num(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprintf("not a number: %v", v)
}

// at parses an RFC 3339 instant: the oracle writes 22:13:20.5Z, the facts below 22:13:20.500Z.
func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func (e rec) timeIs(s string) bool {
	return e.r.Time != nil && at(*e.r.Time).Equal(at(s))
}

func (e rec) kindTime(kind, s string) bool {
	v, ok := e.r.Times[kind]
	return ok && at(v).Equal(at(s))
}

func (e rec) noTime() bool { return e.r.Time == nil }

// oracleRecords indexes the expected records of a variant by table and rowid.
func oracleRecords(t *testing.T, variant string) (map[string]map[int64]rec, parsertest.Expect) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[string]map[int64]rec{}
	for _, r := range fx.Expect.Records {
		raw, _ := r.Payload["raw"].(map[string]any)
		if out[r.Table] == nil {
			out[r.Table] = map[int64]rec{}
		}
		out[r.Table][r.Rowid] = rec{r: r, p: r.Payload, raw: raw}
	}
	return out, fx.Expect
}

// TestAndroidGoogleMessagesOracleStatesTheSpecRules: the oracle's literal facts, written from the spec (§6.1-§6.8, §7
// R1-R15, §8.2 for the orphan shape, the plan's calls 4I5-P2 to P5 and P9, B2-B7, C1, C9) and never read back from the
// generator, on the CORE scenario of gm-aosp-v2 and the shapes of the other variants. A generator that drifts from the
// rules fails here, not silently in the pins.
func TestAndroidGoogleMessagesOracleStatesTheSpecRules(t *testing.T) {
	all, exp := oracleRecords(t, "gm-aosp-v2")
	msgs, parts := all["messages"], all["parts"]
	if len(msgs) != 36 {
		t.Fatalf("%d messages records, want 36 (M1-M36)", len(msgs))
	}
	want := func(id string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("fact %s does not hold", id)
		}
	}
	msg := func(n int64) rec {
		t.Helper()
		e, ok := msgs[n]
		if !ok {
			t.Fatalf("no messages record for rowid %d", n)
		}
		return e
	}
	part := func(n int64) rec {
		t.Helper()
		e, ok := parts[n]
		if !ok {
			t.Fatalf("no parts record for rowid %d", n)
		}
		return e
	}
	for n, e := range msgs {
		_, has := e.p["guid"]
		want(fmt.Sprintf("M%d has no guid (C2)", n), !has && e.r.Type == "message" && e.rawStr("app") == "google_messages" && !e.r.Deleted)
	}
	// §6.1-§6.8: an incoming SMS from a resolved contact; the time is the received column, the sent column is Times.sent
	m1 := msg(1)
	want("M1 shape", m1.str("channel") == "sms" && m1.str("direction") == "in" && m1.str("kind") == "text" && m1.str("status") == "received" &&
		m1.body() == "Hello from Ann" && m1.str("text_source") == "part" && m1.p["read"] == true && num(m1.p["subscription"]) == "1")
	want("M1 party", reflect.DeepEqual(m1.parties(), []string{"from 5550000002"}) && m1.party0()["name"] == "Ann Example" && m1.party0()["kind"] == "phone")
	want("M1 times", m1.timeIs("2023-11-14T22:13:21Z") && m1.kindTime("received", "2023-11-14T22:13:21Z") && m1.kindTime("sent", "2023-11-14T22:13:20.500Z") && len(m1.r.Times) == 2)
	want("M1 thread", m1.threadStr("id") == "1" && m1.threadStr("title") == "Ann Example" && m1.obj("thread")["group"] == false && reflect.DeepEqual(m1.members(), []string{"5550000002"}))
	link := m1.rawObj("sms_link")
	want("M1 raw", link["authority"] == "sms" && num(link["id"]) == "41" && m1.rawStr("sms_link_id_reuse") == "possible" && m1.rawStr("label_message_status") == "incoming_complete" &&
		m1.rawStr("label_message_protocol") == "sms" && m1.rawStr("time_basis") == "received" && m1.rawStr("from_address_basis") == "send_destination" &&
		m1.rawStr("from_name_basis") == "cached_contact" && m1.raw["sender_is_self"] == false)
	// §6.3 C9, B2: an outgoing message has no from and lists the current non-self members as member
	m2 := msg(2)
	want("M2", m2.str("direction") == "out" && m2.str("status") == "sent" && reflect.DeepEqual(m2.parties(), []string{"member 5550000002"}) && m2.rawStr("members_basis") == "current" &&
		m2.raw["sender_is_self"] == true && m2.rawObj("sms_link")["authority"] == "sms" && num(m2.rawObj("sms_link")["id"]) == "42")
	// B2: the self participant P1 is a member row of C2 and is left out of the parties, the thread members and the count
	m3 := msg(3)
	three := []string{"+15550000003", "5550000002", "5550000004"}
	want("M3", m3.str("status") == "delivered" && reflect.DeepEqual(m3.parties(), []string{"member +15550000003", "member 5550000002", "member 5550000004"}) &&
		m3.obj("thread")["group"] == true && reflect.DeepEqual(m3.members(), three) && num(m3.raw["members_total"]) == "3")
	// §6.4, §6.5: text parts in id order joined with a newline; a media part is an attachment with source_ref only
	m4 := msg(4)
	att := m4.attachments()
	apart, _ := m4.raw["attachment_parts"].([]any)
	var ap0 map[string]any
	if len(apart) == 1 {
		ap0, _ = apart[0].(map[string]any)
	}
	want("M4 body", m4.str("channel") == "mms" && m4.str("kind") == "media" && m4.body() == "Look\nsecond line" && m4.str("subject") == "Pics" && m4.p["read"] == false)
	want("M4 attachment", len(att) == 1 && len(att[0]) == 2 && att[0]["mime"] == "image/jpeg" && att[0]["source_ref"] == "content://mms/part/90" &&
		ap0 != nil && num(ap0["part_id"]) == "5" && num(ap0["width"]) == "640" && num(ap0["height"]) == "480")
	want("M4 sender", reflect.DeepEqual(m4.parties(), []string{"from +15550000003"}) && m4.party0()["name"] == nil && m4.rawStr("from_full_name") == "+1 555-000-0003" &&
		m4.rawStr("from_address_basis") == "normalized_destination" && m4.rawObj("sms_link")["authority"] == "mms" && m4.rawStr("sms_link_id_reuse") == "none")
	// §6.1: protocol 2 is a WAP push notification, MMS transport; no parts is stated, never read as an empty body
	m5 := msg(5)
	want("M5", m5.str("channel") == "mms" && m5.rawStr("label_message_protocol") == "mms_push_notification" && m5.str("direction") == "in" && m5.str("status") == "unknown" &&
		m5.str("kind") == "unknown" && m5.r.Body == nil && num(m5.raw["parts"]) == "0" && m5.rawStr("body_state") == "no-parts" && num(m5.raw["mms_expiry"]) == "1700086400000" &&
		m5.rawStr("mms_transaction_id") == "tx-5" && m5.rawStr("mms_content_location") == "x-mms://example.invalid/m5" && m5.rawStr("sent_timestamp_status") == "sentinel" && m5.r.Times["sent"] == "")
	// §6.2: the cited status codes
	_, m6link := msg(6).raw["sms_message_uri"]
	_, m6link2 := msg(6).raw["sms_link"]
	_, m6link3 := msg(6).raw["sms_link_state"]
	want("M6", msg(6).str("direction") == "out" && msg(6).str("kind") == "text" && msg(6).str("status") == "draft" && !m6link && !m6link2 && !m6link3)
	want("M7 M8", msg(7).str("direction") == "out" && msg(7).str("status") == "failed" && msg(8).str("direction") == "out" && msg(8).str("status") == "queued" && msg(8).rawStr("label_message_status") == "outgoing_sending")
	want("M9", msg(9).str("direction") == "in" && msg(9).str("status") == "unknown" && msg(9).rawStr("label_message_status") == "incoming_expired_or_not_available")
	want("M10", msg(10).str("direction") == "unknown" && msg(10).str("status") == "unknown" && msg(10).rawStr("label_message_status") == "unknown")
	// B7: an uncited status is unknown even with a decidable sender; the sender fact stays
	m11 := msg(11)
	_, m11label := m11.raw["label_message_status"]
	_, m11conflict := m11.raw["direction_conflict"]
	want("M11", m11.str("direction") == "unknown" && m11.str("status") == "unknown" && !m11label && num(m11.raw["message_status"]) == "150" && m11.raw["sender_is_self"] == false &&
		reflect.DeepEqual(m11.parties(), []string{"from 5550000002"}) && !m11conflict)
	// C1, C1a: the transport is the cited protocol or the free token unknown, never a guess
	_, m13label := msg(13).raw["label_message_protocol"]
	_, m14proto := msg(14).raw["message_protocol"]
	want("M12-M14", msg(12).str("channel") == "unknown" && msg(13).str("channel") == "unknown" && msg(14).str("channel") == "unknown" && msg(12).rawStr("label_message_protocol") == "unknown" &&
		!m13label && num(msg(13).raw["message_protocol"]) == "7" && msg(14).rawStr("message_protocol_state") == "null" && !m14proto)
	// §7 R4, R5: a deleted sender (SET NULL) and dangling joins are stated, never empty
	want("M15", msg(15).unknownParts() && msg(15).rawStr("sender_state") == "absent")
	want("M16", msg(16).unknownParts() && msg(16).rawStr("sender_state") == "no-participants-row")
	_, m17sub := msg(17).p["subscription"]
	want("M17", msg(17).rawStr("self_row_state") == "no-participants-row" && !m17sub)
	want("M18", reflect.DeepEqual(msg(18).obj("thread"), map[string]any{"id": "97"}) && msg(18).rawStr("conversation_state") == "no-conversations-row")
	// §6.2 cross-check (B7): only a decided sender that contradicts a decided direction
	want("M19", msg(19).str("direction") == "unknown" && msg(19).str("status") == "received" && msg(19).rawObj("direction_conflict")["status_direction"] == "in" && msg(19).rawObj("direction_conflict")["sender_is_self"] == true)
	want("M20", msg(20).str("direction") == "unknown" && msg(20).str("status") == "sent" && msg(20).rawObj("direction_conflict")["status_direction"] == "out" && msg(20).rawObj("direction_conflict")["sender_is_self"] == false)
	conflict := false
	for _, w := range exp.Warnings {
		conflict = conflict || strings.Contains(w.Contains, "googlemessages-direction-conflict: 2 records")
	}
	want("conflict warning", conflict)
	// §6.6 B5, 4I5-P2: the sent time only on a stored 0; a 0 is no time; seconds, negative and after 2100 are invalid
	want("M21", msg(21).timeIs("2023-11-14T22:13:41Z") && msg(21).rawStr("time_basis") == "sent" && msg(21).rawStr("received_timestamp_status") == "sentinel")
	want("M22", msg(22).noTime() && msg(22).rawStr("time_basis") == "absent")
	for _, n := range []int64{23, 24, 25} {
		want(fmt.Sprintf("M%d", n), msg(n).noTime() && msg(n).rawStr("time_basis") == "invalid" && msg(n).rawStr("received_timestamp_status") == "invalid")
	}
	// §6.8: the telephony link is a stated reference parsed strictly; an identifier over 1 KiB is never cut
	want("M26", msg(26).rawStr("sms_link_state") == "unparsed" && msg(26).rawStr("sms_message_uri") == "content://sms/12a")
	_, m27uri := msg(27).raw["sms_message_uri"]
	want("M27", !m27uri && msg(27).rawStr("sms_message_uri_state") == "too-long" && num(msg(27).raw["sms_message_uri_len"]) == "1114" && len(msg(27).rawStr("sms_message_uri_sha256")) == 64)
	want("M28", msg(28).rawStr("sms_link_state") == "unparsed")
	// §6.4: a part with both text and uri is both; a part with neither is counted
	m29 := msg(29)
	want("M29", num(m29.raw["parts_text_and_uri"]) == "1" && m29.body() == "both" && m29.str("kind") == "media" && len(m29.attachments()) == 1 &&
		m29.attachments()[0]["mime"] == "text/plain" && m29.attachments()[0]["source_ref"] == "content://mms/part/91")
	m30 := msg(30)
	want("M30", num(m30.raw["parts_empty"]) == "1" && m30.str("kind") == "unknown" && m30.r.Body == nil && m30.attachments() == nil)
	// §6.7: read is 0 or 1 only
	_, m31read := msg(31).p["read"]
	want("M31", !m31read && num(msg(31).raw["read"]) == "2")
	// §6.3: archive and blocking stay raw; a self row with a SIM; an undecided self-ness keeps the member and omits the group
	want("M32", msg(32).threadStr("title") == "" && num(msg(32).raw["archive_status"]) == "1" && reflect.DeepEqual(msg(32).raw["blocked_member_ids"], []any{json.Number("4")}) &&
		reflect.DeepEqual(msg(32).parties(), []string{"from 5550000004"}))
	want("M33", num(msg(33).p["subscription"]) == "2" && num(msg(33).raw["self_sub_id"]) == "2" && num(msg(33).raw["self_sim_slot_id"]) == "1" && msg(33).rawStr("self_subscription_name") == "SIM Two" &&
		reflect.DeepEqual(msg(33).parties(), []string{"member 5550000002"}))
	_, m34group := msg(34).obj("thread")["group"]
	want("M34", reflect.DeepEqual(msg(34).members(), []string{"5550000002", "5550000006"}) && msg(34).rawStr("self_state") == "undecided" && !m34group && msg(34).rawStr("group_state") == "undecided")
	_, m35conflict := msg(35).raw["direction_conflict"]
	want("M35", msg(35).unknownParts() && msg(35).rawStr("sender_state") == "self-undecided" && !m35conflict)
	want("M36", msg(36).party0()["address"] == "ann@example.test" && msg(36).party0()["kind"] == "email" && msg(36).party0()["name"] == "Ann Mail")
	// §8.2 orphan shape (R11): a part whose message_id names no message is a record of its own
	if len(parts) != 2 {
		t.Fatalf("%d parts records, want 2 (the orphan parts 900 and 901)", len(parts))
	}
	for _, id := range []int64{900, 901} {
		p := part(id)
		want(fmt.Sprintf("part %d", id), p.str("kind") == "text" && p.str("channel") == "unknown" && p.str("direction") == "unknown" && p.unknownParts() && p.rawStr("part_state") == "orphan" &&
			reflect.DeepEqual(p.obj("thread"), map[string]any{"id": "1"}) && p.noTime())
	}
	want("part 900", part(900).body() == "orphan text" && num(part(900).raw["part_message_id"]) == "999")
	want("part 901", part(901).rawStr("part_message_id_state") == "null" && part(901).body() == "null message id")
	// notes
	for k, v := range map[string]string{"orphan_parts": "2", "user_version": "2", "user_version_cited": "2", "journal_mode": "rollback", "app": "google_messages", "database_user": "0"} {
		if exp.Notes[k] != v {
			t.Errorf("note %s = %q, want %q", k, exp.Notes[k], v)
		}
	}

	// the other variants
	v1, v1exp := oracleRecords(t, "gm-aosp-v1")
	a, b := all["messages"][1], v1["messages"][1]
	delete(a.raw, "is_enterprise")
	want("v1 M1 is v2 M1 without is_enterprise", reflect.DeepEqual(a.p, b.p) && a.body() == b.body() && reflect.DeepEqual(a.r.Times, b.r.Times) && a.r.Time != nil && b.r.Time != nil && *a.r.Time == *b.r.Time)
	if v1exp.Notes["columns_missing.conversations"] != "IS_ENTERPRISE" {
		t.Errorf("gm-aosp-v1 columns_missing.conversations = %q, want IS_ENTERPRISE", v1exp.Notes["columns_missing.conversations"])
	}
	prop, pexp := oracleRecords(t, "gm-proprietary-shaped")
	if pexp.Notes["user_version"] != "9000" || pexp.Notes["user_version_cited"] != "no" || pexp.Notes["unmapped_columns.parts"] != "file_size_bytes,local_cache_path,fixture_extra_part" {
		t.Errorf("gm-proprietary-shaped notes %v", pexp.Notes)
	}
	want("proprietary M13", prop["messages"][13].str("channel") == "unknown")
	uncited := false
	for _, w := range pexp.Warnings {
		uncited = uncited || strings.Contains(w.Contains, "googlemessages-schema-uncited: user_version 9000")
	}
	want("proprietary uncited warning", uncited)
	wal, wexp := oracleRecords(t, "gm-aosp-v2-wal")
	_, has40 := wal["messages"][40]
	_, has41 := wal["messages"][41]
	want("wal: M40 live, M41 not", has40 && !has41 && wal["messages"][40].body() == "only in wal" && wexp.Notes["journal_mode"] == "wal")
	jr, _ := oracleRecords(t, "gm-journal")
	_, has42 := jr["messages"][42]
	want("journal: M42 rolled back", !has42 && len(jr["messages"]) == 2)
	for _, v := range []string{"gm-other-kind", "gm-not-sqlite"} {
		_, e := oracleRecords(t, v)
		if len(e.Records) != 0 {
			t.Errorf("%s states %d records, want none (no signature at the app path)", v, len(e.Records))
		}
	}
}
