package androidtelegram_test

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// TestAndroidTelegramOracleStatesTheSpecRules: the oracle's literal facts, written from the spec (§8.4-§8.10, §9,
// §10, calls 4-6, T4, T7, the plan's state vocabularies and 4I4-P10/P11/P12, preflight F1, F5, F10, F18, F22, F32,
// F33 and the Task 5 ruling on R9/R59: TL_documentEncrypted reads a boxed PhotoSize the table lacks before
// attributes) and never read back from the generator, on the CORE scenario of telegram-v181 and the pre-84 rows of
// telegram-v64. A generator that drifts from the rules fails here, not silently in the pins.
func TestAndroidTelegramOracleStatesTheSpecRules(t *testing.T) {
	all := oracleRecords(t, "telegram-v181")
	v2 := all["messages_v2"]
	// F1: R1-R59 without R52, which lies only in an uncommitted WAL frame
	if len(v2) != 58 {
		t.Fatalf("%d messages_v2 records, want 58 (R1-R59 without the uncommitted R52)", len(v2))
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
	// spec 8.7: a conference-call action is not in the table: a system message, partial at the action
	want("R18 R19", msg(18).str("kind") == "system" && msg(18).raw["data_state"] == "partial" && msg(18).raw["data_stop_id"] == "0x2ffe2f7a" && msg(19).str("kind") == "system")
	// spec 8.5: messageEmpty claims nothing about deletion
	want("R20", msg(20).str("kind") == "unknown" && msg(20).raw["label_constructor"] == "messageEmpty" && !msg(20).r.Deleted)
	// spec 8.5 media summary
	want("R21", msg(21).str("kind") == "media" && msg(21).raw["data_state"] == "decoded" && msg(21).rawObj("media")["type"] == "photo" && num(msg(21).rawObj("media")["id"]) == "9001")
	want("R22", msg(22).raw["data_state"] == "partial" && msg(22).raw["data_stop_path"] == "media.photo.sizes[0]" && num(msg(22).rawObj("media")["id"]) == "9001" && msg(22).body() == "")
	want("R23", msg(23).att0() == att{mime: "application/pdf", size: "12345"} && msg(23).raw["data_state"] == "partial")
	want("R24 R25", num(msg(24).rawObj("media")["lat"]) == "52.52" && num(msg(24).rawObj("media")["long"]) == "13.405" && msg(25).rawObj("media")["lat_state"] == "out-of-range")
	want("R29", msg(29).str("kind") == "media" && msg(29).rawObj("media")["type"] == "dice" && msg(29).raw["data_stop_kind"] == "opaque")
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
	// F18
	want("U5 U6 U9", user(1000005).warned && user(1000006).warned && user(1000009).raw["data_state"] == "unknown-constructor")
	// Review Focus 3: a present but empty name part is absent
	want("U7", user(1000007).given() == "" && user(1000007).str("display_name") == "Solo")
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
}
