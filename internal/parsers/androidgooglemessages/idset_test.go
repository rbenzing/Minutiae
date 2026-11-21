package androidgooglemessages_test

import (
	"strconv"
	"strings"
	"testing"
)

// B6, R12 (Task 6 review I1a): a messages table the census cannot read whole leaves the decided id set incomplete (Parse
// states it before the run), so the parts of the messages on the damaged page are row warnings, never orphan records, and
// every parts row is joined, an orphan or a row warning.
func TestGoogleMessagesUnreadableMessagesTableIsNoOrphan(t *testing.T) {
	var msgs, parts []mrow
	for i := int64(1); i <= 120; i++ {
		msgs = append(msgs, mrow{i, map[string]any{"mms_subject": strings.Repeat("s", 200)}})
		parts = append(parts, mrow{i, map[string]any{"message_id": i, "text": "t"}})
	}
	b := store{participants: []mrow{person(2, "+15550000002")}, conversations: []mrow{{1, map[string]any{}}}, messages: msgs, parts: parts}.builder(t)
	leaves := b.Object("messages").Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d messages leaves, want several", len(leaves))
	}
	dmg := b.PageBytes(leaves[1])
	dmg[3], dmg[4] = 0, 0 // cell count 0: the engine calls the page corrupt
	res := parseAt(t, remoteApp, dbOnly(b.Bytes()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Notes["messages"] != "unreadable" {
		t.Fatalf("note messages = %q, want unreadable (the census read the damaged table whole)", res.Notes["messages"])
	}
	got := byRowid(t, res)
	if len(got) == 0 || len(got) >= 120 {
		t.Fatalf("%d messages records, want some but not all of 120", len(got))
	}
	if n := len(partsByRowid(t, res)); n != 0 {
		t.Errorf("%d orphan records for the parts of messages on a damaged page", n)
	}
	joined, err := strconv.Atoi(res.Notes["parts.joined"])
	if err != nil {
		t.Fatalf("note parts.joined = %q", res.Notes["parts.joined"])
	}
	warned := 0
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=parts") {
			if !strings.Contains(w.Reason, "message-id set is incomplete (the messages table could not be read whole)") {
				t.Errorf("warning %s: %s", w.Locator, w.Reason)
			}
			warned++
		}
	}
	if joined != len(got) || warned != 120-len(got) || joined+warned != 120 || res.Notes["orphan_parts"] != "0" {
		t.Errorf("joined %d + warnings %d of 120 parts, %d messages records, orphan_parts %q", joined, warned, len(got), res.Notes["orphan_parts"])
	}
}
