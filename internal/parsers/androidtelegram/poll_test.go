package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Wire builders for Go-built polls and mentions, written from the field order of the table's facts, independently of
// the parser.
const (
	idTextWithEntities      = 0x751f3146 // TL_textWithEntities: text, entities
	idMediaPoll             = 0x773f4e66 // TL_messageMediaPoll: flags, poll, results, [F.0 attached_media]
	idPoll                  = 0x966e2dbf // TL_poll: id, flags, question, answers, [F.4], [F.5], [F.12 countries_iso2], hash
	idPollAnswer            = 0x4b7d786a // TL_pollAnswer: flags, text, option
	idPollResults           = 0xba7bb15e // TL_pollResults: flags, ...
	idInputUser             = 0xf21158c6 // TL_inputUser: user_id, access_hash
	idInputUserFromMessage  = 0x1da448e2 // TL_inputUserFromMessage: peer, msg_id, user_id
	idInputPeerSelf         = 0x7da07ec9 // TL_inputPeerSelf
	idInputPeerUserFromMsg  = 0xa87b0a1c // TL_inputPeerUserFromMessage: peer, msg_id, user_id
	idMentionName           = 0x208e68c9 // TL_inputMessageEntityMentionName: offset, length, user_id
	idUnknownPollAnswer     = 0x0badfeed // an ID of no PollAnswer row
	bitPollCountries        = 1 << 12
	bitMessageEntities      = 1 << 7
	sha256OfSixtyFiveBytes7 = "2fa7eeba09baebc8ecef6fcfd44a61d64619aefae0b51f724658aa6dc03e8424"
)

func tlText(s string) []byte { return slices.Concat(le32(idTextWithEntities), tlStr(s), tlVec()) }

func tlPollAnswer(text, option string) []byte {
	return slices.Concat(le32(idPollAnswer), le32(0), tlText(text), tlStr(option))
}

// tlPoll is a TL_poll of id 77 and hash 5 with the answers (already encoded) and, when countries is not nil, its F.12
// country list (a vector of bare strings).
func tlPoll(question string, countries []string, answers ...[]byte) []byte {
	flags := uint32(0)
	var list []byte
	if countries != nil {
		flags |= bitPollCountries
		list = slices.Concat(le32(idTLVector), le32(uint32(len(countries))))
		for _, c := range countries {
			list = append(list, tlStr(c)...)
		}
	}
	return slices.Concat(le32(idPoll), le64(77), le32(flags), tlText(question), tlVec(answers...), list, le64(5))
}

// tlPollResults is a TL_pollResults of flags 0 (nothing read after the flags), or with F.4 solution when one is given.
func tlPollResults(solution string) []byte {
	if solution == "" {
		return slices.Concat(le32(idPollResults), le32(0))
	}
	return slices.Concat(le32(idPollResults), le32(1<<4), tlStr(solution), tlVec())
}

func tlPollMedia(poll, results []byte) []byte {
	return slices.Concat(le32(idMediaPoll), le32(0), poll, results)
}

// builtMessageRaw parses one Go-built message and returns its record and raw.
func builtMessageRaw(t *testing.T, m tlMessage) (map[string]any, string) {
	t.Helper()
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: idAnn, data: m.bytes()}}))).Records)
	r, ok := got["messages_v2/1"]
	if !ok {
		t.Fatal("no record for the Go-built message")
	}
	return rawOf(r), jsonText(r.Payload)
}

func pollMessage(media []byte) tlMessage {
	return tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", media: media}
}

// Design 9.3, rulings D9-D11, Q9, Q12-Q14, Review Focus 3 and 5: polls are raw only. R88-R94 state the poll, its
// answers, its results and their voters as addresses, never a Body, a participant or a time; a poll cut inside its
// answers or results states <key>_state partial and none of that object's lists (H3301..); an option is hex up to 64
// bytes (an empty one is "", an exact value), else too-long with its length and SHA-256 (H2909); the dates of a poll
// keep the unit unestablished; the nested media of a poll is stated by label and constructor only.
func TestTelegramPollsAreRawOnly(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r88 := rows["messages_v2/88"]
	raw := rawOf(r88)
	wantJSON(t, "R88 media", raw["media"], `{"constructor":"0x773f4e66","type":"poll"}`)
	wantJSON(t, "R88 poll", raw["poll"],
		`{"answers_total":2,"attached_media_constructor":"0x56e0d474","attached_media_type":"geo","close_date":1700000600,"close_date_unit":"unestablished","close_period":600,"close_period_unit":"unestablished","constructor":"0x966e2dbf","countries_iso2":["FR","IT"],"countries_iso2_total":2,"creator":true,"hash":77,"id":8000001,"public_voters":true,"question":"Capital?","question_entities_total":1,"quiz":true}`)
	wantJSON(t, "R88 poll_answers", raw["poll_answers"],
		`[{"constructor":"0x4b7d786a","entities_total":0,"index":0,"option":"00","text":"Paris"},{"added_by":"user:1000009","constructor":"0x4b7d786a","date":1700000005,"date_unit":"unestablished","entities_total":0,"index":1,"media_constructor":"0x56e0d474","media_type":"geo","option":"01","text":"Rome"}]`)
	wantJSON(t, "R88 poll_results", raw["poll_results"],
		`{"constructor":"0xba7bb15e","has_unread_votes":true,"option_voters_total":3,"recent_voters":["user:1000002"],"recent_voters_total":1,"results_total":2,"solution":"Because","solution_entities_total":0,"total_voters":4}`)
	wantJSON(t, "R88 poll_results_options", raw["poll_results_options"],
		`[{"chosen":true,"correct":true,"option":"00","recent_voters_total":2,"voters":3},{"option":"01","recent_voters_total":1,"voters":1}]`)
	wantJSON(t, "R88 poll_voters", raw["poll_voters"],
		`[{"option":"00","peer":"user:1000002"},{"option":"00","peer":"user:1000009"},{"option":"01","peer":"channel:3000001"}]`)

	// R89-R94: the older classes read whole; the voters of a vec<id32@user> and a vec<id64@user> as stored
	for n, want := range map[string]string{
		"89": "3", "90": "1", "91": "1", "92": "1", "93": "1", "94": "1",
	} {
		r := rawOf(rows["messages_v2/"+n])
		if m, _ := r["poll"].(map[string]any); m == nil || jsonText(m["answers_total"]) != want || r["poll_results"] == nil {
			t.Errorf("R%s: poll %s, poll_results %v; want %s answers and results read whole", n, jsonText(r["poll"]), r["poll_results"], want)
		}
	}
	wantJSON(t, "R90 recent_voters", rawOf(rows["messages_v2/90"])["poll_results"].(map[string]any)["recent_voters"], `["user:1000002","user:1000009"]`)
	wantJSON(t, "R91 recent_voters", rawOf(rows["messages_v2/91"])["poll_results"].(map[string]any)["recent_voters"], `["user:1000009"]`)
	wantJSON(t, "R92 recent_voters", rawOf(rows["messages_v2/92"])["poll_results"].(map[string]any)["recent_voters"], `["user:1000002"]`)
	wantJSON(t, "R94 media", rawOf(rows["messages_v2/94"])["media"], `{"constructor":"0x4bd6e798","type":"poll"}`)
	wantJSON(t, "R94 poll_answers", rawOf(rows["messages_v2/94"])["poll_answers"], `[{"constructor":"0x4b7d786a","entities_total":0,"index":0,"option":"","text":"Ok"}]`)

	// raw only: no Body, Summary, time or participant from a poll; the voters, the answer author and the channel are
	// addresses in raw alone; participants stay the 4I4 rule's
	for _, n := range []string{"88", "89", "90", "91", "92", "93", "94"} {
		r := rows["messages_v2/"+n]
		if r.Body != "" || len(r.Times) != 0 {
			t.Errorf("R%s: Body %q, times %v; a poll states neither", n, r.Body, r.Times)
		}
		if ps := participantsOf(r.Payload); !slices.Equal(ps, []string{"user:1000002"}) {
			t.Errorf("R%s: participants %v, want the sender user:1000002 only", n, ps)
		}
		for _, text := range []string{"Capital?", "Because", "Paris", "Rome", "Pick", "Old?", "Short?"} {
			if strings.Contains(r.Body+r.Summary+jsonText(r.Payload["participants"]), text) {
				t.Errorf("R%s: %q reached the Body, the Summary or the participants", n, text)
			}
		}
		for k := range r.Payload {
			if k != "raw" && strings.Contains(jsonText(r.Payload[k]), "Capital?") {
				t.Errorf("R%s: the question reached payload.%s", n, k)
			}
		}
	}
	if strings.Contains(r88.Summary+jsonText(r88.Payload["participants"]), "3000001") {
		t.Error("R88: a voter reached the Summary or the participants")
	}

	pollsHostile(t)
	pollsGoBuilt(t)
}

// pollsHostile is the fixture part of TestTelegramPollsAreRawOnly: H2909's 65-byte option and every cut of R88 (H3301..).
func pollsHostile(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	l := listOf(t, rawOf(hostile["messages_v2/2909"]), "poll_answers")
	if len(l) != 1 || l[0]["option_state"] != "too-long" || jsonText(l[0]["option_len"]) != "65" || l[0]["option_sha256"] != sha256OfSixtyFiveBytes7 || l[0]["option"] != nil {
		t.Errorf("H2909: answers %s; want option_state too-long, option_len 65 and the digest of 65 bytes of 0x07, no option", jsonText(l))
	}
	if sum := sha256.Sum256([]byte(strings.Repeat("\x07", 65))); hex.EncodeToString(sum[:]) != sha256OfSixtyFiveBytes7 {
		t.Fatal("the pinned digest of 65 bytes of 0x07 is wrong")
	}
	cuts, polls, results := 0, 0, 0
	for id := int64(3301); id < 3601; id++ {
		r, ok := hostile["messages_v2/"+itoa(id)]
		if !ok {
			continue
		}
		raw := rawOf(r)
		cuts++
		stop, _ := raw["data_stop_path"].(string)
		under := func(p string) bool {
			return stop == p || strings.HasPrefix(stop, p+".") || strings.HasPrefix(stop, p+"[")
		}
		switch {
		case under("media.poll"):
			polls++
			if raw["poll_state"] != "partial" || raw["poll"] != nil || raw["poll_answers"] != nil || raw["poll_results"] != nil || raw["poll_voters"] != nil {
				t.Errorf("H%d (stop %q): want poll_state partial and no poll list: %s", id, stop, jsonText(raw))
			}
		case under("media.results"):
			results++
			if raw["poll"] == nil || raw["poll_results_state"] != "partial" || raw["poll_results"] != nil || raw["poll_results_options"] != nil || raw["poll_voters"] != nil {
				t.Errorf("H%d (stop %q): want the poll read whole, poll_results_state partial and none of its lists", id, stop)
			}
		}
	}
	if cuts != 80 || polls == 0 || results == 0 {
		t.Errorf("%d cuts of R88 (%d in the poll, %d in the results); want 80 and both kinds", cuts, polls, results)
	}
}

// pollsGoBuilt is the Go-built part of TestTelegramPollsAreRawOnly: an unknown PollAnswer ID, an empty option and the
// nested media of an answer by label only.
func pollsGoBuilt(t *testing.T) {
	t.Helper()
	// an unknown answer ID cuts the poll inside its answers: partial, none of its lists, the stop kind and path stated
	// (the reactions bit is set with no reactions after the cut: the tail the decode never reached is data_undecided)
	m := pollMessage(tlPollMedia(tlPoll("Q", nil, tlPollAnswer("A", "x"), le32(idUnknownPollAnswer)), tlPollResults("")))
	m.flags = bitReactions
	raw, _ := builtMessageRaw(t, m)
	wantJSON(t, "unknown answer: media", raw["media"], `{"constructor":"0x773f4e66","type":"poll"}`)
	if raw["poll_state"] != "partial" || raw["poll"] != nil || raw["poll_answers"] != nil || raw["poll_results"] != nil {
		t.Errorf("an unknown PollAnswer ID: %s; want poll_state partial and no list", jsonText(raw))
	}
	if raw["data_stop_kind"] != "unknown-constructor" || raw["data_stop_path"] != "media.poll.answers[1]" || raw["data_undecided"] != "reactions" {
		t.Errorf("an unknown PollAnswer ID: stop %v at %v, undecided %v; want unknown-constructor at media.poll.answers[1] and the reactions undecided",
			raw["data_stop_kind"], raw["data_stop_path"], raw["data_undecided"])
	}

	// an empty option is "" (the hex of zero bytes): present, never absent
	raw, _ = builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll("Q", nil, tlPollAnswer("A", "")), tlPollResults(""))))
	l, _ := raw["poll_answers"].([]any)
	if len(l) != 1 {
		t.Fatalf("an empty option: %d answers, want 1", len(l))
	}
	if opt, present := l[0].(map[string]any)["option"]; !present || opt != "" {
		t.Errorf("an empty option: option %v (present %v), want \"\"", opt, present)
	}
}

// Rulings Q6, C4, F2, R7, Review Focus 4: a mention states user:<id> from a full InputUser's own user_id (the classes
// user and from_message), keeps user_class, and self and empty give no user; a from-message InputUser also states its
// message id and its peer's address by the peer's own id field, or the peer's label when it has none, never following
// an inner peer; access_hash is never emitted, only user_access_hash_present; a chain of nested peers past the depth
// bound is a cap stop and entities_state partial.
func TestTelegramMentionUserFromInputUser(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r95, r96 := rawOf(rows["messages_v2/95"]), rawOf(rows["messages_v2/96"])
	wantJSON(t, "R95 entities", r95["entities"],
		`[{"length":1,"offset":0,"type":"input_mention_name","user":"user:1000009","user_access_hash_present":true,"user_class":"user"},`+
			`{"length":1,"offset":1,"type":"input_mention_name","user":"user:1000010","user_access_hash_present":true,"user_class":"user"},`+
			`{"length":1,"offset":2,"type":"input_mention_name","user":"user:1000011","user_access_hash_present":true,"user_class":"from_message","user_from_msg_id":55,"user_from_peer":"user:1000002"},`+
			`{"length":1,"offset":3,"type":"input_mention_name","user":"user:1000012","user_access_hash_present":true,"user_class":"from_message","user_from_msg_id":56,"user_from_peer":"channel:3000001"},`+
			`{"length":1,"offset":4,"type":"input_mention_name","user_class":"self"},`+
			`{"length":1,"offset":5,"type":"input_mention_name","user_class":"empty"}]`)
	ents := listOf(t, r96, "entities")
	if len(ents) != 12 {
		t.Fatalf("R96: %d entities, want 12", len(ents))
	}
	want96 := map[int]string{
		0:  `{"length":1,"offset":0,"type":"input_mention_name","user":"user:1000020","user_class":"from_message","user_from_msg_id":60,"user_from_peer":"chat:2000001"}`,
		1:  `{"length":1,"offset":1,"type":"input_mention_name","user":"user:1000021","user_class":"from_message","user_from_msg_id":61,"user_from_peer_class":"user_from_message"}`,
		6:  `{"length":1,"offset":6,"type":"input_mention_name","user":"user:1000026","user_class":"from_message","user_from_msg_id":66,"user_from_peer_class":"self"}`,
		7:  `{"length":1,"offset":7,"type":"input_mention_name","user":"user:1000027","user_class":"from_message","user_from_msg_id":67,"user_from_peer_class":"empty"}`,
		8:  `{"length":1,"offset":8,"type":"input_mention_name","user":"user:1000028","user_class":"from_message","user_from_msg_id":68,"user_from_peer_class":"channel_from_message"}`,
		11: `{"length":1,"offset":11,"type":"input_mention_name","user":"user:1000031","user_access_hash_present":true,"user_class":"from_message","user_from_msg_id":71,"user_from_peer":"user:1000004"}`,
	}
	for i, w := range want96 {
		wantJSON(t, fmt.Sprintf("R96 entity %d", i), ents[i], w)
	}
	// no access_hash value anywhere: the only key is the presence flag
	for n, raw := range map[string]map[string]any{"R95": r95, "R96": r96} {
		s := jsonText(raw)
		if strings.Count(s, "access_hash") != strings.Count(s, "user_access_hash_present") {
			t.Errorf("%s: an access_hash key other than the presence flag: %s", n, s)
		}
	}
	for _, n := range []string{"95", "96"} {
		if ps := participantsOf(rows["messages_v2/"+n].Payload); !slices.Equal(ps, []string{"user:1000002"}) {
			t.Errorf("R%s: participants %v, want the sender only (a mentioned user is never a participant)", n, ps)
		}
	}

	mentionsGoBuilt(t)
}

// mention is a TL_inputMessageEntityMentionName at offset 0, length 1 for the encoded InputUser.
func mention(user []byte) []byte {
	return slices.Concat(le32(idMentionName), le32(0), le32(1), user)
}

// inputUserFromMessage is a TL_inputUserFromMessage of the encoded InputPeer, message 9 and user 1000040.
func inputUserFromMessage(peer []byte) []byte {
	return slices.Concat(le32(idInputUserFromMessage), peer, le32(9), le64(1000040))
}

func mentionMessage(user []byte) tlMessage {
	return tlMessage{flags: bitMessageEntities, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: tlVec(mention(user))}
}

// mentionsGoBuilt is the Go-built part of TestTelegramMentionUserFromInputUser.
func mentionsGoBuilt(t *testing.T) {
	t.Helper()
	// a from-message peer that is itself a from-message peer: its label, the inner peer never followed
	inner := slices.Concat(le32(idInputPeerUserFromMsg), le32(idInputPeerSelf), le32(3), le64(1000041))
	raw, _ := builtMessageRaw(t, mentionMessage(inputUserFromMessage(inner)))
	wantJSON(t, "from-message peer of a from-message peer", raw["entities"],
		`[{"length":1,"offset":0,"type":"input_mention_name","user":"user:1000040","user_class":"from_message","user_from_msg_id":9,"user_from_peer_class":"user_from_message"}]`)
	if strings.Contains(jsonText(raw), "1000041") {
		t.Error("the inner from-message peer was followed: its user id is stated")
	}

	// 40 nested from-message peers pass the depth bound: a cap stop and the entities list partial, nothing invented
	deep := le32(idInputPeerSelf)
	for range 40 {
		deep = slices.Concat(le32(idInputPeerUserFromMsg), deep, le32(3), le64(1000041))
	}
	raw, _ = builtMessageRaw(t, mentionMessage(inputUserFromMessage(deep)))
	if raw["data_stop_kind"] != "cap" || raw["entities_state"] != "partial" || raw["entities"] != nil {
		t.Errorf("40 nested peers: stop %v, entities_state %v, entities %v; want cap, partial and no entity", raw["data_stop_kind"], raw["entities_state"], raw["entities"])
	}

	// an access hash is never emitted: not as a decimal, a hex string or a key, only the presence flag
	const hash = int64(0x1122334455667788)
	user := slices.Concat(le32(idInputUser), le64(1000009), le64(hash))
	raw, text := builtMessageRaw(t, mentionMessage(user))
	wantJSON(t, "full InputUser", raw["entities"],
		`[{"length":1,"offset":0,"type":"input_mention_name","user":"user:1000009","user_access_hash_present":true,"user_class":"user"}]`)
	for _, form := range []string{"1234605616436508552", "1122334455667788", "8877665544332211"} {
		if strings.Contains(strings.ToLower(text), form) {
			t.Errorf("the access hash reached the record as %s", form)
		}
	}
}

// pollCaps4I4c is plan 4I4c Task 7's part of TestTelegramCapsAreCountedNeverSilent (ruling T5r1 and the plan's Raw
// shapes caps): H2907 keeps 32 answers of 33, H2911 16 recent voters of 17, H2912 64 voters of 65 options' voters, a
// country list keeps 64 and states its cut by countries_iso2_truncated only when cut, a country over 64 bytes is never
// cut (state too-long, no list), and a poll text over its cap is cut and named in raw.text_flags.
func pollCaps4I4c(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	raw := rawOf(hostile["messages_v2/2907"])
	p, _ := raw["poll"].(map[string]any)
	if l, _ := raw["poll_answers"].([]any); len(l) != 32 || jsonText(p["answers_total"]) != "33" || p["answers_truncated"] != true {
		t.Errorf("H2907: %d answers kept, total %v, truncated %v; want 32 of 33 stated", len(l), p["answers_total"], p["answers_truncated"])
	}
	r, _ := rawOf(hostile["messages_v2/2911"])["poll_results"].(map[string]any)
	if l, _ := r["recent_voters"].([]any); len(l) != 16 || jsonText(r["recent_voters_total"]) != "17" || r["recent_voters_truncated"] != true {
		t.Errorf("H2911: %d recent voters kept, total %v, truncated %v; want 16 of 17 stated", len(l), r["recent_voters_total"], r["recent_voters_truncated"])
	}
	raw = rawOf(hostile["messages_v2/2912"])
	r, _ = raw["poll_results"].(map[string]any)
	if l, _ := raw["poll_voters"].([]any); len(l) != 64 || jsonText(r["option_voters_total"]) != "65" || r["option_voters_truncated"] != true {
		t.Errorf("H2912: %d voters kept, total %v, truncated %v; want 64 of 65 stated", len(l), r["option_voters_total"], r["option_voters_truncated"])
	}

	countries := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("C%02d", i%100)
		}
		return out
	}
	poll := func(raw map[string]any) map[string]any {
		m, _ := raw["poll"].(map[string]any)
		return m
	}
	raw, _ = builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll("Q", countries(65)), tlPollResults(""))))
	m := poll(raw)
	if l, _ := m["countries_iso2"].([]any); len(l) != 64 || jsonText(m["countries_iso2_total"]) != "65" || m["countries_iso2_truncated"] != true {
		t.Errorf("65 countries: %d kept, total %v, truncated %v; want 64 of 65 stated", len(l), m["countries_iso2_total"], m["countries_iso2_truncated"])
	}
	raw, _ = builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll("Q", countries(64)), tlPollResults(""))))
	m = poll(raw)
	if l, _ := m["countries_iso2"].([]any); len(l) != 64 || jsonText(m["countries_iso2_total"]) != "64" || m["countries_iso2_truncated"] != nil {
		t.Errorf("64 countries: %d kept, total %v, truncated %v; want all 64 and no truncated key", len(l), m["countries_iso2_total"], m["countries_iso2_truncated"])
	}
	raw, _ = builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll("Q", []string{"FR", strings.Repeat("Z", 65)}), tlPollResults(""))))
	m = poll(raw)
	if m["countries_iso2_state"] != "too-long" || m["countries_iso2"] != nil || jsonText(m["countries_iso2_total"]) != "2" || m["countries_iso2_truncated"] != nil {
		t.Errorf("a 65-byte country: %s; want countries_iso2_state too-long, no list and the total 2", jsonText(m))
	}

	// the text caps: a question 4 KiB, an answer 1 KiB, a solution 4 KiB; a cut is named in raw.text_flags, never silent
	long := strings.Repeat("q", 5000)
	raw, _ = builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll(long, nil, tlPollAnswer(long, "x")), tlPollResults(long))))
	q, _ := poll(raw)["question"].(string)
	res, _ := raw["poll_results"].(map[string]any)
	sol, _ := res["solution"].(string)
	ans := listOf(t, raw, "poll_answers")
	if len(ans) != 1 || len(q) != 4<<10 || len(sol) != 4<<10 {
		t.Fatalf("question of %d bytes, solution of %d bytes, %d answers; want 4 KiB, 4 KiB and 1", len(q), len(sol), len(ans))
	}
	if text, _ := ans[0]["text"].(string); len(text) != 1<<10 {
		t.Errorf("answer text of %d bytes, want 1 KiB", len(text))
	}
	if flags, _ := raw["text_flags"].(string); flags != "poll.question,poll_answers.text,poll_results.solution" {
		t.Errorf("text_flags %q, want poll.question,poll_answers.text,poll_results.solution", flags)
	}
}

// pollVocabulary is plan 4I4c Task 7's part of TestTelegramStateVocabularyIsPinned: poll_state and poll_results_state
// partial, option_state and countries_iso2_state too-long, every _unit unestablished, and the InputPeer labels of R96,
// each by its hard-coded value.
func pollVocabulary(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	states := map[string]bool{}
	for id := int64(3301); id < 3601; id++ {
		if r, ok := hostile["messages_v2/"+itoa(id)]; ok {
			raw := rawOf(r)
			for _, k := range []string{"poll_state", "poll_results_state"} {
				if s, _ := raw[k].(string); s != "" {
					states[k+"="+s] = true
				}
			}
		}
	}
	for _, k := range []string{"poll_state=partial", "poll_results_state=partial"} {
		if !states[k] {
			t.Errorf("no cut of R88 states %s", k)
		}
	}
	if len(states) != 2 {
		t.Errorf("poll states %v, want exactly poll_state and poll_results_state partial", states)
	}
	if l := listOf(t, rawOf(hostile["messages_v2/2909"]), "poll_answers"); len(l) != 1 || l[0]["option_state"] != "too-long" {
		t.Errorf("H2909: option_state %v, want too-long", l)
	}
	raw, _ := builtMessageRaw(t, pollMessage(tlPollMedia(tlPoll("Q", []string{strings.Repeat("Z", 65)}), tlPollResults(""))))
	if m, _ := raw["poll"].(map[string]any); m["countries_iso2_state"] != "too-long" {
		t.Errorf("countries_iso2_state %v, want too-long", m["countries_iso2_state"])
	}

	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r88 := rawOf(rows["messages_v2/88"])
	p, _ := r88["poll"].(map[string]any)
	for _, k := range []string{"close_period_unit", "close_date_unit"} {
		if p[k] != "unestablished" {
			t.Errorf("R88 poll.%s = %v, want unestablished", k, p[k])
		}
	}
	if a := listOf(t, r88, "poll_answers"); a[1]["date_unit"] != "unestablished" {
		t.Errorf("R88 answer date_unit = %v, want unestablished", a[1]["date_unit"])
	}
	labels := map[string]bool{}
	for _, e := range listOf(t, rawOf(rows["messages_v2/96"]), "entities") {
		if l, ok := e["user_from_peer_class"].(string); ok {
			labels[l] = true
		}
	}
	for _, l := range []string{"self", "empty", "user_from_message", "channel_from_message"} {
		if !labels[l] {
			t.Errorf("R96 has no InputPeer label %s", l)
		}
	}
	if len(labels) != 4 {
		t.Errorf("R96 InputPeer labels %v, want exactly self, empty, user_from_message and channel_from_message", labels)
	}
}

// pollParticipants is plan 4I4c Task 7's part of TestTelegramReactorsAreNeverParticipants: the voters of R88 and the
// profile-button user of R83 are in raw only.
func pollParticipants(t *testing.T) {
	t.Helper()
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	for _, c := range []struct{ row, key, who string }{
		{"88", "poll_voters", "user:1000009"}, {"88", "poll_voters", "channel:3000001"}, {"83", "reply_markup_buttons", "user:1000009"},
	} {
		r := rows["messages_v2/"+c.row]
		if !strings.Contains(jsonText(rawOf(r)[c.key]), c.who) {
			t.Errorf("R%s: %s is missing from raw.%s", c.row, c.who, c.key)
		}
		if strings.Contains(jsonText(r.Payload["participants"])+r.Summary+r.Body, strings.SplitN(c.who, ":", 2)[1]) {
			t.Errorf("R%s: %s reached the participants, the Summary or the Body", c.row, c.who)
		}
		if ps := participantsOf(r.Payload); len(ps) != 1 || ps[0] != "user:1000002" {
			t.Errorf("R%s: participants %v, want the sender user:1000002 only", c.row, ps)
		}
	}
}
