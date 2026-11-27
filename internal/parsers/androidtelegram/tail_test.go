package androidtelegram_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
)

// mappedTail4I4b are the fourteen names plan 4I4b "Raw shapes" adds to the mapped fields after ttl_period, in order.
func mappedTail4I4b() []string {
	return []string{
		"forwards", "post_author", "replies", "reactions", "restriction_reason", "quick_reply_shortcut_id", "effect",
		"factcheck", "report_delivery_until_date", "paid_message_stars", "suggested_post", "schedule_repeat_period",
		"summary_from_language", "entities",
	}
}

// tailKeys are the raw keys of the plan's Tail that no record field and no payload key outside raw may carry.
func tailKeys() []string {
	return []string{
		"blob_forwards", "forwards_conflict", "post_author", "quick_reply_shortcut_id", "effect", "paid_message_stars",
		"report_delivery_until_date", "schedule_repeat_period", "summary_from_language", "replies", "reactions",
		"reactions_results", "reactions_recent", "reactions_top_reactors", "restriction_reason", "factcheck",
		"suggested_post", "entities", "replies_data", "replies_conflict",
	}
}

// wantJSON fails when the value got is not the JSON text want (map keys sorted, as encoding/json writes them).
func wantJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	if g := jsonText(got); g != want {
		t.Errorf("%s = %s, want %s", what, g, want)
	}
}

// Plan 4I4b section 8.3, P3, P4, P8, Review Focus 2: the tail of the message blob (the blob's forwards and its
// conflict with the column, post_author, the replies, reactions, restriction reasons, fact check, suggested post and
// entities) is stated in raw only, never as a record field; every list is capped with its _total; an object or list a
// stop cut is "partial", never shown; a time of no pinned unit is raw with _unit unestablished.
func TestTelegramTailObjectsAreRawOnly(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	raw := func(n string) map[string]any { return rawOf(rows["messages_v2/"+n]) }

	r60 := raw("60")
	for k, want := range map[string]string{
		"blob_forwards": "4", "post_author": `"Desk"`, "quick_reply_shortcut_id": "7", "effect": "5000000002",
		"report_delivery_until_date": "1700001000", "report_delivery_until_date_unit": `"unestablished"`,
		"paid_message_stars": "10", "schedule_repeat_period": "86400", "schedule_repeat_period_unit": `"unestablished"`,
		"summary_from_language": `"en"`, "entities_total": "1",
		"entities": `[{"length":6,"offset":0,"type":"text_url","url":"fixture.invalid/r"}]`,
	} {
		wantJSON(t, "R60 raw."+k, r60[k], want)
	}
	if r60["forwards_conflict"] != nil {
		t.Errorf("R60: forwards_conflict %v, the blob and the column both say 4", r60["forwards_conflict"])
	}
	if raw("61")["forwards_conflict"] != true {
		t.Errorf("R61: forwards_conflict %v, want true (the blob says 5, the column 4: B10)", raw("61")["forwards_conflict"])
	}

	r62 := raw("62")
	wantJSON(t, "R62 raw.reactions_results", r62["reactions_results"],
		`[{"chosen_order":1,"count":2,"reaction":"emoji:👍"},{"count":1,"reaction":"custom:5000000001"},{"count":3,"reaction":"paid"},{"count":0,"reaction":"empty"}]`)
	recent, _ := r62["reactions_recent"].([]any)
	var peers []string
	for _, e := range recent {
		m, _ := e.(map[string]any)
		peers = append(peers, jsonText(m["peer"]))
	}
	if strings.Join(peers, ",") != `"user:1000002","user:1000001","user:1000002","user:1000009"` {
		t.Errorf("R62 reactions_recent peers %v", peers)
	}
	var first map[string]any
	if len(recent) > 0 {
		first, _ = recent[0].(map[string]any)
	}
	if jsonText(first["date"]) != "1700000800" || first["date_unit"] != "unestablished" || first["big"] != true {
		t.Errorf("R62 first recent reaction %v: want date 1700000800 with date_unit unestablished (P4) and big", first)
	}
	if l, _ := r62["reactions_top_reactors"].([]any); len(l) != 3 {
		t.Errorf("R62 reactions_top_reactors has %d entries, want 3", len(l))
	}
	obj, _ := r62["reactions"].(map[string]any)
	wantJSON(t, "R62 raw.reactions totals", []any{obj["results_total"], obj["recent_total"], obj["top_reactors_total"]}, `[4,4,3]`)

	r65 := raw("65")
	wantJSON(t, "R65 raw.restriction_reason", r65["restriction_reason"], `[{"platform":"android","reason":"sensitive","text":"Hidden on Android"}]`)
	wantJSON(t, "R65 raw.restriction_reason_total", r65["restriction_reason_total"], "1")
	fc, _ := r65["factcheck"].(map[string]any)
	if fc["country"] != "US" || fc["text"] != "Context added" || jsonText(fc["entities_total"]) != "1" || jsonText(fc["hash"]) != "99" || fc["need_check"] != true {
		t.Errorf("R65 raw.factcheck = %v", fc)
	}
	sp, _ := r65["suggested_post"].(map[string]any)
	wantJSON(t, "R65 suggested_post.price", sp["price"], `{"amount":50,"kind":"stars","nanos":0}`)
	if sp["accepted"] != true || jsonText(sp["schedule_date"]) != "1700000900" || sp["schedule_date_unit"] != "unestablished" {
		t.Errorf("R65 raw.suggested_post = %v", sp)
	}
	sp, _ = raw("66")["suggested_post"].(map[string]any)
	wantJSON(t, "R66 suggested_post.price", sp["price"], `{"amount":1000000000,"kind":"ton"}`)
	if sp["rejected"] != true {
		t.Errorf("R66 raw.suggested_post = %v, want rejected", sp)
	}

	// no tail value is a payload field or a Body; they are raw only
	for _, n := range []string{"60", "61", "62", "63", "65", "66"} {
		for _, k := range tailKeys() {
			if _, has := rows["messages_v2/"+n].Payload[k]; has {
				t.Errorf("R%s: payload.%s is a record field; the tail is raw only", n, k)
			}
		}
	}

	// a stop inside the reactions: partial, nothing of them shown (Review Focus 2)
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	for _, n := range []string{"2806", "2807"} {
		h := rawOf(hostile["messages_v2/"+n])
		if h["reactions_state"] != "partial" || h["data_stop_id"] != "0x6fb250d1" {
			t.Errorf("H%s: reactions_state %v, data_stop_id %v; want partial at 0x6fb250d1", n, h["reactions_state"], h["data_stop_id"])
		}
		for _, k := range []string{"reactions", "reactions_results", "reactions_recent", "reactions_top_reactors"} {
			if _, has := h[k]; has {
				t.Errorf("H%s: raw.%s stated although the decode stopped inside the reactions", n, k)
			}
		}
	}

	// an identifier over its limit is never cut: summary_from_language of 65 bytes
	lang := strings.Repeat("l", 65)
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: idAnn, data: tlMessage{
		flags2: 1 << 11, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: tlStr(lang),
	}.bytes()}})))
	g := rawOf(byRow(res.Records)["messages_v2/1"])
	if g["summary_from_language_state"] != "too-long" || jsonText(g["summary_from_language_len"]) != "65" ||
		g["summary_from_language_sha256"] != sha256Hex(lang) || g["summary_from_language"] != nil {
		t.Errorf("summary_from_language of 65 bytes: %v; want too-long with its length and digest, no value", g)
	}
	mentionUserClass(t)
}

// mentionUserClass is the final-review ruling F2 part of TestTelegramTailObjectsAreRawOnly, as ruling Q6 amends it
// (plan 4I4c): a TL_inputMessageEntityMentionName whose user_id is an InputUser object states user_class, the object's
// class label; the telegram-layers rows of TL_inputUserEmpty and TL_inputUserSelf never state a user, and a full
// InputUser (user, from_message) states the user of its own user_id.
func mentionUserClass(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	for _, r := range parseFixture(t, "telegram-layers").Records {
		l, _ := rawOf(r)["entities"].([]any)
		for _, e := range l {
			m, _ := e.(map[string]any)
			if m["type"] != "input_mention_name" {
				continue
			}
			c, _ := m["user_class"].(string)
			switch {
			case c == "":
				t.Errorf("%s: input_mention_name %v; want a user_class", r.Locator, m)
			case (c == "self" || c == "empty") && m["user"] != nil:
				t.Errorf("%s: input_mention_name %v; want no user for the class %s", r.Locator, m, c)
			case (c == "user" || c == "from_message") && m["user"] == nil:
				t.Errorf("%s: input_mention_name %v; want the user of the full InputUser", r.Locator, m)
			}
			seen[c] = true
		}
	}
	if !seen["empty"] || !seen["self"] {
		t.Errorf("user_class values seen %v; want empty and self", seen)
	}
}

// Plan 4I4b section 8.3: the people in a reaction list, a top-reactor list or the recent repliers are never
// participants. R62 has reactors ANN, SELF and user 1000009: the participants are the 4I4 rule's (the sender ANN
// only), and user:1000009 appears only in raw.reactions_recent.
func TestTelegramReactorsAreNeverParticipants(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	for _, n := range []string{"62", "63"} {
		r := rows["messages_v2/"+n]
		ps, _ := r.Payload["participants"].([]any)
		if len(ps) != 1 || party(r, "from") != "user:1000002" {
			t.Errorf("R%s: participants %v, want the sender user:1000002 only", n, ps)
		}
		if strings.Contains(jsonText(ps), "1000009") {
			t.Errorf("R%s: a reactor is a participant: %v", n, ps)
		}
	}
	r62 := rows["messages_v2/62"]
	if !strings.Contains(jsonText(rawOf(r62)["reactions_recent"]), "user:1000009") {
		t.Error("R62: user:1000009 is missing from raw.reactions_recent")
	}
	if strings.Contains(jsonText(r62.Payload["participants"])+r62.Summary+r62.Body, "1000009") {
		t.Error("R62: user:1000009 reached the participants, the Summary or the Body")
	}
	if got := jsonText(rawOf(rows["messages_v2/63"])["replies"]); !strings.Contains(got, `"recent_repliers":["user:1000002"]`) {
		t.Errorf("R63: raw.replies %s, want the replier as an address in raw", got)
	}
	pollParticipants(t)
}

// Plan 4I4b design D13, rulings B8, P3 (Review Focus 3): replies_data is decoded as a MessageReplies beside the blob
// and nothing is merged: raw.replies_data holds its replies shape with replies_data_state, and raw.replies_conflict
// is stated only when both the blob's replies and the cell decoded whole and the two decoded values differ. A cell
// beside a blob without replies, or whose decode stopped before replies, states no conflict; an undecodable cell is a
// state, counted, never silent.
func TestTelegramRepliesDataConflictIsStated(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r63, r64 := rawOf(rows["messages_v2/63"]), rawOf(rows["messages_v2/64"])
	for n, raw := range map[string]map[string]any{"R63": r63, "R64": r64} {
		if raw["replies_data_state"] != "decoded" || raw["replies_data"] == nil {
			t.Errorf("%s: replies_data_state %v, replies_data %v; want decoded and stated", n, raw["replies_data_state"], raw["replies_data"])
		}
	}
	if r63["replies_conflict"] != nil {
		t.Errorf("R63: replies_conflict %v, the two agree", r63["replies_conflict"])
	}
	if r64["replies_conflict"] != true || jsonText(r64["replies_data"].(map[string]any)["replies"]) != "4" {
		t.Errorf("R64: replies_conflict %v, replies_data %v; want a conflict with replies 4 in the cell", r64["replies_conflict"], r64["replies_data"])
	}

	// Go-built: TL_messageReplies (flags 0, replies 3, replies_pts 77)
	cell := slices.Concat(le32(0x83d60fc2), le32(0), le32(3), le32(77))
	withReplies := tlMessage{flags: 1 << 23, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: cell}
	noReplies := tlMessage{id: 2, from: idAnn, peer: idSelf, date: baseDate, text: "x"}
	// the decode stops at an unknown reply markup (P11), before replies (F.6 before F.23)
	stopped := tlMessage{flags: 1 << 23, id: 3, from: idAnn, peer: idSelf, date: baseDate, text: "x", markup: le32(idUnknownMarkup)}
	badCell := []byte{1, 2, 3}
	diff := slices.Concat(le32(0x83d60fc2), le32(0), le32(5), le32(77))
	mk := func(mid int64, m tlMessage, rd []byte) msgCells {
		return msgCells{mid: mid, uid: idAnn, data: m.bytes(), repliesData: rd}
	}
	v2 := []msgCells{mk(1, withReplies, cell), mk(2, noReplies, cell), mk(3, stopped, cell), mk(4, noReplies, badCell), mk(5, withReplies, diff), mk(6, withReplies, badCell)}
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, v2)))
	got := byRow(res.Records)
	want := `{"replies":3,"replies_pts":77}`
	for _, n := range []string{"1", "2", "3"} {
		raw := rawOf(got["messages_v2/"+n])
		if raw["replies_data_state"] != "decoded" || jsonText(raw["replies_data"]) != want || raw["replies_conflict"] != nil {
			t.Errorf("row %s: replies_data_state %v, replies_data %v, replies_conflict %v; want decoded %s, no conflict", n, raw["replies_data_state"], raw["replies_data"], raw["replies_conflict"], want)
		}
	}
	if raw := rawOf(got["messages_v2/1"]); jsonText(raw["replies"]) != want {
		t.Errorf("row 1: raw.replies %v, want %s", raw["replies"], want)
	}
	if raw := rawOf(got["messages_v2/2"]); raw["replies"] != nil {
		t.Errorf("row 2: raw.replies %v, the blob has no replies", raw["replies"])
	}
	// plan 4I4c P11: the stop before replies is the unknown markup, at reply_markup
	if raw := rawOf(got["messages_v2/3"]); raw["replies"] != nil || raw["data_stop_kind"] != "unknown-constructor" || raw["data_stop_path"] != "reply_markup" {
		t.Errorf("row 3: raw.replies %v, data_stop_kind %v at %v; want none and an unknown-constructor stop at reply_markup", raw["replies"], raw["data_stop_kind"], raw["data_stop_path"])
	}
	if raw := rawOf(got["messages_v2/5"]); raw["replies_conflict"] != true {
		t.Errorf("row 5: replies_conflict %v, the blob says 3 and the cell 5", raw["replies_conflict"])
	}
	// a cell of 3 bytes: truncated, no shape, one counted warning for the column
	for _, n := range []string{"4", "6"} {
		raw := rawOf(got["messages_v2/"+n])
		if raw["replies_data_state"] != "truncated" || raw["replies_data"] != nil || raw["replies_conflict"] != nil {
			t.Errorf("row %s: replies_data_state %v, replies_data %v, replies_conflict %v; want truncated and nothing stated", n, raw["replies_data_state"], raw["replies_data"], raw["replies_conflict"])
		}
	}
	if !strings.Contains(res.Notes["blob_states"], "messages_v2.replies_data=decoded:4,truncated:2") {
		t.Errorf("blob_states %q, want the replies_data column counted (P6)", res.Notes["blob_states"])
	}
	var warned []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "telegram-blob-undecoded: messages_v2.replies_data") {
			warned = append(warned, w.Reason)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "2 records (truncated:2)") {
		t.Errorf("replies_data warnings %v, want one counting 2 truncated records", warned)
	}
	repliersPast16(t)
}

// repliersPast16 is the final-review ruling F1 part of TestTelegramRepliesDataConflictIsStated (B8: "different" is
// judged on decoded values): the conflict compares the full decoded replier lists, never the lists cut to 16 for the
// output. Two replies of 17 repliers that agree in the first 16 and differ only at the 17th state the same capped
// shape and replies_conflict true; 17 equal repliers state none.
func repliersPast16(t *testing.T) {
	t.Helper()
	// TL_messageReplies (flags F.1, replies 17, replies_pts 1, recent_repliers [17 peers]); last is the 17th user id
	replies := func(last int64) []byte {
		b := slices.Concat(le32(0x83d60fc2), le32(1<<1), le32(17), le32(1), le32(idTLVector), le32(17))
		for i := range int64(16) {
			b = append(b, peerUser(1000100+i)...)
		}
		return append(b, peerUser(last)...)
	}
	blob := replies(1000116)
	m := tlMessage{flags: 1 << 23, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: blob}
	v2 := []msgCells{
		{mid: 1, uid: idAnn, data: m.bytes(), repliesData: blob},
		{mid: 2, uid: idAnn, data: m.bytes(), repliesData: replies(1000999)},
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, v2))).Records)
	same, diff := rawOf(got["messages_v2/1"]), rawOf(got["messages_v2/2"])
	if same["replies_conflict"] != nil {
		t.Errorf("17 equal repliers: replies_conflict %v, want none", same["replies_conflict"])
	}
	if jsonText(diff["replies"]) != jsonText(diff["replies_data"]) {
		t.Fatalf("the capped shapes differ (%v, %v): the case does not isolate the 17th replier", diff["replies"], diff["replies_data"])
	}
	if diff["replies_conflict"] != true {
		t.Errorf("repliers differing only at the 17th: replies_conflict %v, want true (F1: the full lists are compared)", diff["replies_conflict"])
	}
}

// Plan 4I4b P3, Raw shapes caps: the tail lists are cut at their caps and the cut is counted, never silent. H2808 has 65
// reaction results (64 kept), H2809 17 restriction reasons (16), H2810 65 entities (64), H2811 17 top reactors (16),
// H2812 17 recent repliers (16).
func tailCaps4I4b(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	check := func(id, what string, list any, kept int, total any, truncated any) {
		t.Helper()
		if l, _ := list.([]any); len(l) != kept || jsonText(total) != itoa(int64(kept+1)) || truncated != true {
			t.Errorf("H%s: %d %s kept, total %v, truncated %v; want %d of %d stated", id, len(l), what, total, truncated, kept, kept+1)
		}
	}
	raw := rawOf(hostile["messages_v2/2808"])
	o, _ := raw["reactions"].(map[string]any)
	check("2808", "reaction results", raw["reactions_results"], 64, o["results_total"], o["results_truncated"])
	raw = rawOf(hostile["messages_v2/2809"])
	check("2809", "restriction reasons", raw["restriction_reason"], 16, raw["restriction_reason_total"], raw["restriction_reason_truncated"])
	raw = rawOf(hostile["messages_v2/2810"])
	check("2810", "entities", raw["entities"], 64, raw["entities_total"], raw["entities_truncated"])
	raw = rawOf(hostile["messages_v2/2811"])
	o, _ = raw["reactions"].(map[string]any)
	check("2811", "top reactors", raw["reactions_top_reactors"], 16, o["top_reactors_total"], o["top_reactors_truncated"])
	raw = rawOf(hostile["messages_v2/2812"])
	o, _ = raw["replies"].(map[string]any)
	check("2812", "recent repliers", o["recent_repliers"], 16, o["recent_repliers_total"], o["recent_repliers_truncated"])
}

// tailVocabulary4I4b is plan 4I4b Task 6's part of TestTelegramStateVocabularyIsPinned: the states of the tail keys.
func tailVocabulary4I4b(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	if got := rawOf(hostile["messages_v2/2806"])["reactions_state"]; got != "partial" {
		t.Errorf("H2806: reactions_state %v, want partial", got)
	}
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r62 := rawOf(rows["messages_v2/62"])
	for _, e := range r62["reactions_recent"].([]any) {
		if m, _ := e.(map[string]any); m["date"] != nil && m["date_unit"] != "unestablished" {
			t.Errorf("reaction date %v: date_unit %v, want unestablished", m["date"], m["date_unit"])
		}
	}
	kinds := map[string]bool{}
	for _, n := range []string{"60", "62", "65", "66"} {
		raw := rawOf(rows["messages_v2/"+n])
		for _, k := range []string{"report_delivery_until_date_unit", "schedule_repeat_period_unit"} {
			if v, ok := raw[k]; ok && v != "unestablished" {
				t.Errorf("R%s: %s %v, want unestablished", n, k, v)
			}
		}
		for _, e := range []any{raw["reactions_results"]} {
			l, _ := e.([]any)
			for _, x := range l {
				m, _ := x.(map[string]any)
				s, _ := m["reaction"].(string)
				kind, _, _ := strings.Cut(s, ":")
				kinds[kind] = true
			}
		}
		if sp, ok := raw["suggested_post"].(map[string]any); ok {
			kinds["price:"+jsonText(sp["price"].(map[string]any)["kind"])] = true
		}
	}
	for _, k := range []string{"emoji", "custom", "paid", "empty", `price:"stars"`, `price:"ton"`} {
		if !kinds[k] {
			t.Errorf("the reaction and price kinds read are %v: %s is missing", slices.Sorted(mapsKeysBool(kinds)), k)
		}
	}
	// the price kinds are exactly stars, ton and unknown (a price class the table does not name; none is in a fixture)
	for k := range kinds {
		if strings.HasPrefix(k, "price:") && !slices.Contains([]string{`price:"stars"`, `price:"ton"`, `price:"unknown"`}, k) {
			t.Errorf("price kind %s is outside the vocabulary stars, ton, unknown", k)
		}
	}
	for _, k := range []string{"2806", "2807"} {
		if rawOf(hostile["messages_v2/"+k])["replies_data_state"] != nil {
			t.Errorf("H%s: replies_data_state %v on a row without a cell", k, rawOf(hostile["messages_v2/"+k])["replies_data_state"])
		}
	}
	lang := strings.Repeat("l", 65)
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: idAnn, data: tlMessage{
		flags2: 1 << 11, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: tlStr(lang),
	}.bytes(), repliesData: []byte{1, 2, 3}}})))
	raw := rawOf(byRow(res.Records)["messages_v2/1"])
	if raw["summary_from_language_state"] != "too-long" || raw["replies_data_state"] != "truncated" {
		t.Errorf("summary_from_language_state %v, replies_data_state %v; want too-long and truncated", raw["summary_from_language_state"], raw["replies_data_state"])
	}
}

func mapsKeysBool(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// noFieldAfterStopTail4I4b is plan 4I4b Task 6's part of TestTelegramNoFieldAfterStop (spec 8.4): the mapped list is
// the plan's, and a stop at reply_markup lists reactions and restriction_reason (their bits set) in data_undecided.
func noFieldAfterStopTail4I4b(t *testing.T) {
	t.Helper()
	got := androidtelegram.MappedFields()
	if !slices.Equal(got[len(got)-14:], mappedTail4I4b()) || slices.Index(got, "ttl_period") != len(got)-15 {
		t.Errorf("MappedFields %v, want the fourteen tail names after ttl_period in the plan's order", got)
	}
	m := tlMessage{flags: 1<<20 | 1<<22, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", markup: le32(idUnknownMarkup)}
	raw, _ := androidtelegram.DecodeBlob(m.bytes(), androidtelegram.TypeMessage, parse.NewBudget(1<<20).View())
	// an unknown ID is stated as raw.reply_markup {constructor}, so it is not undecided; the fields after it are
	if raw["data_stop_path"] != "reply_markup" || raw["data_undecided"] != "reactions,restriction_reason" {
		t.Errorf("a stop at reply_markup: data_stop_path %v, data_undecided %v; want reactions,restriction_reason", raw["data_stop_path"], raw["data_undecided"])
	}
}
