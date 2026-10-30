package iossms_test

import "testing"

// A message joined to chat rows with repeated chat ids lists each chat once, sorted, and counts it once.
func TestChatFanOutIsDedupedAndCounted(t *testing.T) {
	const distinct = 1200
	d := newSMS(t, "message", "chat_message_join")
	d.msg(1, nil)
	for rep := 0; rep < 2; rep++ {
		for c := int64(distinct); c >= 1; c-- {
			d.join("chat_message_join", map[string]any{"chat_id": c, "message_id": int64(1), "message_date": int64(1)})
		}
	}
	raw := rawOfRec(rec(t, d.run(t), 1))
	if raw["thread_state"] != "several-chats" || raw["chat_ids_total"] != int64(distinct) || raw["chat_ids_truncated"] != true {
		t.Errorf("fan-out: state %v total %v truncated %v", raw["thread_state"], raw["chat_ids_total"], raw["chat_ids_truncated"])
	}
	if ids, _ := raw["chat_ids"].([]any); len(ids) != 1000 || ids[0] != int64(1) || ids[999] != int64(1000) {
		t.Errorf("chat_ids not the sorted first 1000: %d", len(ids))
	}
}
