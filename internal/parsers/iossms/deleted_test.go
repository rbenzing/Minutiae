package iossms_test

import (
	"slices"
	"testing"
	"time"
)

// The recently-deleted marker is the app's own: the message is Deleted with the join as its source, the delete
// time is a Times entry, and nothing is presented as recovered (no Recovery, no Confidence).
func TestRecentlyDeletedMessageIsTheAppsOwnMarker(t *testing.T) {
	d := newSMS(t)
	d.dims()
	d.msg(1, nil)
	d.msg(2, nil)
	d.msg(3, nil)
	d.inChat(1, 1)
	d.join("chat_recoverable_message_join", map[string]any{"chat_id": int64(1), "message_id": int64(2), "delete_date": int64(700000000_000000000)})
	d.join("chat_recoverable_message_join", map[string]any{"chat_id": int64(2), "message_id": int64(3), "delete_date": int64(0)})
	res := d.run(t)

	r := rec(t, res, 2)
	if !r.Deleted {
		t.Error("the joined message is not Deleted")
	}
	if del, _ := r.Payload["deleted"].(map[string]any); del["source"] != "chat_recoverable_message_join" {
		t.Errorf("deleted = %v", r.Payload["deleted"])
	}
	if got, want := timeOf(r, "deleted"), cocoaEpoch.Add(700000000*time.Second); got == nil || !got.T.Equal(want) {
		t.Errorf("Times.deleted = %v, want %v", got, want)
	}
	if r.Recovery != "" || r.Confidence != nil {
		t.Errorf("recovery %q confidence %v: the app's own marker is not a recovery", r.Recovery, r.Confidence)
	}
	raw := rawOfRec(r)
	if !is(raw["recoverable_chat_id"], 1) || raw["delete_date_unit"] != "nanoseconds" {
		t.Errorf("raw %v", raw)
	}
	if raw["thread_state"] != "no-chat-join" {
		t.Errorf("thread_state %v", raw["thread_state"])
	}

	// a sentinel delete date is stated, never a time; the message is still deleted
	r = rec(t, res, 3)
	if !r.Deleted || timeOf(r, "deleted") != nil || rawOfRec(r)["delete_date_status"] != "sentinel" {
		t.Errorf("sentinel delete_date: deleted %v times %v raw %v", r.Deleted, r.Times, rawOfRec(r))
	}
	if r := rec(t, res, 1); r.Deleted || r.Payload["deleted"] != nil || timeOf(r, "deleted") != nil {
		t.Errorf("a message without a join row is Deleted: %v", r.Payload)
	}

	// a database without the table: nothing is deleted, nothing is invented
	d2 := newSMS(t, "message", "handle")
	d2.msg(1, nil)
	if r := rec(t, d2.run(t), 1); r.Deleted || r.Payload["deleted"] != nil {
		t.Errorf("no recoverable table, yet Deleted: %v", r.Payload)
	}
}

// An undecidable recoverable join is unknown: the table is named, the message is not called deleted.
func TestUndecidableRecoverableJoinIsNamed(t *testing.T) {
	d := newSMS(t)
	d.dims()
	for i := int64(1); i <= 600; i++ {
		d.msg(i, nil)
		d.join("chat_recoverable_message_join", map[string]any{"chat_id": int64(1), "message_id": i, "delete_date": int64(700000000)})
	}
	leaves := d.tbl["chat_recoverable_message_join"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	data := d.b.Bytes()
	ps := d.b.PageSize()
	page := data[int(leaves[1]-1)*ps:][:ps]
	page[3], page[4] = 0, 0
	res := runBytes(t, data)
	named := 0
	for i := int64(1); i <= 600; i++ {
		r := rec(t, res, i)
		if slices.Equal(anyStrs(rawOfRec(r)["join_undecided"]), []string{"chat_recoverable_message_join"}) {
			named++
			if r.Deleted && r.Payload["deleted"] == nil {
				t.Errorf("row %d: Deleted without a deleted source", i)
			}
		}
	}
	if named != 600 {
		t.Errorf("%d of 600 messages name the undecided recoverable join; every answer of a lossy index is flagged", named)
	}
}
