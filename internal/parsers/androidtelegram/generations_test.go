package androidtelegram_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// Spec 8.3 (F5): one scenario maps alike in both generations. R1 in telegram-v84 and telegram-v181 (messages_v2) and
// P5 in telegram-v64 (messages, with R1's mid, uid, date, text, read_state and send_state) give equal channel,
// direction, kind, guid, Body, Time, status and read; only the source table, the constructors and the blob keys
// differ.
func TestTelegramGenerationsMapAlike(t *testing.T) {
	type view struct {
		channel, direction, kind, guid, status, body any
		read                                         any
		time                                         string
	}
	of := func(r records.Record) view {
		tm := ""
		if r.Time != nil {
			tm = r.Time.T.UTC().String()
		}
		p := r.Payload
		return view{p["channel"], p["direction"], p["kind"], p["guid"], p["status"], r.Body, p["read"], tm}
	}
	cases := []struct{ variant, key, table string }{
		{"telegram-v64", "messages/101", "messages"},
		{"telegram-v84", "messages_v2/1", "messages_v2"},
		{"telegram-v181", "messages_v2/1", "messages_v2"},
	}
	var first view
	for i, c := range cases {
		r, ok := byRow(parseFixture(t, c.variant).Records)[c.key]
		if !ok {
			t.Fatalf("%s: no record %s", c.variant, c.key)
		}
		if got := rawOf(r)["source_table"]; got != c.table {
			t.Errorf("%s %s: raw.source_table %v", c.variant, c.key, got)
		}
		v := of(r)
		if v.time == "" || v.body != "Hello Ann here" || v.kind != "text" {
			t.Errorf("%s %s: %+v", c.variant, c.key, v)
		}
		if i == 0 {
			first = v
			continue
		}
		if v != first {
			t.Errorf("%s %s maps to %+v, %s to %+v", c.variant, c.key, v, cases[0].variant, first)
		}
	}
}

// Spec 8.3 (facts.tsv predicate.pre84_channel_mid, predicate.pre84_secret_uid; F10, F32): a messages row is read by
// the pinned pre-84 predicates only. P1 packs its channel into mid (message_id 44); P2's uid is a high-id secret chat
// (class secret, enc_chat_id 77, dialog_basis); P3's negative ttl is a forward peer; P4's mid does not pack its
// channel (mid_state undecided); P5, a user row, has no mid_state. A messages_v2 row with a high-id-shaped uid is a
// user dialog: the predicate is the old table's only.
func TestTelegramPre84DialogConventions(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v64").Records)
	cases := []struct {
		key  string
		want map[string]any // raw keys; nil: must be absent
	}{
		{"messages/12884906182967340", map[string]any{"message_id": int64(44), "message_id_basis": "pre-84-channel-packing", "mid_state": nil, "dialog_class": "negative"}},
		{"messages/-7", map[string]any{"dialog_class": "secret", "enc_chat_id": int64(77), "dialog_basis": "pre-84-high-id", "mid_state": nil}},
		{"messages/47", map[string]any{"ttl_state": "pre-84-forward-peer", "dialog_class": "user", "mid_state": nil}},
		{"messages/12884910477934637", map[string]any{"mid_state": "undecided", "message_id": nil, "message_id_basis": nil}},
		{"messages/101", map[string]any{"mid_state": nil, "message_id": nil, "dialog_basis": nil, "ttl_state": nil, "dialog_class": "user"}},
	}
	for _, c := range cases {
		r, ok := rows[c.key]
		if !ok {
			t.Errorf("%s: no record", c.key)
			continue
		}
		raw := rawOf(r)
		for k, v := range c.want {
			if got, has := raw[k]; (v == nil && has) || (v != nil && got != v) {
				t.Errorf("%s: raw.%s = %v (present %v), want %v", c.key, k, got, has, v)
			}
		}
	}
	m := tlMessage{id: 9, from: 1000002, peer: 1000001, date: baseDate, text: "v2 high id"}
	db := goBuiltMessages(181, nil, []msgCells{{mid: 9, uid: 77 << 32, data: m.bytes()}})
	got := parseAt(t, remoteApp, dbOnly(db)).Records
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	raw := rawOf(got[0])
	if raw["dialog_class"] != "user" || raw["dialog_basis"] != nil || raw["enc_chat_id"] != nil {
		t.Errorf("messages_v2 uid 77<<32: raw %v, want a user dialog without the pre-84 keys", raw)
	}
}
