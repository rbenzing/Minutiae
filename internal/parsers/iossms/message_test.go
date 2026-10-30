package iossms_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
)

var cocoaEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// Review focus 4: the unit boundary, the precision edge, the sentinel and the unread receipt.
func TestMessageTimesFromTheSpec(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"date": int64(700000000123456789)})
	d.msg(2, map[string]any{"date": int64(700000000)})
	d.msg(3, map[string]any{"date": int64(0)})
	d.msg(4, map[string]any{"date": int64(99_999_999_999)})
	d.msg(5, map[string]any{"is_read": int64(0), "date_read": int64(0)})
	d.out(6, map[string]any{
		"date": int64(700000000_000000000), "date_read": int64(700000001_000000000), "date_delivered": int64(700000002_000000000),
		"date_played": int64(700000003_000000000), "date_edited": int64(700000004_000000007), "date_retracted": int64(700000005_000000000),
	})
	d.msg(7, map[string]any{"date": int64(700000000_000000001)})
	d.msg(8, map[string]any{"date": 700000000.5})
	res := d.run(t)

	r := rec(t, res, 1)
	if want := cocoaEpoch.Add(700000000*time.Second + 123456789); r.Time == nil || !r.Time.T.Equal(want) {
		t.Errorf("nanosecond date: time %v, want %v", r.Time, want)
	}
	if raw := rawOfRec(r); raw["date_unit"] != "nanoseconds" || !is(raw["date"], int64(700000000123456789)) {
		t.Errorf("nanosecond date: raw %v", raw)
	}
	r = rec(t, res, 7)
	if want := cocoaEpoch.Add(700000000*time.Second + 1); r.Time == nil || !r.Time.T.Equal(want) {
		t.Errorf("a nanosecond date with a non-zero last digit lost it: %v, want %v", r.Time, want)
	}
	r = rec(t, res, 2)
	if want := cocoaEpoch.Add(700000000 * time.Second); r.Time == nil || !r.Time.T.Equal(want) || rawOfRec(r)["date_unit"] != "seconds" {
		t.Errorf("seconds date: time %v raw %v", r.Time, rawOfRec(r))
	}
	r = rec(t, res, 3)
	if raw := rawOfRec(r); r.Time != nil || raw["date_status"] != "sentinel" || !is(raw["date"], 0) {
		t.Errorf("date 0: time %v raw %v", r.Time, raw)
	}
	r = rec(t, res, 4)
	if raw := rawOfRec(r); r.Time != nil || raw["date_status"] != "invalid" {
		t.Errorf("date 99999999999: time %v raw %v", r.Time, raw)
	}
	r = rec(t, res, 5)
	if raw := rawOfRec(r); timeOf(r, "read") != nil || raw["date_read_status"] != "sentinel" {
		t.Errorf("date_read 0 on an unread message: times %v raw %v", r.Times, raw)
	}
	r = rec(t, res, 6)
	base := cocoaEpoch.Add(700000000 * time.Second)
	for kind, want := range map[string]time.Time{
		"read": base.Add(time.Second), "delivered": base.Add(2 * time.Second), "played": base.Add(3 * time.Second),
		"edited": base.Add(4*time.Second + 7), "retracted": base.Add(5 * time.Second),
	} {
		if got := timeOf(r, kind); got == nil || !got.T.Equal(want) {
			t.Errorf("Times.%s = %v, want %v", kind, got, want)
		}
	}
	if len(r.Times) != 5 {
		t.Errorf("times %v", r.Times)
	}
	r = rec(t, res, 8)
	if want := cocoaEpoch.Add(700000000*time.Second + 500*time.Millisecond); r.Time == nil || !r.Time.T.Equal(want) || rawOfRec(r)["date"] != 700000000.5 {
		t.Errorf("REAL date: time %v raw %v", r.Time, rawOfRec(r))
	}
}

func TestMessageDirectionStatusAndChannel(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, nil)                                                          // incoming
	d.out(2, map[string]any{"is_delivered": int64(1)})                     // delivered
	d.out(3, nil)                                                          // sent
	d.out(4, map[string]any{"is_sent": int64(0)})                          // nothing known
	d.out(5, map[string]any{"error": int64(22), "is_delivered": int64(1)}) // failed wins
	d.msg(6, map[string]any{"is_from_me": int64(2)})                       // odd direction
	d.msg(7, map[string]any{"service": "SMS"})
	d.msg(8, map[string]any{"service": "RCS"})
	d.msg(9, map[string]any{"service": nil})
	d.msg(10, map[string]any{"service": "Foo"})
	d.msg(11, map[string]any{"is_read": int64(0)})
	d.msg(12, map[string]any{"is_read": int64(2)})
	d.out(13, map[string]any{"is_read": int64(1)})
	d.msg(14, map[string]any{"service": "imessage"}) // exact match only
	res := d.run(t)

	for _, c := range []struct {
		id              int64
		dir, status, ch string
	}{
		{1, "in", "received", "imessage"},
		{2, "out", "delivered", "imessage"},
		{3, "out", "sent", "imessage"},
		{4, "out", "unknown", "imessage"},
		{5, "out", "failed", "imessage"},
		{6, "unknown", "unknown", "imessage"},
		{7, "in", "received", "sms"},
		{8, "in", "received", "rcs"},
		{9, "in", "received", "unknown"},
		{10, "in", "received", "unknown"},
		{14, "in", "received", "unknown"},
	} {
		p := rec(t, res, c.id).Payload
		if p["direction"] != c.dir || p["status"] != c.status || p["channel"] != c.ch {
			t.Errorf("row %d: direction %v status %v channel %v, want %s %s %s", c.id, p["direction"], p["status"], p["channel"], c.dir, c.status, c.ch)
		}
	}
	if r := rec(t, res, 10); rawOfRec(r)["service"] != "Foo" || r.Payload["service"] != "Foo" {
		t.Errorf("service Foo: %v", r.Payload)
	}
	if r := rec(t, res, 9); r.Payload["service"] != nil || rawOfRec(r)["service"] != nil {
		t.Errorf("a NULL service was given a value: %v", r.Payload)
	}
	// 4E-R31: read only for incoming messages, 0 and 1
	if p := rec(t, res, 11).Payload; p["read"] != false {
		t.Errorf("unread incoming: read %v", p["read"])
	}
	if p := rec(t, res, 1).Payload; p["read"] != true {
		t.Errorf("read incoming: read %v", p["read"])
	}
	if r := rec(t, res, 12); r.Payload["read"] != nil || !is(rawOfRec(r)["is_read"], 2) {
		t.Errorf("is_read 2: %v", r.Payload)
	}
	if r := rec(t, res, 13); r.Payload["read"] != nil || !is(rawOfRec(r)["is_read"], 1) {
		t.Errorf("outgoing is_read: read %v raw %v", r.Payload["read"], rawOfRec(r))
	}
}

func TestMessageParticipantsAndThread(t *testing.T) {
	d := newSMS(t)
	d.dims()
	d.msg(1, nil)                                    // incoming, chat 1
	d.out(2, map[string]any{"handle_id": int64(0)})  // outgoing to the group
	d.out(3, map[string]any{"handle_id": int64(2)})  // outgoing, no chat join
	d.out(4, nil)                                    // outgoing, in two chats
	d.msg(5, map[string]any{"handle_id": int64(99)}) // dangling handle
	d.msg(6, map[string]any{"handle_id": int64(0)})  // unknown sender
	d.out(7, map[string]any{"handle_id": int64(0)})  // outgoing, no chat, no handle
	d.msg(8, nil)                                    // dangling chat
	d.msg(9, map[string]any{"is_from_me": int64(2)}) // odd direction
	d.inChat(1, 1)
	d.inChat(2, 2)
	d.inChat(4, 1)
	d.inChat(4, 2)
	d.inChat(5, 1)
	d.inChat(6, 1)
	d.inChat(8, 7)
	d.inChat(9, 1)
	res := d.run(t)

	r := rec(t, res, 1)
	if p := partsOf(r); len(p) != 1 || p[0]["role"] != "from" || p[0]["address"] != "+15551230001" || p[0]["kind"] != "phone" || p[0]["name"] != nil {
		t.Errorf("row 1 participants %v", partsOf(r))
	}
	th, _ := r.Payload["thread"].(map[string]any)
	if th["id"] != "1" || th["group"] != false || th["title"] != nil || !slices.Equal(anyStrs(th["members"]), []string{"+15551230001"}) {
		t.Errorf("row 1 thread %v", th)
	}
	if rawOfRec(r)["thread_state"] != nil || rawOfRec(r)["handle_state"] != nil {
		t.Errorf("row 1 raw %v", rawOfRec(r))
	}

	r = rec(t, res, 2)
	want := []string{"+15551230001", "alice@example.test", "+15551230003"} // handle ROWID order, not join order
	if got := addrsOf(r); !slices.Equal(got, want) {
		t.Errorf("row 2 to = %v, want %v", got, want)
	}
	for _, p := range partsOf(r) {
		if p["role"] != "to" {
			t.Errorf("row 2 role %v", p)
		}
	}
	th, _ = r.Payload["thread"].(map[string]any)
	if th["id"] != "2" || th["group"] != true || th["title"] != "Group" || !slices.Equal(anyStrs(th["members"]), want) {
		t.Errorf("row 2 thread %v", th)
	}

	r = rec(t, res, 3)
	if got := addrsOf(r); !slices.Equal(got, []string{"alice@example.test"}) || partsOf(r)[0]["kind"] != "email" {
		t.Errorf("row 3 to = %v", partsOf(r))
	}
	if rawOfRec(r)["thread_state"] != "no-chat-join" || r.Payload["thread"] != nil {
		t.Errorf("row 3: raw %v thread %v", rawOfRec(r), r.Payload["thread"])
	}

	r = rec(t, res, 4)
	raw := rawOfRec(r)
	if raw["thread_state"] != "several-chats" || r.Payload["thread"] != nil || sprint(raw["chat_ids"]) != "[1 2]" {
		t.Errorf("row 4: raw %v thread %v", raw, r.Payload["thread"])
	}
	if got := addrsOf(r); !slices.Equal(got, []string{"+15551230001"}) {
		t.Errorf("row 4 to = %v (the handle of the message, not a chat's members)", got)
	}

	r = rec(t, res, 5)
	if rawOfRec(r)["handle_state"] != "no-handle-row" || r.Payload["participants_unknown"] != true || len(partsOf(r)) != 0 {
		t.Errorf("row 5: %v", r.Payload)
	}
	r = rec(t, res, 6)
	if r.Payload["participants_unknown"] != true || len(partsOf(r)) != 0 || rawOfRec(r)["handle_state"] != nil {
		t.Errorf("row 6: %v", r.Payload)
	}
	r = rec(t, res, 7)
	if r.Payload["participants_unknown"] != true || len(partsOf(r)) != 0 || rawOfRec(r)["thread_state"] != "no-chat-join" {
		t.Errorf("row 7: %v", r.Payload)
	}
	r = rec(t, res, 8)
	th, _ = r.Payload["thread"].(map[string]any)
	if th["id"] != "7" || rawOfRec(r)["thread_state"] != "no-chat-row" || th["members"] != nil {
		t.Errorf("row 8: thread %v raw %v", th, rawOfRec(r))
	}
	r = rec(t, res, 9)
	if p := partsOf(r); len(p) != 1 || p[0]["role"] != "member" {
		t.Errorf("row 9 (unknown direction): %v", p)
	}

	// an absent optional join table is "no-join-table", never "none"
	d2 := newSMS(t, "message", "handle")
	d2.add("handle", 1, map[string]any{"id": "+15551230001", "service": "iMessage"})
	d2.msg(1, nil)
	r = rec(t, d2.run(t), 1)
	if rawOfRec(r)["thread_state"] != "no-join-table" || r.Payload["thread"] != nil {
		t.Errorf("no chat_message_join table: raw %v", rawOfRec(r))
	}
	if got := addrsOf(r); !slices.Equal(got, []string{"+15551230001"}) {
		t.Errorf("no chat_message_join table: from %v", got)
	}
}

func anyStrs(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, e := range l {
		out = append(out, e.(string))
	}
	return out
}

// 4E-R17.
func TestMessageKindsAndReactions(t *testing.T) {
	const g = "E8F3C1A2-4B5D-4E6F-8A9B-0C1D2E3F4A5B"
	d := newSMS(t, "message")
	assoc := func(id, typ int64, guid any, over map[string]any) {
		row := map[string]any{"associated_message_type": typ, "associated_message_guid": guid}
		for k, v := range over {
			row[k] = v
		}
		d.msg(id, row)
	}
	assoc(1, 2000, "p:0/"+g, nil)
	assoc(2, 3999, "bp:"+g, nil)
	assoc(3, 2001, g, nil)
	assoc(4, 1999, g, nil)        // below the range: text
	assoc(5, 4000, g, nil)        // above the range: text
	assoc(6, 2001, "x:abc", nil)  // malformed
	assoc(7, 2001, g[:35], nil)   // 35 characters
	assoc(8, 2001, "p:x/"+g, nil) // non-decimal prefix
	assoc(9, 2001, "p:12/"+strings.ToLower(g), nil)
	assoc(10, 2001, "p:/"+g, nil) // empty decimal
	assoc(11, 2001, g, map[string]any{"item_type": int64(2)})
	d.msg(12, map[string]any{"item_type": int64(2)})
	d.msg(13, map[string]any{"text": nil})
	d.msg(14, map[string]any{"text": ""})
	res := d.run(t)

	for id, kind := range map[int64]string{1: "reaction", 2: "reaction", 3: "reaction", 4: "text", 5: "text", 6: "reaction", 11: "reaction", 12: "system", 13: "unknown", 14: "text"} {
		if got := rec(t, res, id).Payload["kind"]; got != kind {
			t.Errorf("row %d: kind %v, want %s", id, got, kind)
		}
	}
	for id, want := range map[int64]string{1: g, 2: g, 3: g, 9: strings.ToLower(g)} {
		if got := rec(t, res, id).Payload["target_guid"]; got != want {
			t.Errorf("row %d: target_guid %v, want %s", id, got, want)
		}
	}
	for _, id := range []int64{6, 7, 8, 10} {
		r := rec(t, res, id)
		if _, has := r.Payload["target_guid"]; has {
			t.Errorf("row %d: target_guid %v for a malformed value", id, r.Payload["target_guid"])
		}
		if rawOfRec(r)["associated_message_guid"] == nil {
			t.Errorf("row %d: the stored value was not kept in raw", id)
		}
	}
	if p := rec(t, res, 13).Payload; p["text_source"] != nil {
		t.Errorf("NULL text: %v", p)
	}
	if r := rec(t, res, 14); r.Payload["text_source"] != "text" || r.Body != "" {
		t.Errorf("empty text: %v", r.Payload)
	}
}

// 4E-R14.
func TestAttributedBodySelfCheckAndFallback(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"text": "plain"})
	d.msg(2, map[string]any{"text": nil, "attributedBody": typedstream([]byte("from the blob \U0001F44D"))})
	d.msg(3, map[string]any{"text": "same", "attributedBody": typedstream([]byte("same"))})
	d.msg(4, map[string]any{"text": "the column", "attributedBody": typedstream([]byte("the blob"))})
	cut := typedstream([]byte("a text that is cut short"))
	d.msg(5, map[string]any{"text": nil, "attributedBody": cut[:len(cut)-6]})
	d.msg(6, map[string]any{"text": "kept", "attributedBody": cut[:len(cut)-6]})
	d.msg(7, map[string]any{"text": nil, "attributedBody": typedstream([]byte("bad \xff bytes"))})
	d.msg(8, map[string]any{"text": nil, "attributedBody": []byte("not a typedstream at all")})
	d.msg(9, map[string]any{"text": nil, "attributedBody": []byte{}})
	d.msg(10, map[string]any{"text": nil})
	res := d.run(t)

	r := rec(t, res, 1)
	if r.Body != "plain" || r.Payload["text_source"] != "text" || rawOfRec(r)["attributed_body_differs"] != nil || rawOfRec(r)["attributed_body_status"] != nil {
		t.Errorf("row 1: %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 2)
	if r.Body != "from the blob \U0001F44D" || r.Payload["text_source"] != "attributedBody" || r.Payload["kind"] != "text" {
		t.Errorf("row 2: %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 3)
	if r.Body != "same" || r.Payload["text_source"] != "text" || rawOfRec(r)["attributed_body_differs"] != nil {
		t.Errorf("row 3: %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 4)
	if r.Body != "the column" || r.Payload["text_source"] != "text" || rawOfRec(r)["attributed_body_differs"] != true {
		t.Errorf("row 4: %q %v", r.Body, r.Payload)
	}
	if res.Notes["attributed_body_differs"] != "1" {
		t.Errorf("note attributed_body_differs = %q, want 1 (notes %v)", res.Notes["attributed_body_differs"], res.Notes)
	}
	r = rec(t, res, 5)
	if r.Body != "" || r.Payload["text_source"] != nil || rawOfRec(r)["attributed_body_status"] != "undecodable" || r.Payload["kind"] == "text" {
		t.Errorf("row 5 (cut blob, no text): %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 6)
	if r.Body != "kept" || rawOfRec(r)["attributed_body_status"] != "undecodable" || rawOfRec(r)["attributed_body_differs"] != nil {
		t.Errorf("row 6 (cut blob, text kept): %q %v", r.Body, rawOfRec(r))
	}
	r = rec(t, res, 7)
	if r.Body != "bad \ufffd bytes" || r.Payload["text_source"] != "attributedBody" || !slices.Equal(anyStrs(r.Payload["body_flags"]), []string{"invalid_utf8"}) || r.Payload["body_raw_b64"] == nil {
		t.Errorf("row 7 (invalid UTF-8 in the blob): %q %v", r.Body, r.Payload)
	}
	for _, id := range []int64{8, 9} {
		r = rec(t, res, id)
		if r.Body != "" || rawOfRec(r)["attributed_body_status"] != "undecodable" || r.Payload["kind"] == "text" {
			t.Errorf("row %d: %q %v", id, r.Body, r.Payload)
		}
	}
	r = rec(t, res, 10)
	if rawOfRec(r)["attributed_body_status"] != nil || r.Payload["kind"] != "unknown" {
		t.Errorf("row 10: %v", r.Payload)
	}
}

func TestMessageTextHygiene(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"text": "nul\x00in text"})
	d.msg(2, map[string]any{"text": "bad \xff\xfe text"})
	d.msg(3, map[string]any{"text": strings.Repeat("a", 300<<10)})
	d.msg(4, map[string]any{"subject": "bad \xff subject", "guid": "ok-\xfe-guid"})
	d.msg(5, map[string]any{"subject": "fine"})
	res := d.run(t)

	r := rec(t, res, 1)
	if r.Body != "nul\ufffdin text" || !slices.Equal(anyStrs(r.Payload["body_flags"]), []string{"nul"}) || r.Payload["body_raw_b64"] == nil {
		t.Errorf("NUL: %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 2)
	if r.Body != "bad \ufffd\ufffd text" || !slices.Equal(anyStrs(r.Payload["body_flags"]), []string{"invalid_utf8"}) {
		t.Errorf("invalid UTF-8: %q %v", r.Body, r.Payload)
	}
	r = rec(t, res, 3)
	if len(r.Body) != 256<<10 || r.Payload["body_truncated"] != true || !is(r.Payload["body_total_bytes"], 300<<10) || !slices.Contains(anyStrs(r.Payload["body_flags"]), "truncated") {
		t.Errorf("300 KiB: body %d bytes, %v", len(r.Body), r.Payload["body_truncated"])
	}
	r = rec(t, res, 4)
	if r.Payload["subject"] != "bad \ufffd subject" || r.Payload["guid"] != "ok-\ufffd-guid" ||
		!slices.Equal(anyStrs(rawOfRec(r)["text_flags"]), []string{"guid", "subject"}) {
		t.Errorf("altered subject and guid: %v", r.Payload)
	}
	r = rec(t, res, 5)
	if r.Payload["subject"] != "fine" || rawOfRec(r)["text_flags"] != nil {
		t.Errorf("clean subject: %v", r.Payload)
	}
}

// 4E-R41: an item or reaction type that is not a decided integer is unknown, never text.
func TestUnknownItemOrAssociatedTypeIsKindUnknown(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"item_type": nil})
	d.msg(2, map[string]any{"associated_message_type": "abc"})
	d.msg(3, map[string]any{"item_type": "x"})
	d.msg(4, map[string]any{})
	d.msg(5, map[string]any{"associated_message_type": nil})
	res := d.run(t)
	for _, c := range []struct {
		id  int64
		col string
		why string
	}{{1, "item_type", "null"}, {2, "associated_message_type", "type-mismatch"}, {3, "item_type", "type-mismatch"}, {5, "associated_message_type", "null"}} {
		r := rec(t, res, c.id)
		uc, _ := rawOfRec(r)["unknown_columns"].(map[string]any)
		if r.Payload["kind"] != "unknown" || uc[c.col] != c.why || r.Body == "" {
			t.Errorf("row %d: kind %v unknown_columns %v (body %q)", c.id, r.Payload["kind"], uc, r.Body)
		}
	}
	if r := rec(t, res, 4); r.Payload["kind"] != "text" || rawOfRec(r)["unknown_columns"] != nil {
		t.Errorf("row 4: %v", r.Payload)
	}

	// item_type as a virtual generated column is never evaluated: its cell is unreadable, not zero
	gen := func(sql string) string {
		return strings.Replace(sql, "item_type INTEGER DEFAULT 0", "item_type INTEGER GENERATED ALWAYS AS (0) VIRTUAL", 1)
	}
	g := newMem(t, "sms-ios17.sql", gen, "message")
	g.msg(1, nil)
	r := rec(t, g.run(t), 1)
	uc, _ := rawOfRec(r)["unknown_columns"].(map[string]any)
	if r.Payload["kind"] != "unknown" || uc["item_type"] == nil {
		t.Errorf("an unreadable item_type: kind %v unknown_columns %v", r.Payload["kind"], uc)
	}

	// a variant without the columns still gives text
	o := newMem(t, "sms-ios10.sql", nil, "message")
	o.msg(1, nil)
	r = rec(t, o.run(t), 1)
	if r.Payload["kind"] != "text" || rawOfRec(r)["unknown_columns"] != nil {
		t.Errorf("ios10 without associated_message_*: %v", r.Payload)
	}
}

// Review focus 1 for 4E-R20: a join that cannot be decided is unknown, never an empty member list.
func TestUndecidableChatJoinIsUnknownNotEmpty(t *testing.T) {
	const rows = 800
	d := newSMS(t)
	d.dims()
	for i := int64(1); i <= rows; i++ {
		if i%2 == 0 {
			d.out(i, nil)
		} else {
			d.msg(i, nil)
		}
		d.join("chat_message_join", map[string]any{"chat_id": int64(2), "message_id": i, "message_date": int64(1 << 40)})
	}
	leaves := d.tbl["chat_message_join"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	data := d.b.Bytes()
	ps := d.b.PageSize()
	page := data[int(leaves[1]-1)*ps:][:ps]
	page[3], page[4] = 0, 0 // the second leaf holds no cells any more
	res := runBytes(t, data)
	if len(res.Records) != rows {
		t.Fatalf("%d records, want all %d messages", len(res.Records), rows)
	}
	for i := int64(1); i <= rows; i++ {
		r := rec(t, res, i)
		raw := rawOfRec(r)
		if !slices.Equal(anyStrs(raw["join_undecided"]), []string{"chat_message_join"}) {
			t.Errorf("row %d: join_undecided = %v", i, raw["join_undecided"])
		}
		if r.Payload["thread"] != nil || raw["thread_state"] != nil {
			t.Errorf("row %d: thread %v state %v from an undecided join", i, r.Payload["thread"], raw["thread_state"])
		}
		if i%2 == 0 && (r.Payload["participants_unknown"] != true || len(partsOf(r)) != 0) {
			t.Errorf("row %d: outgoing participants %v", i, r.Payload)
		}
		if i%2 == 1 && !slices.Equal(addrsOf(r), []string{"+15551230001"}) {
			t.Errorf("row %d: incoming from %v", i, addrsOf(r))
		}
	}
}

func TestParserIdentity(t *testing.T) {
	if iossms.Name != "ios.sms" || iossms.Version != "1.0.1" {
		t.Errorf("%s %s", iossms.Name, iossms.Version)
	}
}

// An outgoing message whose error cell cannot be read as an integer has an unknown status, never delivered or sent.
func TestOutgoingStatusIsUnknownWhenErrorCellUndecided(t *testing.T) {
	d := newSMS(t, "message")
	d.out(1, map[string]any{"error": nil, "is_delivered": int64(1)})
	d.out(2, map[string]any{"error": "boom", "is_delivered": int64(1)})
	d.out(3, map[string]any{"error": 1.5, "is_sent": int64(1)})
	d.out(4, map[string]any{"error": int64(0), "is_delivered": int64(1)})
	res := d.run(t)
	for id, want := range map[int64]string{1: "unknown", 2: "unknown", 3: "unknown", 4: "delivered"} {
		if got := rec(t, res, id).Payload["status"]; got != want {
			t.Errorf("row %d: status %v, want %s", id, got, want)
		}
	}
}
