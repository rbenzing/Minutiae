package androidgooglemessages_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The plan's state vocabularies (B8), the keys and state words Task 4 owns: one Go-built row per value, each asserted with
// a hard-coded expected value (a nil want means the key is absent), and the B8 row of docs/invariants/parsers.md must
// list every one of them. Tasks 5, 6 and 8 extend this function.
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
		}
		for _, w := range words {
			if !strings.Contains(row, "`"+w+"`") {
				t.Errorf("the B8 row does not list %q", w)
			}
		}
	})
}
