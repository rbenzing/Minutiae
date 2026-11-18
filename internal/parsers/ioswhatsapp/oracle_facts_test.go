package ioswhatsapp_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator (tools/fixtures/whatsapp_ios_chats.py) is the scenario oracle; these literal facts are written from the spec
// (6.7, 6.8, 6.11, 7.1 to 7.3, 7.6) and the rulings 4I2-R3, R5, R6 and the state vocabulary 4I2-R7, never read back from the
// generator, so a generator that drifted from the rules fails here before any parser exists. The oracle and these facts are
// two readings of the same spec by one author (4I2-R13): they catch a divergence between the text and the implementation,
// not a reading both share.

type chatRecs struct {
	t  *testing.T
	by map[int64]parsertest.ExpRecord
}

func recsOf(t *testing.T, variant string) (chatRecs, parsertest.Fixture) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	c := chatRecs{t: t, by: map[int64]parsertest.ExpRecord{}}
	for _, r := range fx.Expect.Records {
		c.by[r.Rowid] = r
		if r.Table != "ZWAMESSAGE" || r.Type != "message" {
			t.Errorf("%s: record %s/%d of type %q", variant, r.Table, r.Rowid, r.Type)
		}
	}
	return c, fx
}

// rec is one record view.
type rec struct {
	parsertest.ExpRecord
	raw map[string]any
}

func (c chatRecs) rec(n int64) rec {
	c.t.Helper()
	r, ok := c.by[n]
	if !ok {
		c.t.Fatalf("no oracle record for ZWAMESSAGE %d", n)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	return rec{ExpRecord: r, raw: raw}
}

func (r rec) kind() any   { return r.Payload["kind"] }
func (r rec) status() any { return r.Payload["status"] }

func (r rec) unknownParticipants() bool {
	v, _ := r.Payload["participants_unknown"].(bool)
	return v
}

func (r rec) textSource() any { return r.Payload["text_source"] }

func (r rec) text() string {
	if r.Body == nil {
		return ""
	}
	return *r.Body
}

func (r rec) attachments() []any {
	a, _ := r.Payload["attachments"].([]any)
	return a
}

func (r rec) thread() map[string]any {
	th, _ := r.Payload["thread"].(map[string]any)
	return th
}

// addr is the address of the participant of the given role ("" when there is none).
func (r rec) addr(role string) string {
	ps, _ := r.Payload["participants"].([]any)
	for _, p := range ps {
		m, _ := p.(map[string]any)
		if m["role"] == role {
			return fmt.Sprint(m["address"])
		}
	}
	return ""
}

// rawIs compares a raw value with want as printed (JSON numbers decode as json.Number).
func (r rec) rawIs(key string, want any) bool {
	v, has := r.raw[key]
	return has && fmt.Sprint(v) == fmt.Sprint(want)
}

func (r rec) rawAbsent(key string) bool { _, has := r.raw[key]; return !has }

func (r rec) timeIs(want string) bool { return r.Time != nil && *r.Time == want }

func TestIOSWhatsAppChatsOracleStatesTheSpecRules(t *testing.T) {
	c, fx := recsOf(t, "ios-chatstorage")
	want := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			t.Errorf(format, args...)
		}
	}
	_, has46 := c.by[46]
	want(len(c.by) == 45 && !has46, "ios-chatstorage has %d records, want 45 (messages 1-44 in the file, 45 in a committed WAL frame, 46 in an uncommitted one: no record)", len(c.by))
	want(fx.Expect.Notes["entity.WAMessage"] == "9" && fx.Expect.Notes["entity.WAChatSession"] == "4", "entity notes %v", fx.Expect.Notes)

	// iOS ZMESSAGETYPE 2 = video, 3 = audio, the reverse of Android (spec 7.1, research 7.2)
	m5 := c.rec(5)
	want(m5.kind() == "media" && m5.raw["label_ZMESSAGETYPE"] == "video", "message 5: %v", m5.Payload)
	want(c.rec(6).raw["label_ZMESSAGETYPE"] == "audio", "message 6 label %v", c.rec(6).raw["label_ZMESSAGETYPE"])
	// delivery status: outgoing 6 and 8 are delivered, 8 labelled read_by_recipient, never read (spec 7.3)
	want(c.rec(2).status() == "delivered" && c.rec(3).status() == "delivered" && c.rec(3).raw["label_ZMESSAGESTATUS"] == "read_by_recipient",
		"messages 2 and 3: %v %v", c.rec(2).status(), c.rec(3).status())
	want(c.rec(1).status() == "unknown" && c.rec(1).rawAbsent("label_ZMESSAGESTATUS"), "message 1 (incoming) takes no status from ZMESSAGESTATUS")
	want(c.rec(1).Payload["direction"] == "in" && c.rec(2).Payload["direction"] == "out", "direction of 1 and 2")
	want(c.rec(37).Payload["direction"] == "unknown" && c.rec(37).rawIs("ZISFROMME", 2), "message 37 (ZISFROMME 2): %v", c.rec(37).Payload)
	// the incoming group sender is the member, never ZFROMJID, which is the group JID (S7)
	m10 := c.rec(10)
	want(m10.addr("from") == "15550000002@s.whatsapp.net" && m10.raw["ZFROMJID"] == "120363000000000001@g.us", "message 10: %v", m10.Payload)
	want(m10.thread()["group"] == true && m10.thread()["title"] == "Group A" && m10.thread()["id"] == "120363000000000001@g.us", "message 10 thread %v", m10.thread())
	m11 := c.rec(11)
	want(m11.unknownParticipants() && m11.raw["sender_state"] == "absent" && m11.addr("from") == "", "message 11: %v", m11.Payload)
	want(c.rec(12).raw["sender_state"] == "no-member-jid" && c.rec(12).unknownParticipants(), "message 12: %v", c.rec(12).raw)
	want(c.rec(13).raw["sender_state"] == "no-ZWAGROUPMEMBER-row" && c.rec(13).unknownParticipants(), "message 13: %v", c.rec(13).raw)
	want(c.rec(14).raw["sender_state"] == "not-a-person" && c.rec(14).unknownParticipants(), "message 14: %v", c.rec(14).raw)
	want(c.rec(15).addr("to") == "120363000000000001@g.us", "message 15 participants %v", c.rec(15).Payload["participants"])
	// an incoming status update has no attributed author; the stored columns stay raw (R3)
	m16 := c.rec(16)
	want(m16.unknownParticipants() && m16.raw["ZPUSHNAME"] == "Ann" && m16.raw["ZFROMJID"] == "15550000003@s.whatsapp.net" && m16.rawIs("ZGROUPMEMBER", 1),
		"message 16: %v", m16.Payload)
	want(m16.raw["chat_class"] == "status" && m16.raw["label_ZSESSIONTYPE"] == "status", "message 16 class %v", m16.raw)
	want(c.rec(17).addr("to") == "status@broadcast", "message 17 participants %v", c.rec(17).Payload["participants"])
	// group events (spec 7.2): 2 is participant_added, 12 is a conflict row and has no label
	m18 := c.rec(18)
	want(m18.kind() == "system" && m18.raw["label_ZGROUPEVENTTYPE"] == "participant_added", "message 18: %v", m18.Payload)
	want(c.rec(19).rawAbsent("label_ZGROUPEVENTTYPE") && c.rec(19).text() == "New name" && c.rec(19).rawIs("ZGROUPEVENTTYPE", 12), "message 19: %v", c.rec(19).Payload)
	want(c.rec(20).kind() == "system" && c.rec(20).raw["label_ZMESSAGETYPE"] == "system_notice" && c.rec(20).rawAbsent("label_ZGROUPEVENTTYPE"),
		"message 20: %v", c.rec(20).raw) // a type 10 row never reads ZGROUPEVENTTYPE (all unknown)
	// a revoke: kind unknown, no Body, deleted by the type, the row's text and the vCard name kept raw (spec 6.5, 6.11)
	m21 := c.rec(21)
	want(m21.kind() == "unknown" && m21.text() == "" && m21.Deleted && m21.raw["revoked_row_text"] == "leftover" && m21.raw["ZVCARDNAME"] == "x1" &&
		reflect.DeepEqual(m21.Payload["deleted"], map[string]any{"source": "ZMESSAGETYPE"}) && m21.textSource() == nil, "message 21: %v", m21.Payload)
	// an unmapped type is unknown with the stored value and no label (R4)
	want(c.rec(22).kind() == "unknown" && c.rec(22).rawAbsent("label_ZMESSAGETYPE") && c.rec(22).rawIs("ZMESSAGETYPE", 999), "message 22: %v", c.rec(22).raw)
	want(c.rec(23).kind() == "unknown" && c.rec(23).rawAbsent("label_ZMESSAGETYPE"), "message 23 (type 12): %v", c.rec(23).raw)
	// a contact card is the vCard of its item with text_source part, and no attachment; a location is raw only (type 5)
	m7 := c.rec(7)
	want(m7.text() == "BEGIN:VCARD\r\nFN:Ann\r\nEND:VCARD\r\n" && m7.textSource() == "part" && len(m7.attachments()) == 0 && m7.raw["body_column"] == "ZVCARDSTRING",
		"message 7: %v %q", m7.Payload, m7.text())
	m8, m9 := c.rec(8), c.rec(9)
	want(reflect.DeepEqual(m8.raw["location"], map[string]any{"latitude": json.Number("52.52"), "longitude": json.Number("13.405")}) && len(m8.attachments()) == 0,
		"message 8 location %v", m8.raw["location"])
	want(m9.raw["location"] == nil && m9.rawIs("ZLATITUDE", 640) && m9.rawIs("ZLONGITUDE", 480), "message 9 (type 1: latitude is the width): %v", m9.raw)
	// times: Cocoa seconds; a value out of range is invalid, a fraction is kept, NULL is no time (spec 7.6)
	want(c.rec(1).timeIs("2020-01-06T10:40:01Z"), "message 1 time %v", c.rec(1).Time)
	want(c.rec(25).Time == nil && c.rec(25).raw["ZMESSAGEDATE_status"] == "invalid", "message 25: %v", c.rec(25).raw)
	want(c.rec(24).Time == nil && c.rec(24).rawAbsent("ZMESSAGEDATE") && c.rec(24).rawAbsent("ZMESSAGEDATE_status"), "message 24 (NULL): %v", c.rec(24).raw)
	want(c.rec(26).timeIs("2020-01-06T10:40:00.25Z") && c.rec(27).timeIs("2020-01-06T10:40:00Z"), "messages 26, 27: %v %v", c.rec(26).Time, c.rec(27).Time)
	want(len(c.rec(1).Times) == 0 && c.rec(1).rawAbsent("ZSENTDATE"), "message 1 has no other time")
	// the app comes from the backup domain only (spec 6.8, 4I2-R5)
	want(c.rec(1).raw["app"] == "whatsapp" && c.rec(1).raw["app_basis"] == "backup_domain" && c.rec(1).rawAbsent("app_state"), "message 1 app %v", c.rec(1).raw)
	want(c.rec(1).Payload["guid"] == "3EB0A001" && c.rec(1).Payload["channel"] == "whatsapp", "message 1 guid")
	// the chat of a row: no session, an empty JID, a JID cleaning altered (never a thread id), an unmapped session type
	m35, m36, m42 := c.rec(35), c.rec(36), c.rec(42)
	want(m35.raw["chat_state"] == "no-contact-jid" && m35.unknownParticipants() && m35.thread()["id"] == nil, "message 35: %v", m35.Payload)
	want(m36.raw["chat_state"] == "no-ZWACHATSESSION-row" && m36.unknownParticipants() && m36.thread() == nil, "message 36: %v", m36.Payload)
	want(m42.raw["chat_state"] == "altered" && m42.thread()["id"] == nil && m42.unknownParticipants() && strings.Contains(fmt.Sprint(m42.raw["chat_jid"]), "�") &&
		reflect.DeepEqual(m42.raw["text_flags"], []any{"chat_jid"}) && m42.rawAbsent("chat_class"), "message 42: %v", m42.Payload)
	want(c.rec(39).addr("to") == "1234@broadcast" && c.rec(39).raw["label_ZSESSIONTYPE"] == "broadcast" && c.rec(39).thread()["group"] == false && c.rec(39).raw["chat_class"] == "broadcast",
		"message 39: %v", c.rec(39).Payload)
	m40, m41 := c.rec(40), c.rec(41)
	want(m40.addr("from") == "15550000004@s.whatsapp.net" && m40.thread()["id"] == "120363000000000002@g.us" && m40.thread()["group"] == nil,
		"message 40 (a group JID, ZSESSIONTYPE NULL): %v", m40.Payload)
	want(m41.unknownParticipants() && m41.raw["sender_state"] == "absent", "message 41: %v", m41.Payload)
	// a stored ZFROMJID that disagrees with the chat of a 1:1 row is a conflict; one that agrees, and a group row, are not
	want(reflect.DeepEqual(c.rec(34).raw["address_conflict"], []any{"ZFROMJID"}) && c.rec(34).addr("from") == "15550000001@s.whatsapp.net", "message 34: %v", c.rec(34).Payload)
	want(c.rec(1).rawAbsent("address_conflict") && c.rec(2).rawAbsent("address_conflict") && c.rec(10).rawAbsent("address_conflict") && c.rec(16).rawAbsent("address_conflict"),
		"an agreeing 1:1 row, a group row or a status row has no address_conflict")
	// a text over the cap is cut and flagged, never rejected
	m38 := c.rec(38)
	want(len(m38.text()) == 256<<10 && reflect.DeepEqual(m38.raw["text_flags"], []any{"body"}) && m38.Summary == nil, "message 38 body %d bytes, flags %v", len(m38.text()), m38.raw["text_flags"])
	// a receipt blob is never decoded: its length and SHA-256 (spec 8)
	m44 := c.rec(44)
	ri, _ := m44.raw["receipt_info"].(map[string]any)
	want(ri != nil && fmt.Sprint(ri["bytes"]) == "10" && len(fmt.Sprint(ri["sha256"])) == 64 && m44.rawAbsent("receipt_info_state"), "message 44 receipt %v", m44.raw)
	// media: the one link, a message whose file is of another source, a file that is missing, an undecidable join,
	// a path no lookup is made for, and no media row at all (4I2-R6, spec 6.7)
	m4 := c.rec(4)
	want(m4.text() == "pic" && m4.textSource() == "part" && m4.raw["body_column"] == "ZTITLE" && m4.rawAbsent("ZTITLE"), "message 4 body %q %v", m4.text(), m4.raw)
	a4, _ := m4.attachments()[0].(map[string]any)
	want(len(m4.attachments()) == 1 && a4["mime"] == "image/jpeg" && fmt.Sprint(a4["size"]) == "12345" &&
		a4["source_ref"] == "Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg" &&
		a4["artifact_id"] == "@id:ios:"+domainConsumer+"/Message/Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg", "message 4 attachment %v", a4)
	want(m4.rawIs("attachments_resolved", 1) && m4.rawIs("attachments_unresolved", 0), "message 4 counters %v", m4.raw)
	md, _ := m4.raw["ZMETADATA"].(map[string]any)
	want(fmt.Sprint(md["bytes"]) == "20" && len(fmt.Sprint(md["sha256"])) == 64, "message 4 ZMETADATA %v", md)
	want(c.rec(9).rawIs("attachments_resolved", 0) && c.rec(9).rawIs("attachments_unresolved", 1) && len(c.rec(9).attachments()) == 1, "message 9 (no file): %v", c.rec(9).raw)
	want(c.rec(28).rawIs("attachments_unresolved", 1) && len(c.rec(28).attachments()) == 1, "message 28 (both directions agree, no file): %v", c.rec(28).raw)
	m29 := c.rec(29)
	want(reflect.DeepEqual(m29.raw["join_undecided"], []any{"ZWAMEDIAITEM"}) && len(m29.attachments()) == 0 && m29.rawAbsent("attachments_resolved"),
		"message 29 (only ZMEDIAITEM names a row): %v", m29.raw)
	m30 := c.rec(30)
	want(m30.raw["media_state"] == "lookup-path-not-expressible" && m30.rawIs("attachments_lookup_undecided", 1) && m30.rawIs("attachments_unresolved", 0) && m30.rawIs("attachments_resolved", 0),
		"message 30 (a path with *): %v", m30.raw)
	want(c.rec(31).raw["media_state"] == "lookup-path-refused" && c.rec(31).rawIs("attachments_unresolved", 1), "message 31 (..): %v", c.rec(31).raw)
	want(c.rec(32).raw["media_state"] == "no-local-path" && c.rec(32).rawIs("attachments_unresolved", 1), "message 32 (NULL path): %v", c.rec(32).raw)
	want(c.rec(33).raw["media_state"] == "no-ZWAMEDIAITEM-row" && len(c.rec(33).attachments()) == 0, "message 33 (no item): %v", c.rec(33).raw)
	want(c.rec(5).raw["media_state"] == "no-ZWAMEDIAITEM-row" && c.rec(6).raw["media_state"] == "no-ZWAMEDIAITEM-row", "messages 5, 6 have no item")
	m43 := c.rec(43)
	want(m43.rawIs("attachments_other_source", 1) && m43.rawIs("attachments_resolved", 0) && m43.rawIs("attachments_unresolved", 0) && m43.raw["label_ZMESSAGETYPE"] == "sticker",
		"message 43 (a file of another device): %v", m43.raw)
	want(len(fx.Expect.ExtraFiles) == 2 && fx.Expect.ExtraFiles[0].Domain == domainConsumer && fx.Expect.ExtraFiles[0].RelativePath == "Message/Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg" &&
		fx.Expect.ExtraFiles[1].Device == "IOS2", "extra files %+v", fx.Expect.ExtraFiles)
}

func TestIOSWhatsAppChatsOracleStatesTheApp(t *testing.T) {
	c, fx := recsOf(t, "ios-chatstorage-business")
	if len(c.by) != 3 || c.rec(1).raw["app"] != "whatsapp_business" || c.rec(1).raw["app_basis"] != "backup_domain" || fx.Expect.Notes["container"] != "backup_domain" {
		t.Errorf("business: %d records, raw %v, notes %v", len(c.by), c.rec(1).raw, fx.Expect.Notes)
	}
	c, fx = recsOf(t, "ios-chatstorage-extract")
	r := c.rec(1)
	// an extraction names no app: the random container name is no evidence, and no container metadata is held (4I2-R5)
	if len(c.by) != 3 || r.raw["app"] != "unknown" || r.raw["app_state"] != "no-container-metadata" || !r.rawAbsent("app_basis") || fx.Expect.Notes["container"] != "app_group" {
		t.Errorf("extract: %d records, raw %v, notes %v", len(c.by), r.raw, fx.Expect.Notes)
	}
	if len(fx.Expect.ExtraFiles) != 0 {
		t.Errorf("an extraction variant has extras %v, BundleIOS refuses them", fx.Expect.ExtraFiles)
	}
}

// TestIOSWhatsAppChatsOracleStatesTheModelVariants: the entity numbers are read from Z_PRIMARYKEY, never hard-coded
// (4I2-R3); a store without Z_PRIMARYKEY, one naming another model, or another app's database, yields no record.
func TestIOSWhatsAppChatsOracleStatesTheModelVariants(t *testing.T) {
	c, fx := recsOf(t, "ios-chatstorage-renumbered")
	if len(c.by) != 3 || fx.Expect.Notes["entity.WAMessage"] != "2" || fx.Expect.Notes["entity.WAChatSession"] != "3" || !c.rec(1).rawIs("Z_ENT", 2) {
		t.Errorf("renumbered: %d records, notes %v, raw %v", len(c.by), fx.Expect.Notes, c.rec(1).raw)
	}
	for _, v := range []string{"ios-chatstorage-nomodel", "ios-chatstorage-othermodel", "ios-chatstorage-otherapp"} {
		c, fx := recsOf(t, v)
		if len(c.by) != 0 || len(fx.Expect.Notes) != 0 {
			t.Errorf("%s states %d records and notes %v; the parser refuses it", v, len(c.by), fx.Expect.Notes)
		}
	}
}

// TestIOSWhatsAppChatsOracleStatesTheAbsentOptionalTables (plan 4I2 PF3): ZWAGROUPMEMBER, ZWAMESSAGEINFO and ZWAMEDIAITEM
// are optional tables; their absence is stated as an undecided join, never as "no row".
func TestIOSWhatsAppChatsOracleStatesTheAbsentOptionalTables(t *testing.T) {
	c, _ := recsOf(t, "ios-chatstorage-nooptional")
	if len(c.by) != 6 {
		t.Fatalf("%d records, want 6", len(c.by))
	}
	m4, m10, m44 := c.rec(4), c.rec(10), c.rec(44)
	if !reflect.DeepEqual(m4.raw["join_undecided"], []any{"ZWAMEDIAITEM"}) || !m4.rawAbsent("media_state") || len(m4.attachments()) != 0 {
		t.Errorf("message 4: %v", m4.Payload)
	}
	if !reflect.DeepEqual(m10.raw["join_undecided"], []any{"ZWAGROUPMEMBER"}) || m10.raw["sender_state"] != "undecided" || !m10.unknownParticipants() {
		t.Errorf("message 10: %v", m10.Payload)
	}
	if !reflect.DeepEqual(m44.raw["join_undecided"], []any{"ZWAMESSAGEINFO"}) || !m44.rawAbsent("receipt_info") || !m44.rawAbsent("receipt_info_state") {
		t.Errorf("message 44: %v", m44.Payload)
	}
	for _, n := range []int64{1, 2, 3} {
		if !c.rec(n).rawAbsent("join_undecided") {
			t.Errorf("message %d (text, no join) states a join", n)
		}
	}
}
