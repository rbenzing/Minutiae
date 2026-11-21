package androidtelegram_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

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
