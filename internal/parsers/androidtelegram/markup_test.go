package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Wire builders for Go-built reply markup, written from the field order of the table's facts, independently of the
// parser.
const (
	idKeyboardMarkup   = 0x85dd99d1 // TL_replyKeyboardMarkup: flags, rows, [placeholder]
	idInlineMarkup     = 0xb2b15770 // TL_replyInlineMarkup: flags, rows
	idKeyboardRow      = 0x77608b83 // KeyboardButtonRow: buttons
	idInlineRow        = 0x19420af6 // TL_keyboardInlineButtonRow: buttons
	idButtonPlain223   = 0xa2fa4880 // TL_keyboardButton_layer223: text
	idButtonCallback   = 0x683a5e46 // TL_keyboardButtonCallback_layer117: text, data
	emptyDataSHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func tlVec(elems ...[]byte) []byte {
	b := append(le32(idTLVector), le32(uint32(len(elems)))...)
	for _, e := range elems {
		b = append(b, e...)
	}
	return b
}

func plainButton(text string) []byte { return append(le32(idButtonPlain223), tlStr(text)...) }

func callbackButton(text, data string) []byte {
	return append(append(le32(idButtonCallback), tlStr(text)...), tlStr(data)...)
}

func keyboardMarkup(rows ...[]byte) []byte {
	return append(append(le32(idKeyboardMarkup), le32(0)...), tlVec(rows...)...)
}

func inlineMarkup(rows ...[]byte) []byte {
	return append(append(le32(idInlineMarkup), le32(0)...), tlVec(rows...)...)
}

func buttonRow(id uint32, buttons ...[]byte) []byte { return append(le32(id), tlVec(buttons...)...) }

// Design 9.1, 9.2, rulings C3, PL5, T2a, Review Focus 1 and 2: reply markup is raw only. R81-R87 state the four kinds
// and their buttons (R85 by the Q5 branch: decoded); a stop inside the markup is reply_markup_state partial with no
// button (H2901, H2908), an inline class in a reply keyboard is read through the union (H2905, T2a), 65 buttons keep 64
// (H2906); the cuts of R83 state nothing at or after the cut; a markup read whole survives a stop in the reactions after
// it; a reply copy holding markup states none; a profile button's user is never a participant.
func TestTelegramReplyMarkupButtons(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	raw := func(n string) map[string]any { return rawOf(rows["messages_v2/"+n]) }

	wantJSON(t, "R81 reply_markup", raw("81")["reply_markup"],
		`{"buttons_total":6,"constructor":"0x85dd99d1","is_persistent":true,"kind":"keyboard","placeholder":"Pick one","resize":true,"rows_total":3,"single_use":true}`)
	b81, _ := raw("81")["reply_markup_buttons"].([]any)
	if len(b81) != 6 {
		t.Fatalf("R81: %d buttons, want 6", len(b81))
	}
	if k := kindsOf(b81); !slices.Equal(k, []string{"default", "request_phone", "request_geo_location", "request_poll", "simple_web_view", "request_peer"}) {
		t.Errorf("R81: kinds %v", k)
	}
	wantJSON(t, "R81 button 3", b81[3], `{"column":0,"constructor":"0x2f67a72f","kind":"request_poll","quiz":true,"row":1,"text":"Quiz"}`)
	wantJSON(t, "R81 button 4", b81[4], `{"column":1,"constructor":"0x2f67a72f","kind":"simple_web_view","row":1,"style_icon":5000000005,"style_primary":true,"text":"App","url":"fixture.invalid/app"}`)
	wantJSON(t, "R81 button 5", b81[5], `{"bot_admin_rights":"post_messages","button_id":7,"column":0,"constructor":"0x2f67a72f","kind":"request_peer","max_quantity":1,"peer_type":"chat","peer_type_creator":true,"row":2,"text":"Pick chat","user_admin_rights":"change_info,ban_users"}`)
	if r := rows["messages_v2/81"]; len(r.Times) != 1 || r.Times[0].Kind != "edited" {
		t.Errorf("R81: times %v, want edited (D+60)", r.Times)
	}

	if k := kindsOf(raw("82")["reply_markup_buttons"]); !slices.Equal(k, []string{"default", "request_phone"}) {
		t.Errorf("R82: kinds %v, want default and request_phone", k)
	}

	// R83: kinds, the callback as length and digest, the profile user, and no participant from it
	b83, _ := raw("83")["reply_markup_buttons"].([]any)
	if k := kindsOf(b83); !slices.Equal(k, []string{"url", "callback", "switch_inline", "copy", "user_profile"}) {
		t.Fatalf("R83: kinds %v", k)
	}
	cbSum := sha256.Sum256([]byte("SYNTHETIC-CALLBA")) // cb(1, 16): SYNTHETIC-CALLBACK-01- cut to 16 bytes
	wantJSON(t, "R83 callback", b83[1], `{"column":1,"constructor":"0x9c1c0c55","data_len":16,"data_sha256":"`+hex.EncodeToString(cbSum[:])+`","kind":"callback","requires_password":true,"row":0,"text":"Press"}`)
	wantJSON(t, "R83 switch_inline", b83[2], `{"column":0,"constructor":"0x991399fc","kind":"switch_inline","peer_types":"pm,chat,broadcast","peer_types_total":3,"query":"q","row":1,"same_peer":true,"text":"Share"}`)
	if b83[4].(map[string]any)["user"] != "user:1000009" {
		t.Errorf("R83 profile button: %s, want user user:1000009", jsonText(b83[4]))
	}
	if slices.Contains(participantsOf(rows["messages_v2/83"].Payload), "user:1000009") {
		t.Errorf("R83: the profile-button user is a participant")
	}

	b84, _ := raw("84")["reply_markup_buttons"].([]any)
	wantJSON(t, "R84 button 2", b84[2], `{"ahead_app_drops":true,"column":2,"constructor":"0x7d170cff","kind":"default","row":0,"text":"Plain"}`)
	for _, i := range []int{0, 1} {
		if b84[i].(map[string]any)["ahead_app_drops"] != nil {
			t.Errorf("R84 button %d: ahead_app_drops stated for an inline class", i)
		}
	}
	wantJSON(t, "R85 reply_markup", raw("85")["reply_markup"], `{"buttons_total":3,"constructor":"0x48a30254","kind":"inline","rows_total":1}`)
	if k := kindsOf(raw("85")["reply_markup_buttons"]); !slices.Equal(k, []string{"url", "callback", "game"}) {
		t.Errorf("R85: kinds %v, want url, callback, game", k)
	}
	wantJSON(t, "R86 reply_markup", raw("86")["reply_markup"], `{"constructor":"0x86b40b08","kind":"force_reply","placeholder":"Reply here","single_use":true}`)
	wantJSON(t, "R87 reply_markup", raw("87")["reply_markup"], `{"constructor":"0xa03e5b85","kind":"hide","selective":true}`)
	if r := rows["messages_v2/87"]; len(r.Times) != 1 || r.Times[0].Kind != "edited" {
		t.Errorf("R87: times %v, want edited (D+70)", r.Times)
	}

	// the hostile rows
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	hraw := func(n string) map[string]any { return rawOf(hostile["messages_v2/"+n]) }
	for _, n := range []string{"2901", "2908"} {
		if hraw(n)["reply_markup_state"] != "partial" || hraw(n)["reply_markup"] != nil || hraw(n)["reply_markup_buttons"] != nil {
			t.Errorf("H%s: reply_markup_state %v, reply_markup %v; want partial and no button", n, hraw(n)["reply_markup_state"], hraw(n)["reply_markup"])
		}
	}
	if hraw("2901")["data_stop_kind"] != "unknown-constructor" || hraw("2908")["data_stop_kind"] != "opaque" || hraw("2908")["data_stop_id"] != "0xd02e7fd4" {
		t.Errorf("H2901/H2908 stops: %v / %v", hraw("2901")["data_stop_kind"], hraw("2908")["data_stop_kind"])
	}
	if k := kindsOf(hraw("2905")["reply_markup_buttons"]); !slices.Equal(k, []string{"url"}) || hraw("2905")["reply_markup_state"] != nil {
		t.Errorf("H2905: kinds %v, state %v; want the inline url read through the union", k, hraw("2905")["reply_markup_state"])
	}

	// every cut of R83 (H3001...) states no button and no markup value
	cuts := 0
	for id := int64(3001); id < 3100; id++ {
		r, ok := hostile["messages_v2/"+itoa(id)]
		if !ok {
			continue
		}
		raw := rawOf(r)
		if st := raw["data_state"]; st != "truncated" && st != "partial" {
			continue
		}
		cuts++
		if raw["reply_markup_buttons"] != nil {
			t.Errorf("H%d: buttons stated at a cut: %s", id, jsonText(raw["reply_markup_buttons"]))
		}
		if m, isMap := raw["reply_markup"].(map[string]any); isMap && (len(m) != 1 || m["state"] == nil) {
			t.Errorf("H%d: reply_markup %s at a cut, want only a state", id, jsonText(m))
		}
	}
	if cuts < 40 {
		t.Errorf("%d cuts of R83 inspected, want at least 40", cuts)
	}

	markupGoBuilt(t)
}

// kindsOf is the kinds of a reply_markup_buttons list.
func kindsOf(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		m, _ := e.(map[string]any)
		k, _ := m["kind"].(string)
		out = append(out, k)
	}
	return out
}

// participantsOf is the addresses of a message payload's participants.
func participantsOf(payload map[string]any) []string {
	var out []string
	l, _ := payload["participants"].([]any)
	for _, e := range l {
		m, _ := e.(map[string]any)
		a, _ := m["address"].(string)
		out = append(out, a)
	}
	return out
}

// markupGoBuilt is the Go-built part of TestTelegramReplyMarkupButtons.
func markupGoBuilt(t *testing.T) {
	t.Helper()
	const ann, self = 1000002, 1000001
	markup := keyboardMarkup(buttonRow(idKeyboardRow, plainButton("A")))
	old := append(append(append(le32(idReactionCountOld), le32(0)...), tlStr("x")...), le32(2)...)
	whole := tlMessage{
		flags: bitReactions, id: 1, from: ann, peer: self, date: baseDate, editDate: baseDate + 9, text: "x",
		markup: markup, tail: reactionsWith(old),
	}
	copyWith := tlMessage{id: 9, from: ann, peer: self, date: baseDate - 1, text: "q", markup: markup}
	copyWithout := tlMessage{id: 9, from: ann, peer: self, date: baseDate - 1, text: "q"}
	plain := tlMessage{id: 2, from: ann, peer: self, date: baseDate, text: "x"}.bytes()
	cut := tlMessage{id: 4, from: ann, peer: self, date: baseDate, text: "x", markup: []byte{1, 2}}
	cells := []msgCells{
		{mid: 1, uid: ann, data: whole.bytes()},
		{mid: 2, uid: ann, data: plain, replydata: copyWith.bytes()},
		{mid: 3, uid: ann, data: plain, replydata: copyWithout.bytes()},
		{mid: 4, uid: ann, data: cut.bytes()},
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)

	r1, raw := got["messages_v2/1"], rawOf(got["messages_v2/1"])
	wantJSON(t, "markup then a stop in reactions: reply_markup", raw["reply_markup"], `{"buttons_total":1,"constructor":"0x85dd99d1","kind":"keyboard","rows_total":1}`)
	if raw["reactions_state"] != "partial" || len(r1.Times) != 1 || r1.Times[0].Kind != "edited" {
		t.Errorf("markup then a stop in reactions: reactions_state %v, times %v; want partial and edited", raw["reactions_state"], r1.Times)
	}
	if und, _ := raw["data_undecided"].(string); strings.Contains(und, "reply_markup") {
		t.Errorf("data_undecided %q names a markup that was read whole", und)
	}

	with, without := rawOf(got["messages_v2/2"]), rawOf(got["messages_v2/3"])
	for k := range with {
		if strings.HasPrefix(k, "reply_markup") {
			t.Errorf("a reply copy holding markup states raw.%s", k)
		}
	}
	if jsonText(with["reply_copy"]) != jsonText(without["reply_copy"]) || with["replydata_state"] != "decoded" {
		t.Errorf("reply_copy %s with markup, %s without; want equal", jsonText(with["reply_copy"]), jsonText(without["reply_copy"]))
	}

	raw = rawOf(got["messages_v2/4"])
	wantJSON(t, "markup ID cut", raw["reply_markup"], `{"state":"truncated"}`)
	if und, _ := raw["data_undecided"].(string); !strings.HasPrefix(und, "reply_markup") {
		t.Errorf("a cut at reply_markup: data_undecided %q, want reply_markup first", und)
	}
}

// Design D8, ruling Q8, Review Focus 5: a bot callback's data is stated as length and SHA-256 only. Over every variant's
// records, warnings and notes no callback byte appears raw, in hex of either case, in base64 or in any 8-byte window
// (a variant's files must hold the pattern, so the search is not vacuous); an empty data is data_len 0 with the digest
// of zero bytes.
func TestTelegramCallbackDataIsDigestOnly(t *testing.T) {
	ns := map[string][]string{}
	for n := range 10 {
		unit := "SYNTHETIC-CALLBACK-0" + string(rune('0'+n)) + "-"
		p := []byte(strings.Repeat(unit, 4))
		for i := 0; i+8 <= len(p); i++ {
			ns[unit] = append(ns[unit], encodedForms(p[i:i+8])...)
		}
	}
	f := newFinder(ns)
	held := false
	for _, v := range variants {
		files := fixtureFiles(t, v)
		for _, role := range []string{"db", "wal", "journal"} {
			if len(f.labels(string(files[role]))) > 0 {
				held = true
			}
		}
		for _, s := range outputTexts(t, parseAt(t, remoteApp, files)) {
			if name, form, ok := f.find(s); ok {
				t.Errorf("%s: the output holds %s (form %q)", v, name, form)
			}
		}
	}
	if !held {
		t.Error("no variant holds a callback pattern: the search is vacuous")
	}

	m := tlMessage{
		id: 1, from: 1000002, peer: 1000001, date: baseDate, text: "x",
		markup: inlineMarkup(buttonRow(idInlineRow, callbackButton("Go", ""))),
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: 1000002, data: m.bytes()}}))).Records)
	b, _ := rawOf(got["messages_v2/1"])["reply_markup_buttons"].([]any)
	if len(b) != 1 {
		t.Fatalf("%d buttons, want 1", len(b))
	}
	btn := b[0].(map[string]any)
	if jsonText(btn["data_len"]) != "0" || btn["data_sha256"] != emptyDataSHA256Hex {
		t.Errorf("an empty callback: data_len %v, data_sha256 %v; want 0 and the digest of zero bytes", btn["data_len"], btn["data_sha256"])
	}

	// the typed pass (android.telegram.recover) states the same: a deleted message whose callback button lies only in
	// WAL frames is recovered with data_len and data_sha256 and no callback byte (Task 6 review, carried into Task 8)
	t.Run("typed-pass/go-built-wal", func(t *testing.T) {
		files := callbackRowInWAL(t)
		if len(f.labels(string(files["wal"]))) == 0 {
			t.Fatal("the WAL holds no callback pattern: the check proves nothing")
		}
		res := typedRun(t, files)
		stated := 0
		for _, r := range res.recs {
			bs, _ := rawOf(r)["reply_markup_buttons"].([]any)
			for _, b := range bs {
				btn, _ := b.(map[string]any)
				if jsonText(btn["data_len"]) == strconv.Itoa(len(callbackData)) && btn["data_sha256"] == sha256Bytes([]byte(callbackData)) {
					stated++
				}
			}
		}
		if stated != 1 {
			t.Fatalf("the typed pass states %d callback buttons with the digest (%d records): the check proves nothing", stated, len(res.recs))
		}
		for _, s := range res.texts(t) {
			if name, form, ok := f.find(s); ok {
				t.Errorf("the typed pass's output holds %s (form %q)", name, form)
			}
		}
	})
}

// callbackData is the bytes of the deleted message's callback button: the fixtures' callback pattern, 88 bytes.
var callbackData = strings.Repeat("SYNTHETIC-CALLBACK-07-", 4)

// callbackRowInWAL is a Go-built database whose main file holds one live message and whose WAL holds, in a first
// committed frame set, a second message with an inline callback button (data callbackData) and in a second the same
// pages after that row was deleted: the recovered row and its callback bytes lie only in WAL frames (as secretRowsInWAL).
func callbackRowInWAL(t *testing.T) map[string][]byte {
	t.Helper()
	return deletedMessageInWAL(t, tlMessage{
		id: 2, from: idAnn, peer: idSelf, date: baseDate, text: "deleted",
		markup: inlineMarkup(buttonRow(idInlineRow, callbackButton("Go", callbackData))),
	})
}

// deletedMessageInWAL is the Go-built database of callbackRowInWAL for any message (id 2): the row and its bytes lie only
// in WAL frames.
func deletedMessageInWAL(t *testing.T, gone tlMessage) map[string][]byte {
	t.Helper()
	live := tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "live"}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	b.CreateTable("users", ddlUsers)
	v2 := b.CreateTable("messages_v2", ddlV2Full)
	v2.Insert(1, msgCells{mid: 1, uid: idAnn, data: live.bytes()}.v2Values()...)
	main := b.Bytes()
	w := b.NewWAL(false, 1, 2, 0)
	frameAll := func() {
		n := b.Snapshot().Pages()
		for p := uint32(1); p <= n; p++ {
			commit := uint32(0)
			if p == n {
				commit = n
			}
			w.Frame(p, b.PageBytes(p), commit)
		}
	}
	v2.Insert(2, msgCells{mid: 2, uid: idAnn, data: gone.bytes()}.v2Values()...)
	frameAll()
	v2.Delete(2)
	frameAll()
	return map[string][]byte{"db": main, "wal": w.Bytes()}
}

// markupMappedAfterMedia is the part of TestTelegramBlobStatesArePinned for the mapped list and the stop at the markup
// (spec 8.4, design D12): reply_markup follows media, and a blob cut at the markup lists it first.
func markupMappedAfterMedia(t *testing.T) {
	t.Helper()
	got := androidtelegram.MappedFields()
	if i := slices.Index(got, "media"); i < 0 || slices.Index(got, "reply_markup") != i+1 {
		t.Errorf("MappedFields %v, want reply_markup right after media", got)
	}
	m := tlMessage{flags: 1<<20 | 1<<22, id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "x", markup: []byte{1, 2}}
	raw, _ := androidtelegram.DecodeBlob(m.bytes(), androidtelegram.TypeMessage, parse.NewBudget(1<<20).View())
	if raw["data_undecided"] != "reply_markup,reactions,restriction_reason" {
		t.Errorf("a stop at reply_markup: data_undecided %v, want reply_markup,reactions,restriction_reason", raw["data_undecided"])
	}
}

// markupCaps4I4c is plan 4I4c Task 6's part of TestTelegramCapsAreCountedNeverSilent (ruling T5r1): H2906 keeps 64
// buttons of 65 and H2910 keeps 8 peer types of 9, each cut stated by its _total and _truncated.
func markupCaps4I4c(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	raw := rawOf(hostile["messages_v2/2906"])
	m, _ := raw["reply_markup"].(map[string]any)
	if l, _ := raw["reply_markup_buttons"].([]any); len(l) != 64 || jsonText(m["buttons_total"]) != "65" || m["buttons_truncated"] != true {
		t.Errorf("H2906: %d buttons kept, total %v, truncated %v; want 64 of 65 stated", len(l), m["buttons_total"], m["buttons_truncated"])
	}
	l, _ := rawOf(hostile["messages_v2/2910"])["reply_markup_buttons"].([]any)
	if len(l) != 1 {
		t.Fatalf("H2910: %d buttons, want 1", len(l))
	}
	b := l[0].(map[string]any)
	if n := len(strings.Split(b["peer_types"].(string), ",")); n != 8 || jsonText(b["peer_types_total"]) != "9" || b["peer_types_truncated"] != true {
		t.Errorf("H2910: %d peer types kept, total %v, truncated %v; want 8 of 9 stated", n, b["peer_types_total"], b["peer_types_truncated"])
	}
	markupTextCaps(t)
}

// markupTextCaps is Go-built messages whose button texts are one byte over their caps and exactly at them (ruling T5r1,
// plan Raw shapes: text, fwd_text, query and the suggested names 1 KiB; url and copy_text 4 KiB; the keyboard
// placeholder 1 KiB): a text over a cap is kept at the cap and named in raw.text_flags, never cut silently; one exactly
// at a cap is kept whole and not flagged (an off-by-one would show here).
func markupTextCaps(t *testing.T) {
	t.Helper()
	const (
		idButtonURL     = 0x258aff05 // TL_keyboardButtonUrl_layer223: text, url
		idButtonURLAuth = 0x10b78d29 // TL_keyboardButtonUrlAuth_layer223: flags, text, fwd_text (F.0), url, button_id
		idButtonCopy    = 0xbcc4af10 // TL_keyboardButtonCopy_layer228: flags, text, copy_text
		idButtonSwitch  = 0x991399fc // TL_keyboardButtonSwitchInline_layer228: flags, text, query
		idButtonRequest = 0x5b0f15f5 // TL_keyboardButtonRequestPeer_layer228: flags, text, button_id, peer_type, max_quantity
		idCreateBot     = 0x3e81e078 // TL_requestPeerTypeCreateBot: flags, suggested_name (F.1), suggested_username (F.2)
	)
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }
	// the inline and keyboard markups whose every text is its cap plus delta bytes
	markups := func(delta int) (inline, keyboard []byte) {
		txt := func(n int) string { return strings.Repeat("a", n+delta) }
		url := cat(le32(idButtonURL), tlStr(txt(1<<10)), tlStr(txt(4<<10)))
		auth := cat(le32(idButtonURLAuth), le32(1), tlStr("u"), tlStr(txt(1<<10)), tlStr("x"), le32(1))
		cp := cat(le32(idButtonCopy), le32(0), tlStr("c"), tlStr(txt(4<<10)))
		sw := cat(le32(idButtonSwitch), le32(0), tlStr("s"), tlStr(txt(1<<10)))
		bot := cat(le32(idCreateBot), le32(6), tlStr(txt(1<<10)), tlStr(txt(1<<10)))
		req := cat(le32(idButtonRequest), le32(0), tlStr("r"), le32(1), bot, le32(1))
		keyboard = cat(le32(idKeyboardMarkup), le32(8), tlVec(buttonRow(idKeyboardRow, req)), tlStr(txt(1<<10)))
		return inlineMarkup(buttonRow(idInlineRow, url, auth, cp, sw)), keyboard
	}
	cell := func(id int32, markup []byte) msgCells {
		return msgCells{mid: int64(id), uid: idAnn, data: tlMessage{id: id, from: idAnn, peer: idSelf, date: baseDate, text: "x", markup: markup}.bytes()}
	}
	overIn, overKb := markups(1)
	exactIn, exactKb := markups(0)
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{
		cell(1, overIn), cell(2, overKb), cell(3, exactIn), cell(4, exactKb),
	}))).Records)
	length := func(m map[string]any, k string) int { s, _ := m[k].(string); return len(s) }
	buttons := func(row string) []map[string]any {
		var out []map[string]any
		l, _ := rawOf(got["messages_v2/"+row])["reply_markup_buttons"].([]any)
		for _, b := range l {
			out = append(out, b.(map[string]any))
		}
		return out
	}
	check := func(inRow, kbRow string, flagged bool) {
		in, kb := buttons(inRow), buttons(kbRow)
		if len(in) != 4 || len(kb) != 1 {
			t.Fatalf("rows %s and %s: %d inline and %d keyboard buttons, want 4 and 1", inRow, kbRow, len(in), len(kb))
		}
		m, _ := rawOf(got["messages_v2/"+kbRow])["reply_markup"].(map[string]any)
		for _, c := range []struct {
			name, key  string
			have, want int
		}{
			{"reply_markup_buttons.text", "text", length(in[0], "text"), 1 << 10},
			{"reply_markup_buttons.url", "url", length(in[0], "url"), 4 << 10},
			{"reply_markup_buttons.fwd_text", "fwd_text", length(in[1], "fwd_text"), 1 << 10},
			{"reply_markup_buttons.copy_text", "copy_text", length(in[2], "copy_text"), 4 << 10},
			{"reply_markup_buttons.query", "query", length(in[3], "query"), 1 << 10},
			{"reply_markup_buttons.suggested_name", "suggested_name", length(kb[0], "suggested_name"), 1 << 10},
			{"reply_markup_buttons.suggested_username", "suggested_username", length(kb[0], "suggested_username"), 1 << 10},
			{"reply_markup.placeholder", "placeholder", length(m, "placeholder"), 1 << 10},
		} {
			if c.have != c.want {
				t.Errorf("rows %s, %s: kept %s of %d bytes, want %d", inRow, kbRow, c.key, c.have, c.want)
			}
			named := false
			for _, r := range []string{inRow, kbRow} {
				flags, _ := rawOf(got["messages_v2/"+r])["text_flags"].(string)
				named = named || slices.Contains(strings.Split(flags, ","), c.name)
			}
			if named != flagged {
				t.Errorf("rows %s, %s: text_flags names %s = %v, want %v (flagged only over the cap)", inRow, kbRow, c.name, named, flagged)
			}
		}
	}
	check("1", "2", true)
	check("3", "4", false)
}

// markupVocabulary is plan 4I4c Task 6's part of TestTelegramStateVocabularyIsPinned: the four markup kinds, the button
// kinds of R81-R85 and the state of a markup a stop cut (H2901), each by its hard-coded value.
func markupVocabulary(t *testing.T) {
	t.Helper()
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	kinds := map[string]bool{}
	buttons := map[string]bool{}
	for _, n := range []string{"81", "82", "83", "84", "85", "86", "87"} {
		raw := rawOf(rows["messages_v2/"+n])
		kinds[raw["reply_markup"].(map[string]any)["kind"].(string)] = true
		for _, k := range kindsOf(raw["reply_markup_buttons"]) {
			buttons[k] = true
		}
	}
	for _, k := range []string{"keyboard", "inline", "force_reply", "hide"} {
		if !kinds[k] {
			t.Errorf("no markup of kind %s among R81-R87", k)
		}
	}
	if len(kinds) != 4 {
		t.Errorf("markup kinds %v, want exactly the four", kinds)
	}
	for _, k := range []string{"default", "request_phone", "request_geo_location", "request_poll", "simple_web_view", "request_peer", "url", "callback", "switch_inline", "copy", "user_profile", "game"} {
		if !buttons[k] {
			t.Errorf("no button of kind %s among R81-R85", k)
		}
	}
	if st := rawOf(byRow(parseFixture(t, "telegram-hostile").Records)["messages_v2/2901"])["reply_markup_state"]; st != "partial" {
		t.Errorf("H2901: reply_markup_state %v, want partial", st)
	}
}
