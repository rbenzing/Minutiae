package iossms_test

import (
	"slices"
	"strings"
	"testing"
)

const plistHead = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0">`

func xmlPlist(body string) []byte { return []byte(plistHead + body + `</plist>`) }

// archive is an NSKeyedArchiver document whose root object is an instance of class.
func archive(class string) []byte {
	return xmlPlist(`<dict><key>$archiver</key><string>NSKeyedArchiver</string><key>$version</key><integer>100000</integer>` +
		`<key>$top</key><dict><key>root</key><dict><key>CF$UID</key><integer>1</integer></dict></dict>` +
		`<key>$objects</key><array><string>$null</string>` +
		`<dict><key>$class</key><dict><key>CF$UID</key><integer>2</integer></dict><key>title</key><dict><key>CF$UID</key><integer>3</integer></dict></dict>` +
		`<dict><key>$classname</key><string>` + class + `</string><key>$classes</key><array><string>` + class + `</string><string>NSObject</string></array></dict>` +
		`<string>Example</string></array></dict>`)
}

// 4E-R18: the keys, the edit count and the root class; a corrupt document costs one warning, never the message.
func TestSummaryInfoAndPayloadData(t *testing.T) {
	d := newSMS(t, "message", "handle")
	edits := xmlPlist(`<dict><key>ec</key><dict><key>0</key><array><dict/><dict/></array><key>1</key><array><dict/></array></dict><key>otr</key><dict/><key>amc</key><integer>1</integer></dict>`)
	d.msg(1, map[string]any{"message_summary_info": edits, "payload_data": archive("LPLinkMetadata")})
	d.msg(2, map[string]any{"payload_data": []byte("bplist00\xd1\x01\x02\xff"), "message_summary_info": []byte("not a plist")})
	d.msg(3, map[string]any{"message_summary_info": xmlPlist(`<dict><key>b</key><integer>1</integer><key>a</key><integer>2</integer><key>a</key><integer>3</integer></dict>`)})
	d.msg(4, map[string]any{"message_summary_info": xmlPlist(`<dict><key>ec</key><array/></dict>`)})
	d.msg(5, map[string]any{"message_summary_info": xmlPlist(`<array><integer>1</integer></array>`)})
	d.msg(6, map[string]any{"payload_data": xmlPlist(`<dict><key>$archiver</key><string>NSKeyedArchiver</string><key>$version</key><integer>100000</integer><key>$top</key><dict><key>root</key><dict><key>CF$UID</key><integer>1</integer></dict></dict><key>$objects</key><array><string>$null</string><string>plain</string></array></dict>`)})
	d.msg(7, nil)
	res := d.run(t)

	raw := rawOfRec(rec(t, res, 1))
	si, _ := raw["summary_info"].(map[string]any)
	if !slices.Equal(anyStrs(si["keys"]), []string{"amc", "ec", "otr"}) || si["duplicate_keys"] != nil {
		t.Errorf("summary_info = %v", raw["summary_info"])
	}
	if !is(raw["edit_count"], 3) {
		t.Errorf("edit_count = %v, want 3", raw["edit_count"])
	}
	if pd, _ := raw["payload_data"].(map[string]any); pd["root_class"] != "LPLinkMetadata" || raw["payload_data_status"] != nil {
		t.Errorf("payload_data = %v status %v", raw["payload_data"], raw["payload_data_status"])
	}

	// corrupt documents: a status each, exactly one warning each, the message still emitted
	r := rec(t, res, 2)
	raw = rawOfRec(r)
	if raw["payload_data_status"] != "undecodable" || raw["payload_data"] != nil {
		t.Errorf("corrupt payload_data: raw %v", raw)
	}
	if raw["message_summary_info_status"] != "undecodable" || raw["summary_info"] != nil || raw["edit_count"] != nil {
		t.Errorf("corrupt message_summary_info: raw %v", raw)
	}
	if r.Body != "m2" {
		t.Errorf("a corrupt plist cost the message its body: %q", r.Body)
	}
	var w2 []string
	for _, w := range res.Warnings {
		if w.Locator == "sqlite:table=message;rowid=2" {
			w2 = append(w2, w.Reason)
		}
	}
	if len(w2) != 2 || !strings.Contains(w2[0]+w2[1], "payload_data") || !strings.Contains(w2[0]+w2[1], "message_summary_info") {
		t.Errorf("warnings of row 2: %v, want one per corrupt column", w2)
	}
	if len(res.Warnings) != 2 {
		t.Errorf("%d warnings %v, want the 2 of row 2 and none for the good rows", len(res.Warnings), res.Warnings)
	}

	// a duplicate key decodes leniently and is counted; the keys are the distinct ones, sorted
	si, _ = rawOfRec(rec(t, res, 3))["summary_info"].(map[string]any)
	if !slices.Equal(anyStrs(si["keys"]), []string{"a", "b"}) || !is(si["duplicate_keys"], 1) {
		t.Errorf("duplicate key: summary_info = %v", si)
	}
	// ec that is not a dictionary of arrays gives no count; a document that is not a dictionary has no keys
	raw = rawOfRec(rec(t, res, 4))
	if raw["edit_count"] != nil {
		t.Errorf("edit_count = %v from ec that is no dictionary", raw["edit_count"])
	}
	raw = rawOfRec(rec(t, res, 5))
	if raw["summary_info"] != nil || raw["message_summary_info_status"] != "not-a-dictionary" {
		t.Errorf("array document: raw %v", raw)
	}
	// a root that is not a class instance has no class to state
	raw = rawOfRec(rec(t, res, 6))
	if raw["payload_data"] != nil || raw["payload_data_status"] != "no-root-class" {
		t.Errorf("a plain string root: raw %v", raw)
	}
	raw = rawOfRec(rec(t, res, 7))
	for _, k := range []string{"summary_info", "payload_data", "payload_data_status", "message_summary_info_status", "edit_count"} {
		if raw[k] != nil {
			t.Errorf("a message without plists has raw.%s = %v", k, raw[k])
		}
	}
}

// Hostile plists are bounded: a document that expands past the limits is a status and a warning, not an
// allocation and not a lost message.
func TestHostilePlistIsBoundedNotFatal(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<dict>`)
	for range 70 {
		sb.WriteString(`<key>k</key><dict>`)
	}
	for range 70 {
		sb.WriteString(`</dict>`)
	}
	sb.WriteString(`</dict>`)
	d := newSMS(t, "message", "handle")
	d.msg(1, map[string]any{"message_summary_info": xmlPlist(sb.String()), "payload_data": archive("X")[:40]})
	res := d.run(t)
	if len(res.Records) != 1 {
		t.Fatalf("%d records", len(res.Records))
	}
	raw := rawOfRec(rec(t, res, 1))
	if raw["payload_data_status"] != "undecodable" {
		t.Errorf("truncated archive: raw %v", raw)
	}
	if len(res.Warnings) == 0 {
		t.Error("no warning for an undecodable payload")
	}
}
