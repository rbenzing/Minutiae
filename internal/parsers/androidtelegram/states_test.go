package androidtelegram_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// unk is the raw of a key cell that holds no usable value: listed under unknown_columns and absent as a raw key
// (raw.date is the blob's own date: an unreadable date column gives no date_conflict instead).
func unk(col, kind string) map[string]any {
	raw := map[string]any{"unknown_columns": map[string]any{"messages_v2." + col: kind}}
	if col == "date" {
		raw["date_conflict"], raw["date_status"] = nil, nil
	} else {
		raw[col] = nil
	}
	return raw
}

// stateRow is one Go-built row and the values it must state: raw keys (nil: absent) and payload fields (nil: absent).
type stateRow struct {
	name    string
	cells   msgCells
	raw     map[string]any
	payload map[string]any
}

// The plan's "State vocabularies" (Task 8's keys; Tasks 9-12 and 14 extend this function): one Go-built row per value
// of every key the message mapping owns, each with its hard-coded expected value. Blob states (spec 8.4, 4I4-P10;
// decoded-prefix and budget: TestTelegramBlobStatesArePinned), direction and its conflict, the status labels, read,
// the date status (spec 10), the interim dialog class from the uid (F10), the cell states (spec 12) and the pre-84
// keys (spec 8.3, F32).
func TestTelegramStateVocabularyIsPinned(t *testing.T) {
	const ann, self = 1000002, 1000001
	in := func(id int32) []byte {
		return tlMessage{id: id, from: ann, peer: self, date: baseDate, text: "x"}.bytes()
	}
	out := func(id int32) []byte {
		return tlMessage{flags: bitOut, id: id, peer: ann, date: baseDate, text: "x"}.bytes()
	}
	v2 := []stateRow{
		{
			"decoded",
			msgCells{mid: 1, uid: ann, data: in(1)},
			map[string]any{"data_state": "decoded", "data_stop_kind": nil, "dialog_class": "user", "direction_conflict": nil},
			map[string]any{"direction": "in", "status": "received", "read": true, "kind": "text", "guid": "1"},
		},
		{
			"null blob",
			msgCells{mid: 2, uid: ann},
			map[string]any{"data_state": "null", "data_len": nil},
			map[string]any{"kind": "unknown"},
		},
		{
			"truncated",
			msgCells{mid: 3, uid: ann, data: in(3)[:10]},
			map[string]any{"data_state": "truncated", "data_stop_kind": "truncated", "data_stop_path": "flags2", "data_stop_offset": int64(8)},
			map[string]any{"kind": "unknown"},
		},
		{
			"unknown constructor",
			msgCells{mid: 4, uid: ann, data: append(le32(0x0badf00d), make([]byte, 8)...)},
			map[string]any{"data_state": "unknown-constructor", "data_stop_id": "0x0badf00d", "data_constructor": "0x0badf00d", "blob_flags": nil},
			map[string]any{"kind": "unknown"},
		},
		{
			"sending",
			msgCells{mid: 5, uid: ann, out: int64(1), read: int64(0), send: int64(1), data: out(5)},
			map[string]any{"label_send_state": "sending"},
			map[string]any{"direction": "out", "status": "queued", "read": nil},
		},
		{
			"editing",
			msgCells{mid: 6, uid: ann, out: int64(1), read: int64(0), send: int64(3), data: out(6)},
			map[string]any{"label_send_state": "editing"},
			map[string]any{"status": "unknown"},
		},
		{
			"failed",
			msgCells{mid: 7, uid: ann, out: int64(1), read: int64(0), send: int64(2), data: out(7)},
			map[string]any{"label_send_state": nil},
			map[string]any{"status": "failed"},
		},
		{
			"sent",
			msgCells{mid: 8, uid: ann, out: int64(1), read: int64(0), data: out(8)},
			nil,
			map[string]any{"status": "sent"},
		},
		{
			"out 7",
			msgCells{mid: 9, uid: ann, out: int64(7), data: in(9)},
			map[string]any{"out": int64(7), "direction_conflict": nil},
			map[string]any{"direction": "unknown", "status": "unknown", "read": nil},
		},
		{
			"read_state out of range",
			msgCells{mid: 10, uid: ann, read: int64(9), data: in(10)},
			map[string]any{"read_state_state": "out-of-range", "read_state": int64(9)},
			map[string]any{"read": nil},
		},
		{
			"date sentinel",
			msgCells{mid: 11, uid: ann, date: int64(0), data: in(11)},
			map[string]any{"date_status": "sentinel"},
			nil,
		},
		{
			"date invalid",
			msgCells{mid: 12, uid: ann, date: int64(5_000_000_000), data: in(12)},
			map[string]any{"date_status": "invalid"},
			nil,
		},
		{
			"uid NULL",
			msgCells{mid: 13, nullUID: true, data: in(13)},
			map[string]any{"thread_state": "undecided", "dialog_class": "unknown", "uid": nil, "unknown_columns": map[string]any{"messages_v2.uid": "null"}},
			map[string]any{"thread": nil},
		},
		{
			"mid NULL",
			msgCells{nullMid: true, uid: ann, data: in(14)},
			map[string]any{"guid_state": "undecided", "mid": nil, "unknown_columns": map[string]any{"messages_v2.mid": "null"}},
			map[string]any{"guid": nil},
		},
		// review M5: a REAL, NaN or TEXT key cell is listed in unknown_columns, never kept and never read as a value
		{"uid REAL", msgCells{mid: 22, uidCell: 1.5, data: in(22)}, unk("uid", "type-mismatch"), map[string]any{"thread": nil}},
		{"uid NaN", msgCells{mid: 23, uidCell: math.NaN(), data: in(23)}, unk("uid", "type-mismatch"), map[string]any{"thread": nil}},
		{"uid TEXT", msgCells{mid: 24, uidCell: "1000002", data: in(24)}, unk("uid", "type-mismatch"), map[string]any{"thread": nil}},
		{"mid REAL", msgCells{midCell: 25.5, uid: ann, data: in(25)}, unk("mid", "type-mismatch"), map[string]any{"guid": nil}},
		{"mid NaN", msgCells{midCell: math.NaN(), uid: ann, data: in(26)}, unk("mid", "type-mismatch"), map[string]any{"guid": nil}},
		{"mid TEXT", msgCells{midCell: "27", uid: ann, data: in(27)}, unk("mid", "type-mismatch"), map[string]any{"guid": nil}},
		{"date REAL", msgCells{mid: 28, uid: ann, date: float64(baseDate) + 0.5, data: in(28)}, unk("date", "type-mismatch"), nil},
		{"date NaN", msgCells{mid: 29, uid: ann, date: math.NaN(), data: in(29)}, unk("date", "type-mismatch"), nil},
		{"date TEXT", msgCells{mid: 30, uid: ann, date: "1700000000", data: in(30)}, unk("date", "type-mismatch"), nil},
		{"date NULL", msgCells{mid: 31, uid: ann, date: nullCell{}, data: in(31)}, unk("date", "null"), nil},
		{"out REAL", msgCells{mid: 32, uid: ann, out: 1.0, data: in(32)}, unk("out", "type-mismatch"), map[string]any{"direction": "unknown"}},
		{"out NaN", msgCells{mid: 33, uid: ann, out: math.NaN(), data: in(33)}, unk("out", "type-mismatch"), map[string]any{"direction": "unknown"}},
		{"out TEXT", msgCells{mid: 34, uid: ann, out: "0", data: in(34)}, unk("out", "type-mismatch"), map[string]any{"direction": "unknown"}},
		{"out NULL", msgCells{mid: 35, uid: ann, out: nullCell{}, data: in(35)}, unk("out", "null"), map[string]any{"direction": "unknown"}},
		{"secret", msgCells{mid: 15, uid: 1<<62 | 5, data: in(15)}, map[string]any{"dialog_class": "secret", "dialog_basis": nil}, nil},
		{"folder", msgCells{mid: 16, uid: 1<<61 | 1, data: in(16)}, map[string]any{"dialog_class": "folder"}, nil},
		{"negative", msgCells{mid: 17, uid: -2000001, data: in(17)}, map[string]any{"dialog_class": "negative"}, nil},
		{"uid 0", msgCells{mid: 18, uid: 0, data: in(18)}, map[string]any{"dialog_class": "unknown", "thread_state": nil}, nil},
		{
			"direction conflict",
			msgCells{mid: 19, uid: ann, data: tlMessage{flags: bitOut, id: 19, from: ann, peer: self, date: baseDate, text: "x"}.bytes()},
			map[string]any{"direction_conflict": true},
			map[string]any{"direction": "unknown"},
		},
		{
			"edit date",
			msgCells{mid: 20, uid: ann, data: tlMessage{id: 20, from: ann, peer: self, date: baseDate, editDate: baseDate + 5, text: "x"}.bytes()},
			map[string]any{"edit_date": int64(baseDate + 5), "edit_date_unit": "seconds", "edit_date_status": nil},
			nil,
		},
		// plan 4I4b Task 3 (design D9, rulings R5, B3): the date-status rule of the date column on edit_date
		{
			"edit date sentinel",
			msgCells{mid: 47, uid: ann, data: tlMessage{id: 47, from: ann, peer: self, date: baseDate, editSet: true, text: "x"}.bytes()},
			map[string]any{"edit_date": int64(0), "edit_date_unit": "seconds", "edit_date_status": "sentinel"},
			nil,
		},
		{
			"edit date invalid",
			msgCells{mid: 48, uid: ann, data: tlMessage{id: 48, from: ann, peer: self, date: baseDate, editDate: -100, text: "x"}.bytes()},
			map[string]any{"edit_date": int64(-100), "edit_date_unit": "seconds", "edit_date_status": "invalid"},
			nil,
		},
		{
			"id and date conflicts",
			msgCells{mid: 21, uid: ann, data: tlMessage{id: 999, from: ann, peer: self, date: baseDate + 1, text: "x"}.bytes()},
			map[string]any{"id": int64(999), "id_conflict": true, "date": int64(baseDate + 1), "date_conflict": true},
			nil,
		},
		// Task 9: the trailer (spec 8.8), the media coordinates (spec 8.5, 12) and the reply copy (spec 8.5)
		{"trailer none", msgCells{mid: 36, uid: ann, data: in(36)}, map[string]any{"trailer_state": "none"}, nil},
		{
			"trailer empty marker",
			msgCells{mid: 37, uid: ann, data: tlMessage{id: 37, from: ann, peer: self, date: baseDate, text: "x", tail: tlStr(" ")}.bytes()},
			map[string]any{"trailer_state": "read", "attach_path_state": "empty-marker", "attach_path": nil},
			nil,
		},
		{
			"trailer params cut",
			msgCells{mid: 38, uid: ann, data: tlMessage{id: 38, from: ann, peer: self, date: baseDate, text: "x", tail: tlStr(params(65) + "p")}.bytes()},
			map[string]any{"trailer_state": "read", "attach_params_total": 65, "attach_params_truncated": true, "attach_path": "p"},
			nil,
		},
		{
			"trailer path flags",
			msgCells{mid: 39, uid: ann, data: tlMessage{id: 39, from: ann, peer: self, date: baseDate, text: "x", tail: tlStr("a\x00b")}.bytes()},
			map[string]any{"trailer_state": "read", "attach_path": "a�b", "attach_path_flags": "nul", "attach_params_total": nil},
			nil,
		},
		{
			"trailer unparsed",
			msgCells{mid: 40, uid: ann, data: tlMessage{id: 40, from: ann, peer: self, date: baseDate, text: "x", tail: []byte{0xfe}}.bytes()},
			map[string]any{"trailer_state": "unparsed", "trailer_len": 1, "trailer_sha256": sha256Of([]byte{0xfe})},
			nil,
		},
		{
			"trailer unreachable",
			msgCells{mid: 41, uid: ann, data: in(41)[:30]},
			map[string]any{"data_state": "truncated", "trailer_state": "unreachable"},
			nil,
		},
		{
			"coordinates out of range",
			msgCells{mid: 42, uid: ann, data: tlMessage{id: 42, from: ann, peer: self, date: baseDate, media: geoMedia(math.Inf(1), math.NaN())}.bytes()},
			map[string]any{"media": map[string]any{
				"type": "geo", "constructor": "0x56e0d474", "lat_state": "out-of-range", "lat_bits": "0x7ff8000000000001",
				"long_state": "out-of-range", "long_bits": "0x7ff0000000000000",
			}},
			map[string]any{"kind": "media"},
		},
		{
			// review I1: a blob cut right before the media ID states the stop, never an ID that was not read
			"media ID cut",
			msgCells{mid: 45, uid: ann, data: cutBeforeMedia(45)},
			map[string]any{"data_state": "truncated", "data_stop_path": "media", "media": map[string]any{"state": "truncated"}},
			map[string]any{"kind": "unknown"},
		},
		{
			// review M2: a param cut at 1 KiB is named in raw.text_flags
			"trailer param cut",
			msgCells{mid: 46, uid: ann, data: tlMessage{id: 46, from: ann, peer: self, date: baseDate, text: "x", tail: tlStr("||k|=|" + strings.Repeat("v", 2048) + "||p")}.bytes()},
			map[string]any{"trailer_state": "read", "attach_params_total": 1, "text_flags": "attach_params", "attach_path": "p"},
			nil,
		},
		{
			"reply copy",
			msgCells{mid: 43, uid: ann, data: in(43), replydata: tlMessage{id: 9, from: ann, peer: self, date: baseDate - 1, text: "q\x00"}.bytes()},
			map[string]any{
				"replydata_state": "decoded", "reply_copy_state": nil, "text_flags": "reply_copy.text",
				"reply_copy": map[string]any{"copy": true, "constructor": "0x7600b9d3", "id": 9, "date": baseDate - 1, "text": "q�"},
			},
			nil,
		},
		{
			"reply copy undecided",
			msgCells{mid: 44, uid: ann, data: in(44), replydata: append(le32(0x0badf00d), 0, 0, 0, 0)},
			map[string]any{"replydata_state": "unknown-constructor", "reply_copy_state": "unknown-constructor", "reply_copy": nil},
			nil,
		},
	}
	var cells []msgCells
	for _, r := range v2 {
		cells = append(cells, r.cells)
	}
	checkStates(t, "messages_v2", v2, parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)

	msg := func(id int32) []byte {
		return tlMessage{id: id, from: ann, peer: self, date: baseDate, text: "x"}.bytes()
	}
	v1 := []stateRow{
		{
			"channel packing",
			msgCells{mid: 44 | 3000001<<32, uid: -3000001, data: msg(44)},
			map[string]any{"message_id": int64(44), "message_id_basis": "pre-84-channel-packing", "mid_state": nil, "id_conflict": nil},
			nil,
		},
		{
			"mid undecided",
			msgCells{mid: 45 | 3000002<<32, uid: -3000001, data: msg(45)},
			map[string]any{"mid_state": "undecided", "message_id": nil},
			nil,
		},
		{
			"high-id secret",
			msgCells{mid: -7, uid: 77 << 32, data: msg(-7)},
			map[string]any{"dialog_class": "secret", "enc_chat_id": int64(77), "dialog_basis": "pre-84-high-id"},
			nil,
		},
		{
			"forward peer ttl",
			msgCells{mid: 47, uid: ann, ttl: int64(-1000002), data: msg(47)},
			map[string]any{"ttl_state": "pre-84-forward-peer", "mid_state": nil},
			nil,
		},
	}
	cells = nil
	for _, r := range v1 {
		cells = append(cells, r.cells)
	}
	checkStates(t, "messages", v1, parseAt(t, remoteApp, dbOnly(goBuiltMessages(64, cells, nil))).Records)
	dialogStates(t)
	callStates(t)
	contactStates(t)
	mediaStates(t)
	tailVocabulary4I4b(t)
}

// callStates are Task 11's keys (spec 8.7, Q5; the plan's "Calls" vocabulary): the outcome per reason and direction,
// the labels, reason_state, address_state, the duration and its end, the video flag and a slug over its limit.
func callStates(t *testing.T) {
	const ann, self = 1000002, 1000001
	n := func(v int32) *int32 { return &v }
	in := func(id int32, c tlCall) []byte { return tlService(id, ann, peerUser(self), c.bytes()) }
	long := append(le32(0x9fbbf1f7), tlStr(strings.Repeat("x", 1100))...)
	rows := []stateRow{
		{
			"missed in",
			msgCells{mid: 1, uid: ann, data: in(1, tlCall{id: 1, reason: le32(idReasonMissed)})},
			map[string]any{"label_reason": nil, "reason": "0x85e42301", "reason_state": nil, "address_state": nil},
			map[string]any{"outcome": "missed", "direction": "in", "address": "user:1000002", "kind": "voice"},
		},
		{
			"busy in",
			msgCells{mid: 2, uid: ann, data: in(2, tlCall{id: 2, reason: le32(idReasonBusy)})},
			map[string]any{"label_reason": nil},
			map[string]any{"outcome": "rejected"},
		},
		{
			"busy out",
			msgCells{mid: 3, uid: ann, out: int64(1), read: int64(0), data: outgoing(tlService(3, 0, peerUser(ann), tlCall{id: 3, reason: le32(idReasonBusy)}.bytes()))},
			map[string]any{"label_reason": "busy"},
			map[string]any{"outcome": "unknown", "direction": "out"},
		},
		{
			"no reason",
			msgCells{mid: 4, uid: ann, data: in(4, tlCall{id: 4, duration: n(9)})},
			map[string]any{"reason_state": "not-recorded", "reason": nil, "label_reason": nil, "duration": int64(9), "time_end_basis": "start+duration", "video": nil, "flags": int64(2), "call_id": int64(4)},
			map[string]any{"outcome": "unknown", "duration_s": 9},
		},
		{
			"video",
			msgCells{mid: 5, uid: ann, data: in(5, tlCall{id: 5, video: true})},
			map[string]any{"video": true, "duration": nil, "time_end_basis": nil},
			map[string]any{"kind": "video", "duration_s": nil},
		},
		{
			"negative duration",
			msgCells{mid: 6, uid: ann, data: in(6, tlCall{id: 6, duration: n(-3)})},
			map[string]any{"duration": int64(-3), "time_end_basis": nil},
			map[string]any{"duration_s": nil},
		},
		{
			"chat dialog",
			msgCells{mid: 7, uid: -2000001, data: tlService(7, ann, peerChat(2000001), tlCall{id: 7, reason: le32(idReasonMissed)}.bytes())},
			map[string]any{"address_state": "not-user-dialog", "dialog_class": "chat"},
			map[string]any{"address": nil, "outcome": "missed"},
		},
		{
			"slug over its limit",
			msgCells{mid: 8, uid: ann, data: in(8, tlCall{id: 8, reason: long})},
			map[string]any{"slug_state": "too-long", "slug_len": int64(1100), "slug": nil, "label_reason": "migrate_conference_call"},
			nil,
		},
	}
	checkStates(t, "messages_v2", rows, parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cellsOf(rows)))).Records)
}

// dialogStates are Task 10's keys (spec 8.6, 9, 4I4-P6, P11, P12; the plan's "Dialog" and "Joins" vocabularies): the
// resolved classes and their conflict, the secret flag and key, the sender states, and every join state, live
// (users, chats and enc_chats present; a guarded chats and an absent enc_chats) and in the typed pass.
func dialogStates(t *testing.T) {
	const ann, self = 1000002, 1000001
	in := func(id int32, peer []byte, from int64) []byte {
		return tlMessage{id: id, from: from, peerObj: peer, date: baseDate, text: "x"}.bytes()
	}
	out := func(id int32, peer []byte) []byte {
		return tlMessage{flags: bitOut, id: id, peerObj: peer, date: baseDate, text: "x"}.bytes()
	}
	o := func(c msgCells) msgCells { c.out, c.read = int64(1), int64(0); return c }
	cut := tlMessage{id: 13, from: ann, peerObj: peerChat(2000001), date: baseDate, text: "x"}.bytes()
	post := tlMessage{flags: bitPost, id: 4, peerObj: peerChannel(3000001), date: baseDate, text: "x"}.bytes()
	rows := []stateRow{
		{
			"user with users row",
			msgCells{mid: 1, uid: ann, data: in(1, peerUser(self), ann)},
			map[string]any{"dialog_class": "user", "thread_user_state": nil, "peer_id": "user:1000001"},
			map[string]any{"thread": map[string]any{"id": "1000002", "group": false}},
		},
		{
			"user without users row",
			msgCells{mid: 2, uid: 1000099, data: in(2, peerUser(self), 1000099)},
			map[string]any{"thread_user_state": "no-users-row"},
			nil,
		},
		{
			"chat",
			msgCells{mid: 3, uid: -2000001, data: in(3, peerChat(2000001), ann)},
			map[string]any{"dialog_class": "chat", "thread_title_state": nil, "chat_data_state": "decoded-prefix", "peer_id": "chat:2000001"},
			map[string]any{
				"thread":       map[string]any{"id": "-2000001", "title": "Family", "group": true},
				"participants": []any{map[string]any{"role": "from", "address": "user:1000002"}},
			},
		},
		{
			"channel post",
			msgCells{mid: 4, uid: -3000001, data: post},
			map[string]any{"dialog_class": "channel", "sender_basis": "channel-post", "sender_state": nil},
			map[string]any{"participants": []any{map[string]any{"role": "from", "address": "channel:3000001"}}},
		},
		{
			"class conflict",
			msgCells{mid: 5, uid: -3000003, data: in(5, peerChat(3000003), ann)},
			map[string]any{"dialog_class": "negative", "dialog_class_state": "conflict", "thread_title_state": "class-mismatch"},
			map[string]any{"thread": map[string]any{"id": "-3000003"}},
		},
		{
			"no chats row",
			msgCells{mid: 6, uid: -2000009, data: in(6, peerChat(2000009), ann)},
			map[string]any{"dialog_class": "chat", "thread_title_state": "no-chats-row", "chat_data_state": nil},
			nil,
		},
		{
			"chats blob NULL",
			msgCells{mid: 7, uid: -2000010, data: in(7, peerChat(2000010), ann)},
			map[string]any{"dialog_class": "chat", "thread_title_state": "blob-undecided"},
			nil,
		},
		{
			"sender absent",
			msgCells{mid: 8, uid: -2000001, data: in(8, peerChat(2000001), 0)},
			map[string]any{"sender_state": "absent"},
			map[string]any{"participants_unknown": true},
		},
		{
			"secret without enc_chats row",
			msgCells{mid: 9, uid: 1<<62 | 9, data: in(9, peerUser(self), ann)},
			map[string]any{"secret_chat": true, "dialog_class": "secret", "enc_chat_id": 9, "counterparty_state": "no-enc_chats-row"},
			map[string]any{"thread": map[string]any{"id": "4611686018427387913", "group": false}},
		},
		{
			"secret zero key", o(msgCells{mid: 10, uid: 1 << 62, data: out(10, peerUser(ann))}),
			map[string]any{"enc_chat_id": 0, "counterparty_state": "zero-key"},
			map[string]any{"participants_unknown": true},
		},
		{
			"secret counterparty", o(msgCells{mid: 11, uid: 1<<62 | 77, data: out(11, peerUser(ann))}),
			map[string]any{"enc_chat_id": 77, "counterparty_state": nil},
			map[string]any{"participants": []any{map[string]any{"role": "to", "address": "user:1000002"}}},
		},
		{
			"secret user NULL", o(msgCells{mid: 12, uid: 1<<62 | 78, data: out(12, peerUser(ann))}),
			map[string]any{"counterparty_state": nil, "unknown_columns": map[string]any{"enc_chats.user": "null"}},
			map[string]any{"participants_unknown": true},
		},
		{
			"sender blob-undecided",
			msgCells{mid: 13, uid: -2000001, data: cut[:20]},
			map[string]any{"sender_state": "blob-undecided", "data_state": "truncated"},
			map[string]any{"participants_unknown": true},
		},
		{
			// M5: a negative dialog left unresolved keeps the decoded sender in raw, never as a participant
			"unresolved sender",
			msgCells{mid: 15, uid: -2000020, data: in(15, peerUser(self), ann)},
			map[string]any{"dialog_class": "negative", "dialog_class_state": "conflict", "from_id": "user:1000002", "sender_state": nil},
			map[string]any{"participants_unknown": true, "participants": []any{}},
		},
		{
			"folder",
			msgCells{mid: 14, uid: 1<<61 | 1, data: in(14, peerUser(self), ann)},
			map[string]any{"dialog_class": "folder", "sender_state": nil},
			map[string]any{"participants_unknown": true, "thread": map[string]any{"id": "2305843009213693953"}},
		},
	}
	db := goBuiltWith(181, cellsOf(rows), usersTable(ann),
		chatsTable(
			builtRow{2000001, map[string]any{"data": tlChat(2000001, "Family")}},
			builtRow{3000001, map[string]any{"data": tlChannel(3000001, "News")}},
			builtRow{3000003, map[string]any{"data": tlChannel(3000003, "Mismatch")}},
			builtRow{2000010, map[string]any{"name": "null blob"}},
		),
		encChatsTable(builtRow{77, map[string]any{"user": int64(ann)}}, builtRow{78, map[string]any{}}))
	checkStates(t, "messages_v2", rows, parseAt(t, remoteApp, dbOnly(db)).Records)

	// a chats table whose uid is not the rowid alias is guarded, an enc_chats table the database lacks is absent:
	// both joins are undecided, never a miss
	undecided := []stateRow{
		{
			"chats guarded",
			msgCells{mid: 1, uid: -2000001, data: in(1, peerChat(2000001), ann)},
			map[string]any{"thread_title_state": "undecided", "join_undecided": []any{"chats"}, "dialog_class": "chat"},
			nil,
		},
		{
			"enc_chats absent", o(msgCells{mid: 2, uid: 1<<62 | 77, data: out(2, peerUser(ann))}),
			map[string]any{"counterparty_state": "undecided", "join_undecided": []any{"enc_chats"}},
			map[string]any{"participants_unknown": true},
		},
	}
	guarded := builtTable{
		"chats", "CREATE TABLE chats (uid INTEGER, name TEXT, data BLOB)",
		[]string{"uid", "name", "data"},
		"",
		[]builtRow{{1, map[string]any{"uid": int64(2000001), "data": tlChat(2000001, "Family")}}},
	}
	checkStates(t, "messages_v2", undecided, parseAt(t, remoteApp, dbOnly(goBuiltWith(181, cellsOf(undecided), guarded))).Records)

	// 4I4-P6: the typed pass joins nothing; ruling T2 (Task 14): it states each row's own (mid, uid) against the live
	// rows of its table (these rows are themselves live: present; a key cell that is not an integer: undecided)
	typed := []stateRow{
		{
			"typed user",
			msgCells{mid: 1, uid: ann, data: in(1, peerUser(self), ann)},
			map[string]any{"thread_user_state": "not-joined-recovered", "account": nil, "live_key_state": "present"},
			nil,
		},
		{
			"typed chat",
			msgCells{mid: 2, uid: -2000001, data: in(2, peerChat(2000001), ann)},
			map[string]any{"thread_title_state": "not-joined-recovered", "dialog_class": "chat", "chat_data_state": nil, "live_key_state": "present"},
			map[string]any{"thread": map[string]any{"id": "-2000001", "group": true}},
		},
		{
			"typed secret", o(msgCells{mid: 3, uid: 1<<62 | 77, data: out(3, peerUser(ann))}),
			map[string]any{"counterparty_state": "not-joined-recovered", "secret_chat": true, "enc_chat_id": 77, "live_key_state": "present"},
			map[string]any{"participants_unknown": true},
		},
		{
			"typed real mid",
			msgCells{midCell: 4.5, uid: ann, data: in(4, peerUser(self), ann)},
			map[string]any{"live_key_state": "undecided", "thread_user_state": "not-joined-recovered"},
			map[string]any{"guid": nil},
		},
		{
			"typed null uid",
			msgCells{mid: 5, nullUID: true, data: in(5, peerUser(self), ann)},
			map[string]any{"live_key_state": "undecided", "thread_state": "undecided"},
			nil,
		},
	}
	db = goBuiltWith(181, cellsOf(typed), usersTable(ann), chatsTable(builtRow{2000001, map[string]any{"data": tlChat(2000001, "Family")}}),
		encChatsTable(builtRow{77, map[string]any{"user": int64(ann)}}))
	checkStates(t, "messages_v2", typed, typedPass(t, db))

	// rulings T2, T9 in the old generation: messages is keyed by its mid alone, the rowid alias, which the lookup
	// matches as stored; a row whose uid cannot be read is still present by its mid
	typedV1 := []stateRow{
		{
			"typed v1",
			msgCells{mid: 7, uid: ann, data: in(7, peerUser(self), ann)},
			map[string]any{"live_key_state": "present", "thread_user_state": "not-joined-recovered"},
			map[string]any{"guid": "7"},
		},
		{
			"typed v1 null uid",
			msgCells{mid: 8, nullUID: true, data: in(8, peerUser(self), ann)},
			map[string]any{"live_key_state": "present", "thread_state": "undecided"},
			map[string]any{"guid": "8"},
		},
	}
	checkStates(t, "messages", typedV1, typedPass(t, goBuiltMessages(64, cellsOf(typedV1), nil)))
}

// cellsOf are the cells of the state rows, in order.
func cellsOf(rows []stateRow) []msgCells {
	var out []msgCells
	for _, r := range rows {
		out = append(out, r.cells)
	}
	return out
}

// typedPass maps every row of the message tables of db through the typed pass's mappings (Parser.Mappings, a nil
// env, 4I4-P6) with rowrun.Run, as the recovery host does for a recovered row: no join runs.
func typedPass(t *testing.T, db []byte) []records.Record {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	in := inputAt(t, h, p, remoteApp, dbOnly(db), false)
	var steps []rowrun.Step
	for _, m := range p.Mappings() {
		steps = append(steps, rowrun.Step{Mapping: m, Optional: true})
	}
	e := &recordSink{}
	if _, err := rowrun.Run(t.Context(), in, e, steps); err != nil {
		t.Fatal(err)
	}
	return e.recs
}

// recordSink is an emitter that keeps the records.
type recordSink struct{ recs []records.Record }

func (e *recordSink) Emit(_ context.Context, r records.Record) error {
	e.recs = append(e.recs, r)
	return nil
}
func (*recordSink) Warn(context.Context, string, string) error { return nil }
func (*recordSink) Note(string, string)                        {}
func (*recordSink) Progress(int64, int64)                      {}

// cutBeforeMedia is a TL_message with the media flag set whose blob ends right before the media constructor ID.
func cutBeforeMedia(id int32) []byte {
	full := tlMessage{id: id, from: 1000002, peer: 1000001, date: baseDate, text: "x", media: le32(0x3ded6320)}.bytes()
	return full[:len(full)-4]
}

// params is the params prefix of a trailer string with n pairs k<i>|=|v (spec 6.5).
func params(n int) string {
	var sb strings.Builder
	sb.WriteString("||")
	for i := range n {
		sb.WriteString("k" + strconv.Itoa(i) + "|=|v||")
	}
	return sb.String()
}

// checkStates matches the records of one Go-built table to its rows (messages_v2 by position, messages by mid).
func checkStates(t *testing.T, table string, rows []stateRow, recs []records.Record) {
	t.Helper()
	var mine []records.Record
	for _, r := range recs {
		if strings.HasPrefix(locatorRow(r.Locator), table+"/") {
			mine = append(mine, r)
		}
	}
	recs = mine
	got := byRow(recs)
	if len(recs) != len(rows) {
		t.Errorf("%s: %d records for %d rows", table, len(recs), len(rows))
	}
	for i, row := range rows {
		key := table + "/" + itoa(int64(i+1))
		if table == "messages" {
			key = table + "/" + itoa(row.cells.mid)
		}
		r, ok := got[key]
		if !ok {
			t.Errorf("%s (%s): no record", row.name, key)
			continue
		}
		if s, _ := rawOf(r)["source_table"].(string); s != table {
			t.Errorf("%s: raw.source_table %q", row.name, s)
		}
		for k, want := range row.raw {
			v, has := rawOf(r)[k]
			if (want == nil && has) || (want != nil && jsonText(v) != jsonText(want)) {
				t.Errorf("%s: raw.%s = %v (present %v), want %v", row.name, k, v, has, want)
			}
		}
		for k, want := range row.payload {
			v, has := r.Payload[k]
			if (want == nil && has) || (want != nil && jsonText(v) != jsonText(want)) {
				t.Errorf("%s: %s = %v (present %v), want %v", row.name, k, v, has, want)
			}
		}
	}
}

// tlUser is a TL_user (0xb1b8cc83) blob: flags (set from the fields and extra), flags2 0, id, access_hash when hash,
// then the name, username and phone strings that are non-nil (constructors.tsv field order, flag bits 1-4).
func tlUser(id int64, extra uint32, hash bool, first, last, username, phone *string) []byte {
	flags := extra
	var body []byte
	if hash {
		flags |= 1
		body = append(body, le64(7)...)
	}
	for i, p := range []*string{first, last, username, phone} {
		if p != nil {
			flags |= 1 << (i + 1)
			body = append(body, tlStr(*p)...)
		}
	}
	b := append(append(le32(0xb1b8cc83), le32(flags)...), le32(0)...)
	return append(append(b, le64(id)...), body...)
}

// contactStates are Task 12's keys (spec 8.9, T4; the plan's "Contacts" vocabulary): the status label and expiry, a
// NULL status and blob, a status of another class, the contact join and its mutual flag, the about join and its
// state, a phone over its limit, the self and bot flags, and an empty name part.
func contactStates(t *testing.T) {
	s := func(v string) *string { return &v }
	const bitSelf, bitBot = 1 << 10, 1 << 14
	tooLong := strings.Repeat("9", 1100)
	row := func(uid int64, status, data any) builtRow {
		return builtRow{uid, map[string]any{"name": "x", "status": status, "data": data}}
	}
	users := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: []builtRow{
		row(1, int64(-102), tlUser(1, 0, false, s("A"), nil, nil, nil)),
		row(2, int64(5000), tlUser(2, 0, true, s("B"), s("C"), s("bc"), s("555"))),
		row(3, nil, nil),
		row(4, "x", tlUser(4, 0, false, s("D"), nil, nil, nil)),
		row(5, int64(0), tlUser(5, 0, false, s("E"), nil, nil, s(tooLong))),
		row(6, int64(-1000), tlUser(6, bitSelf|bitBot, false, s(""), s("F"), nil, nil)),
	}}
	contacts := builtTable{
		name: "contacts", ddl: "CREATE TABLE contacts (uid INTEGER PRIMARY KEY, mutual INTEGER)", cols: []string{"uid", "mutual"},
		alias: "uid", rows: []builtRow{{2, map[string]any{"mutual": int64(1)}}},
	}
	about := builtTable{
		name: "users_data", ddl: ddlUsersData, cols: []string{"uid", "about"}, alias: "uid",
		rows: []builtRow{{2, map[string]any{"about": "hi"}}},
	}
	res := parseAt(t, remoteApp, dbOnly(goBuiltWith(181, nil, users, contacts, about)))
	got := byRow(res.Records)
	want := map[string]map[string]any{
		"users/1": {"status": int64(-102), "status_label": "last_month", "status_expires": nil, "contact_row": false, "about_state": "no-users_data-row", "data_state": "decoded-prefix"},
		"users/2": {"status_expires": int64(5000), "status_label": nil, "contact_row": true, "mutual": int64(1), "about_state": nil, "access_hash_present": true},
		"users/3": {"status": nil, "data_state": "null", "data_len": nil},
		"users/4": {"status": nil, "unknown_columns": map[string]any{"users.status": "type-mismatch"}},
		"users/5": {"phone_state": "too-long", "phone_len": int64(1100), "phone_sha256": sha256Hex(tooLong)},
		"users/6": {"self": true, "bot": true, "status_label": "recently_by_me", "display_name_source": "composed"},
	}
	for key, w := range want {
		r, ok := got[key]
		if !ok {
			t.Errorf("%s: no record", key)
			continue
		}
		for k, v := range w {
			g, has := rawOf(r)[k]
			if (v == nil && has && k != "status") || (v != nil && jsonText(g) != jsonText(v)) {
				t.Errorf("%s: raw.%s = %v (present %v), want %v", key, k, g, has, v)
			}
		}
	}
	if g, has := rawOf(got["users/3"])["status"]; !has || g != nil {
		t.Errorf("users/3: a stored NULL status is raw nil, got %v (present %v)", g, has)
	}
	if _, has := got["users/5"].Payload["phones"]; has {
		t.Error("users/5: a phone over its limit is never cut or shown")
	}
	if got["users/6"].Payload["display_name"] != "F" {
		t.Errorf("users/6: display_name %v, want F (empty first name absent)", got["users/6"].Payload["display_name"])
	}
	if res.Notes["self_users"] != "1" {
		t.Errorf("self_users %q, want 1", res.Notes["self_users"])
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// attrFilename is a TL_documentAttributeFilename.
func attrFilename(name string) []byte { return append(le32(0x15590068), tlStr(name)...) }

// mediaStates are plan 4I4b Task 5's keys (D10, R6, B6, P3, P5; the plan's "Raw shapes"): the four file_name_state
// words, media_sizes_state, the duration state, the conflict state of a value two attributes disagree on, and the nine
// size kinds.
func mediaStates(t *testing.T) {
	media := func(m []byte) map[string]any {
		t.Helper()
		got, _ := rawOf(goMediaRow(t, m))["media"].(map[string]any)
		return got
	}
	cutDoc := goDocument(attrFilename("a.txt"), attrImageSize(1, 2))
	for _, c := range []struct {
		name  string
		media []byte
		want  map[string]any
	}{
		{"absent", goDocument(attrVideo(3, 4, 5)), map[string]any{"file_name_state": "absent", "file_name": nil}},
		{"empty", goDocument(attrFilename("")), map[string]any{"file_name_state": "empty", "file_name": nil}},
		{"conflict", goDocument(attrFilename("a"), attrFilename("b")), map[string]any{"file_name_state": "conflict", "file_names": []string{"a", "b"}, "file_names_total": 2, "file_name": nil}},
		{"undecided", cutDoc[:len(cutDoc)-4], map[string]any{"file_name_state": "undecided", "file_name": nil, "attributes_state": "partial", "attributes": nil}},
		{"one name", goDocument(attrFilename("a"), attrFilename("a")), map[string]any{"file_name": "a", "file_name_state": nil}},
		{"duration NaN", goDocument(attrVideo(math.NaN(), 4, 5)), map[string]any{"duration_state": "out-of-range", "duration_bits": "0x7ff8000000000001", "duration": nil}},
		{"values conflict", goDocument(attrVideo(3, 4, 5), attrImageSize(6, 7)), map[string]any{"w_state": "conflict", "h_state": "conflict", "w": nil, "h": nil}},
	} {
		wantKeys(t, c.name, media(c.media), c.want)
	}
	if got := rawOf(goMediaRow(t, goPhoto([][]byte{photoSize("m", 1, 2, 3), photoSize("x", 4, 5, 6)}, 20)))["media_sizes_state"]; got != "partial" {
		t.Errorf("a photo cut in its sizes: media_sizes_state %v, want partial", got)
	}

	kinds := map[string]bool{}
	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		for _, r := range res.Records { // a refused variant (a user version outside the band) has none
			for _, key := range []string{"media_sizes", "media_video_sizes", "media_thumbs", "media_video_thumbs"} {
				for _, e := range listOf(t, rawOf(r), key) {
					k, _ := e["kind"].(string)
					kinds[k] = true
				}
			}
		}
	}
	for _, k := range []string{"size", "cached", "stripped", "path", "progressive", "empty", "video", "emoji_markup", "sticker_markup"} {
		if !kinds[k] {
			t.Errorf("no committed record states a %q size entry", k)
		}
		delete(kinds, k)
	}
	if len(kinds) != 0 {
		t.Errorf("size kinds beyond the nine of the plan: %v", kinds)
	}
}
