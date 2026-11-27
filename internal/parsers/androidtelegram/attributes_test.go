package androidtelegram_test

import (
	"strings"
	"testing"
)

// Rulings R6, B6 and design D10, plan 4I4b Review Focus 1: an attachment's name and raw.media.file_name come from the
// document's own filename attributes only. One name (or the same name repeated) is the name; two different names are a
// stated conflict with neither chosen; an empty one is "empty"; none is "absent"; an attribute list the decode did not
// read whole is "undecided". A live photo's, a link preview's and an alt document's filename never name the attachment.
func TestTelegramAttachmentNameFromOneFilenameAttribute(t *testing.T) {
	v181 := byRow(parseFixture(t, "telegram-v181").Records)
	type want struct {
		table  string
		row    int64
		name   string // the attachment name and raw.media.file_name ("" none)
		state  string // raw.media.file_name_state ("" none)
		hasAtt bool
	}
	cases := []struct {
		variant string
		w       want
	}{
		{"telegram-v181", want{"messages_v2", 60, "report.pdf", "", true}}, // R60: one filename attribute
		{"telegram-v181", want{"messages_v2", 74, "same.txt", "", true}},   // R74: the same name twice is one name (B6)
		{"telegram-v181", want{"messages_v2", 73, "", "conflict", true}},   // R73: a.txt and b.txt
		{"telegram-v181", want{"messages_v2", 75, "", "empty", true}},      // R75: an empty name is no name
		{"telegram-v181", want{"messages_v2", 68, "", "absent", true}},     // R68: a sticker has no filename attribute
		{"telegram-v181", want{"messages_v2", 78, "main.mp4", "", true}},   // R78: alt.mp4 of alt_documents never names it
		{"telegram-v181", want{"messages_v2", 80, "secret.pdf", "", true}}, // R80: a secret document
		{"telegram-v84", want{"messages_v2", 8, "old.doc", "", true}},      // V8: the old class's inline file_name
		{"telegram-v64", want{"messages", 109, "IMG_0001.mp4", "", true}},  // P8: pre-84 layer-117 document
	}
	for _, c := range cases {
		rows := byRow(parseFixture(t, c.variant).Records)
		r, ok := rows[c.w.table+"/"+itoa(c.w.row)]
		if !ok {
			t.Errorf("%s %s/%d: no record", c.variant, c.w.table, c.w.row)
			continue
		}
		m, _ := rawOf(r)["media"].(map[string]any)
		atts, _ := r.Payload["attachments"].([]any)
		var att map[string]any
		if len(atts) == 1 {
			att, _ = atts[0].(map[string]any)
		}
		if (att != nil) != c.w.hasAtt {
			t.Errorf("%s %s/%d: attachments %s", c.variant, c.w.table, c.w.row, jsonText(r.Payload["attachments"]))
			continue
		}
		gotName, _ := att["name"].(string)
		if gotName != c.w.name {
			t.Errorf("%s %s/%d: attachment name %q, want %q", c.variant, c.w.table, c.w.row, gotName, c.w.name)
		}
		fn, _ := m["file_name"].(string)
		fs, _ := m["file_name_state"].(string)
		if fn != c.w.name || fs != c.w.state {
			t.Errorf("%s %s/%d: raw.media file_name %q state %q, want %q and %q", c.variant, c.w.table, c.w.row, fn, fs, c.w.name, c.w.state)
		}
	}

	// R73: both names stay raw, in wire order, and none is the attachment's
	m := rawOf(v181["messages_v2/73"])["media"].(map[string]any)
	if jsonText(m["file_names"]) != `["a.txt","b.txt"]` || jsonText(m["file_names_total"]) != "2" {
		t.Errorf("R73: file_names %s total %v, want [a.txt b.txt] and 2", jsonText(m["file_names"]), m["file_names_total"])
	}

	// R76 (a live photo): no attachment, the document's id and mime type are raw, its file name is nowhere as a name
	r76 := v181["messages_v2/76"]
	if _, has := r76.Payload["attachments"]; has {
		t.Errorf("R76: a live photo has attachments %s", jsonText(r76.Payload["attachments"]))
	}
	m = rawOf(r76)["media"].(map[string]any)
	if jsonText(m["live_photo_document_id"]) != "9201" || m["live_photo_mime_type"] != "video/mp4" {
		t.Errorf("R76: live_photo_document_id %v, live_photo_mime_type %v", m["live_photo_document_id"], m["live_photo_mime_type"])
	}
	if strings.Contains(jsonText(r76.Payload), "live.mov") {
		t.Errorf("R76: the live photo's filename reached the record: %s", jsonText(r76.Payload))
	}

	// R77 (a link preview): no attachment, the preview document's name is raw.media.document_file_name
	r77 := v181["messages_v2/77"]
	if _, has := r77.Payload["attachments"]; has {
		t.Errorf("R77: a link preview has attachments %s", jsonText(r77.Payload["attachments"]))
	}
	if m = rawOf(r77)["media"].(map[string]any); m["document_file_name"] != "bundle.zip" || m["file_name"] != nil {
		t.Errorf("R77: document_file_name %v, file_name %v", m["document_file_name"], m["file_name"])
	}

	// R78: alt_documents is counted, its names never read
	if m = rawOf(v181["messages_v2/78"])["media"].(map[string]any); jsonText(m["alt_documents_total"]) != "1" || strings.Contains(jsonText(v181["messages_v2/78"].Payload), "alt.mp4") {
		t.Errorf("R78: alt_documents_total %v, or alt.mp4 reached the record", m["alt_documents_total"])
	}

	// H2601...: a record cut inside or before the attributes list has no name and says it is undecided; one cut after
	// it keeps the name
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	for id := int64(2614); id <= 2636; id++ {
		r := hostile["messages_v2/"+itoa(id)]
		m, _ := rawOf(r)["media"].(map[string]any)
		if m["file_name_state"] != "undecided" || m["file_name"] != nil {
			t.Errorf("H%d: file_name %v state %v, want undecided", id, m["file_name"], m["file_name_state"])
		}
		if atts, _ := r.Payload["attachments"].([]any); len(atts) == 1 {
			if a, _ := atts[0].(map[string]any); a["name"] != nil {
				t.Errorf("H%d: attachment name %v from an undecided list", id, a["name"])
			}
		}
	}
	if m, _ := rawOf(hostile["messages_v2/2637"])["media"].(map[string]any); m["file_name"] != "report.pdf" {
		t.Errorf("H2637: file_name %v, want report.pdf (the list was read whole)", m["file_name"])
	}

	// H2814: nine distinct names are a conflict that keeps eight in raw and counts nine
	m, _ = rawOf(hostile["messages_v2/2814"])["media"].(map[string]any)
	names, _ := m["file_names"].([]any)
	if m["file_name_state"] != "conflict" || len(names) != 8 || jsonText(m["file_names_total"]) != "9" || m["file_name"] != nil {
		t.Errorf("H2814: state %v, %d names, total %v; want a conflict of 8 kept and 9 counted", m["file_name_state"], len(names), m["file_names_total"])
	}
}
