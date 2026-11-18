package androidwhatsapp_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Spec 6.5 as amended by 4I-R20: only message_add_on type 56 is a record. The add-on ids 1, 2, 3, 7 and 8 of
// android-modern-2026 give records; 4 (an edit), 5 (keep in chat) and 6 (an unknown type) give none and no error.
func TestWhatsAppReactionsAreRecordsOnlyForType56(t *testing.T) {
	recs, res := recordsOf(t, "android-modern-2026")
	for id := 1; id <= 8; id++ {
		_, has := recs[fmt.Sprintf("message_add_on/%d", id)]
		want := id == 1 || id == 2 || id == 3 || id == 7 || id == 8
		if has != want {
			t.Errorf("add-on %d: record %v, want %v", id, has, want)
		}
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=message_add_on") {
			t.Errorf("a warning for an add-on row: %+v", w)
		}
	}

	r1 := recs["message_add_on/1"]
	raw1 := rawMap(t, r1)
	if r1.Body != "\U0001F44D" || r1.Payload["kind"] != "reaction" || r1.Payload["target_guid"] != "K02" || r1.Payload["guid"] != "A01" {
		t.Errorf("add-on 1: body %q kind %v target %v guid %v", r1.Body, r1.Payload["kind"], r1.Payload["target_guid"], r1.Payload["guid"])
	}
	if r1.Time == nil || r1.Time.T.UTC().Format(time.RFC3339) != "2020-09-13T12:31:40Z" {
		t.Errorf("add-on 1: time %v, want 2020-09-13T12:31:40Z", r1.Time)
	}
	if r1.Payload["direction"] != "unknown" || fmt.Sprint(raw1["from_me"]) != "1" || r1.Payload["participants_unknown"] != true {
		t.Errorf("add-on 1: direction %v raw.from_me %v participants_unknown %v", r1.Payload["direction"], raw1["from_me"], r1.Payload["participants_unknown"])
	}
	if fmt.Sprint(raw1["sender_timestamp"]) != "1600000299000" || raw1["sender_timestamp_unit"] != "unestablished" {
		t.Errorf("add-on 1: raw sender_timestamp %v unit %v", raw1["sender_timestamp"], raw1["sender_timestamp_unit"])
	}
	if th := threadOf(r1); th["id"] != "15550000001@s.whatsapp.net" || r1.Deleted {
		t.Errorf("add-on 1: thread %v deleted %v, want the parent's chat and live", th, r1.Deleted)
	}
	if raw1["source_table"] != "message_add_on" || raw1["label_message_add_on_type"] != "reaction" || fmt.Sprint(raw1["sender_jid_row_id"]) != "-1" {
		t.Errorf("add-on 1: raw %v", raw1)
	}

	r2 := recs["message_add_on/2"]
	if r2.Body != "" || rawMap(t, r2)["reaction_empty"] != true || r2.Payload["text_source"] != nil {
		t.Errorf("add-on 2: body %q raw %v, want no body and reaction_empty", r2.Body, rawMap(t, r2))
	}

	r3 := recs["message_add_on/3"]
	raw3 := rawMap(t, r3)
	if r3.Payload["target_guid"] != nil || raw3["parent_state"] != "no-message-row" {
		t.Errorf("add-on 3: target %v raw.parent_state %v, want none and no-message-row", r3.Payload["target_guid"], raw3["parent_state"])
	}
	if th := threadOf(r3); th["id"] != "15550000001@s.whatsapp.net" {
		t.Errorf("add-on 3: thread %v, want the add-on's own chat", th)
	}

	// 7: a group reaction whose sender is a LID: never a participant, the LID stays in raw.sender_jid
	r7 := recs["message_add_on/7"]
	raw7 := rawMap(t, r7)
	if n := len(partsOf(r7)); n != 0 || r7.Payload["participants_unknown"] != true {
		t.Errorf("add-on 7: %d participants, unknown %v", n, r7.Payload["participants_unknown"])
	}
	if raw7["sender_jid"] != "100000000000002@lid" || threadOf(r7)["group"] != true {
		t.Errorf("add-on 7: raw.sender_jid %v thread %v", raw7["sender_jid"], threadOf(r7))
	}

	// 8: type 56 with no message_add_on_reaction row: the state is stated, never an empty reaction
	r8 := recs["message_add_on/8"]
	if rawMap(t, r8)["reaction_state"] != "no-reaction-row" || rawMap(t, r8)["reaction_empty"] != nil || r8.Body != "" {
		t.Errorf("add-on 8: raw %v body %q", rawMap(t, r8), r8.Body)
	}
}

// 4I-R20 (G3): no reaction of any variant has a direction other than unknown, or any participant.
func TestWhatsAppReactionDirectionAndSenderStayUnknown(t *testing.T) {
	n := 0
	for _, v := range variants {
		fx, _ := runVariant(t, v)
		for _, r := range fx.Records {
			if r.Payload["kind"] != "reaction" {
				continue
			}
			n++
			if r.Payload["direction"] != "unknown" || len(partsOf(r)) != 0 || r.Payload["participants_unknown"] != true {
				t.Errorf("%s %s: direction %v participants %v unknown %v", v, r.Locator, r.Payload["direction"], partsOf(r), r.Payload["participants_unknown"])
			}
		}
	}
	if n == 0 {
		t.Errorf("no reaction record in any variant")
	}
}

// 4I-R22 vocabulary of a reaction, pinned on rows the test wrote: a NULL parent key, an absent reaction table, a
// reaction cell of the wrong type, an empty (NULL) reaction, and an add-on type that is not an INTEGER.
func TestWhatsAppReactionStatesArePinned(t *testing.T) {
	withText := true
	fill := func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "parent")
		tb["message_add_on"].Insert(1, nil, int64(1), int64(0), "A1", int64(-1), nil, goodTS, int64(56))
		tb["message_add_on"].Insert(2, nil, int64(1), int64(0), "A2", int64(-1), int64(1), goodTS, int64(56))
		tb["message_add_on"].Insert(3, nil, int64(1), int64(0), "A3", int64(-1), int64(1), goodTS, int64(56))
		tb["message_add_on"].Insert(4, nil, int64(1), int64(0), "A4", int64(-1), int64(1), goodTS, "x")
		tb["message_add_on"].Insert(5, nil, int64(1), int64(0), "A5", int64(-1), int64(1), goodTS, int64(74))
		if rt := tb["message_add_on_reaction"]; rt != nil {
			if !withText {
				rt.Insert(1, nil, nil)
				rt.Insert(2, nil, nil)
				rt.Insert(3, nil, nil)
			} else {
				rt.Insert(1, nil, "o", nil)
				rt.Insert(2, nil, nil, nil)
				rt.Insert(3, nil, int64(7), nil)
			}
		}
	}
	db, _ := buildDB(nil, fill)
	res := runBytes(t, db, nil, remoteLive)
	recs := byRowid(t, res)
	r1, r2, r3 := rawMap(t, recs["message_add_on/1"]), rawMap(t, recs["message_add_on/2"]), rawMap(t, recs["message_add_on/3"])
	if r1["parent_state"] != "no-parent-key" || threadOf(recs["message_add_on/1"])["id"] != "15550000001@s.whatsapp.net" || recs["message_add_on/1"].Body != "o" {
		t.Errorf("add-on 1: raw %v thread %v, want no-parent-key and the add-on's own chat", r1, threadOf(recs["message_add_on/1"]))
	}
	if r2["reaction_empty"] != true || r2["reaction_state"] != nil || r2["parent_state"] != nil || recs["message_add_on/2"].Payload["target_guid"] != "KB" {
		t.Errorf("add-on 2: raw %v, want reaction_empty for a NULL reaction", r2)
	}
	if r3["reaction_state"] != "unreadable" || !reflect.DeepEqual(r3["unknown_columns"], map[string]any{"message_add_on_reaction.reaction": "type-mismatch"}) {
		t.Errorf("add-on 3: raw %v, want unreadable and type-mismatch", r3)
	}
	for _, id := range []string{"4", "5"} {
		if _, has := recs["message_add_on/"+id]; has {
			t.Errorf("add-on %s is a record", id)
		}
	}
	if n := len(warningsContaining(res, "message_add_on_type")); n != 1 {
		t.Errorf("%d warnings for the add-on type that is not an INTEGER, want 1: %v", n, res.Warnings)
	}

	db, _ = buildDB(map[string]string{"message_add_on_reaction": ""}, fill)
	r := rawMap(t, byRowid(t, runBytes(t, db, nil, remoteLive))["message_add_on/1"])
	if r["reaction_state"] != "no-reaction-table" {
		t.Errorf("without the table: raw %v, want no-reaction-table", r)
	}
	withText = false
	db, _ = buildDB(map[string]string{"message_add_on_reaction": "create table message_add_on_reaction(message_add_on_row_id integer primary key, sender_timestamp integer)"}, fill)
	r = rawMap(t, byRowid(t, runBytes(t, db, nil, remoteLive))["message_add_on/1"])
	if r["reaction_state"] != "unreadable" || !reflect.DeepEqual(r["unknown_columns"], map[string]any{"message_add_on_reaction.reaction": "missing"}) {
		t.Errorf("without the column: raw %v, want unreadable and missing", r)
	}
}
