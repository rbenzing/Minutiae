package androidtelegram

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// 4I4-P11, T5, review M4(a): an inline forward origin of a class the table leaves unresolved gives no address, its
// id as stored and from_class_state "contradictory". The table holds no such field today (the facts generator
// refuses one), so the decode result is built by hand.
func TestTelegramForwardUnresolvedClassIsContradictory(t *testing.T) {
	v := newView(tl.Result{Fields: []tl.Named{
		{Path: "fwd_from", Kind: tl.KObject, ID: 0xc786ddcb, Type: typeFwd},
		{Path: "fwd_from.flags", Kind: tl.KFlags, Int: 1},
		{Path: "fwd_from.from_id", Kind: tl.KID32, Int: 1000003, Class: "unknown"},
		{Path: "fwd_from.date", Kind: tl.KInt32, Int: 1700000000},
	}})
	raw := map[string]any{}
	fwdRaw(v, raw)
	fwd, _ := raw["fwd"].(map[string]any)
	if fwd["from_id"] != "1000003" || fwd["from_class_state"] != stateContradictory || fwd["from"] != nil || fwd["date"] != int64(1700000000) {
		t.Errorf("raw.fwd %v", fwd)
	}
}

// Spec 8.5, 9, review M4(c): a TL_documentEncrypted read to its key and iv (here through a PhotoSize the test adds
// to the table: the real table lacks PhotoSize, 4I4b) states key_present and iv_present, never the bytes.
func TestTelegramSecretDocumentKeyIsPresenceOnly(t *testing.T) {
	const idThumb = 0x7e57e57e
	tab := newTable()
	tab[tl.Key{Type: "PhotoSize", ID: idThumb}] = tl.Constructor{ID: idThumb, Name: "TL_photoSizeTest", Type: "PhotoSize", Body: tl.BodyFull}
	key := []byte("SYNTHETIC-NOT-A-KEY-0123456789ab")
	le := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	str := func(b []byte) []byte {
		out := append([]byte{byte(len(b))}, b...)
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
		return out
	}
	var b []byte
	b = append(b, le(0x7600b9d3)...)                                 // TL_message
	b = append(b, le(1<<9)...)                                       // flags: media
	b = append(b, le(0)...)                                          // flags2
	b = append(b, le(5)...)                                          // id
	b = append(append(b, le(0x59511722)...), 1, 0, 0, 0, 0, 0, 0, 0) // peer_id: peerUser 1
	b = append(b, le(1700000000)...)                                 // date
	b = append(b, str(nil)...)                                       // message
	b = append(b, le(0x52d8ccd9)...)                                 // media: TL_messageMediaDocument
	b = append(b, le(1)...)                                          // flags: document
	b = append(b, le(0x55555558)...)                                 // TL_documentEncrypted
	b = append(append(b, le(7001)...), 0, 0, 0, 0)                   // id
	b = append(append(b, le(5)...), 0, 0, 0, 0)                      // access_hash
	b = append(b, le(1700000000)...)                                 // date
	b = append(b, str([]byte("image/jpeg"))...)                      // mime_type
	b = append(b, le(2048)...)                                       // size
	b = append(b, le(idThumb)...)                                    // thumbs
	b = append(b, le(2)...)                                          // dc_id
	b = append(append(b, le(0x1cb5c415)...), 0, 0, 0, 0)             // attributes: an empty vector
	b = append(b, str(key)...)                                       // key
	b = append(b, str(key[:16])...)                                  // iv
	raw := map[string]any{}
	v, state := decodeBlob(raw, colData, b, typeMessage, tab, unlimited{})
	c, ok := topOf(v, tab, typeMessage, state)
	if state != stateDecoded || !ok {
		t.Fatalf("state %s, stop %+v", state, v.r.Stop)
	}
	atts := mediaSummary(v, tab, c, raw)
	m, _ := raw["media"].(map[string]any)
	if m["key_present"] != true || m["iv_present"] != true || m["dc_id"] != int64(2) || len(atts) != 1 {
		t.Errorf("raw.media %v, attachments %v", m, atts)
	}
	text := fmt.Sprint(raw, atts)
	if strings.Contains(text, string(key[:16])) {
		t.Errorf("the key bytes reach raw: %s", text)
	}
}

// unlimited is a budget that admits every charge.
type unlimited struct{}

func (unlimited) Alloc(int64) error { return nil }
func (unlimited) Free(int64)        {}
