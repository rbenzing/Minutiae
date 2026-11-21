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

// Spec 6.5, 8.8 (TLRPC:59562-59690), review M4(b): a secret class writes the string alone, whatever its flags and
// id: a TL_message_secret with flags 0x4 and a negative id reads its string and no fwd_msg_id, and the same string
// followed by an int32 is unparsed.
func TestTelegramSecretTrailerIsStringOnly(t *testing.T) {
	const ann, self = 1000002, 1000001
	secret := func(id int32, tail []byte) []byte {
		b := append(le32(0x555555fa), le32(flagFwdBit)...)
		b = append(b, le32(uint32(id))...)
		b = append(b, le32(0)...)   // ttl
		b = append(b, le32(ann)...) // from_id
		b = append(b, peerUser(self)...)
		b = append(b, le32(baseDate)...)
		b = append(b, tlStr("s")...)
		b = append(b, le32(0x3ded6320)...)                     // media: empty
		b = append(append(b, le32(0x1cb5c415)...), 0, 0, 0, 0) // entities: none
		return append(b, tail...)
	}
	path := tlStr("/sdcard/s.jpg")
	cells := []msgCells{
		{mid: -5, uid: 1<<62 | 5, data: secret(-5, path)},
		{mid: -6, uid: 1<<62 | 5, data: secret(-6, append(append([]byte{}, path...), le32(9)...))},
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)
	r1, r2 := rawOf(got["messages_v2/1"]), rawOf(got["messages_v2/2"])
	if r1["data_state"] != "decoded" || r1["trailer_state"] != "read" || r1["attach_path"] != "/sdcard/s.jpg" || r1["fwd_msg_id"] != nil {
		t.Errorf("string only: %v %v %v fwd %v", r1["data_state"], r1["trailer_state"], r1["attach_path"], r1["fwd_msg_id"])
	}
	if r2["trailer_state"] != "unparsed" || r2["attach_path"] != nil || r2["fwd_msg_id"] != nil {
		t.Errorf("string and int: %v %v %v", r2["trailer_state"], r2["attach_path"], r2["fwd_msg_id"])
	}
}

// flagFwdBit is MESSAGE_FLAG_FWD (TLRPC:64).
const flagFwdBit = 0x4
