package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// The media constructors of the Go-built rows (constructors.tsv).
const (
	idMediaGeo = 0x56e0d474 // TL_messageMediaGeo: geo
	idGeoPoint = 0xb2a2f663 // TL_geoPoint: flags, _long, lat, access_hash
)

func f64(x float64) []byte { return binary.LittleEndian.AppendUint64(nil, math.Float64bits(x)) }

// geoMedia is a TL_messageMediaGeo holding one TL_geoPoint (longitude first on the wire, recon 3.6).
func geoMedia(long, lat float64) []byte {
	b := append(le32(idMediaGeo), le32(idGeoPoint)...)
	b = append(b, le32(0)...)
	b = append(b, f64(long)...)
	b = append(b, f64(lat)...)
	return append(b, le64(0)...)
}

// mediaLabels are the type labels of the 50 TL_messageMedia* classes of the table and the two MessageMedia* ones
// (spec 6.6: the 50 IDs -> type label; the label is the snake case of the class name between TL_messageMedia and the
// first layer suffix).
func mediaLabels() map[string]string {
	return map[string]string{
		"TL_messageMediaAudio_layer45":            "audio",
		"TL_messageMediaContact":                  "contact",
		"TL_messageMediaContact_layer131":         "contact",
		"TL_messageMediaContact_layer81":          "contact",
		"TL_messageMediaDice":                     "dice",
		"TL_messageMediaDice_layer111":            "dice",
		"TL_messageMediaDice_layer220":            "dice",
		"TL_messageMediaDocument":                 "document",
		"TL_messageMediaDocument_layer159":        "document",
		"TL_messageMediaDocument_layer187":        "document",
		"TL_messageMediaDocument_layer197":        "document",
		"TL_messageMediaDocument_layer197_2":      "document",
		"TL_messageMediaDocument_layer68":         "document",
		"TL_messageMediaDocument_layer74":         "document",
		"TL_messageMediaDocument_old":             "document",
		"TL_messageMediaEmpty":                    "empty",
		"TL_messageMediaGame":                     "game",
		"TL_messageMediaGeo":                      "geo",
		"TL_messageMediaGeoLive":                  "geo_live",
		"TL_messageMediaGeoLive_layer119":         "geo_live",
		"TL_messageMediaGiveaway":                 "giveaway",
		"TL_messageMediaGiveawayResults":          "giveaway_results",
		"TL_messageMediaGiveawayResults_layer186": "giveaway_results",
		"TL_messageMediaGiveaway_layer167":        "giveaway",
		"TL_messageMediaGiveaway_layer186":        "giveaway",
		"TL_messageMediaInvoice":                  "invoice",
		"TL_messageMediaInvoice_layer145":         "invoice",
		"TL_messageMediaPaidMedia":                "paid_media",
		"TL_messageMediaPhoto":                    "photo",
		"TL_messageMediaPhoto_layer223":           "photo",
		"TL_messageMediaPhoto_layer68":            "photo",
		"TL_messageMediaPhoto_layer74":            "photo",
		"TL_messageMediaPhoto_old":                "photo",
		"TL_messageMediaPoll":                     "poll",
		"TL_messageMediaPoll_layer223":            "poll",
		"TL_messageMediaStory":                    "story",
		"TL_messageMediaStory_layer162":           "story",
		"TL_messageMediaToDo":                     "to_do",
		"TL_messageMediaUnsupported":              "unsupported",
		"TL_messageMediaUnsupported_old":          "unsupported",
		"TL_messageMediaVenue":                    "venue",
		"TL_messageMediaVenue_layer71":            "venue",
		"TL_messageMediaVideoStream":              "video_stream",
		"TL_messageMediaVideo_layer45":            "video",
		"TL_messageMediaVideo_old":                "video",
		"TL_messageMediaWebPage":                  "web_page",
		"TL_messageMediaWebPage_layer165":         "web_page",
		"TL_messageMediaWidget":                   "widget",
		"TL_messageMediaWidget_layer233":          "widget",
		"TL_messageMediaWidget_old":               "widget",
		"MessageMediaStoryFull":                   "story_full",
		"MessageMediaStoryFull_old":               "story_full",
	}
}

// Spec 8.5 (media summary, raw only), 6.6 (the 50 IDs and their labels), T3 (sub-types beyond the summary are 4I4b),
// call 5 (never source_ref), R9/R59 (T5: a secret document stops at media.document.thumbs, PhotoSize is 4I4b, so its
// key is never reached: key_present absent): the media of R9, R21-R29 and the telegram-layers media rows as the facts;
// a document's attachment carries mime, size and (plan 4I4b D10) the name of its one filename attribute; no record of
// any variant has a source_ref. The 4I4b summaries follow in mediaSummaries4I4b.
func TestTelegramMediaSummaries(t *testing.T) {
	labels := mediaLabels()
	if len(labels) != 52 {
		t.Fatalf("%d labels, want the 50 TL_messageMedia classes and 2 MessageMedia ones", len(labels))
	}
	for cls, want := range labels {
		if got := androidtelegram.MediaLabel(cls); got != want {
			t.Errorf("mediaLabel(%q) = %q, want %q", cls, got, want)
		}
	}
	if got, want := androidtelegram.MediaClassCount(), len(labels); got != want {
		t.Errorf("the table holds %d MessageMedia constructors, the label table %d", got, want)
	}

	rows := byRow(parseFixture(t, "telegram-v181").Records)
	secretDoc := map[string]any{
		"type": "document", "constructor": "0x52d8ccd9", "id": 7001, "date": baseDate, "mime_type": "image/jpeg",
		"size": 2048, "access_hash_present": true, "file_name_state": "undecided", // the decode stopped before the attributes
	}
	want := map[int64]map[string]any{
		9:  secretDoc,
		59: secretDoc,
		21: {"type": "photo", "constructor": "0xe216eb63", "id": 9001, "date": baseDate, "dc_id": 2, "access_hash_present": true, "file_reference_present": true},
		22: {"type": "photo", "constructor": "0xe216eb63", "id": 9001, "date": baseDate, "access_hash_present": true, "file_reference_present": true},
		23: {
			"type": "document", "constructor": "0x52d8ccd9", "id": 9002, "date": baseDate, "mime_type": "application/pdf",
			"size": 12345, "dc_id": 2, "access_hash_present": true, "file_reference_present": true,
			"attributes_state": "partial", "file_name_state": "undecided", // the decode stopped inside the attributes
		},
		24: {"type": "geo", "constructor": "0x56e0d474", "lat": 52.52, "long": 13.405},
		25: {"type": "geo", "constructor": "0x56e0d474", "lat_state": "out-of-range", "lat_bits": "0x4056c00000000000", "long": 13.405},
		26: {
			"type": "contact", "constructor": "0x70322949", "phone_number": "+15550000009", "first_name": "Cee",
			"last_name": "Contact", "vcard": "BEGIN:VCARD\nVERSION:3.0\nFN:Cee Contact\nTEL:+15550000009\nEND:VCARD",
			"user_id": 1000002,
		},
		27: {"type": "venue", "constructor": "0x2ec0533f", "lat": 52.52, "long": 13.405, "title": "Office", "address": "1 Main St"},
		28: {"type": "web_page", "constructor": "0xddf10c3b", "url": "fixture.invalid/a", "display_url": "fixture.invalid/a"},
		29: {"type": "dice", "constructor": "0x08cbec07", "emoticon": "🎲", "value": 3}, // plan 4I4b Task 5: the dice summary
	}
	for id, m := range want {
		r := rows["messages_v2/"+itoa(id)]
		if got := rawOf(r)["media"]; jsonText(got) != jsonText(m) {
			t.Errorf("R%d: raw.media %s, want %s", id, jsonText(got), jsonText(m))
		}
		if r.Payload["kind"] != "media" {
			t.Errorf("R%d: kind %v", id, r.Payload["kind"])
		}
	}
	for _, id := range []int64{9, 59} {
		raw := rawOf(rows["messages_v2/"+itoa(id)])
		if raw["data_state"] != "partial" || raw["data_stop_path"] != "media.document.thumbs" {
			t.Errorf("R%d: %v at %v, want partial at media.document.thumbs (R9/R59)", id, raw["data_state"], raw["data_stop_path"])
		}
	}
	atts := map[int64]string{
		9: `[{"mime":"image/jpeg","size":2048}]`, 59: `[{"mime":"image/jpeg","size":2048}]`,
		23: `[{"mime":"application/pdf","size":12345}]`,
	}
	for _, id := range []int64{1, 9, 21, 23, 24, 26, 28, 29, 59} {
		got, has := rows["messages_v2/"+itoa(id)].Payload["attachments"]
		if w, ok := atts[id]; (ok && jsonText(got) != w) || (!ok && has) {
			t.Errorf("R%d: attachments %s (present %v), want %q", id, jsonText(got), has, w)
		}
	}
	if m := rawOf(rows["messages_v2/1"])["media"]; m != nil {
		t.Errorf("R1: raw.media %v: a message without media has none", m)
	}

	// the telegram-layers media rows 1501...: one per full MessageMedia wrapper and TL_messageMediaUnsupported_old
	// (opaque), each with the text "media <class>"; its type is the class's label (spec 6.6). Plan 4I4b Task 1 made
	// more wrappers full (32 full of 52), so the rows are 33 (11 before) and the type follows the class, never a
	// position.
	layers := byRow(parseFixture(t, "telegram-layers").Records)
	n := 0
	for id := int64(1501); id < 1601; id++ {
		r, ok := layers["messages_v2/"+itoa(id)]
		if !ok {
			break
		}
		n++
		cls, isMedia := strings.CutPrefix(r.Body, "media ")
		m, _ := rawOf(r)["media"].(map[string]any)
		if !isMedia || labels[cls] == "" || m["type"] != labels[cls] {
			t.Errorf("messages_v2/%d (%q): raw.media.type %v, want the label of the class", id, r.Body, m["type"])
		}
	}
	if n != 33 {
		t.Errorf("%d telegram-layers media rows, want 33", n)
	}

	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		for _, r := range res.Records {
			noAttachmentRef(t, v, r)
		}
	}
	mediaSummaries4I4b(t)
}

// noAttachmentRef fails when an attachment of r names anything but a name read from a filename attribute (call 5: a media
// summary is never a reference; plan 4I4b D10 lets Name through, TestTelegramAttachmentNameFromOneFilenameAttribute).
func noAttachmentRef(t *testing.T, variant string, r records.Record) {
	t.Helper()
	l, _ := r.Payload["attachments"].([]any)
	for _, a := range l {
		m, _ := a.(map[string]any)
		for _, k := range []string{"source_ref", "artifact_id", "artifact_sha256"} {
			if _, has := m[k]; has {
				t.Errorf("%s %s: attachment %s %v", variant, r.Locator, k, m)
			}
		}
	}
}

// pad is the ASCII "<prefix>%02d-" repeated and cut to k bytes (the fixture generator's thumb(n, k) and wave(n, k)).
func pad(prefix string, n, k int) []byte {
	unit := fmt.Sprintf("%s%02d-", prefix, n)
	return []byte(strings.Repeat(unit, k/len(unit)+1)[:k])
}

// listOf is raw[key] as its maps.
func listOf(t *testing.T, raw map[string]any, key string) []map[string]any {
	t.Helper()
	l, _ := raw[key].([]any)
	out := make([]map[string]any, len(l))
	for i, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("raw.%s[%d] is %T, want an object", key, i, e)
		}
		out[i] = m
	}
	return out
}

// wantKeys fails for every key of want that got (or its absence, nil) does not hold.
func wantKeys(t *testing.T, name string, got, want map[string]any) {
	t.Helper()
	for k, w := range want {
		v, has := got[k]
		if (w == nil && has) || (w != nil && jsonText(v) != jsonText(w)) {
			t.Errorf("%s: %s = %s (present %v), want %s", name, k, jsonText(v), has, jsonText(w))
		}
	}
}

// goDocument is a TL_messageMediaDocument holding a TL_document (flags 0, no thumbs) with the encoded attributes.
func goDocument(attrs ...[]byte) []byte {
	b := append(le32(0x52d8ccd9), le32(1)...)
	b = append(b, le32(0x8fd4c4d8)...)
	b = append(b, le32(0)...)
	b = append(b, le64(5)...)
	b = append(b, le64(6)...)
	b = append(b, tlStr("SYNTHETIC-REF")...)
	b = append(b, le32(baseDate)...)
	b = append(b, tlStr("video/mp4")...)
	b = append(b, le64(1000)...)
	b = append(b, le32(2)...)
	b = append(b, le32(0x1cb5c415)...)
	b = append(b, le32(uint32(len(attrs)))...)
	for _, a := range attrs {
		b = append(b, a...)
	}
	return b
}

// attrVideo is a TL_documentAttributeVideo (flags 0): duration, w, h.
func attrVideo(duration float64, w, h uint32) []byte {
	b := append(le32(0x43c57c48), le32(0)...)
	b = append(b, f64(duration)...)
	return append(append(b, le32(w)...), le32(h)...)
}

// attrImageSize is a TL_documentAttributeImageSize.
func attrImageSize(w, h uint32) []byte {
	return append(append(le32(0x6c37c15c), le32(w)...), le32(h)...)
}

// goPhoto is a TL_messageMediaPhoto holding a TL_photo with the encoded sizes; cut >= 0 ends the blob after that many
// bytes of the sizes' elements.
func goPhoto(sizes [][]byte, cut int) []byte {
	b := append(le32(0xe216eb63), le32(1)...)
	b = append(b, le32(0xfb197a65)...)
	b = append(b, le32(0)...)
	b = append(b, le64(7)...)
	b = append(b, le64(8)...)
	b = append(b, tlStr("SYNTHETIC-REF")...)
	b = append(b, le32(baseDate)...)
	b = append(b, le32(0x1cb5c415)...)
	b = append(b, le32(uint32(len(sizes)))...)
	var body []byte
	for _, s := range sizes {
		body = append(body, s...)
	}
	if cut >= 0 {
		return append(b, body[:cut]...)
	}
	b = append(b, body...)
	return append(b, le32(2)...)
}

// photoSize is a TL_photoSize: type, w, h, size.
func photoSize(typ string, w, h, size uint32) []byte {
	b := append(le32(0x75c78e60), tlStr(typ)...)
	return append(append(append(b, le32(w)...), le32(h)...), le32(size)...)
}

// goMediaRow parses one Go-built message whose media is m and returns its record.
func goMediaRow(t *testing.T, m []byte) records.Record {
	t.Helper()
	cells := []msgCells{{mid: 1, uid: 1000002, data: tlMessage{id: 1, from: 1000002, peer: 1000001, date: baseDate, text: "x", media: m}.bytes()}}
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells)))
	if res.Err != nil || len(res.Violations) != 0 {
		t.Fatalf("Parse: err %v, violations %v", res.Err, res.Violations)
	}
	return byRow(res.Records)["messages_v2/1"]
}

// mediaSummaries4I4b are the plan 4I4b Task 5 facts of TestTelegramMediaSummaries (Review Focus 2 and 5; the plan's
// "Raw shapes" and fixture scenario).
func mediaSummaries4I4b(t *testing.T) {
	t.Helper()
	v181 := byRow(parseFixture(t, "telegram-v181").Records)
	media := func(id int64) map[string]any {
		m, _ := rawOf(v181["messages_v2/"+itoa(id)])["media"].(map[string]any)
		return m
	}

	// R76: the live photo's sizes and video sizes (B1: lists at the top of raw, entries scalar-only)
	raw := rawOf(v181["messages_v2/76"])
	sizes := listOf(t, raw, "media_sizes")
	if len(sizes) != 4 || jsonText(raw["media_sizes_total"]) != "4" || raw["media_sizes_truncated"] != nil {
		t.Fatalf("R76: %d sizes, total %v", len(sizes), raw["media_sizes_total"])
	}
	for i, w := range []map[string]any{
		{"kind": "stripped", "type": "i", "bytes_len": 24, "bytes_sha256": sha256Hex(string(pad("SYNTHETIC-THUMB-", 2, 24)))},
		{"kind": "cached", "type": "s", "w": 90, "h": 90, "bytes_len": 48, "bytes_sha256": sha256Hex(string(pad("SYNTHETIC-THUMB-", 3, 48)))},
		{"kind": "progressive", "type": "y", "w": 1280, "h": 960, "progressive_total": 3},
		{"kind": "path", "type": "j", "bytes_len": 16, "bytes_sha256": sha256Hex(string(pad("SYNTHETIC-THUMB-", 4, 16)))},
	} {
		wantKeys(t, fmt.Sprintf("R76 media_sizes[%d]", i), sizes[i], w)
	}
	vs := listOf(t, raw, "media_video_sizes")
	if len(vs) != 3 || jsonText(raw["media_video_sizes_total"]) != "3" {
		t.Fatalf("R76: %d video sizes, total %v", len(vs), raw["media_video_sizes_total"])
	}
	for i, w := range []map[string]any{
		{"kind": "video", "type": "u", "w": 800, "h": 800, "size": 4000},
		{"kind": "emoji_markup", "emoji_id": 5000000003, "background_colors_total": 2},
		{"kind": "sticker_markup", "sticker_id": 5000000004, "background_colors_total": 1, "stickerset_kind": "empty"},
	} {
		wantKeys(t, fmt.Sprintf("R76 media_video_sizes[%d]", i), vs[i], w)
	}
	wantKeys(t, "R76 media", media(76), map[string]any{"live_photo": true, "live_photo_document_id": 9201, "live_photo_mime_type": "video/mp4", "dc_id": 2})

	// R70 to R72, R68, R69: the attribute scalars (P5), the sticker set and the dice outcome
	wantKeys(t, "R70", media(70), map[string]any{
		"attributes": []string{"audio"}, "attributes_total": 1, "voice": true, "audio_voice": true, "duration": 12, "duration_unit": "unestablished",
		"title": "Memo", "performer": "Ann", "waveform_len": 63, "waveform_sha256": sha256Hex(string(pad("SYNTHETIC-WAVE-", 1, 63))),
		"file_name_state": "absent",
	})
	wantKeys(t, "R71", media(71), map[string]any{
		"round": true, "video_round_message": true, "supports_streaming": true, "duration": 7.5, "duration_unit": "unestablished",
		"video_codec": "h264", "video_start_ts": 0.5, "w": 240, "h": 240,
	})
	// T5c: the video attribute's own flag is video_round_message; the wrapper's round stays, no merged round_message
	if _, merged := media(71)["round_message"]; merged {
		t.Errorf("R71: round_message stated; the video attribute's flag is video_round_message (T5c)")
	}
	if vt := listOf(t, rawOf(v181["messages_v2/71"]), "media_video_thumbs"); len(vt) != 1 || vt[0]["kind"] != "video" {
		t.Errorf("R71: media_video_thumbs %s, want one video entry", jsonText(vt))
	}
	wantKeys(t, "R68", media(68), map[string]any{
		"attributes": []string{"sticker", "image_size"}, "sticker_alt": "😀", "mask": true,
		"mask_coords": map[string]any{"n": 1, "x": 0.5, "y": -0.25, "zoom": 1.5}, "stickerset_kind": "id", "stickerset_id": 7000001,
		"stickerset_access_hash_present": true, "w": 512, "h": 512, "file_name_state": "absent",
	})
	wantKeys(t, "R69", media(69), map[string]any{
		"sticker_alt": "⭐", "stickerset_kind": "short_name", "stickerset_short_name": "FixtureSet", "attributes": []string{"custom_emoji"},
	})
	wantKeys(t, "R72", media(72), map[string]any{
		"emoticon": "🎲", "value": 5, "game_outcome": map[string]any{"stake_ton_amount": 100, "ton_amount": 200, "seed_len": 19},
	})

	// R79: a web page whose cached page the table cannot read
	m := media(79)
	raw = rawOf(v181["messages_v2/79"])
	if m["cached_page_class"] != "TL_page_layer110" || raw["data_state"] != "partial" || raw["data_stop_path"] != "media.webpage.cached_page" ||
		raw["data_undecided"] != "edit_date" {
		t.Errorf("R79: cached_page_class %v, data_state %v at %v, data_undecided %v", m["cached_page_class"], raw["data_state"], raw["data_stop_path"], raw["data_undecided"])
	}

	// P7 (telegram-v64): a layer-117 photo decodes with three sizes; no file location value reaches the record (R7)
	v64 := byRow(parseFixture(t, "telegram-v64").Records)
	p7 := v64["messages/108"]
	raw = rawOf(p7)
	if raw["data_state"] != "decoded" {
		t.Errorf("P7: data_state %v", raw["data_state"])
	}
	ps := listOf(t, raw, "media_sizes")
	if len(ps) != 3 || ps[0]["location_class"] != "TL_fileLocation_layer97" || ps[2]["location_class"] != "TL_fileLocation_layer82" || ps[1]["location_class"] != nil {
		t.Errorf("P7: sizes %s, want location_class on the first and third only", jsonText(ps))
	}
	for _, k := range []string{"volume_id", "local_id", "secret"} {
		if strings.Contains(jsonText(p7.Payload), k) {
			t.Errorf("P7: %q reached the record: a FileLocation field is never emitted", k)
		}
	}
	// P8 (telegram-v64): a document with Video and ImageSize attributes of the same size agrees (P5)
	p8, _ := rawOf(v64["messages/109"])["media"].(map[string]any)
	wantKeys(t, "P8", p8, map[string]any{"w": 1280, "h": 720, "duration": 30, "file_name": "IMG_0001.mp4"})
	if rawOf(v64["messages/109"])["data_state"] != "decoded" {
		t.Errorf("P8: data_state %v", rawOf(v64["messages/109"])["data_state"])
	}

	// V5 to V7 (telegram-v84): a legacy caption is raw and no Body; a legacy video and audio are {mime, size} attachments
	v84 := byRow(parseFixture(t, "telegram-v84").Records)
	v5, _ := rawOf(v84["messages_v2/5"])["media"].(map[string]any)
	if r := v84["messages_v2/5"]; r.Body != "" || v5["caption"] != "Old caption" || r.Payload["kind"] != "media" {
		t.Errorf("V5: Body %q, media %s, kind %v; want no Body, the caption in raw.media and kind media", r.Body, jsonText(v5), r.Payload["kind"])
	}
	for id, w := range map[int64]string{6: `[{"mime":"video/mp4","size":2000}]`, 7: `[{"mime":"audio/mpeg","size":3000}]`} {
		if got := jsonText(v84["messages_v2/"+itoa(id)].Payload["attachments"]); got != w {
			t.Errorf("V%d: attachments %s, want %s", id, got, w)
		}
	}
	v6, _ := rawOf(v84["messages_v2/6"])["media"].(map[string]any)
	wantKeys(t, "V6", v6, map[string]any{"type": "video", "duration": 5, "duration_unit": "unestablished", "w": 640, "h": 480})

	// the hostile rows: a cap inside the sizes list (H2802) and a duration that is no number (H2805)
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	raw = rawOf(hostile["messages_v2/2802"])
	if raw["data_state"] != "cap" || raw["data_stop_path"] != "media.photo.sizes" || raw["media_sizes_state"] != "partial" || raw["media_sizes"] != nil {
		t.Errorf("H2802: data_state %v at %v, media_sizes_state %v, media_sizes %v; want cap at the list and partial with no entry",
			raw["data_state"], raw["data_stop_path"], raw["media_sizes_state"], raw["media_sizes"])
	}
	h5, _ := rawOf(hostile["messages_v2/2805"])["media"].(map[string]any)
	wantKeys(t, "H2805", h5, map[string]any{"duration_state": "out-of-range", "duration_bits": "0x7ff8000000000000", "duration": nil})

	// Go-built: a photo cut inside sizes[1] states the list partial and none of its entries (P3)
	full := [][]byte{photoSize("m", 320, 240, 100), photoSize("x", 800, 600, 200)}
	raw = rawOf(goMediaRow(t, goPhoto(full, len(full[0])+8))) // sizes[1]: its ID and its type
	if raw["media_sizes_state"] != "partial" || raw["media_sizes"] != nil || raw["media_sizes_total"] != nil || raw["data_state"] != "truncated" {
		t.Errorf("a photo cut in sizes[1]: media_sizes_state %v, media_sizes %v, total %v, data_state %v", raw["media_sizes_state"], raw["media_sizes"], raw["media_sizes_total"], raw["data_state"])
	}
	raw = rawOf(goMediaRow(t, goPhoto(full, -1)))
	if got := listOf(t, raw, "media_sizes"); len(got) != 2 || jsonText(got[1]["w"]) != "800" {
		t.Errorf("an uncut photo: media_sizes %s", jsonText(raw["media_sizes"]))
	}

	// Go-built: attributes that disagree on a value state a conflict, never one of them (P5)
	doc, _ := rawOf(goMediaRow(t, goDocument(attrVideo(30, 1280, 720), attrImageSize(800, 600))))["media"].(map[string]any)
	wantKeys(t, "conflicting attributes", doc, map[string]any{"w_state": "conflict", "h_state": "conflict", "w": nil, "h": nil, "duration": 30})
	doc, _ = rawOf(goMediaRow(t, goDocument(attrVideo(30, 1280, 720), attrImageSize(1280, 720))))["media"].(map[string]any)
	wantKeys(t, "agreeing attributes", doc, map[string]any{"w": 1280, "h": 720, "w_state": nil})
}

// Design D11, ruling R7: the bytes of a thumbnail, a waveform and a dice seed are digests and lengths only, in every
// record, warning and note of every variant (raw, hex of either case, base64). R76's stripped size states the SHA-256 of
// its 24 thumbnail bytes.
func TestTelegramThumbnailBytesAreDigestOnly(t *testing.T) {
	var needles []string
	for _, p := range []string{"SYNTHETIC-THUMB-", "SYNTHETIC-WAVE-", "SYNTHETIC-SEED-"} {
		s := p + "0"
		needles = append(needles, s, hex.EncodeToString([]byte(s)), strings.ToUpper(hex.EncodeToString([]byte(s))))
		// base64 of the same bytes at each of the three alignments: the characters that do not depend on a neighbour
		for off := range 3 {
			e := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("A", off) + s))
			needles = append(needles, e[4:len(e)-4])
		}
	}
	seen := 0
	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		text := jsonText(res.Records) + jsonText(res.Notes)
		for _, w := range res.Warnings {
			text += w.Reason
		}
		for _, n := range needles {
			if strings.Contains(text, n) {
				t.Errorf("%s: %q (a thumbnail, waveform or seed byte pattern) reached an output", v, n)
			}
		}
		seen += len(res.Records)
	}
	if seen == 0 {
		t.Fatal("no record parsed")
	}
	sizes := listOf(t, rawOf(byRow(parseFixture(t, "telegram-v181").Records)["messages_v2/76"]), "media_sizes")
	want := sha256.Sum256(pad("SYNTHETIC-THUMB-", 2, 24))
	if len(sizes) == 0 || sizes[0]["bytes_sha256"] != hex.EncodeToString(want[:]) {
		t.Errorf("R76 stripped size: %s, want bytes_sha256 %x (the SHA-256 of thumb(2, 24))", jsonText(sizes), want)
	}
}

// Ruling B1, plan 4I4b Task 5: the two widest variants parse with no record refused for the schema
// (common.MaxContainerDepth), every message record passes message.Validate again, and every list holding an object
// is a direct value of payload.raw whose entries hold scalars only.
func TestTelegramDeepestRawShapeValidates(t *testing.T) {
	n := 0
	for _, v := range []string{"telegram-v181", "telegram-hostile"} {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		if res.Err != nil || len(res.Violations) != 0 || res.Refused != 0 { // the writer refuses a record that fails the schema
			t.Errorf("%s: Parse err %v, violations %v, refused %d", v, res.Err, res.Violations, res.Refused)
		}
		for _, r := range res.Records {
			if r.Type != "message" {
				continue
			}
			n++
			if err := message.Validate(r.Payload); err != nil {
				t.Errorf("%s %s: %v", v, r.Locator, err)
			}
			for k, val := range rawOf(r) {
				switch x := val.(type) {
				case []any:
					for _, e := range x {
						if m, ok := e.(map[string]any); ok {
							for ek, ev := range m {
								switch ev.(type) {
								case map[string]any, []any:
									t.Errorf("%s %s: raw.%s has an entry with a nested %s", v, r.Locator, k, ek)
								}
							}
						}
					}
				case map[string]any:
					for mk, mv := range x {
						if l, ok := mv.([]any); ok {
							for _, e := range l {
								if _, isMap := e.(map[string]any); isMap {
									t.Errorf("%s %s: raw.%s.%s is a list of objects below the top of raw (B1)", v, r.Locator, k, mk)
								}
							}
						}
					}
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no message record")
	}
	// plan 4I4b Task 6: the tail objects sit in the deepest shapes (R62 reactions, R63 replies with a replier list, R65 a
	// fact check, a suggested post and a restriction reason) and pass the schema.
	v181 := byRow(parseFixture(t, "telegram-v181").Records)
	for _, id := range []string{"62", "63", "65", "81", "83", "88", "95"} {
		r, ok := v181["messages_v2/"+id]
		if !ok {
			t.Fatalf("R%s: no record", id)
		}
		if err := message.Validate(r.Payload); err != nil {
			t.Errorf("R%s: %v", id, err)
		}
	}
	for id, key := range map[string]string{"62": "reactions_recent", "63": "replies", "65": "factcheck", "81": "reply_markup_buttons", "83": "reply_markup_buttons", "88": "poll_answers", "95": "entities"} {
		if rawOf(v181["messages_v2/"+id])[key] == nil {
			t.Errorf("R%s: raw.%s is not stated, so the shape is not exercised", id, key)
		}
	}
}
