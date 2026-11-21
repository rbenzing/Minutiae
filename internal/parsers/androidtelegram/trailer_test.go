package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Spec 8.8 (call 5): the app-local trailer is raw only and never looked up. R3 (a path, two params and fwd_msg_id 55
// of a local id), R41 (" ": the empty-path marker), R42 (two bytes that are no string: unparsed with length and
// SHA-256), R43 (no byte after the body: none) as the facts; a Go-built row whose params carry legacy_layer and whose
// path starts poll_with_media= (recon 2 item 1, TLRPC:59644-59671) keeps both as stored; a partial row is unreachable;
// no record of any variant has an attachment reference, and no Body comes from a trailer.
func TestTelegramTrailerIsRawOnly(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	want := map[int64]map[string]any{
		3: {
			"trailer_state": "read", "attach_path": "/sdcard/x.jpg", "attach_params_total": 2, "fwd_msg_id": 55,
			"attach_params":     []any{map[string]any{"key": "a", "value": "1"}, map[string]any{"key": "b", "value": "2"}},
			"attach_path_state": nil, "trailer_len": nil,
		},
		41: {"trailer_state": "read", "attach_path_state": "empty-marker", "attach_path": nil, "attach_params": nil, "fwd_msg_id": nil},
		42: {"trailer_state": "unparsed", "trailer_len": 2, "trailer_sha256": sha256Of([]byte("ab")), "attach_path": nil},
		43: {"trailer_state": "none", "trailer_len": nil, "attach_path": nil},
		22: {"trailer_state": "unreachable", "trailer_len": nil}, // partial at media.photo.sizes[0]
		7:  {"trailer_state": nil},                               // unknown-constructor: no body, no trailer
	}
	for id, keys := range want {
		r := rows["messages_v2/"+itoa(id)]
		raw := rawOf(r)
		for k, w := range keys {
			v, has := raw[k]
			if (w == nil && has) || (w != nil && jsonText(v) != jsonText(w)) {
				t.Errorf("R%d: raw.%s = %v (present %v), want %v", id, k, v, has, w)
			}
		}
		if strings.Contains(r.Body, "sdcard") {
			t.Errorf("R%d: Body %q holds the trailer", id, r.Body)
		}
	}
	if b := rows["messages_v2/3"].Body; b != "Forwarded" {
		t.Errorf("R3: Body %q, want the message text only", b)
	}

	const ann, self = 1000002, 1000001
	path := "||legacy_layer|=|73||poll_with_media=|=|1||poll_with_media=/sdcard/poll.jpg"
	data := tlMessage{id: 7, from: ann, peer: self, date: baseDate, text: "poll", tail: tlStr(path)}.bytes()
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 7, uid: ann, data: data}})))
	if len(res.Records) != 1 {
		t.Fatalf("%d records", len(res.Records))
	}
	raw := rawOf(res.Records[0])
	wantRaw := map[string]any{
		"trailer_state": "read", "attach_path": "poll_with_media=/sdcard/poll.jpg", "attach_params_total": 2,
		"attach_params": []any{
			map[string]any{"key": "legacy_layer", "value": "73"}, map[string]any{"key": "poll_with_media=", "value": "1"},
		},
	}
	for k, w := range wantRaw {
		if jsonText(raw[k]) != jsonText(w) {
			t.Errorf("legacy/poll row: raw.%s = %s, want %s", k, jsonText(raw[k]), jsonText(w))
		}
	}
	if res.Records[0].Body != "poll" {
		t.Errorf("legacy/poll row: Body %q", res.Records[0].Body)
	}

	for _, v := range variants {
		for _, r := range parseAt(t, remoteApp, fixtureFiles(t, v)).Records {
			noAttachmentRef(t, v, r)
			if r.Type == "message" && rawOf(r)["data_state"] == "partial" && rawOf(r)["trailer_state"] != "unreachable" {
				t.Errorf("%s %s: partial with trailer_state %v", v, r.Locator, rawOf(r)["trailer_state"])
			}
		}
	}
}

// Review Focus 5, spec 8.8: a trailer whose shape is not the pinned one is unparsed with its length and SHA-256,
// never a partly read path or params: an R43-shaped body followed by a valid string and 3 extra bytes; and a
// v64-era shape (a forwarded local message, flags & 0x4 with id < 0, that holds the string only where the pinned rule
// expects the int fwd_msg_id after it). The pinned shape with the int reads.
func TestTelegramTrailerThatLeavesBytesIsUnparsed(t *testing.T) {
	const ann, self = 1000002, 1000001
	extra := append(tlStr("/sdcard/a.jpg"), 1, 2, 3)
	fwd := fwdModern(peerUser(1000003), "", baseDate-60, "")
	withInt := append(tlStr("/sdcard/b.jpg"), le32(55)...)
	cells := []msgCells{
		{mid: 1, uid: ann, data: tlMessage{id: 1, from: ann, peer: self, date: baseDate, text: "No trailer", tail: extra}.bytes()},
		{mid: -2, uid: ann, out: int64(1), data: tlMessage{flags: bitOut, id: -2, peer: ann, date: baseDate, text: "v64", fwd: fwd, tail: tlStr("/sdcard/b.jpg")}.bytes()},
		{mid: -3, uid: ann, out: int64(1), data: tlMessage{flags: bitOut, id: -3, peer: ann, date: baseDate, text: "pinned", fwd: fwd, tail: withInt}.bytes()},
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)
	tails := [][]byte{extra, tlStr("/sdcard/b.jpg")}
	for i, tail := range tails {
		raw := rawOf(got["messages_v2/"+itoa(int64(i+1))])
		if raw["data_state"] != "decoded" || raw["trailer_state"] != "unparsed" || jsonText(raw["trailer_len"]) != itoa(int64(len(tail))) ||
			raw["trailer_sha256"] != sha256Of(tail) {
			t.Errorf("row %d: %v %v len %v sha %v, want decoded, unparsed, %d, %s", i+1, raw["data_state"], raw["trailer_state"],
				raw["trailer_len"], raw["trailer_sha256"], len(tail), sha256Of(tail))
		}
		for _, k := range []string{"attach_path", "attach_path_state", "attach_params", "attach_params_total", "fwd_msg_id"} {
			if v, has := raw[k]; has {
				t.Errorf("row %d: raw.%s = %v on an unparsed trailer", i+1, k, v)
			}
		}
	}
	raw := rawOf(got["messages_v2/3"])
	if raw["trailer_state"] != "read" || raw["attach_path"] != "/sdcard/b.jpg" || jsonText(raw["fwd_msg_id"]) != "55" {
		t.Errorf("pinned shape: %v %v %v", raw["trailer_state"], raw["attach_path"], raw["fwd_msg_id"])
	}
}
