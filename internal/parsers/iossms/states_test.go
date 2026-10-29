package iossms_test

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Review fix 1: members that cannot be resolved never turn into the handle_id fallback.
func TestOutgoingWithUnresolvedMembersIsNeverTheHandleFallback(t *testing.T) {
	d := newSMS(t)
	d.dims()
	d.join("chat_handle_join", map[string]any{"chat_id": int64(2), "handle_id": int64(99)}) // dangling member of the group
	d.add("chat", 3, map[string]any{"guid": "c3", "style": int64(45)})
	d.join("chat_handle_join", map[string]any{"chat_id": int64(3), "handle_id": int64(98)}) // the only member is dangling
	d.out(1, map[string]any{"handle_id": int64(3)})
	d.out(2, map[string]any{"handle_id": int64(1)})
	d.inChat(1, 2)
	d.inChat(2, 3)
	res := d.run(t)

	r := rec(t, res, 1)
	if got := addrsOf(r); !slices.Equal(got, []string{"+15551230001", "alice@example.test", "+15551230003"}) {
		t.Errorf("partly resolved group: to = %v", got)
	}
	if r.Payload["participants_unknown"] != true || !is(rawOfRec(r)["members_unresolved"], 1) {
		t.Errorf("a partly resolved group is incomplete and must say so: %v", r.Payload)
	}
	r = rec(t, res, 2)
	if len(partsOf(r)) != 0 || r.Payload["participants_unknown"] != true || !is(rawOfRec(r)["members_unresolved"], 1) {
		t.Errorf("a chat with only unresolved members fell back to the handle: %v", r.Payload)
	}
}

// Review fix 2 (P-4-1): a non-finite REAL is stated by its IEEE bits, never as a value or a time.
func TestNonFiniteDateIsIEEEBitsNotATime(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"date": math.Inf(1)})
	d.msg(2, map[string]any{"date_read": math.Inf(-1)})
	res := d.run(t)
	for id, bits := range map[int64]float64{1: math.Inf(1)} {
		r := rec(t, res, id)
		raw := rawOfRec(r)
		want := strconv.FormatUint(math.Float64bits(bits), 16)
		if _, has := raw["date"]; has || raw["date_ieee"] != want || raw["date_status"] != "invalid" || r.Time != nil {
			t.Errorf("row %d: want date_ieee %s, status invalid, no date, no time; got raw %v time %v", id, want, raw, r.Time)
		}
	}
	r := rec(t, res, 2)
	if raw := rawOfRec(r); raw["date_read_ieee"] == nil || raw["date_read_status"] != "invalid" || timeOf(r, "read") != nil {
		t.Errorf("date_read -Inf: %v %v", raw, r.Times)
	}
}

// Review fix 3: every state value of the contract has a direct test.
func TestHandleAndChatStates(t *testing.T) {
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"handle_id": int64(5)})
	d.msg(2, map[string]any{"handle_id": int64(-1)})
	r := rec(t, d.run(t), 1)
	if rawOfRec(r)["handle_state"] != "no-handle-table" || r.Payload["participants_unknown"] != true {
		t.Errorf("no handle table: %v", r.Payload)
	}
	// review fix 5: a negative handle id is stated, not treated as no handle
	r = rec(t, d.run(t), 2)
	if rawOfRec(r)["handle_state"] != "invalid-id" || !is(rawOfRec(r)["handle_id"], -1) || r.Payload["participants_unknown"] != true {
		t.Errorf("handle_id -1: %v", r.Payload)
	}

	h := newSMS(t, "message", "handle")
	h.add("handle", 1, map[string]any{"id": "", "service": "SMS"})
	h.add("handle", 2, map[string]any{"id": nil, "service": "SMS"})
	h.add("handle", 3, map[string]any{"id": strings.Repeat("9", 1025), "service": "SMS"})
	h.add("handle", 4, map[string]any{"id": strings.Repeat("9", 1024), "service": "SMS"})
	for i := int64(1); i <= 4; i++ {
		h.msg(i, map[string]any{"handle_id": i})
	}
	res := h.run(t)
	for id, want := range map[int64]string{1: "no-address", 2: "no-address", 3: "address-too-long"} {
		r := rec(t, res, id)
		if rawOfRec(r)["handle_state"] != want || len(partsOf(r)) != 0 || r.Payload["participants_unknown"] != true {
			t.Errorf("handle %d: want %s, got %v", id, want, r.Payload)
		}
	}
	if r := rec(t, res, 4); len(partsOf(r)) != 1 || rawOfRec(r)["handle_state"] != nil {
		t.Errorf("an address of exactly 1024 bytes is kept: %v", r.Payload)
	}

	c := newSMS(t, "message", "handle", "chat_message_join", "chat_handle_join")
	c.add("handle", 1, map[string]any{"id": "+15551230001", "service": "iMessage"})
	c.join("chat_handle_join", map[string]any{"chat_id": int64(1), "handle_id": int64(1)})
	c.msg(1, nil)
	c.inChat(1, 1)
	r = rec(t, c.run(t), 1)
	th, _ := r.Payload["thread"].(map[string]any)
	if rawOfRec(r)["thread_state"] != "no-chat-table" || th["id"] != "1" || th["members"] != nil {
		t.Errorf("no chat table: thread %v raw %v", th, rawOfRec(r))
	}

	j := newSMS(t, "message", "handle", "chat", "chat_message_join")
	j.add("handle", 1, map[string]any{"id": "+15551230001", "service": "iMessage"})
	j.add("chat", 1, map[string]any{"guid": "c1", "style": int64(45)})
	j.out(1, nil)
	j.inChat(1, 1)
	r = rec(t, j.run(t), 1)
	th, _ = r.Payload["thread"].(map[string]any)
	if rawOfRec(r)["members_state"] != "no-join-table" || th["id"] != "1" || th["members"] != nil ||
		len(partsOf(r)) != 0 || r.Payload["participants_unknown"] != true {
		t.Errorf("no chat_handle_join table: thread %v raw %v participants %v", th, rawOfRec(r), r.Payload["participants"])
	}
}

// membersOf with an undecidable chat_handle_join: unknown members, never an empty list or a handle fallback.
func TestUndecidableChatHandleJoinIsUnknownNotEmpty(t *testing.T) {
	d := newSMS(t)
	d.dims()
	d.out(1, map[string]any{"handle_id": int64(1)})
	d.inChat(1, 1)
	for i := int64(0); i < 1500; i++ { // filler rows of other chats, to spread the table over several leaves
		d.join("chat_handle_join", map[string]any{"chat_id": 1000 + i, "handle_id": int64(1)})
	}
	leaves := d.tbl["chat_handle_join"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	data := d.b.Bytes()
	ps := d.b.PageSize()
	page := data[int(leaves[1]-1)*ps:][:ps]
	page[3], page[4] = 0, 0
	r := rec(t, runBytes(t, data), 1)
	raw := rawOfRec(r)
	th, _ := r.Payload["thread"].(map[string]any)
	if !slices.Equal(anyStrs(raw["join_undecided"]), []string{"chat_handle_join"}) || th["members"] != nil ||
		len(partsOf(r)) != 0 || r.Payload["participants_unknown"] != true {
		t.Errorf("thread %v raw %v participants %v", th, raw, r.Payload["participants"])
	}
}

// Review fix 4: a well-formed stream without a string object is not "undecodable".
func TestAttributedBodyWithoutStringIsNoString(t *testing.T) {
	stream := append([]byte{0x04, 0x0B}, "streamtyped"...)
	stream = append(stream, 0x81, 0xE8, 0x03)
	stream = append(stream, make([]byte, 70000)...)
	d := newSMS(t, "message")
	d.msg(1, map[string]any{"text": nil, "attributedBody": stream})
	r := rec(t, d.run(t), 1)
	if rawOfRec(r)["attributed_body_status"] != "no-string" || r.Body != "" || r.Payload["kind"] == "text" {
		t.Errorf("%q %v", r.Body, r.Payload)
	}
}
