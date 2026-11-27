package androidtelegram_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
)

// Spec 12 (text, identifiers, lists), C4, F11, B9: every cap is counted or stated, never a silent cut.
//   - Body (R53 of telegram-v181, 300 KiB of text) is cut at 256 KiB with body_truncated and body_total_bytes;
//   - a reply copy's text of 10 KiB is cut at 4 KiB and named reply_copy.text in raw.text_flags;
//   - an attach path of 10 KiB is cut at 4 KiB with attach_path_flags truncated;
//   - 100 attach params keep 64 with attach_params_total 100 and attach_params_truncated;
//   - a 2 KiB username and a 2 KiB phone are identifiers, never cut: absent from the payload, with <key>_state
//     too-long, <key>_len and <key>_sha256 of the stored bytes;
//   - 70 distinct top constructors name 64 in constructors_seen, then other:6, the whole within its 1,000-byte cut
//     (F11);
//   - 1,500 users without a name, a phone or a username give 1,000 warnings and one summary of the other 500 (as
//     Task 12), every record kept.
func TestTelegramCapsAreCountedNeverSilent(t *testing.T) {
	// Body: R53
	r53 := byRow(parseFixture(t, "telegram-v181").Records)["messages_v2/53"]
	if r53.Payload["body_truncated"] != true || jsonText(r53.Payload["body_total_bytes"]) != "307205" ||
		len(r53.Body) != 256<<10 || !strings.HasPrefix(r53.Body, "long xxx") {
		t.Errorf("R53: body_truncated %v, body_total_bytes %v, Body of %d bytes; want the 256 KiB cut stated",
			r53.Payload["body_truncated"], r53.Payload["body_total_bytes"], len(r53.Body))
	}

	// the reply copy, the attach path and the attach params of Go-built rows
	long := strings.Repeat("r", 10<<10)
	msg := func(id int32, tail []byte) []byte {
		return tlMessage{id: id, from: idAnn, peer: idSelf, date: baseDate, text: "x", tail: tail}.bytes()
	}
	reply := tlMessage{id: 9, from: idSelf, peer: idAnn, date: baseDate, text: long}.bytes()
	v2 := []msgCells{
		{mid: 1, uid: idAnn, data: msg(1, nil), replydata: reply},
		{mid: 2, uid: idAnn, data: msg(2, tlStr(strings.Repeat("p", 10<<10)))},
		{mid: 3, uid: idAnn, data: msg(3, tlStr(params(100)+"/sdcard/p.jpg"))},
	}
	tooLong := strings.Repeat("u", 2<<10)
	phone := strings.Repeat("7", 2<<10)
	first := "Name"
	users := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: []builtRow{
		{idAnn, map[string]any{"name": "x", "status": int64(0), "data": tlUser(idAnn, 0, false, &first, nil, &tooLong, &phone)}},
	}}
	res := parseAt(t, remoteApp, dbOnly(goBuiltWith(181, v2, users)))
	if res.Err != nil || len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("Parse: err %v, violations %v, refused %d", res.Err, res.Violations, res.Refused)
	}
	recs := byRow(res.Records)
	raw := rawOf(recs["messages_v2/1"])
	cp, _ := raw["reply_copy"].(map[string]any)
	if text, _ := cp["text"].(string); text != long[:4<<10] || !strings.Contains(fmt.Sprint(raw["text_flags"]), "reply_copy.text") {
		t.Errorf("reply copy: text of %d bytes, text_flags %v; want 4 KiB and reply_copy.text named", len(text), raw["text_flags"])
	}
	raw = rawOf(recs["messages_v2/2"])
	if p, _ := raw["attach_path"].(string); len(p) != 4<<10 || raw["attach_path_flags"] != "truncated" {
		t.Errorf("attach path: %d bytes, attach_path_flags %v; want 4 KiB and truncated", len(p), raw["attach_path_flags"])
	}
	raw = rawOf(recs["messages_v2/3"])
	list, _ := raw["attach_params"].([]any)
	if len(list) != 64 || jsonText(raw["attach_params_total"]) != "100" || raw["attach_params_truncated"] != true || raw["attach_path"] != "/sdcard/p.jpg" {
		t.Errorf("attach params: %d kept, total %v, truncated %v, path %v; want 64 of 100 stated", len(list), raw["attach_params_total"], raw["attach_params_truncated"], raw["attach_path"])
	}
	var c records.Record
	for _, r := range res.Records {
		if r.Type == "contact" {
			c = r
		}
	}
	raw = rawOf(c)
	for key, stored := range map[string]string{"username": tooLong, "phone": phone} {
		if raw[key+"_state"] != "too-long" || jsonText(raw[key+"_len"]) != "2048" || raw[key+"_sha256"] != sha256Hex(stored) {
			t.Errorf("%s of 2 KiB: state %v, len %v, sha256 %v; want too-long with the stored bytes' length and digest", key, raw[key+"_state"], raw[key+"_len"], raw[key+"_sha256"])
		}
	}
	if c.Payload["phones"] != nil || strings.Contains(jsonText(c.Payload), tooLong[:1100]) || strings.Contains(jsonText(c.Payload), phone[:1100]) {
		t.Error("an identifier over its limit reached the payload, whole or cut")
	}
	for _, e := range imsOf(c) {
		if e["kind"] != "telegram" {
			t.Errorf("ims %v: a username over its limit is never an entry", e)
		}
	}

	// constructors_seen: 70 distinct top constructors
	var seen []msgCells
	for i := range 70 {
		seen = append(seen, msgCells{mid: int64(i + 1), uid: idAnn, data: append(le32(0x0bad0000+uint32(i)), make([]byte, 8)...)})
	}
	cs := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, seen))).Notes["constructors_seen"]
	entries := strings.Split(cs, ",")
	if len(entries) != 65 || entries[0] != "0x0bad0000:1" || entries[63] != "0x0bad003f:1" || entries[64] != "other:6" || len(cs) > 1000 {
		t.Errorf("constructors_seen %q (%d entries, %d bytes); want 64 entries then other:6 within 1,000 bytes", cs, len(entries), len(cs))
	}
	if !utf8.ValidString(cs) || strings.HasSuffix(cs, ",…") {
		t.Errorf("constructors_seen %q was cut: 64 entries and other fit within its bound (F11)", cs)
	}

	// 1,500 users without a name, a phone or a username (as Task 12)
	var rows []builtRow
	for i := range 1500 {
		rows = append(rows, builtRow{int64(i + 1), map[string]any{"name": "x", "status": int64(0)}})
	}
	big := parseAt(t, remoteApp, dbOnly(goBuiltWith(181, nil, builtTable{
		name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: rows,
	})))
	one, summary := 0, ""
	for _, w := range big.Warnings {
		switch {
		case strings.HasPrefix(w.Reason, "users row has no name, phone or username:"):
			summary = w.Reason
		case w.Reason == "users row has no name, phone or username":
			one++
		}
	}
	if one != 1000 || summary != "users row has no name, phone or username: 1500 records, 500 not warned one by one" || len(contactsOf(big)) != 1500 {
		t.Errorf("%d warnings, summary %q, %d contacts; want 1000, the 1500/500 summary and every record", one, summary, len(contactsOf(big)))
	}
	mediaCaps4I4b(t)
	tailCaps4I4b(t)
	markupCaps4I4c(t)
	pollCaps4I4c(t)
}

// imsOf is a contact's ims entries.
func imsOf(r records.Record) []map[string]any {
	var out []map[string]any
	list, _ := r.Payload["ims"].([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// mediaCaps4I4b is plan 4I4b Task 5's part of TestTelegramCapsAreCountedNeverSilent (design D5/P3 and the plan's Raw shapes caps): the media lists and names are cut at their caps and the
// cut is counted, never silent. H2801 has 17 sizes (16 kept, total 17, truncated), H2813 33 attributes (32 kept), H2814
// nine distinct filenames (8 kept, 9 counted).
func mediaCaps4I4b(t *testing.T) {
	t.Helper()
	hostile := byRow(parseFixture(t, "telegram-hostile").Records)
	raw := rawOf(hostile["messages_v2/2801"])
	if l, _ := raw["media_sizes"].([]any); len(l) != 16 || jsonText(raw["media_sizes_total"]) != "17" || raw["media_sizes_truncated"] != true {
		t.Errorf("H2801: %d sizes kept, total %v, truncated %v; want 16 of 17 stated", len(l), raw["media_sizes_total"], raw["media_sizes_truncated"])
	}
	m, _ := rawOf(hostile["messages_v2/2813"])["media"].(map[string]any)
	if l, _ := m["attributes"].([]any); len(l) != 32 || jsonText(m["attributes_total"]) != "33" || m["attributes_truncated"] != true {
		t.Errorf("H2813: %d attributes kept, total %v, truncated %v; want 32 of 33 stated", len(l), m["attributes_total"], m["attributes_truncated"])
	}
	m, _ = rawOf(hostile["messages_v2/2814"])["media"].(map[string]any)
	if l, _ := m["file_names"].([]any); len(l) != 8 || jsonText(m["file_names_total"]) != "9" {
		t.Errorf("H2814: %d file names kept, total %v; want 8 of 9 stated", len(l), m["file_names_total"])
	}
}
