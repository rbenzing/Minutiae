package androidtelegram_test

import (
	"context"
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
			map[string]any{"edit_date": int64(baseDate + 5), "edit_date_unit": "unestablished"},
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

	// 4I4-P6: the typed pass joins nothing
	typed := []stateRow{
		{
			"typed user",
			msgCells{mid: 1, uid: ann, data: in(1, peerUser(self), ann)},
			map[string]any{"thread_user_state": "not-joined-recovered", "account": nil},
			nil,
		},
		{
			"typed chat",
			msgCells{mid: 2, uid: -2000001, data: in(2, peerChat(2000001), ann)},
			map[string]any{"thread_title_state": "not-joined-recovered", "dialog_class": "chat", "chat_data_state": nil},
			map[string]any{"thread": map[string]any{"id": "-2000001", "group": true}},
		},
		{
			"typed secret", o(msgCells{mid: 3, uid: 1<<62 | 77, data: out(3, peerUser(ann))}),
			map[string]any{"counterparty_state": "not-joined-recovered", "secret_chat": true, "enc_chat_id": 77},
			map[string]any{"participants_unknown": true},
		},
	}
	db = goBuiltWith(181, cellsOf(typed), usersTable(ann), chatsTable(builtRow{2000001, map[string]any{"data": tlChat(2000001, "Family")}}),
		encChatsTable(builtRow{77, map[string]any{"user": int64(ann)}}))
	checkStates(t, "messages_v2", typed, typedPass(t, db))
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
