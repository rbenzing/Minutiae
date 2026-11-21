package androidtelegram_test

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/records"
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
// a document's attachment carries mime and size only; no record of any variant has an attachment name or source_ref.
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
		"size": 2048, "access_hash_present": true,
	}
	want := map[int64]map[string]any{
		9:  secretDoc,
		59: secretDoc,
		21: {"type": "photo", "constructor": "0xe216eb63", "id": 9001, "date": baseDate, "access_hash_present": true, "file_reference_present": true},
		22: {"type": "photo", "constructor": "0xe216eb63", "id": 9001, "date": baseDate, "access_hash_present": true, "file_reference_present": true},
		23: {
			"type": "document", "constructor": "0x52d8ccd9", "id": 9002, "date": baseDate, "mime_type": "application/pdf",
			"size": 12345, "dc_id": 2, "access_hash_present": true, "file_reference_present": true,
		},
		24: {"type": "geo", "constructor": "0x56e0d474", "lat": 52.52, "long": 13.405},
		25: {"type": "geo", "constructor": "0x56e0d474", "lat_state": "out-of-range", "lat_bits": "0x4056c00000000000", "long": 13.405},
		26: {
			"type": "contact", "constructor": "0x70322949", "phone_number": "+15550000009", "first_name": "Cee",
			"last_name": "Contact", "vcard": "BEGIN:VCARD\nVERSION:3.0\nFN:Cee Contact\nTEL:+15550000009\nEND:VCARD",
			"user_id": 1000002,
		},
		27: {"type": "venue", "constructor": "0x2ec0533f", "lat": 52.52, "long": 13.405, "title": "Office", "address": "1 Main St"},
		28: {"type": "web_page", "constructor": "0xddf10c3b", "url": "https://example.test/a", "display_url": "example.test/a"},
		29: {"type": "dice", "constructor": "0x08cbec07"}, // opaque: the type from the table, nothing read
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

	layers := byRow(parseFixture(t, "telegram-layers").Records)
	types := []string{"venue", "empty", "document", "geo", "contact", "poll", "unsupported", "geo_live", "web_page", "photo", "unsupported"}
	for i, typ := range types {
		key := "messages_v2/" + itoa(int64(1501+i))
		m, _ := rawOf(layers[key])["media"].(map[string]any)
		if m["type"] != typ {
			t.Errorf("%s: raw.media.type %v, want %s", key, m["type"], typ)
		}
	}

	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		for _, r := range res.Records {
			noAttachmentRef(t, v, r)
		}
	}
}

// noAttachmentRef fails when an attachment of r names a file (call 5: a media summary is never a reference).
func noAttachmentRef(t *testing.T, variant string, r records.Record) {
	t.Helper()
	l, _ := r.Payload["attachments"].([]any)
	for _, a := range l {
		m, _ := a.(map[string]any)
		for _, k := range []string{"name", "source_ref", "artifact_id", "artifact_sha256"} {
			if _, has := m[k]; has {
				t.Errorf("%s %s: attachment %s %v", variant, r.Locator, k, m)
			}
		}
	}
}
