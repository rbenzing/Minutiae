package ioswhatsapp_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// chatFixture parses a committed variant and returns its records by Z_PK.
func chatFixture(t *testing.T, variant string) map[int64]records.Record {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	h := parsertest.NewHarness(t)
	b := bundleOf(h, fx, variant)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatalf("%s: %v", variant, res.Err)
	}
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		out[rowidOf(r.Locator)] = r
	}
	return out
}

// 4I2-R8: an unmapped code stays unknown with its stored value in raw and no label; a cell of another storage class is
// stated in unknown_columns, never read as a code.
func TestIOSWhatsAppChatsUnmappedCodesStayUnknownWithRaw(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	for _, c := range []struct {
		row  int64
		key  string
		want int64
	}{{22, "ZMESSAGETYPE", 999}, {23, "ZMESSAGETYPE", 12}} {
		r := recs[c.row]
		raw := rawOf(r)
		if r.Payload["kind"] != "unknown" || raw["label_ZMESSAGETYPE"] != nil || raw[c.key] != c.want {
			t.Errorf("row %d: kind %v label %v %s %v", c.row, r.Payload["kind"], raw["label_ZMESSAGETYPE"], c.key, raw[c.key])
		}
	}
	r37 := recs[37]
	if r37.Payload["direction"] != "unknown" || rawOf(r37)["ZISFROMME"] != int64(2) || !unknownParticipants(r37) {
		t.Errorf("row 37: %v", r37.Payload)
	}

	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		m := tb["ZWAMESSAGE"]
		m.put(1, msgRow(map[string]any{"ZMESSAGETYPE": "1"}))
		m.put(2, msgRow(map[string]any{"ZMESSAGETYPE": []byte{1, 2}}))
		m.put(3, msgRow(map[string]any{"ZISFROMME": "1", "ZTOJID": p1}))
		m.put(4, msgRow(map[string]any{"ZMESSAGETYPE": int64(9)})) // one source: unmapped
	})
	got, _ := runChat(t, db, nil)
	r1, r2, r3, r4 := got[1], got[2], got[3], got[4]
	if r1.Payload["kind"] != "unknown" || rawOf(r1)["ZMESSAGETYPE"] != "1" || rawOf(r1)["label_ZMESSAGETYPE"] != nil ||
		payloadMap(r1, "raw")["unknown_columns"].(map[string]any)["ZWAMESSAGE.ZMESSAGETYPE"] != "type-mismatch" {
		t.Errorf("TEXT '1' in ZMESSAGETYPE: %v", rawOf(r1))
	}
	if r2.Payload["kind"] != "unknown" || !reflect.DeepEqual(rawOf(r2)["ZMESSAGETYPE"], map[string]any{"blob_b64": "AQI="}) ||
		rawOf(r2)["unknown_columns"].(map[string]any)["ZWAMESSAGE.ZMESSAGETYPE"] != "type-mismatch" || rawOf(r2)["label_ZMESSAGETYPE"] != nil {
		t.Errorf("BLOB in ZMESSAGETYPE: %v", rawOf(r2))
	}
	if r3.Payload["direction"] != "unknown" || rawOf(r3)["ZISFROMME"] != "1" || !unknownParticipants(r3) {
		t.Errorf("TEXT '1' in ZISFROMME: %v", r3.Payload)
	}
	if r4.Payload["kind"] != "unknown" || rawOf(r4)["ZMESSAGETYPE"] != int64(9) || rawOf(r4)["label_ZMESSAGETYPE"] != nil {
		t.Errorf("type 9: %v", rawOf(r4))
	}
}

// 4I2-R3 (spec R3): an incoming status update has no attributed author: ZFROMJID, ZGROUPMEMBER and ZPUSHNAME are raw only.
// The status is decided by the session type 3 OR the status@broadcast class (4I2-P5).
func TestIOSWhatsAppChatsStatusUpdateAuthorUnknown(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	r16 := recs[16]
	raw := rawOf(r16)
	if !unknownParticipants(r16) || fromOf(r16, "from") != "" || raw["ZFROMJID"] != "15550000003@s.whatsapp.net" || raw["ZGROUPMEMBER"] != int64(1) || raw["ZPUSHNAME"] != "Ann" {
		t.Errorf("row 16: %v", r16.Payload)
	}
	if fromOf(recs[17], "to") != "status@broadcast" {
		t.Errorf("row 17: %v", recs[17].Payload["participants"])
	}
	// a session of type 0 whose JID is status@broadcast is a status all the same
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "status@broadcast"})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZFROMJID": p1}))
	})
	got, _ := runChat(t, db, nil)
	if !unknownParticipants(got[1]) || fromOf(got[1], "from") != "" || rawOf(got[1])["ZFROMJID"] != p1 || rawOf(got[1])["chat_class"] != "status" {
		t.Errorf("status JID on a type-0 session: %v", got[1].Payload)
	}
}

// 4I2-P5, R3: the sender of an incoming group row is the member row's JID, never ZFROMJID (the group, S7) and never the
// chat; without one the participants are unknown and sender_state says why.
func TestIOSWhatsAppChatsGroupSenderNeverTheGroup(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	if got := fromOf(recs[10], "from"); got != "15550000002@s.whatsapp.net" {
		t.Errorf("row 10 from %q", got)
	}
	for n, state := range map[int64]string{11: "absent", 12: "no-member-jid", 13: "no-ZWAGROUPMEMBER-row", 14: "not-a-person"} {
		r := recs[n]
		if !unknownParticipants(r) || fromOf(r, "from") != "" || rawOf(r)["sender_state"] != state {
			t.Errorf("row %d: sender_state %v participants %v", n, rawOf(r)["sender_state"], r.Payload["participants"])
		}
	}
	for n, r := range recs {
		th := payloadMap(r, "thread")
		if th["id"] != "120363000000000001@g.us" {
			continue
		}
		if r.Payload["direction"] == "in" && fromOf(r, "from") == "120363000000000001@g.us" {
			t.Errorf("row %d: an incoming group row is from the group", n)
		}
	}
}

// Review Focus 2: a g.us chat JID whose ZSESSIONTYPE is NULL, out of range or unreadable is a group for the sender: the
// author comes from ZGROUPMEMBER, never ZFROMJID or the group; thread.group is not claimed.
func TestIOSWhatsAppChatsGroupJIDWithUnknownSessionTypeNeverAttributesTheGroup(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	r40, r41 := recs[40], recs[41]
	if fromOf(r40, "from") != p4 || !unknownParticipants(r41) || fromOf(r41, "from") != "" {
		t.Errorf("rows 40, 41: %v %v", r40.Payload["participants"], r41.Payload["participants"])
	}
	for n, r := range map[int64]records.Record{40: r40, 41: r41} {
		if _, has := payloadMap(r, "thread")["group"]; has {
			t.Errorf("row %d: thread.group is stated for an unknown session type: %v", n, r.Payload["thread"])
		}
		if sess, _ := rawOf(r)["session"].(map[string]any); sess["ZSESSIONTYPE"] != nil {
			t.Errorf("row %d: raw.session.ZSESSIONTYPE for a stored NULL: %v", n, rawOf(r)["session"])
		}
	}
	for _, st := range []any{int64(7), "1"} {
		db, _ := buildChat(nil, func(tb chatTables) {
			tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": st, "ZCONTACTJID": grp})
			tb["ZWAGROUPMEMBER"].put(1, map[string]any{"ZMEMBERJID": p4})
			tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZGROUPMEMBER": int64(1), "ZFROMJID": grp}))
			tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZFROMJID": grp}))
		})
		got, _ := runChat(t, db, nil)
		if fromOf(got[1], "from") != p4 || !unknownParticipants(got[2]) || fromOf(got[2], "from") != "" {
			t.Errorf("ZSESSIONTYPE %v: %v %v", st, got[1].Payload["participants"], got[2].Payload["participants"])
		}
		if _, has := payloadMap(got[1], "thread")["group"]; has {
			t.Errorf("ZSESSIONTYPE %v: thread.group is stated: %v", st, got[1].Payload["thread"])
		}
	}
}

// Review Focus 3: ZMESSAGEDATE is Cocoa seconds, read as stored (a REAL with a fraction, an INTEGER), never rescaled by
// magnitude; what is not a time is no time, with cellx's states in raw.
func TestIOSWhatsAppChatsCocoaDateFormsAreReadNotGuessed(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	if r := recs[26]; r.Time == nil || r.Time.T.UTC().Format("2006-01-02T15:04:05.999999999Z") != "2020-01-06T10:40:00.25Z" {
		t.Errorf("row 26 (REAL with a fraction): %v", r.Time)
	}
	if r := recs[27]; r.Time == nil || r.Time.T.UTC().Format("2006-01-02T15:04:05Z") != "2020-01-06T10:40:00Z" {
		t.Errorf("row 27 (INTEGER): %v", r.Time)
	}
	r25 := recs[25]
	if r25.Time != nil || rawOf(r25)["ZMESSAGEDATE"] != int64(600000000000) || rawOf(r25)["ZMESSAGEDATE_status"] != "invalid" {
		t.Errorf("row 25 (millisecond magnitude): time %v raw %v", r25.Time, rawOf(r25))
	}
	if r := recs[24]; r.Time != nil || rawOf(r)["ZMESSAGEDATE"] != nil || rawOf(r)["ZMESSAGEDATE_status"] != nil {
		t.Errorf("row 24 (NULL): %v", rawOf(r))
	}
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		for i, v := range []any{math.NaN(), math.Inf(1), int64(-1), int64(0), int64(math.MaxInt64), "x"} {
			tb["ZWAMESSAGE"].put(int64(i+1), msgRow(map[string]any{"ZMESSAGEDATE": v}))
		}
	})
	got, _ := runChat(t, db, nil)
	type want struct{ status, ieee string }
	// NaN is NULL to the engine (and to the reader): no time and nothing to state; +Inf is stated as its IEEE bits; -1, 0 and
	// MaxInt64 are the sentinels of decode/ts; a TEXT cell is a type mismatch
	for i, w := range []want{{"", ""}, {"invalid", "7ff0000000000000"}, {"sentinel", ""}, {"sentinel", ""}, {"sentinel", ""}, {"", ""}} {
		r := got[int64(i+1)]
		raw := rawOf(r)
		if r.Time != nil {
			t.Errorf("value %d: a time %v is invented", i+1, r.Time)
		}
		if st, _ := raw["ZMESSAGEDATE_status"].(string); st != w.status {
			t.Errorf("value %d: status %q, want %q (raw %v)", i+1, st, w.status, raw)
		}
		if ie, _ := raw["ZMESSAGEDATE_ieee"].(string); ie != w.ieee {
			t.Errorf("value %d: ieee %q, want %q", i+1, ie, w.ieee)
		}
	}
	if rawOf(got[6])["unknown_columns"].(map[string]any)["ZWAMESSAGE.ZMESSAGEDATE"] != "type-mismatch" {
		t.Errorf("TEXT date: %v", rawOf(got[6]))
	}
}

// 6.5, 6.11: a revoke (type 14) is a tombstone: kind unknown, no body, no attachment, deleted by the app's own structure;
// the row's text, and the media row's ZVCARDNAME, stay raw.
func TestIOSWhatsAppChatsRevokeIsTombstoneWithoutContent(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	r := recs[21]
	raw := rawOf(r)
	if r.Payload["kind"] != "unknown" || r.Body != "" || !r.Deleted || !reflect.DeepEqual(r.Payload["deleted"], map[string]any{"source": "ZMESSAGETYPE"}) ||
		raw["label_ZMESSAGETYPE"] != "revoked" || raw["ZVCARDNAME"] != "x1" || raw["revoked_row_text"] != "leftover" ||
		r.Payload["attachments"] != nil || r.Payload["text_source"] != nil || !strings.HasSuffix(r.Summary, "[revoked, no content]") {
		t.Errorf("row 21: %v summary %q", r.Payload, r.Summary)
	}
	// a revoke whose media row holds a title and a file: none of it is taken
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZMESSAGETYPE": int64(14), "ZTEXT": nil}))
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": int64(1), "ZTITLE": "kept?", "ZVCARDNAME": "n", "ZMEDIALOCALPATH": "Media/x.jpg", "ZFILESIZE": int64(5)})
	})
	got, _ := runChat(t, db, nil)
	g := got[1]
	graw := rawOf(g)
	if g.Body != "" || !g.Deleted || g.Payload["attachments"] != nil || graw["ZVCARDNAME"] != "n" || graw["ZTITLE"] != nil || graw["ZFILESIZE"] != nil || graw["body_column"] != nil {
		t.Errorf("revoke with a media row: %v", g.Payload)
	}
}

// 4I2-R6 (the 4I carry-over): a JID that cleaning changed is altered: never parsed, never a thread id, group or
// participant; the cleaned text and its text_flags go to raw.
func TestIOSWhatsAppChatsAlteredJIDIsNeverAnAddress(t *testing.T) {
	recs := chatFixture(t, "ios-chatstorage")
	r := recs[42]
	raw := rawOf(r)
	if raw["chat_state"] != "altered" || payloadMap(r, "thread")["id"] != nil || !unknownParticipants(r) ||
		raw["chat_jid"] != "1555�0000007@s.whatsapp.net" || !reflect.DeepEqual(raw["text_flags"], []any{"chat_jid"}) || raw["chat_class"] != nil {
		t.Errorf("row 42: %v", r.Payload)
	}
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWACHATSESSION"].put(2, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "1555\xff@s.whatsapp.net"})
		tb["ZWAGROUPMEMBER"].put(1, map[string]any{"ZMEMBERJID": "1555\x00@s.whatsapp.net"})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZGROUPMEMBER": int64(1)}))
		tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZCHATSESSION": int64(2), "ZISFROMME": int64(1)}))
	})
	got, _ := runChat(t, db, nil)
	g1, g2 := rawOf(got[1]), rawOf(got[2])
	if g1["sender_state"] != "altered" || g1["member_jid_text"] != "1555�@s.whatsapp.net" || !reflect.DeepEqual(g1["text_flags"], []any{"member_jid_text"}) || !unknownParticipants(got[1]) {
		t.Errorf("member JID with NUL: %v", got[1].Payload)
	}
	if g2["chat_state"] != "altered" || !unknownParticipants(got[2]) || fromOf(got[2], "to") != "" || payloadMap(got[2], "thread")["id"] != nil {
		t.Errorf("chat JID with an invalid byte, outgoing: %v", got[2].Payload)
	}
}

// Rulings O5 and PF1: the media row is found only when the directions agree. Both set and disagreeing, only the
// single-source direction naming a row, and two rows naming one message are undecided: no media field is taken. A
// dangling forward key names no row.
func TestIOSWhatsAppChatsMediaJoinRules(t *testing.T) {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		m, mi := tb["ZWAMESSAGE"], tb["ZWAMEDIAITEM"]
		img := func(kv map[string]any) map[string]any {
			kv["ZMESSAGETYPE"], kv["ZTEXT"] = int64(1), nil
			return msgRow(kv)
		}
		m.put(1, img(map[string]any{}))                        // converse only
		m.put(2, img(map[string]any{"ZMEDIAITEM": int64(2)}))  // both directions agree
		m.put(3, img(map[string]any{"ZMEDIAITEM": int64(1)}))  // both set, disagree (item 3 names message 3, the key names item 1)
		m.put(4, img(map[string]any{"ZMEDIAITEM": int64(4)}))  // only the forward direction names a row
		m.put(5, img(map[string]any{}))                        // two rows name it
		m.put(6, img(map[string]any{"ZMEDIAITEM": int64(99)})) // a dangling forward key
		mi.put(1, map[string]any{"ZMESSAGE": int64(1), "ZTITLE": "t1"})
		mi.put(2, map[string]any{"ZMESSAGE": int64(2), "ZTITLE": "t2"})
		mi.put(3, map[string]any{"ZMESSAGE": int64(3), "ZTITLE": "t3"})
		mi.put(4, map[string]any{"ZMESSAGE": int64(40), "ZTITLE": "t4"})
		mi.put(5, map[string]any{"ZMESSAGE": int64(5), "ZTITLE": "t5a"})
		mi.put(6, map[string]any{"ZMESSAGE": int64(5), "ZTITLE": "t5b"})
	})
	recs, _ := runChat(t, db, nil)
	for n, c := range map[int64]struct {
		body      string
		undecided bool
	}{1: {"t1", false}, 2: {"t2", false}, 3: {"", true}, 4: {"", true}, 5: {"", true}, 6: {"", false}} {
		r := recs[n]
		raw := rawOf(r)
		undecided := reflect.DeepEqual(raw["join_undecided"], []any{"ZWAMEDIAITEM"})
		if r.Body != c.body || undecided != c.undecided || (c.body == "" && (raw["body_column"] != nil || raw["ZTITLE"] != nil)) {
			t.Errorf("message %d: body %q undecided %v raw %v", n, r.Body, undecided, raw)
		}
	}
}
