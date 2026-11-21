package androidgooglemessages_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The plan's state vocabularies (B8), the keys and state words Task 4 owns: one Go-built row per value, each asserted with
// a hard-coded expected value (a nil want means the key is absent), and the B8 row of docs/invariants/parsers.md must
// list every one of them. Tasks 6 and 8 extend this function.
func TestGoogleMessagesStateVocabularyIsPinned(t *testing.T) {
	longURI := "content://sms/" + strings.Repeat("7", 1100)
	longLoc := strings.Repeat("l", 5000)
	cases := []struct {
		id  int64
		kw  map[string]any
		raw map[string]any // raw key -> value
		pay map[string]any // payload key -> value
	}{
		// codes (spec 6.1, 6.2, 4I5-P4)
		{
			1,
			map[string]any{"message_protocol": int64(2), "message_status": int64(2)},
			map[string]any{"message_protocol": int64(2), "label_message_protocol": "mms_push_notification", "message_status": int64(2), "label_message_status": "outgoing_delivered", "message_protocol_state": nil, "message_status_state": nil},
			map[string]any{"channel": "mms", "direction": "out", "status": "delivered"},
		},
		{
			2,
			map[string]any{"message_protocol": nil, "message_status": nil},
			map[string]any{"message_protocol_state": "null", "message_status_state": "null", "message_protocol": nil, "message_status": nil, "label_message_protocol": nil, "label_message_status": nil},
			map[string]any{"channel": "unknown", "direction": "unknown", "status": "unknown"},
		},
		{
			3,
			map[string]any{"message_protocol": int64(0), "message_status": int64(100)},
			map[string]any{"label_message_protocol": "sms", "label_message_status": "incoming_complete"},
			map[string]any{"channel": "sms", "direction": "in", "status": "received"},
		},
		// times (spec 6.6, 4I5-P2)
		{
			4,
			map[string]any{"received_timestamp": int64(0), "sent_timestamp": int64(0)},
			map[string]any{"received_timestamp": int64(0), "received_timestamp_status": "sentinel", "sent_timestamp": int64(0), "sent_timestamp_status": "sentinel", "time_basis": "absent"},
			nil,
		},
		{
			5,
			map[string]any{"received_timestamp": int64(50)},
			map[string]any{"received_timestamp": int64(50), "received_timestamp_status": "invalid", "time_basis": "invalid"},
			nil,
		},
		{
			6,
			map[string]any{"received_timestamp": int64(0)},
			map[string]any{"received_timestamp_status": "sentinel", "sent_timestamp_status": nil, "time_basis": "sent"},
			nil,
		},
		{
			7,
			map[string]any{"received_timestamp": "x"},
			map[string]any{"received_timestamp": nil, "received_timestamp_status": nil, "time_basis": "undecided", "unknown_columns": map[string]any{"received_timestamp": "type-mismatch"}},
			nil,
		},
		{
			8, nil,
			map[string]any{"received_timestamp": tT + 1000, "received_timestamp_status": nil, "sent_timestamp": tT, "time_basis": "received"},
			nil,
		},
		{
			9,
			map[string]any{"sent_timestamp": int64(50), "mms_expiry": int64(50), "retry_start_timestamp": int64(123)},
			map[string]any{"sent_timestamp_status": "invalid", "mms_expiry": int64(50), "mms_expiry_status": "invalid", "retry_start_timestamp": int64(123), "retry_start_timestamp_unit": "unestablished"},
			nil,
		},
		{
			10,
			map[string]any{"mms_expiry": tT},
			map[string]any{"mms_expiry": tT, "mms_expiry_status": nil, "retry_start_timestamp": int64(0), "retry_start_timestamp_unit": "unestablished"},
			nil,
		},
		// as stored (spec 6.7, 6.8)
		{
			11,
			map[string]any{"seen": int64(1), "read": int64(2), "sms_priority": int64(3), "sms_message_size": int64(9), "raw_status": int64(4)},
			map[string]any{"seen": int64(1), "read": int64(2), "sms_priority": int64(3), "sms_message_size": int64(9), "raw_status": int64(4)},
			map[string]any{"read": nil},
		},
		{
			12,
			map[string]any{"read": []byte{1, 2}},
			map[string]any{"read": nil, "unknown_columns": map[string]any{"read": "type-mismatch"}},
			map[string]any{"read": nil},
		},
		{
			13,
			map[string]any{"mms_transaction_id": "tx\xff", "mms_content_location": longLoc},
			map[string]any{"mms_transaction_id": "tx�", "mms_content_location": strings.Repeat("l", 4096), "text_flags": []any{"mms_content_location", "mms_transaction_id"}},
			nil,
		},
		// the telephony link (spec 6.8, C2)
		{
			14,
			map[string]any{"sms_message_uri": longURI},
			map[string]any{"sms_message_uri_state": "too-long", "sms_message_uri_len": int64(len(longURI)), "sms_message_uri_sha256": sha([]byte(longURI)), "sms_message_uri": nil, "sms_link": nil},
			nil,
		},
		{
			15,
			map[string]any{"sms_message_uri": "content://sms/41"},
			map[string]any{"sms_message_uri": "content://sms/41", "sms_link": map[string]any{"authority": "sms", "id": int64(41)}, "sms_link_id_reuse": "possible", "sms_link_state": nil},
			nil,
		},
		{
			16,
			map[string]any{"sms_message_uri": "content://mms/7"},
			map[string]any{"sms_link": map[string]any{"authority": "mms", "id": int64(7)}, "sms_link_id_reuse": "none"},
			nil,
		},
		{
			17,
			map[string]any{"sms_message_uri": "content://sms/x"},
			map[string]any{"sms_message_uri": "content://sms/x", "sms_link_state": "unparsed", "sms_link": nil, "sms_link_id_reuse": nil},
			nil,
		},
		// an intact row states no decoder damage and no flag it does not have
		{18, nil, map[string]any{"row_flags": nil, "unknown_columns": nil, "text_flags": nil, "sms_message_uri": nil, "mms_transaction_id": nil}, nil},
	}
	var rows []mrow
	for _, c := range cases {
		rows = append(rows, mrow{c.id, c.kw})
	}
	// row 90: a record whose body is longer than its header says (the decoder's length_mismatch)
	damaged := func(tb *sqlitetest.Table) { tb.InsertRaw(90, make([]uint64, 19), []byte{'Z'}) }
	_, got := parsedRows(t, rows, damaged)
	for _, c := range cases {
		r, ok := got[c.id]
		if !ok {
			t.Fatalf("row %d: no record", c.id)
		}
		raw := rawOf(r)
		for k, w := range c.raw {
			g, has := raw[k]
			switch {
			case w == nil && has:
				t.Errorf("row %d: raw.%s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("row %d: raw.%s = %#v, want %#v", c.id, k, g, w)
			}
		}
		for k, w := range c.pay {
			g, has := r.Payload[k]
			switch {
			case w == nil && has:
				t.Errorf("row %d: %s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("row %d: %s = %#v, want %#v", c.id, k, g, w)
			}
		}
	}
	if f, _ := rawOf(got[90])["row_flags"].([]any); !reflect.DeepEqual(f, []any{"length_mismatch"}) {
		t.Errorf("row 90: raw.row_flags %v, want [length_mismatch] (raw %v)", f, rawOf(got[90]))
	}
	// the parties, the self, the conversation and the members (Task 5; spec 6.2 to 6.3, 7, B2, B3, 4I5-P3, P5, P9)
	_, parties := store{
		participants: []mrow{
			{1, map[string]any{"sub_id": int64(1), "sim_slot_id": int64(0), "subscription_name": "SIM One"}},
			{2, map[string]any{"normalized_destination": "+15550000002", "send_destination": "5550000002", "display_destination": "(555) 000-0002", "full_name": "Ann", "contact_id": int64(7)}},
			{3, map[string]any{"normalized_destination": "+15550000003", "blocked": int64(1)}},
			{4, map[string]any{"send_destination": "", "normalized_destination": ""}},
			{5, map[string]any{"sub_id": nil, "normalized_destination": "+15550000005"}},
		},
		conversations: []mrow{
			{1, map[string]any{"name": "Team", "sms_thread_id": int64(11), "archive_status": int64(1), "participant_count": int64(3), "sort_timestamp": tT + 5000, "last_read_timestamp": int64(0), "IS_ENTERPRISE": int64(1)}},
			{2, map[string]any{}},
			{3, map[string]any{}},
			{7, map[string]any{}},
		},
		members: append(append(memberRows(1, 1, 2, 3, 4, 999), memberRows(2, 5, 2)...), mrow{0, map[string]any{"conversation_id": int64(7), "participant_id": nil}}),
		messages: []mrow{
			{1, nil},
			{2, map[string]any{"sender_id": int64(1), "message_status": int64(1)}},
			{3, map[string]any{"sender_id": nil, "self_id": nil, "conversation_id": int64(2)}},
			{4, map[string]any{"sender_id": int64(99), "self_id": int64(98), "conversation_id": int64(3)}},
			{5, map[string]any{"sender_id": int64(5), "self_id": "x", "conversation_id": nil}},
			{6, map[string]any{"sender_id": int64(1), "conversation_id": int64(9)}},
			{7, map[string]any{"sender_id": "x", "conversation_id": int64(7)}},
		},
	}.parse(t)
	for _, c := range []struct {
		id  int64
		raw map[string]any
		pay map[string]any
	}{
		{1, map[string]any{
			"sender_id": int64(2), "sender_is_self": false, "from_address_basis": "send_destination", "from_name_basis": "cached_contact",
			"from_destinations": map[string]any{"normalized_destination": "+15550000002", "send_destination": "5550000002", "display_destination": "(555) 000-0002"},
			"from_full_name":    "Ann", "from_contact_id": int64(7), "self_id": int64(1), "self_sub_id": int64(1), "self_sim_slot_id": int64(0), "self_subscription_name": "SIM One",
			"conversation_id": int64(1), "sms_thread_id": int64(11), "archive_status": int64(1), "participant_count": int64(3), "is_enterprise": int64(1),
			"sort_timestamp": tT + 5000, "last_read_timestamp": int64(0), "last_read_timestamp_status": "sentinel", "sort_timestamp_status": nil,
			"members_total": int64(3), "members_dangling": int64(1), "members_without_address": int64(1), "blocked_member_ids": []any{int64(3)},
			"members_basis": nil, "members_cut": nil, "members_state": nil, "self_state": nil, "group_state": nil, "sender_state": nil, "direction_conflict": nil,
			"join_undecided": nil, "conversation_state": nil, "self_row_state": nil,
		}, map[string]any{"subscription": int64(1), "thread": map[string]any{"id": "1", "title": "Team", "group": true, "members": []any{"5550000002", "+15550000003"}}}},
		{2, map[string]any{"sender_is_self": true, "members_basis": "current", "sender_state": nil}, map[string]any{"direction": "out"}},
		{3, map[string]any{
			"sender_state": "absent", "self_row_state": "absent", "self_state": "undecided", "group_state": "undecided", "sender_id": nil, "self_id": nil, "sender_is_self": nil,
		}, map[string]any{"thread": map[string]any{"id": "2", "members": []any{"+15550000005", "5550000002"}}, "subscription": nil, "participants_unknown": true}},
		{
			4,
			map[string]any{"sender_state": "no-participants-row", "self_row_state": "no-participants-row", "members_state": "none", "sender_id": int64(99), "self_id": int64(98), "members_total": int64(0)},
			map[string]any{"thread": map[string]any{"id": "3", "group": false}},
		},
		{5, map[string]any{
			"sender_state": "self-undecided", "conversation_state": "null", "self_row_state": "undecided", "sender_is_self": nil, "conversation_id": nil,
			"unknown_columns": map[string]any{"self_id": "type-mismatch"},
		}, map[string]any{"thread": nil}},
		{6, map[string]any{
			"direction_conflict": map[string]any{"status_direction": "in", "sender_is_self": true}, "sender_state": "self", "sender_is_self": true, "conversation_state": "no-conversations-row",
		}, map[string]any{"direction": "unknown", "status": "received", "thread": map[string]any{"id": "9"}}},
		{7, map[string]any{"sender_state": "undecided", "sender_id": nil, "unknown_columns": map[string]any{"sender_id": "type-mismatch"}, "members_state": "none", "members_dangling": int64(1)}, nil},
	} {
		r, ok := parties[c.id]
		if !ok {
			t.Fatalf("parties row %d: no record", c.id)
		}
		raw := rawOf(r)
		for k, w := range c.raw {
			g, has := raw[k]
			switch {
			case w == nil && has:
				t.Errorf("parties row %d: raw.%s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("parties row %d: raw.%s = %#v, want %#v", c.id, k, g, w)
			}
		}
		for k, w := range c.pay {
			g, has := r.Payload[k]
			switch {
			case w == nil && has:
				t.Errorf("parties row %d: %s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("parties row %d: %s = %#v, want %#v", c.id, k, g, w)
			}
		}
	}
	// the body, the attachments and the parts pass (Task 6; spec 6.4, 6.5, 7 R11, R12, B6, 4I5-P9)
	var many []mrow
	for i := int64(0); i < 1001; i++ {
		many = append(many, mrow{2000 + i, map[string]any{"message_id": int64(3), "text": "p"}})
	}
	pres, pm := store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		messages: []mrow{
			{1, map[string]any{"mms_subject": "Sub\xff"}},
			{2, nil},
			{3, nil},
			{4, map[string]any{"mms_subject": []byte{1}}},
		},
		parts: append([]mrow{
			{1, map[string]any{"message_id": int64(1), "text": "x"}},
			{2, map[string]any{"message_id": int64(1), "uri": "u\xff", "content_type": strings.Repeat("c", 5000), "width": int64(3), "height": int64(4)}},
			{3, map[string]any{"message_id": int64(1), "text": "both", "uri": "content://mms/part/3"}},
			{4, map[string]any{"message_id": int64(1)}},
			{900, map[string]any{"message_id": int64(999), "text": "o\xff", "timestamp": int64(50), "conversation_id": int64(3)}},
			{901, map[string]any{"message_id": nil, "uri": "content://mms/part/901", "content_type": "image/png"}},
			{902, map[string]any{"message_id": "x", "timestamp": "y", "conversation_id": nil}},
		}, many...),
	}.parse(t)
	for _, c := range []struct {
		id  int64
		raw map[string]any
		pay map[string]any
	}{
		{1, map[string]any{
			"parts": int64(4), "parts_text_and_uri": int64(1), "parts_empty": int64(1), "body_state": nil, "parts_total": nil, "parts_truncated": nil,
			"attachment_parts": []any{map[string]any{"part_id": int64(2), "width": int64(3), "height": int64(4)}, map[string]any{"part_id": int64(3), "width": int64(-1), "height": int64(-1)}},
			"text_flags":       []any{"attachments.mime", "attachments.source_ref", "subject"},
		}, map[string]any{"kind": "media", "text_source": "part", "subject": "Sub�"}},
		{2, map[string]any{"parts": int64(0), "body_state": "no-parts", "attachment_parts": nil}, map[string]any{"kind": "unknown", "text_source": nil, "attachments": nil, "subject": nil}},
		{3, map[string]any{"parts": int64(1000), "parts_total": int64(1001), "parts_truncated": true}, map[string]any{"kind": "text"}},
		{4, map[string]any{"unknown_columns": map[string]any{"mms_subject": "type-mismatch"}}, map[string]any{"subject": nil}},
	} {
		r, ok := pm[c.id]
		if !ok {
			t.Fatalf("parts row %d: no record", c.id)
		}
		raw := rawOf(r)
		for k, w := range c.raw {
			g, has := raw[k]
			switch {
			case w == nil && has:
				t.Errorf("parts message %d: raw.%s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("parts message %d: raw.%s = %#v, want %#v", c.id, k, g, w)
			}
		}
		for k, w := range c.pay {
			g, has := r.Payload[k]
			switch {
			case w == nil && has:
				t.Errorf("parts message %d: %s = %#v, want absent", c.id, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("parts message %d: %s = %#v, want %#v", c.id, k, g, w)
			}
		}
	}
	orph := partsByRowid(t, pres)
	for id, w := range map[int64]map[string]any{
		900: {"part_state": "orphan", "part_rowid": int64(900), "part_message_id": int64(999), "timestamp": int64(50), "timestamp_status": "invalid", "conversation_id": int64(3), "text_flags": []any{"body"}, "part_message_id_state": nil},
		901: {"part_message_id_state": "null", "part_message_id": nil, "attachment_parts": []any{map[string]any{"part_id": int64(901), "width": int64(-1), "height": int64(-1)}}, "unknown_columns": nil},
		902: {"part_message_id": nil, "part_message_id_state": nil, "parts_empty": int64(1), "unknown_columns": map[string]any{"message_id": "type-mismatch", "timestamp": "type-mismatch"}},
	} {
		r, ok := orph[id]
		if !ok {
			t.Fatalf("orphan part %d: no record", id)
		}
		raw := rawOf(r)
		for k, v := range w {
			g, has := raw[k]
			switch {
			case v == nil && has:
				t.Errorf("orphan part %d: raw.%s = %#v, want absent", id, k, g)
			case v != nil && !reflect.DeepEqual(g, v):
				t.Errorf("orphan part %d: raw.%s = %#v, want %#v", id, k, g, v)
			}
		}
	}
	if r := orph[900]; r.Payload["kind"] != "text" || r.Payload["text_source"] != "part" || r.Payload["thread"].(map[string]any)["id"] != "3" || r.Payload["body_flags"] == nil {
		t.Errorf("orphan part 900: payload %v", r.Payload)
	}
	if r := orph[901]; r.Payload["kind"] != "media" || !reflect.DeepEqual(r.Payload["attachments"], []any{map[string]any{"mime": "image/png", "source_ref": "content://mms/part/901"}}) {
		t.Errorf("orphan part 901: payload %v", r.Payload)
	}
	if pres.Notes["parts.joined"] != "1005" || pres.Notes["orphan_parts"] != "3" {
		t.Errorf("notes parts.joined %q orphan_parts %q, want 1005 and 3", pres.Notes["parts.joined"], pres.Notes["orphan_parts"])
	}
	// the place (spec 5.2): the app is stated from the path and the basis with it
	for remote, w := range map[string][2]string{
		remoteApp: {"google_messages", "path"}, remoteAOSP: {"aosp_messaging", "path"}, remoteOther: {"unknown", "none"},
	} {
		res := parseAt(t, remote, dbOnly(builtDB(t, []mrow{{1, nil}})))
		if res.Err != nil {
			t.Fatalf("%s: %v", remote, res.Err)
		}
		raw := rawOf(byRowid(t, res)[1])
		if raw["app"] != w[0] || raw["app_basis"] != w[1] {
			t.Errorf("%s: app %v basis %v, want %v", remote, raw["app"], raw["app_basis"], w)
		}
	}
	// the typed pass (Task 8; spec 8.2, 8.3, B1, C5, C5a): the five join states of a recovered message, and the join and its
	// reasons of a recovered part, each pinned literally
	p := androidgooglemessages.Parser{}
	m50 := mapOn(t, p, fixtureFiles(t, "gm-deleted"), recoveredRow(t, "gm-deleted", "messages", 50))
	type typedCase struct {
		name string
		r    records.Record
		raw  map[string]any
	}
	typed := []typedCase{
		{"recovered M50", m50, map[string]any{
			"body_state": "not-joined-recovered", "sender_state": "not-joined-recovered", "self_row_state": "not-joined-recovered",
			"conversation_state": "not-joined-recovered", "members_state": "not-joined-recovered", "part_join": nil, "join_undecided": nil,
		}},
	}
	partOf := func(name string, res *typedEmitter, id int64) records.Record {
		idx := recoveredParts(res)[id]
		if len(idx) == 0 {
			t.Fatalf("%s: no typed record of recovered part %d", name, id)
		}
		return res.recs[idx[0]]
	}
	deleted := typedRun(t, p, fixtureFiles(t, "gm-deleted"))
	p52 := recoveredRow(t, "gm-deleted", "parts", 52)
	notJoined := func(reason string) map[string]any {
		return map[string]any{"part_join": "not-joined", "part_join_reason": reason, "joined_message_rowid": nil, "part_state": nil}
	}
	typed = append(typed,
		typedCase{"part 52", partOf("gm-deleted", deleted, 52), map[string]any{"part_join": "joined", "joined_message_rowid": int64(2), "part_join_reason": nil, "part_rowid": int64(52), "part_message_id": int64(2), "part_state": nil}},
		typedCase{"part 50", partOf("gm-deleted", deleted, 50), notJoined("no-live-message")},
		typedCase{"part 60", mapOn(t, p, fixtureFiles(t, "gm-rebuilt"), rebuiltPart(t, 60)), notJoined("conversation-differs")},
		typedCase{"part 61", mapOn(t, p, fixtureFiles(t, "gm-rebuilt"), rebuiltPart(t, 61)), notJoined("timestamp-differs")},
		typedCase{"no AUTOINCREMENT", mapOn(t, p, joinStore(t, joinOpts{noAuto: true}), p52), notJoined("no-autoincrement")},
		typedCase{"no sqlite_sequence row", mapOn(t, p, joinStore(t, joinOpts{noSeqRow: true}), p52), notJoined("no-sqlite-sequence")},
		typedCase{"undecidable schema", mapOn(t, undecidedSchema{}, fixtureFiles(t, "gm-deleted"), p52), notJoined("undecided")},
	)
	for _, c := range typed {
		raw := rawOf(c.r)
		for k, w := range c.raw {
			g, has := raw[k]
			switch {
			case w == nil && has:
				t.Errorf("%s: raw.%s = %#v, want absent", c.name, k, g)
			case w != nil && !reflect.DeepEqual(g, w):
				t.Errorf("%s: raw.%s = %#v, want %#v", c.name, k, g, w)
			}
		}
	}
	// the B8 row of docs/invariants/parsers.md lists every key and state word this test pins
	t.Run("the B8 row lists them", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "invariants", "parsers.md"))
		if err != nil {
			t.Fatal(err)
		}
		var row string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "Google Messages payload vocabulary") {
				row = l
			}
		}
		if row == "" {
			t.Fatal("docs/invariants/parsers.md has no row \"Google Messages payload vocabulary\"")
		}
		words := []string{
			"message_protocol", "message_status", "label_message_protocol", "label_message_status", "message_protocol_state", "message_status_state",
			"null", "unknown", "received_timestamp", "sent_timestamp", "mms_expiry", "retry_start_timestamp", "retry_start_timestamp_unit",
			"unestablished", "sentinel", "invalid", "time_basis", "received", "sent", "absent", "undecided", "seen", "read", "sms_priority",
			"sms_message_size", "raw_status", "mms_transaction_id", "mms_content_location", "sms_message_uri", "sms_message_uri_state",
			"too-long", "sms_message_uri_len", "sms_message_uri_sha256", "sms_link", "authority", "sms_link_id_reuse", "possible", "none",
			"sms_link_state", "unparsed", "app", "app_basis", "google_messages", "aosp_messaging", "path", "unknown_columns", "row_flags",
			"text_flags", "length_mismatch", "type-mismatch", "mms_push_notification",
			// Task 5: direction, sender, self, thread and members
			"sender_id", "sender_is_self", "direction_conflict", "status_direction", "sender_state", "absent", "no-participants-row", "self-undecided", "self",
			"from_address_basis", "send_destination", "normalized_destination", "display_destination", "from_destinations", "from_address_state", "empty",
			"altered", "from_address_text", "from_address_len", "from_address_sha256", "from_name_basis", "cached_contact", "from_full_name", "from_contact_id",
			"self_id", "self_row_state", "self_sub_id", "self_sim_slot_id", "self_subscription_name", "conversation_id", "conversation_state",
			"no-conversations-row", "sms_thread_id", "archive_status", "is_enterprise", "participant_count", "sort_timestamp", "last_read_timestamp",
			"sort_timestamp_status", "last_read_timestamp_status", "members_basis", "current", "members_total", "members_cut", "members_state", "none",
			"members_dangling", "members_without_address", "blocked_member_ids", "self_state", "group_state", "join_undecided", "participants.sub_id",
			// Task 6: body, attachments, parts and orphan parts
			"subject", "text_source", "part", "kind", "attachments", "mime", "source_ref", "parts", "parts_total", "parts_truncated", "parts_text_and_uri",
			"parts_empty", "attachment_parts", "part_id", "width", "height", "body_state", "no-parts", "conversation_participants.participant_id",
			"part_rowid", "part_state", "orphan", "part_message_id", "part_message_id_state", "timestamp", "timestamp_status", "body", "message_id",
			"attachments.mime", "attachments.source_ref", "parts.text", "parts.uri", "parts.content_type", "parts.width", "parts.height", "mms_subject",
			// Task 8: the typed pass
			"not-joined-recovered", "body_state", "sender_state", "self_row_state", "conversation_state", "members_state", "part_join", "joined",
			"not-joined", "part_join_reason", "joined_message_rowid", "no-autoincrement", "no-sqlite-sequence", "no-live-message",
			"conversation-differs", "timestamp-differs", "undecided", "part_join_basis_undecided",
		}
		for _, w := range words {
			if !strings.Contains(row, "`"+w+"`") {
				t.Errorf("the B8 row does not list %q", w)
			}
		}
	})
}
