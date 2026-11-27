package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// rec is one expected record of an oracle, read through the names the plan's facts use.
type rec struct {
	r       parsertest.ExpRecord
	payload map[string]any
	raw     map[string]any
	warned  bool // a "users row has no name, phone or username" warning names the row
}

type att struct {
	mime string
	size string
}

type im struct{ value, kind string }

func (e rec) str(k string) string {
	s, _ := e.payload[k].(string)
	return s
}

func (e rec) obj(k string) map[string]any {
	m, _ := e.payload[k].(map[string]any)
	return m
}

// party is the address of the first participant of the role ("" when none).
func (e rec) party(role string) string {
	parts, _ := e.payload["participants"].([]any)
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["role"] == role {
			s, _ := m["address"].(string)
			return s
		}
	}
	return ""
}

func (e rec) from() string        { return e.party("from") }
func (e rec) to() string          { return e.party("to") }
func (e rec) body() string        { return deref(e.r.Body) }
func (e rec) summary() string     { return deref(e.r.Summary) }
func (e rec) time() string        { return deref(e.r.Time) }
func (e rec) read() any           { return e.payload["read"] }
func (e rec) unknownParts() bool  { return e.payload["participants_unknown"] == true }
func (e rec) threadID() string    { s, _ := e.obj("thread")["id"].(string); return s }
func (e rec) threadTitle() string { s, _ := e.obj("thread")["title"].(string); return s }
func (e rec) threadGroup() bool   { return e.obj("thread")["group"] == true }

// rawObj is raw[k] as an object (nil when absent).
func (e rec) rawObj(k string) map[string]any {
	m, _ := e.raw[k].(map[string]any)
	return m
}

func (e rec) att0() att {
	l, _ := e.payload["attachments"].([]any)
	if len(l) == 0 {
		return att{}
	}
	m, _ := l[0].(map[string]any)
	s, _ := m["mime"].(string)
	return att{mime: s, size: num(m["size"])}
}

func (e rec) ims() []im {
	l, _ := e.payload["ims"].([]any)
	out := []im{}
	for _, v := range l {
		m, _ := v.(map[string]any)
		a, _ := m["value"].(string)
		b, _ := m["kind"].(string)
		out = append(out, im{a, b})
	}
	return out
}

func (e rec) given() string { s, _ := e.obj("names")["given"].(string); return s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// num renders a JSON number of the oracle (decoded with UseNumber) as its decimal text; "" when absent.
func num(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprintf("not a number: %v", v)
}

// oracleRecords indexes the expected records of a variant by table and rowid.
func oracleRecords(t *testing.T, variant string) map[string]map[int64]rec {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[string]map[int64]rec{}
	for _, r := range fx.Expect.Records {
		raw, _ := r.Payload["raw"].(map[string]any)
		if out[r.Table] == nil {
			out[r.Table] = map[int64]rec{}
		}
		e := rec{r: r, payload: r.Payload, raw: raw}
		for _, w := range fx.Expect.Warnings {
			if w.Table == r.Table && w.Rowid == r.Rowid && strings.Contains(w.Contains, "users row has no name, phone or username") {
				e.warned = true
			}
		}
		out[r.Table][r.Rowid] = e
	}
	return out
}

// TestAndroidTelegramOracleStatesTheSpecRules: the oracle's literal facts (plan 4I4b Task 4: one per row of the plan's
// "Fixture scenario", R60-R80, P7, P8, V5-V8, H2401, H2601, H2801-H2814, from its "Facts" column), written from the spec (§8.4-§8.10, §9,
// §10, calls 4-6, T4, T7, the plan's state vocabularies and 4I4-P10/P11/P12, preflight F1, F5, F10, F18, F22, F32,
// F33 and the Task 5 ruling on R9/R59: TL_documentEncrypted reads a boxed PhotoSize the table lacks before
// attributes) and never read back from the generator, on the CORE scenario of telegram-v181 and the pre-84 rows of
// telegram-v64. A generator that drifts from the rules fails here, not silently in the pins.
func TestAndroidTelegramOracleStatesTheSpecRules(t *testing.T) {
	all := oracleRecords(t, "telegram-v181")
	v2 := all["messages_v2"]
	// F1: R1-R59 without R52, which lies only in an uncommitted WAL frame; plan 4I4b's R60-R80; plan 4I4c's R81-R96
	if len(v2) != 95 {
		t.Fatalf("%d messages_v2 records, want 95 (R1-R96 without the uncommitted R52)", len(v2))
	}
	if _, has := v2[52]; has {
		t.Error("R52 lies only in an uncommitted WAL frame and is not live")
	}
	get := func(table string, id int64, typ string) rec {
		t.Helper()
		e, ok := all[table][id]
		if !ok {
			t.Fatalf("no %s record for rowid %d", table, id)
		}
		if e.r.Type != typ {
			t.Fatalf("%s rowid %d is a %s record, want %s", table, id, e.r.Type, typ)
		}
		return e
	}
	msg := func(n int64) rec { return get("messages_v2", n, "message") }
	call := func(n int64) rec { return get("messages_v2", n, "call") }
	user := func(uid int64) rec { return get("users", uid, "contact") }
	noMessage := func(n int64) bool { return all["messages_v2"][n].r.Type == "call" }
	want := func(id string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("fact %s does not hold", id)
		}
	}
	// spec 8.5, 8.6, 10: an incoming user-dialog text; the time is the date column (D+1)
	want("R1", msg(1).str("channel") == "telegram" && msg(1).str("direction") == "in" && msg(1).str("kind") == "text" && msg(1).from() == "user:1000002" && msg(1).body() == "Hello Ann here" && msg(1).read() == true && msg(1).str("status") == "received" && msg(1).time() == "2023-11-14T22:13:21Z" && msg(1).str("guid") == "101" && msg(1).threadID() == "1000002")
	// spec 8.5: read is incoming only; the stored read_state stays raw
	want("R2", msg(2).to() == "user:1000002" && msg(2).str("status") == "sent" && msg(2).read() == nil && num(msg(2).raw["read_state"]) == "0")
	// spec 8.5 status, 8.8 trailer: the params path and fwd_msg_id of a local id; the forward origin only in raw.fwd
	want("R3", msg(3).str("status") == "queued" && msg(3).raw["label_send_state"] == "sending" && msg(3).raw["attach_path"] == "/sdcard/x.jpg" && num(msg(3).raw["fwd_msg_id"]) == "55" && msg(3).rawObj("fwd")["from"] == "user:1000003")
	// spec 8.6: a group sender from the blob, the title from chats.data
	want("R4", msg(4).from() == "user:1000003" && msg(4).threadTitle() == "Family" && msg(4).threadGroup() && msg(4).raw["dialog_class"] == "chat")
	// spec 8.6: a channel post without from_id is from the channel; the media column is views (6.4)
	want("R5", msg(5).from() == "channel:3000001" && msg(5).raw["sender_basis"] == "channel-post" && num(msg(5).raw["views"]) == "42" && num(msg(5).raw["views_or_media_flag"]) == "42")
	// call 4: an unknown top constructor keeps the plain columns; the thread title still joins (community, 4I4-P11)
	want("R7", msg(7).str("kind") == "unknown" && msg(7).unknownParts() && msg(7).raw["data_state"] == "unknown-constructor" && msg(7).raw["data_constructor"] == "0x0badf00d" && msg(7).threadTitle() == "Hood" && msg(7).raw["dialog_class"] == "community")
	// spec 9, 8.6: a secret chat is flagged and keyed by enc_chats; the Summary names it
	want("R8", msg(8).raw["secret_chat"] == true && msg(8).from() == "user:1000002" && num(msg(8).raw["enc_chat_id"]) == "77" && strings.HasPrefix(msg(8).summary(), "telegram secret in user:1000002: "))
	// Task 5 ruling on F22: TL_documentEncrypted stops at its boxed PhotoSize (thumbs), before attributes, key and iv;
	// mime_type and size were read before the stop; key_present is never stated
	want("R9", msg(9).to() == "user:1000002" && msg(9).att0() == att{mime: "image/jpeg", size: "2048"} && msg(9).raw["data_state"] == "partial" && msg(9).raw["data_stop_path"] == "media.document.thumbs" && msg(9).rawObj("media")["key_present"] == nil)
	// facts.tsv cast.encrypted_chat_id is int32 (4I4-P12): the low 32 bits sign-extended
	want("R10", msg(10).to() == "user:1000099" && num(msg(10).raw["enc_chat_id"]) == "-5")
	// spec 8.7, Q5-Q7: one call per phone-call row, no message
	want("R11", call(11).str("outcome") == "missed" && call(11).str("kind") == "voice" && call(11).str("address") == "user:1000002" && noMessage(11))
	want("R12", call(12).str("direction") == "out" && call(12).str("outcome") == "unknown" && call(12).raw["label_reason"] == "hangup" && call(12).str("kind") == "video")
	want("R13-R17", call(13).str("outcome") == "rejected" && call(14).raw["reason_state"] == "not-recorded" && call(16).raw["slug"] == "conf-1" && call(17).str("address") == "" && call(17).raw["address_state"] == "not-user-dialog")
	// spec 8.7: a conference-call action is an opaque row (plan 4I4b rulings R2, B2): a system message, partial at the
	// action
	want("R18 R19", msg(18).str("kind") == "system" && msg(18).raw["data_state"] == "partial" && msg(18).raw["data_stop_id"] == "0x2ffe2f7a" && msg(19).str("kind") == "system")
	// spec 8.5: messageEmpty claims nothing about deletion
	want("R20", msg(20).str("kind") == "unknown" && msg(20).raw["label_constructor"] == "messageEmpty" && !msg(20).r.Deleted)
	// spec 8.5 media summary
	want("R21", msg(21).str("kind") == "media" && msg(21).raw["data_state"] == "decoded" && msg(21).rawObj("media")["type"] == "photo" && num(msg(21).rawObj("media")["id"]) == "9001")
	want("R22", msg(22).raw["data_state"] == "partial" && msg(22).raw["data_stop_path"] == "media.photo.sizes[0]" && num(msg(22).rawObj("media")["id"]) == "9001" && msg(22).body() == "")
	want("R23", msg(23).att0() == att{mime: "application/pdf", size: "12345"} && msg(23).raw["data_state"] == "partial")
	want("R24 R25", num(msg(24).rawObj("media")["lat"]) == "52.52" && num(msg(24).rawObj("media")["long"]) == "13.405" && msg(25).rawObj("media")["lat_state"] == "out-of-range")
	// plan 4I4b Task 1: TL_messageMediaDice is full, so R29 (a well-formed Dice body since Task 3, Task 1 review M1)
	// decodes whole
	want("R29", msg(29).str("kind") == "media" && msg(29).rawObj("media")["type"] == "dice" && msg(29).raw["data_state"] == "decoded")
	// spec 8.5 reply copy: never Body, never target_guid
	want("R30", num(msg(30).raw["reply_to_msg_id"]) == "101" && msg(30).rawObj("reply_copy")["copy"] == true && msg(30).rawObj("reply_copy")["text"] == "Hello Ann here" && msg(30).str("target_guid") == "")
	// T3: entities and quote replies decode
	want("R31 R32", msg(31).raw["data_state"] == "decoded" && msg(32).raw["data_state"] == "decoded")
	// spec 8.5 direction
	want("R34", msg(34).str("direction") == "unknown" && msg(34).raw["direction_conflict"] == true)
	want("R35", msg(35).raw["date_conflict"] == true && msg(35).raw["id_conflict"] == true && msg(35).time() == "2023-11-14T22:13:55Z")
	want("R36-R40", msg(36).str("status") == "failed" && msg(37).str("status") == "unknown" && msg(37).raw["label_send_state"] == "editing" && msg(38).read() == false && msg(39).read() == nil && msg(39).raw["read_state_state"] == "out-of-range" && msg(40).str("status") == "unknown")
	// spec 8.8 trailer states
	want("R41-R43", msg(41).raw["attach_path_state"] == "empty-marker" && msg(42).raw["trailer_state"] == "unparsed" && num(msg(42).raw["trailer_len"]) == "2" && msg(43).raw["trailer_state"] == "none")
	// Review Focus 1: the same mid in two dialogs
	want("R44", msg(44).str("guid") == "101" && msg(44).threadID() == "1000003")
	// 4I4-P11: a class disagreement is stated, never resolved
	want("R47", msg(47).raw["dialog_class_state"] == "conflict" && msg(47).raw["thread_title_state"] == "class-mismatch" && msg(47).threadTitle() == "")
	want("R48", msg(48).unknownParts() && msg(48).raw["dialog_class"] == "folder")
	want("R49 R50", msg(49).raw["thread_user_state"] == "no-users-row" && msg(50).raw["data_state"] == "null" && msg(50).str("kind") == "unknown")
	want("R53 R55", msg(53).payload["body_truncated"] == true && num(msg(55).raw["custom_params_len"]) == "16" && msg(55).raw["replies_data_constructor"] == "0x0bad0004")
	// spec 8.9, T4: composed display name, prefixed ims
	want("U2", user(1000002).given() == "Ann" && user(1000002).str("display_name") == "Ann Example" && reflect.DeepEqual(user(1000002).ims(), []im{{"user:1000002", "telegram"}, {"ann_example", "telegram_username"}}) && user(1000002).raw["contact_row"] == true && num(user(1000002).raw["mutual"]) == "1")
	// F33
	want("R57", msg(57).raw["secret_chat"] == true && num(msg(57).raw["enc_chat_id"]) == "77" && msg(57).from() == "user:1000002")
	flags, _ := msg(58).raw["data_flags"].(string)
	want("R58", strings.Contains(flags, "unknown-flag-bits") && msg(58).body() == "Flagged")
	// F22 and the Task 5 ruling: the second secret document stops at the same place
	want("R59", msg(59).raw["data_state"] == "partial" && msg(59).raw["data_stop_path"] == "media.document.thumbs" && msg(59).rawObj("media")["key_present"] == nil)
	// coordinator ruling M3 (Task 5 review): TL_messageMediaEmpty reads no field, so it is full and read whole; the
	// secret rows that carry it decode with no stop, and v181 counts 8 undecoded message blobs (7 partial: R29 decodes
	// since plan 4I4b, and plan 4I4b's R79 stops at its opaque cached page), not 11
	for _, n := range []int64{8, 10, 57} {
		_, stopped := msg(n).raw["data_stop_kind"]
		want(fmt.Sprintf("M3 R%d", n), msg(n).raw["data_state"] == "decoded" && !stopped && msg(n).raw["trailer_state"] == "none")
	}
	undecoded := false
	for _, w := range parsertest.LoadFixture(t, "testdata", "telegram-v181").Expect.Warnings {
		undecoded = undecoded || w.Contains == "telegram-blob-undecoded: messages_v2.data: 8 records (partial:7,unknown-constructor:1)"
	}
	want("M3 count", undecoded)
	// F18
	want("U5 U6 U9", user(1000005).warned && user(1000006).warned && user(1000009).raw["data_state"] == "unknown-constructor")
	// Review Focus 3: a present but empty name part is absent
	want("U7", user(1000007).given() == "" && user(1000007).str("display_name") == "Solo")
	// plan 4I4b "Fixture scenario" (Task 4): one literal fact per row, from the plan's "Facts" column and the "Raw
	// shapes" contract (D10, R6, B6, B1, B10, D13, P3-P5, P8, P9, D11), read from the committed oracle
	media := func(n int64) map[string]any { return msg(n).rawObj("media") }
	list := func(e rec, k string) []any { l, _ := e.raw[k].([]any); return l }
	entry := func(l []any, i int) map[string]any {
		if i >= len(l) {
			return nil
		}
		m, _ := l[i].(map[string]any)
		return m
	}
	atts := func(e rec) []any { l, _ := e.payload["attachments"].([]any); return l }
	strs := func(v any) []string {
		l, _ := v.([]any)
		out := []string{}
		for _, x := range l {
			s, _ := x.(string)
			out = append(out, s)
		}
		return out
	}
	_, r60Undecided := msg(60).raw["data_undecided"]
	want("R60", reflect.DeepEqual(atts(msg(60)), []any{map[string]any{"name": "report.pdf", "mime": "application/pdf", "size": json.Number("48213")}}) &&
		reflect.DeepEqual(strs(media(60)["attributes"]), []string{"filename", "image_size"}) && num(media(60)["w"]) == "800" && num(media(60)["h"]) == "600" &&
		len(list(msg(60), "media_thumbs")) == 2 && entry(list(msg(60), "media_thumbs"), 0)["kind"] == "stripped" && entry(list(msg(60), "media_thumbs"), 1)["kind"] == "size" &&
		num(msg(60).raw["media_thumbs_total"]) == "2" && msg(60).r.Times["edited"] == "2023-11-14T22:25:00Z" &&
		num(msg(60).raw["blob_forwards"]) == "4" && msg(60).raw["forwards_conflict"] == nil && msg(60).raw["data_state"] == "decoded" && !r60Undecided &&
		msg(60).raw["post_author"] == "Desk" && num(msg(60).raw["quick_reply_shortcut_id"]) == "7" && num(msg(60).raw["effect"]) == "5000000002" &&
		num(msg(60).raw["paid_message_stars"]) == "10" && msg(60).raw["report_delivery_until_date_unit"] == "unestablished" &&
		num(msg(60).raw["schedule_repeat_period"]) == "86400" && msg(60).raw["summary_from_language"] == "en" &&
		entry(list(msg(60), "entities"), 0)["type"] == "text_url" && entry(list(msg(60), "entities"), 0)["url"] == "fixture.invalid/r")
	// D11: thumbnail content is its length and SHA-256 only
	_, thumbBytes := entry(list(msg(60), "media_thumbs"), 0)["bytes"]
	want("R60 D11", num(entry(list(msg(60), "media_thumbs"), 0)["bytes_len"]) == "40" && !thumbBytes)
	// B10: the blob's forwards beside the column's
	want("R61", num(msg(61).raw["blob_forwards"]) == "5" && num(msg(61).raw["forwards"]) == "4" && msg(61).raw["forwards_conflict"] == true)
	results, recent, top := list(msg(62), "reactions_results"), list(msg(62), "reactions_recent"), list(msg(62), "reactions_top_reactors")
	reactionsOf := func(l []any, k string) []string {
		out := []string{}
		for i := range l {
			s, _ := entry(l, i)[k].(string)
			out = append(out, s)
		}
		return out
	}
	want("R62", reflect.DeepEqual(reactionsOf(results, "reaction"), []string{"emoji:\U0001F44D", "custom:5000000001", "paid", "empty"}) &&
		num(entry(results, 0)["chosen_order"]) == "1" && num(entry(results, 2)["count"]) == "3" &&
		reflect.DeepEqual(reactionsOf(recent, "peer"), []string{"user:1000002", "user:1000001", "user:1000002", "user:1000009"}) &&
		reflect.DeepEqual(reactionsOf(recent, "reaction"), []string{"emoji:❤", "custom:5000000001", "emoji:\U0001F525", "emoji:\U0001F602"}) &&
		entry(recent, 0)["big"] == true && entry(recent, 0)["date_unit"] == "unestablished" && entry(recent, 1)["unread"] == true &&
		reflect.DeepEqual(reactionsOf(top, "peer"), []string{"user:1000002", "", "user:1000001"}) && entry(top, 0)["top"] == true &&
		entry(top, 1)["anonymous"] == true && entry(top, 2)["my"] == true &&
		num(msg(62).rawObj("reactions")["results_total"]) == "4" && num(msg(62).rawObj("reactions")["top_reactors_total"]) == "3" &&
		msg(62).from() == "user:1000002" && len(msg(62).payload["participants"].([]any)) == 1)
	r63 := msg(63).rawObj("replies")
	want("R63 R64", num(r63["replies"]) == "3" && num(r63["replies_pts"]) == "77" && r63["comments"] == true && r63["channel"] == "channel:3000001" &&
		num(r63["max_id"]) == "300" && num(r63["read_max_id"]) == "299" && reflect.DeepEqual(strs(r63["recent_repliers"]), []string{"user:1000002"}) &&
		num(r63["recent_repliers_total"]) == "1" && msg(63).raw["replies_data_state"] == "decoded" && reflect.DeepEqual(msg(63).rawObj("replies_data"), r63) &&
		msg(63).raw["replies_conflict"] == nil && msg(64).raw["replies_conflict"] == true && num(msg(64).rawObj("replies_data")["replies"]) == "4")
	fc, sp := msg(65).rawObj("factcheck"), msg(65).rawObj("suggested_post")
	price, _ := sp["price"].(map[string]any)
	want("R65", reflect.DeepEqual(list(msg(65), "restriction_reason"), []any{map[string]any{"platform": "android", "reason": "sensitive", "text": "Hidden on Android"}}) &&
		fc["need_check"] == true && fc["country"] == "US" && fc["text"] == "Context added" && num(fc["entities_total"]) == "1" && num(fc["hash"]) == "99" &&
		sp["accepted"] == true && price["kind"] == "stars" && num(price["amount"]) == "50" && num(price["nanos"]) == "0" &&
		num(sp["schedule_date"]) == "1700000900" && sp["schedule_date_unit"] == "unestablished")
	ton, _ := msg(66).rawObj("suggested_post")["price"].(map[string]any)
	want("R66", msg(66).rawObj("suggested_post")["rejected"] == true && ton["kind"] == "ton" && num(ton["amount"]) == "1000000000")
	want("R67", msg(67).raw["data_state"] == "decoded" && msg(67).body() == "Nice photo" && num(msg(67).raw["reply_to_msg_id"]) == "117")
	mc, _ := media(68)["mask_coords"].(map[string]any)
	want("R68", media(68)["sticker_alt"] == "\U0001F600" && media(68)["mask"] == true && num(mc["n"]) == "1" && num(mc["x"]) == "0.5" &&
		num(mc["y"]) == "-0.25" && num(mc["zoom"]) == "1.5" && media(68)["stickerset_kind"] == "id" && num(media(68)["stickerset_id"]) == "7000001" &&
		media(68)["stickerset_access_hash_present"] == true && media(68)["file_name_state"] == "absent")
	want("R69", media(69)["stickerset_kind"] == "short_name" && media(69)["stickerset_short_name"] == "FixtureSet" && media(69)["sticker_alt"] == "⭐")
	want("R70", media(70)["voice"] == true && media(70)["audio_voice"] == true && num(media(70)["duration"]) == "12" && media(70)["duration_unit"] == "unestablished" &&
		media(70)["title"] == "Memo" && media(70)["performer"] == "Ann" && num(media(70)["waveform_len"]) == "63" && media(70)["waveform_sha256"] != nil)
	vt := list(msg(71), "media_video_thumbs")
	want("R71", num(media(71)["duration"]) == "7.5" && media(71)["video_round_message"] == true && media(71)["round_message"] == nil && media(71)["supports_streaming"] == true &&
		media(71)["video_codec"] == "h264" && num(media(71)["video_start_ts"]) == "0.5" && len(vt) == 1 && entry(vt, 0)["kind"] == "video")
	g, _ := media(72)["game_outcome"].(map[string]any)
	want("R72", num(g["stake_ton_amount"]) == "100" && num(g["ton_amount"]) == "200" && num(g["seed_len"]) == "19" && len(g) == 3)
	_, r73Name := entry(atts(msg(73)), 0)["name"]
	_, r75Name := entry(atts(msg(75)), 0)["name"]
	want("R73 R74 R75", media(73)["file_name_state"] == "conflict" && reflect.DeepEqual(strs(media(73)["file_names"]), []string{"a.txt", "b.txt"}) &&
		num(media(73)["file_names_total"]) == "2" && !r73Name && media(74)["file_name"] == "same.txt" && entry(atts(msg(74)), 0)["name"] == "same.txt" &&
		media(75)["file_name_state"] == "empty" && !r75Name)
	r76, _ := json.Marshal(msg(76).payload)
	want("R76", len(atts(msg(76))) == 0 && len(list(msg(76), "media_sizes")) == 4 && len(list(msg(76), "media_video_sizes")) == 3 &&
		num(media(76)["live_photo_document_id"]) == "9201" && media(76)["live_photo_mime_type"] == "video/mp4" && !strings.Contains(string(r76), "live.mov"))
	want("R77", len(atts(msg(77))) == 0 && media(77)["document_file_name"] == "bundle.zip" && media(77)["webpage_type"] == "article" &&
		reflect.DeepEqual(strs(media(77)["webpage_attributes"]), []string{"sticker_set"}))
	want("R78", entry(atts(msg(78)), 0)["name"] == "main.mp4" && num(media(78)["alt_documents_total"]) == "1")
	_, r79Edited := msg(79).r.Times["edited"]
	want("R79", msg(79).raw["data_state"] == "partial" && msg(79).raw["data_stop_path"] == "media.webpage.cached_page" &&
		media(79)["cached_page_class"] == "TL_page_layer110" && strings.Contains(","+fmt.Sprint(msg(79).raw["data_undecided"])+",", ",edit_date,") && !r79Edited)
	want("R80", media(80)["key_present"] == true && media(80)["iv_present"] == true && entry(atts(msg(80)), 0)["name"] == "secret.pdf" &&
		entry(list(msg(80), "media_thumbs"), 0)["location_class"] == "TL_fileEncryptedLocation" && msg(80).raw["secret_chat"] == true)
	stageBFacts(t, want, msg)
	n := 0
	for _, tb := range all {
		for _, r := range tb {
			n++
			want(fmt.Sprintf("%s rowid %d live, account 0", r.r.Table, r.r.Rowid), !r.r.Deleted && r.raw["account"] == "0")
		}
	}
	if n == 0 {
		t.Fatal("telegram-v181 has no record")
	}

	// spec 8.3, F10, F32: the pre-84 conventions on telegram-v64 (messages rowid = mid)
	old := oracleRecords(t, "telegram-v64")["messages"]
	byMid := func(mid int64) rec {
		t.Helper()
		e, ok := old[mid]
		if !ok {
			t.Fatalf("telegram-v64: no messages record for mid %d", mid)
		}
		return e
	}
	p1, p2, p3, p4, p5 := byMid(44|3000001<<32), byMid(-7), byMid(47), byMid(45|3000002<<32), byMid(101)
	want("P1", num(p1.raw["message_id"]) == "44" && p1.raw["message_id_basis"] == "pre-84-channel-packing")
	want("P2", p2.raw["dialog_class"] == "secret" && p2.raw["dialog_basis"] == "pre-84-high-id" && num(p2.raw["enc_chat_id"]) == "77")
	_, hasTTLTime := p3.r.Times["ttl"]
	want("P3", p3.raw["ttl_state"] == "pre-84-forward-peer" && !hasTTLTime)
	want("P4", p4.raw["mid_state"] == "undecided")
	_, p5State := p5.raw["mid_state"]
	want("P5", !p5State && p5.body() == "Hello Ann here")

	// spec ruling T7 (Task 5 review M1): the oracle's stop rules on telegram-hostile. A string whose 0xFE prefix
	// declares 0xFFFFFF bytes past the end of the blob is truncated, never a cap (the framing cannot declare one above
	// MaxString), stopped at the length prefix of message (offset 44); a vector count of 0x7FFFFFFF is above MaxVector
	// and is a cap, stopped at entities where the vector starts (offset 60: plan 4I4 Task 8 amended the oracle, which
	// stopped at the count, to decode/tl's rule that a stop is where the failed read began).
	hostile := oracleRecords(t, "telegram-hostile")["messages_v2"]
	hostileMsg := func(rowid int64) rec {
		t.Helper()
		e, ok := hostile[rowid]
		if !ok || e.r.Type != "message" {
			t.Fatalf("telegram-hostile: no messages_v2 message record for rowid %d", rowid)
		}
		return e
	}
	h13, h14 := hostileMsg(2013), hostileMsg(2014)
	want("H2013", num(h13.raw["mid"]) == "2013" && h13.raw["data_state"] == "cap" && h13.raw["data_stop_kind"] == "cap" &&
		h13.raw["data_stop_path"] == "entities" && num(h13.raw["data_stop_offset"]) == "60")
	want("H2014", num(h14.raw["mid"]) == "2014" && h14.raw["data_state"] == "truncated" && h14.raw["data_stop_kind"] == "truncated" &&
		h14.raw["data_stop_path"] == "message" && num(h14.raw["data_stop_offset"]) == "44")
	// plan 4I4b P7, P8 (telegram-v64): the 5.14.0 media path decodes whole to the trailer; P5 (agreeing carriers)
	p7, p8 := byMid(108), byMid(109)
	p7m, p8m := p7.rawObj("media"), p8.rawObj("media")
	p8a, _ := p8.payload["attachments"].([]any)
	want("P7 P8", p7.raw["data_state"] == "decoded" && p8.raw["data_state"] == "decoded" && p7.raw["trailer_state"] == "none" &&
		p8.raw["trailer_state"] == "none" && p7m["type"] == "photo" && num(p7.raw["media_sizes_total"]) == "3" &&
		p8m["file_name"] == "IMG_0001.mp4" && num(p8m["duration"]) == "30" && num(p8m["w"]) == "1280" && num(p8m["h"]) == "720" &&
		len(p8a) == 1 && p8a[0].(map[string]any)["name"] == "IMG_0001.mp4")
	// plan 4I4b V5-V8 (telegram-v84, Review Focus 5): a legacy caption is raw, never Body (Q9); a legacy video or audio
	// is an attachment {mime, size} with no name; an old document's inline file_name names it
	v84 := oracleRecords(t, "telegram-v84")["messages_v2"]
	v5, v6, v7, v8 := v84[5], v84[6], v84[7], v84[8]
	attOf := func(e rec) map[string]any {
		l, _ := e.payload["attachments"].([]any)
		if len(l) != 1 {
			return nil
		}
		m, _ := l[0].(map[string]any)
		return m
	}
	want("V5-V8", v5.body() == "" && v5.rawObj("media")["caption"] == "Old caption" && v5.str("kind") == "media" &&
		reflect.DeepEqual(attOf(v6), map[string]any{"mime": "video/mp4", "size": json.Number("2000")}) && v6.rawObj("media")["type"] == "video" &&
		reflect.DeepEqual(attOf(v7), map[string]any{"mime": "audio/mpeg", "size": json.Number("3000")}) && v7.rawObj("media")["type"] == "audio" &&
		attOf(v8)["name"] == "old.doc" && v8.rawObj("media")["file_name"] == "old.doc")

	// plan 4I4b H2401 and H2601: R76's and R60's blob cut at every boundary of field_offsets(deep=True); each is a
	// record, truncated, and no tail value of a field at or after the cut is stated (spec 8.4)
	tailKeys := map[string]string{
		"forwards": "blob_forwards", "post_author": "post_author", "quick_reply_shortcut_id": "quick_reply_shortcut_id",
		"effect": "effect", "report_delivery_until_date": "report_delivery_until_date", "paid_message_stars": "paid_message_stars",
		"schedule_repeat_period": "schedule_repeat_period", "summary_from_language": "summary_from_language", "entities": "entities",
		"edit_date": "edit_date", "views": "views",
	}
	cutRows := 0
	for rowid := int64(2401); rowid < 2801; rowid++ {
		e, ok := hostile[rowid]
		if !ok {
			continue
		}
		cutRows++
		stop, _ := e.raw["data_stop_path"].(string)
		head, _, _ := strings.Cut(strings.SplitN(stop, "[", 2)[0], ".")
		at := -1
		for i, f := range topFields() {
			if f.name == head {
				at = i
			}
			if k, ok := tailKeys[f.name]; ok && at >= 0 && e.raw[k] != nil {
				want(fmt.Sprintf("H%d: raw.%s from %s, at or after the cut %q", rowid, k, f.name, stop), false)
			}
		}
		want(fmt.Sprintf("H%d truncated", rowid), e.raw["data_state"] == "truncated")
		cutMediaKeys(want, rowid, e, stop, head, at)
	}
	want("H2401 H2601 count", cutRows == 68+51)
	h := func(rowid int64) rec { return hostileMsg(rowid) }
	hm := func(rowid int64) map[string]any { return hostileMsg(rowid).rawObj("media") }
	hl := func(rowid int64, k string) []any { l, _ := hostileMsg(rowid).raw[k].([]any); return l }
	hostileFlags := func(rowid int64) string { s, _ := h(rowid).raw["data_flags"].(string); return s }
	want("H2801", num(h(2801).raw["media_sizes_total"]) == "17" && h(2801).raw["media_sizes_truncated"] == true && len(hl(2801, "media_sizes")) == 16)
	want("H2802", h(2802).raw["data_state"] == "cap" && h(2802).raw["data_stop_path"] == "media.photo.sizes" && h(2802).raw["media_sizes_state"] == "partial")
	want("H2803 H2804", strings.Contains(hostileFlags(2803), "unknown-flag-bits") && strings.Contains(hostileFlags(2804), "unknown-flag-bits"))
	want("H2805", hm(2805)["duration_state"] == "out-of-range" && hm(2805)["duration_bits"] == "0x7ff8000000000000" && hm(2805)["duration"] == nil)
	for _, rowid := range []int64{2806, 2807} {
		want(fmt.Sprintf("H%d", rowid), h(rowid).raw["data_state"] == "partial" && h(rowid).raw["data_stop_kind"] == "opaque" &&
			h(rowid).raw["data_stop_path"] == "reactions.results[0]" && h(rowid).raw["data_stop_id"] == "0x6fb250d1" &&
			h(rowid).raw["reactions_state"] == "partial" && h(rowid).raw["reactions"] == nil && h(rowid).raw["reactions_results"] == nil)
	}
	want("H2808", num(h(2808).rawObj("reactions")["results_total"]) == "65" && h(2808).rawObj("reactions")["results_truncated"] == true && len(hl(2808, "reactions_results")) == 64)
	want("H2809", num(h(2809).raw["restriction_reason_total"]) == "17" && h(2809).raw["restriction_reason_truncated"] == true && len(hl(2809, "restriction_reason")) == 16)
	want("H2810", num(h(2810).raw["entities_total"]) == "65" && h(2810).raw["entities_truncated"] == true && len(hl(2810, "entities")) == 64)
	want("H2811", num(h(2811).rawObj("reactions")["top_reactors_total"]) == "17" && h(2811).rawObj("reactions")["top_reactors_truncated"] == true && len(hl(2811, "reactions_top_reactors")) == 16)
	h12 := h(2812).rawObj("replies")
	want("H2812", num(h12["recent_repliers_total"]) == "17" && h12["recent_repliers_truncated"] == true && len(strs(h12["recent_repliers"])) == 16)
	want("H2813", num(hm(2813)["attributes_total"]) == "33" && hm(2813)["attributes_truncated"] == true && len(strs(hm(2813)["attributes"])) == 32)
	want("H2814", hm(2814)["file_name_state"] == "conflict" && num(hm(2814)["file_names_total"]) == "9" && len(strs(hm(2814)["file_names"])) == 8)
	stageBHostileFacts(want, h, hostile)
}

// stageBList is raw[k] of e as a list of objects (nil entries for anything else).
func stageBList(e rec, k string) []map[string]any {
	l, _ := e.raw[k].([]any)
	out := make([]map[string]any, len(l))
	for i, x := range l {
		out[i], _ = x.(map[string]any)
	}
	return out
}

// field is key k of every entry, as text ("" when absent; numbers by their decimal text).
func field(l []map[string]any, k string) []string {
	out := []string{}
	for _, m := range l {
		switch v := m[k].(type) {
		case string:
			out = append(out, v)
		case nil:
			out = append(out, "")
		case bool:
			out = append(out, strconv.FormatBool(v))
		default:
			out = append(out, num(v))
		}
	}
	return out
}

func strList(v any) []string {
	l, _ := v.([]any)
	out := []string{}
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// stageBFacts: the literal facts of plan 4I4c's "Fixture scenario" rows R81-R96 on telegram-v181, one per row of its
// "Facts" column, written from the plan's "Raw shapes" contract (design 9.1-9.4; rulings Q5, Q6, Q8, Q9, C3, C4, PL5,
// P12, T2a) and never read back from the generator.
func stageBFacts(t *testing.T, want func(string, bool), msg func(int64) rec) {
	t.Helper()
	markup := func(n int64) map[string]any { return msg(n).rawObj("reply_markup") }
	buttons := func(n int64) []map[string]any { return stageBList(msg(n), "reply_markup_buttons") }
	only := func(n int64) bool { // participants unchanged: the sender alone (no button user, voter or mentioned user)
		parts, _ := msg(n).payload["participants"].([]any)
		return len(parts) == 1 && msg(n).from() == "user:1000002"
	}
	// R81 (design 9.1, 9.2): a reply keyboard with every ButtonType, a style and a request-peer button with admin rights
	b81 := buttons(81)
	m81 := markup(81)
	want("R81", m81["kind"] == "keyboard" && m81["constructor"] == "0x85dd99d1" && m81["resize"] == true && m81["single_use"] == true &&
		m81["is_persistent"] == true && m81["selective"] == nil && m81["placeholder"] == "Pick one" && num(m81["rows_total"]) == "3" &&
		num(m81["buttons_total"]) == "6" && m81["buttons_truncated"] == nil && msg(81).raw["data_state"] == "decoded" &&
		reflect.DeepEqual(field(b81, "kind"), []string{"default", "request_phone", "request_geo_location", "request_poll", "simple_web_view", "request_peer"}) &&
		reflect.DeepEqual(field(b81, "text"), []string{"Yes", "Call me", "Where", "Quiz", "App", "Pick chat"}) &&
		reflect.DeepEqual(field(b81, "row"), []string{"0", "0", "0", "1", "1", "2"}) && reflect.DeepEqual(field(b81, "column"), []string{"0", "1", "2", "0", "1", "0"}) &&
		b81[3]["quiz"] == true && b81[4]["url"] == "fixture.invalid/app" && b81[4]["style_primary"] == true && num(b81[4]["style_icon"]) == "5000000005" &&
		num(b81[5]["button_id"]) == "7" && b81[5]["peer_type"] == "chat" && b81[5]["peer_type_creator"] == true &&
		b81[5]["user_admin_rights"] == "change_info,ban_users" && b81[5]["bot_admin_rights"] == "post_messages" && num(b81[5]["max_quantity"]) == "1" &&
		msg(81).r.Times["edited"] == "2023-11-14T22:14:20Z" && reflect.DeepEqual(stageBList(msg(81), "entities"), []map[string]any{
		{"type": "bold", "offset": json.Number("0"), "length": json.Number("3")},
	}) && msg(81).body() == "Keyboard" && only(81))
	// R82 (PL5): plain TL_keyboardButton_layer* is default
	want("R82", markup(82)["constructor"] == "0x3502758c" && markup(82)["kind"] == "keyboard" &&
		reflect.DeepEqual(field(buttons(82), "kind"), []string{"default", "request_phone"}))
	// R83 (Q8): callback data is its length and SHA-256 only; a profile button's user is an address, never a participant
	b83, cbSum := buttons(83), sha256.Sum256([]byte("SYNTHETIC-CALLBA"))
	_, data := b83[1]["data"]
	want("R83", markup(83)["kind"] == "inline" && markup(83)["constructor"] == "0xb2b15770" && num(markup(83)["rows_total"]) == "2" &&
		num(markup(83)["buttons_total"]) == "5" &&
		reflect.DeepEqual(field(b83, "kind"), []string{"url", "callback", "switch_inline", "copy", "user_profile"}) &&
		b83[0]["url"] == "fixture.invalid/o" && b83[1]["requires_password"] == true && num(b83[1]["data_len"]) == "16" &&
		b83[1]["data_sha256"] == hex.EncodeToString(cbSum[:]) && !data && b83[2]["same_peer"] == true && b83[2]["query"] == "q" &&
		b83[2]["peer_types"] == "pm,chat,broadcast" && num(b83[2]["peer_types_total"]) == "3" && num(b83[2]["row"]) == "1" &&
		b83[3]["copy_text"] == "CODE-1" && b83[4]["user"] == "user:1000009" && only(83))
	// R84 (C3): a KeyboardButton element of a TL_keyboardInlineButtonRow_layer228 row is one the A-HEAD app drops
	b84 := buttons(84)
	want("R84", reflect.DeepEqual(field(b84, "kind"), []string{"url", "callback", "default"}) && b84[0]["ahead_app_drops"] == nil &&
		b84[1]["ahead_app_drops"] == nil && b84[2]["ahead_app_drops"] == true && num(b84[1]["data_len"]) == "8")
	// R85 (Q5 held, T2a): the 5.14.0-era inline markup decodes whole
	want("R85", msg(85).raw["data_state"] == "decoded" && markup(85)["constructor"] == "0x48a30254" && markup(85)["kind"] == "inline" &&
		reflect.DeepEqual(field(buttons(85), "kind"), []string{"url", "callback", "game"}) && num(buttons(85)[1]["data_len"]) == "4")
	_, r86Rows := markup(86)["rows_total"]
	want("R86 R87", markup(86)["kind"] == "force_reply" && markup(86)["single_use"] == true && markup(86)["placeholder"] == "Reply here" && !r86Rows &&
		markup(87)["kind"] == "hide" && markup(87)["selective"] == true && msg(87).r.Times["edited"] == "2023-11-14T22:14:30Z")
	// R88 (design 9.3, D9, D10, D11): a poll is raw only, never Body; voters and answer authors are never participants
	poll := func(n int64) map[string]any { return msg(n).rawObj("poll") }
	results := func(n int64) map[string]any { return msg(n).rawObj("poll_results") }
	p88, r88, a88 := poll(88), results(88), stageBList(msg(88), "poll_answers")
	o88, v88 := stageBList(msg(88), "poll_results_options"), stageBList(msg(88), "poll_voters")
	want("R88", msg(88).rawObj("media")["type"] == "poll" && msg(88).rawObj("media")["constructor"] == "0x773f4e66" && msg(88).str("kind") == "media" &&
		p88["constructor"] == "0x966e2dbf" && num(p88["id"]) == "8000001" && p88["quiz"] == true && p88["public_voters"] == true && p88["creator"] == true &&
		p88["multiple_choice"] == nil && p88["question"] == "Capital?" && num(p88["question_entities_total"]) == "1" && num(p88["close_period"]) == "600" &&
		p88["close_period_unit"] == "unestablished" && num(p88["close_date"]) == "1700000600" && p88["close_date_unit"] == "unestablished" &&
		reflect.DeepEqual(strList(p88["countries_iso2"]), []string{"FR", "IT"}) && num(p88["countries_iso2_total"]) == "2" && num(p88["hash"]) == "77" &&
		num(p88["answers_total"]) == "2" && p88["attached_media_type"] == "geo" && p88["attached_media_constructor"] == "0x56e0d474" &&
		reflect.DeepEqual(field(a88, "text"), []string{"Paris", "Rome"}) && reflect.DeepEqual(field(a88, "option"), []string{"00", "01"}) &&
		reflect.DeepEqual(field(a88, "index"), []string{"0", "1"}) && a88[1]["media_type"] == "geo" && a88[1]["added_by"] == "user:1000009" &&
		num(a88[1]["date"]) == "1700000005" && a88[1]["date_unit"] == "unestablished" && a88[0]["media_type"] == nil &&
		r88["constructor"] == "0xba7bb15e" && r88["has_unread_votes"] == true && num(r88["total_voters"]) == "4" && num(r88["results_total"]) == "2" &&
		r88["solution"] == "Because" && num(r88["solution_entities_total"]) == "0" && reflect.DeepEqual(strList(r88["recent_voters"]), []string{"user:1000002"}) &&
		num(r88["recent_voters_total"]) == "1" && num(r88["option_voters_total"]) == "3" &&
		reflect.DeepEqual(field(o88, "option"), []string{"00", "01"}) && reflect.DeepEqual(field(o88, "voters"), []string{"3", "1"}) &&
		o88[0]["chosen"] == true && o88[0]["correct"] == true && o88[1]["chosen"] == nil && reflect.DeepEqual(field(o88, "recent_voters_total"), []string{"2", "1"}) &&
		reflect.DeepEqual(field(v88, "option"), []string{"00", "00", "01"}) &&
		reflect.DeepEqual(field(v88, "peer"), []string{"user:1000002", "user:1000009", "channel:3000001"}) && msg(88).body() == "" && only(88))
	a89 := stageBList(msg(89), "poll_answers")
	want("R89", poll(89)["constructor"] == "0x58747131" && poll(89)["multiple_choice"] == true && num(poll(89)["answers_total"]) == "3" &&
		num(poll(89)["close_date"]) == "1700000900" && reflect.DeepEqual(field(a89, "text"), []string{"A", "B", "C"}) &&
		reflect.DeepEqual(field(a89, "option"), []string{"00", "01", "02"}) && results(89)["constructor"] == "0x7adf2420" &&
		num(results(89)["results_total"]) == "1" && num(results(89)["total_voters"]) == "2" &&
		reflect.DeepEqual(field(stageBList(msg(89), "poll_results_options"), "voters"), []string{"2"}))
	// R90-R93 (T1a, T4): the old classes read question.text and text.text; id-vector voters are user:<id> as stored
	want("R90-R93", poll(90)["constructor"] == "0xd5529d06" && poll(90)["question"] == "Old?" &&
		reflect.DeepEqual(field(stageBList(msg(90), "poll_answers"), "text"), []string{"Yes"}) &&
		reflect.DeepEqual(field(stageBList(msg(90), "poll_answers"), "constructor"), []string{"0x6ca9c2e9"}) &&
		results(90)["constructor"] == "0xc87024a2" && reflect.DeepEqual(strList(results(90)["recent_voters"]), []string{"user:1000002", "user:1000009"}) &&
		poll(91)["constructor"] == "0x86e18161" && num(poll(91)["close_period"]) == "60" && results(91)["constructor"] == "0xdcb82ea3" &&
		reflect.DeepEqual(strList(results(91)["recent_voters"]), []string{"user:1000009"}) &&
		poll(92)["constructor"] == "0xaf746786" && poll(92)["quiz"] == true && results(92)["constructor"] == "0xbadcc1a3" &&
		reflect.DeepEqual(strList(results(92)["recent_voters"]), []string{"user:1000002"}) && results(92)["solution"] == "Since" &&
		poll(93)["constructor"] == "0xb8425be9" && poll(93)["closed"] == true && num(poll(93)["hash"]) == "78" &&
		results(93)["constructor"] == "0x5755785a" && num(results(93)["results_total"]) == "1" && num(results(93)["option_voters_total"]) == "0")
	// R94 (Review Focus 5): TL_messageMediaPoll_layer223 decodes whole; an empty option is "", an exact value
	a94 := stageBList(msg(94), "poll_answers")
	_, opt := a94[0]["option"]
	want("R94", msg(94).raw["data_state"] == "decoded" && msg(94).rawObj("media")["constructor"] == "0x4bd6e798" &&
		msg(94).rawObj("media")["type"] == "poll" && poll(94)["constructor"] == "0x966e2dbf" && opt && a94[0]["option"] == "" && results(94) != nil)
	// R95 (Q6, C4, R7): the mentioned user of each InputUser class; self and empty give none; never an access hash
	e95 := stageBList(msg(95), "entities")
	r95, _ := json.Marshal(msg(95).payload)
	want("R95", reflect.DeepEqual(field(e95, "user_class"), []string{"user", "user", "from_message", "from_message", "self", "empty"}) &&
		reflect.DeepEqual(field(e95, "user"), []string{"user:1000009", "user:1000010", "user:1000011", "user:1000012", "", ""}) &&
		e95[2]["user_from_peer"] == "user:1000002" && num(e95[2]["user_from_msg_id"]) == "55" &&
		e95[3]["user_from_peer"] == "channel:3000001" && num(e95[3]["user_from_msg_id"]) == "56" &&
		reflect.DeepEqual(field(e95, "user_access_hash_present"), []string{"true", "true", "true", "true", "", ""}) &&
		!strings.Contains(string(r95), strconv.FormatUint(secretI64Base+4401, 10)) && only(95))
	// R96: the peer of a from-message InputUser by its own id field, else its class label (its inner peer never followed)
	e96 := stageBList(msg(96), "entities")
	want("R96", len(e96) == 12 &&
		reflect.DeepEqual(field(e96, "user_from_peer"), []string{
			"chat:2000001", "", "channel:3000001", "channel:3000002", "chat:2000002",
			"user:1000003", "", "", "", "", "", "user:1000004",
		}) &&
		reflect.DeepEqual(field(e96, "user_from_peer_class"), []string{
			"", "user_from_message", "", "", "", "", "self", "empty",
			"channel_from_message", "user_from_message", "channel_from_message", "",
		}) &&
		e96[0]["user"] == "user:1000020" && e96[11]["user"] == "user:1000031" && only(96))
}

// stageBHostileFacts: plan 4I4c's H2901-H2912 (one fact per row of the plan's hostile row; H2905 by ruling T2a: an
// inline button in an older reply keyboard is read through the union, never stopped) and the cut rows H3001...
// (R83) and H3301... (R88): each truncated, no list or object of a field the cut lies in or before.
func stageBHostileFacts(want func(string, bool), h func(int64) rec, hostile map[int64]rec) {
	markup := func(n int64) map[string]any { return h(n).rawObj("reply_markup") }
	list := func(n int64, k string) []map[string]any { return stageBList(h(n), k) }
	want("H2901", h(2901).raw["data_stop_kind"] == "unknown-constructor" && h(2901).raw["data_stop_path"] == "reply_markup.rows[0].buttons[0]" &&
		h(2901).raw["data_stop_id"] == "0x0badb0b0" && h(2901).raw["reply_markup_state"] == "partial" && markup(2901) == nil &&
		h(2901).raw["reply_markup_buttons"] == nil)
	want("H2902", h(2902).raw["data_state"] == "cap" && h(2902).raw["data_stop_path"] == "media.results.recent_voters" &&
		h(2902).raw["poll_results_state"] == "partial" && h(2902).rawObj("poll_results") == nil && h(2902).rawObj("poll") != nil)
	want("H2903", h(2903).raw["data_state"] == "truncated" && h(2903).raw["data_stop_path"] == "media.poll.countries_iso2" &&
		h(2903).raw["poll_state"] == "partial" && h(2903).rawObj("poll") == nil)
	want("H2904", h(2904).raw["data_state"] == "truncated" && h(2904).raw["data_stop_path"] == "media.results.recent_voters" &&
		h(2904).raw["poll_results_state"] == "partial")
	want("H2905", h(2905).raw["data_state"] == "decoded" && markup(2905)["kind"] == "keyboard" &&
		reflect.DeepEqual(field(list(2905, "reply_markup_buttons"), "kind"), []string{"url"}) &&
		reflect.DeepEqual(field(list(2905, "reply_markup_buttons"), "constructor"), []string{"0x258aff05"}))
	want("H2906", len(list(2906, "reply_markup_buttons")) == 64 && num(markup(2906)["buttons_total"]) == "65" && markup(2906)["buttons_truncated"] == true)
	want("H2907", len(list(2907, "poll_answers")) == 32 && num(h(2907).rawObj("poll")["answers_total"]) == "33" && h(2907).rawObj("poll")["answers_truncated"] == true)
	want("H2908", h(2908).raw["data_stop_kind"] == "opaque" && h(2908).raw["data_stop_id"] == "0xd02e7fd4" && h(2908).raw["reply_markup_state"] == "partial")
	a09 := list(2909, "poll_answers")
	want("H2909", len(a09) == 1 && a09[0]["option_state"] == "too-long" && num(a09[0]["option_len"]) == "65" && a09[0]["option"] == nil && a09[0]["option_sha256"] != nil)
	b10 := list(2910, "reply_markup_buttons")
	want("H2910", len(b10) == 1 && b10[0]["peer_types"] == "pm,bot_pm,chat,megagroup,broadcast,same_bot_pm,pm,chat" && num(b10[0]["peer_types_total"]) == "9")
	r11 := h(2911).rawObj("poll_results")
	want("H2911", len(strList(r11["recent_voters"])) == 16 && num(r11["recent_voters_total"]) == "17" && r11["recent_voters_truncated"] == true)
	want("H2912", len(list(2912, "poll_voters")) == 64 && num(h(2912).rawObj("poll_results")["option_voters_total"]) == "65" &&
		h(2912).rawObj("poll_results")["option_voters_truncated"] == true)
	cut := func(lo, hi int64, check func(int64, rec, string)) int {
		n := 0
		for rowid := lo; rowid < hi; rowid++ {
			e, ok := hostile[rowid]
			if !ok {
				continue
			}
			n++
			stop, _ := e.raw["data_stop_path"].(string)
			want(fmt.Sprintf("H%d truncated", rowid), e.raw["data_state"] == "truncated")
			check(rowid, e, stop)
		}
		return n
	}
	under := func(stop, p string) bool { return strings.HasPrefix(stop, p+".") || strings.HasPrefix(stop, p+"[") }
	n83 := cut(3001, 3301, func(rowid int64, e rec, stop string) {
		tag := fmt.Sprintf("H%d (stop %q)", rowid, stop)
		want(tag+": no button of a cut markup", e.raw["reply_markup_buttons"] == nil)
		switch {
		case under(stop, "reply_markup"):
			want(tag+": partial", e.raw["reply_markup_state"] == "partial" && e.raw["reply_markup"] == nil)
		case stop == "reply_markup":
			want(tag+": the stop's state", e.rawObj("reply_markup")["state"] == "truncated")
		default:
			want(tag+": no markup key before the markup", e.raw["reply_markup"] == nil && e.raw["reply_markup_state"] == nil)
		}
	})
	n88 := cut(3301, 3601, func(rowid int64, e rec, stop string) {
		tag := fmt.Sprintf("H%d (stop %q)", rowid, stop)
		switch {
		case stop == "media.poll" || under(stop, "media.poll"):
			want(tag+": poll partial", e.raw["poll_state"] == "partial" && e.raw["poll"] == nil && e.raw["poll_answers"] == nil &&
				e.raw["poll_results"] == nil && e.raw["poll_voters"] == nil)
		case stop == "media.results" || under(stop, "media.results"):
			want(tag+": results partial", e.raw["poll"] != nil && e.raw["poll_results_state"] == "partial" && e.raw["poll_results"] == nil &&
				e.raw["poll_results_options"] == nil && e.raw["poll_voters"] == nil)
		case strings.HasPrefix(stop, "media.attached_media"):
			want(tag+": poll and results read whole", e.raw["poll"] != nil && e.raw["poll_results"] != nil)
		default:
			want(tag+": no poll key before the poll", e.raw["poll"] == nil && e.raw["poll_state"] == nil && e.raw["poll_results"] == nil)
		}
	})
	want(fmt.Sprintf("H3001 H3301 rows: %d and %d, want 44 and 80", n83, n88), n83 == 44 && n88 == 80)
}

// photoFieldOrder is TL_photo's fields in wire order (constructors.tsv, TLRPC.java:33327@A-HEAD), then the media
// wrapper's own after-photo fields: what a cut inside the photo leaves unread (Task 4 review M3).
var photoFieldOrder = []string{"has_stickers", "id", "access_hash", "file_reference", "date", "sizes", "video_sizes", "dc_id"}

// cutMediaKeys requires that the oracle's cut row states no media list or live-photo key whose source field starts at
// or after the cut, and states a list the cut lies inside as _state partial with no entry (plan 4I4b P3; Task 4 review
// M3). head is the top-level field of the stop (at its index in topFields, or -1), stop its full path.
func cutMediaKeys(want func(string, bool), rowid int64, e rec, stop, head string, headAt int) {
	mediaAt := -1
	for i, f := range topFields() {
		if f.name == "media" {
			mediaAt = i
		}
	}
	// reached: how much of the media the decode read; 0 none, 1 the sizes list, 2 the video sizes, 3 the live document
	sizesDone, videoDone, docDone := false, false, false
	partialSizes, partialVideo := false, false
	switch {
	case headAt > mediaAt && mediaAt >= 0:
		sizesDone, videoDone, docDone = true, true, true // the media was read whole
	case head == "media":
		rest := strings.TrimPrefix(stop, "media")
		rest = strings.TrimPrefix(rest, ".")
		if first, tail, _ := strings.Cut(rest, "."); first == "photo" {
			field, _, _ := strings.Cut(strings.SplitN(tail, "[", 2)[0], ".")
			at := slices.Index(photoFieldOrder, field)
			sizesDone = at > slices.Index(photoFieldOrder, "sizes")
			videoDone = at > slices.Index(photoFieldOrder, "video_sizes")
			partialSizes = field == "sizes"
			partialVideo = field == "video_sizes"
		} else if first == "ttl_seconds" || first == "document" {
			sizesDone, videoDone = true, true
			// the live document's id and mime_type are fields of media.document: read once the stop lies past them
			field, _, _ := strings.Cut(strings.SplitN(tail, "[", 2)[0], ".")
			docDone = first == "document" && field != "" && field != "id"
		}
	}
	tag := fmt.Sprintf("H%d (stop %q)", rowid, stop)
	if !sizesDone {
		want(tag+": media_sizes stated at or after the cut", e.raw["media_sizes"] == nil)
	}
	if !videoDone {
		want(tag+": media_video_sizes stated at or after the cut", e.raw["media_video_sizes"] == nil)
	}
	if !docDone {
		m, _ := e.raw["media"].(map[string]any)
		want(tag+": live_photo_document_id stated at or after the cut", m["live_photo_document_id"] == nil)
	}
	if partialSizes {
		want(tag+": media_sizes_state is not partial inside the list", e.raw["media_sizes_state"] == "partial")
	}
	if partialVideo {
		want(tag+": media_video_sizes_state is not partial inside the list", e.raw["media_video_sizes_state"] == "partial")
	}
}
