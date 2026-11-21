package androidtelegram_test

import (
	"strconv"
	"strings"
	"testing"
)

// Spec 8.5 (reply copy: never a record, never Body, never target_guid; the 4I quote rule), F6, Q10/T6 (the counted
// tables are notes only): on telegram-v181 the records whose locator names messages_v2 are as many as its rows, none
// comes from replydata (R1's text is the Body of R1 alone though R30 holds a copy of R1), messages_topics or media_v4
// (their row counts are notes); R30's raw.reply_copy is the facts, without target_guid, and its Body is its own text;
// a replydata holding a non-Message ID is stated by its blob state, length and SHA-256, and gives no copy.
func TestTelegramReplyCopyIsNeverAMessage(t *testing.T) {
	res := parseFixture(t, "telegram-v181")
	n, err := strconv.Atoi(res.Notes["rows.messages_v2"])
	if err != nil {
		t.Fatalf("note rows.messages_v2 %q", res.Notes["rows.messages_v2"])
	}
	inV2, r1 := 0, 0
	for _, r := range res.Records {
		if strings.Contains(r.Locator, "table=messages_v2;") {
			inV2++
		}
		for _, tbl := range []string{"replydata", "messages_topics", "media_v4"} {
			if strings.Contains(r.Locator, tbl) {
				t.Errorf("record %s names %s", r.Locator, tbl)
			}
		}
		if r.Body == "Hello Ann here" {
			r1++
		}
	}
	if inV2 != n {
		t.Errorf("%d records name messages_v2, the table has %d rows", inV2, n)
	}
	if r1 != 1 {
		t.Errorf("%d records carry R1's text, want R1 alone (R30's copy is raw only)", r1)
	}
	for _, k := range []string{"rows.messages_topics", "rows.media_v4"} {
		if _, ok := res.Notes[k]; !ok {
			t.Errorf("note %s absent: the counted table is stated by its row count", k)
		}
	}
	r30 := byRow(res.Records)["messages_v2/30"]
	raw := rawOf(r30)
	want := `{"constructor":"0x7600b9d3","copy":true,"date":1700000001,"id":101,"text":"Hello Ann here"}`
	if got := jsonText(raw["reply_copy"]); got != want {
		t.Errorf("R30: raw.reply_copy %s, want %s", got, want)
	}
	if raw["replydata_state"] != "decoded" || raw["reply_copy_state"] != nil {
		t.Errorf("R30: replydata_state %v, reply_copy_state %v", raw["replydata_state"], raw["reply_copy_state"])
	}
	if _, has := r30.Payload["target_guid"]; has || r30.Body != "Reply" {
		t.Errorf("R30: target_guid %v, Body %q: the copy is never a target nor the Body", r30.Payload["target_guid"], r30.Body)
	}

	const ann, self = 1000002, 1000001
	story := append(le32(0x0badf00d), make([]byte, 12)...) // an ID no Message has (a StoryItem or an unknown one)
	cells := []msgCells{{
		mid: 1, uid: ann, data: tlMessage{id: 1, from: ann, peer: self, date: baseDate, text: "re"}.bytes(), replydata: story,
	}}
	got := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records
	if len(got) != 1 {
		t.Fatalf("%d records for one row", len(got))
	}
	raw = rawOf(got[0])
	wantRaw := map[string]any{
		"reply_copy_state": "unknown-constructor", "replydata_state": "unknown-constructor", "replydata_len": len(story),
		"replydata_sha256": sha256Of(story), "replydata_constructor": "0x0badf00d", "reply_copy": nil,
	}
	for k, w := range wantRaw {
		v, has := raw[k]
		if (w == nil && has) || (w != nil && jsonText(v) != jsonText(w)) {
			t.Errorf("non-Message replydata: raw.%s = %v (present %v), want %v", k, v, has, w)
		}
	}
	if got[0].Body != "re" {
		t.Errorf("non-Message replydata: Body %q", got[0].Body)
	}
}
