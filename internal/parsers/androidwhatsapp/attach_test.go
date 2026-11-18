package androidwhatsapp_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// The attachment scenario of plan 4I task 11 (spec 6.7, R5, R5a, R6, 4I-R14): android-modern-2026 sits at
// /data/user/10/com.whatsapp/databases (user 10) and holds rows 80 to 88, one image each, beside the files named in
// EXTRA_FILES of the generator. The facts below are written from the rules.

// parseWithBundle parses the fixture with the given changes to its oracle's extra files and returns the bundle.
func parseWithBundle(t *testing.T, variant, remote string, tweak func(*parsertest.Fixture)) (map[string]records.Record, parsertest.Bundle, parsertest.Result) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	if tweak != nil {
		tweak(&fx)
	}
	h := parsertest.NewHarness(t)
	b := fx.Bundle(h, remote)
	p := androidwhatsapp.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	out := map[string]records.Record{}
	for _, r := range res.Records {
		out[keyOf(r)] = r
	}
	return out, b, res
}

func attachmentsOf(t *testing.T, r records.Record) []map[string]any {
	t.Helper()
	list, _ := r.Payload["attachments"].([]any)
	var out []map[string]any
	for _, a := range list {
		out = append(out, a.(map[string]any))
	}
	return out
}

// linked is the artifact the first attachment is linked to, "" when it is not.
func linked(t *testing.T, r records.Record) string {
	t.Helper()
	if a := attachmentsOf(t, r); len(a) > 0 {
		id, _ := a[0]["artifact_id"].(string)
		return id
	}
	return ""
}

func counter(t *testing.T, r records.Record, key string) string {
	t.Helper()
	return fmt.Sprint(rawMap(t, r)[key])
}

const (
	imgDir = "/WhatsApp/Media/WhatsApp Images/"
	pathA  = "/storage/emulated/10" + imgDir + "IMG-A.jpg"
	pathF  = "/sdcard" + imgDir + "IMG-F.jpg"
	pathG  = "/storage/emulated/10" + imgDir + "IMG-G.jpg"
)

// R5, R6: exactly one file of the database's own source, app and user is a link, by path alone. The stored hash is raw as
// stored and never compared. A legacy and an Android 11 copy together are ambiguous; a copy from another device is
// another source.
func TestWhatsAppAttachmentLinksExactlyOneSameSourceMatch(t *testing.T) {
	recs, b, _ := parseWithBundle(t, "android-modern-2026", remoteUser10, nil)
	r80 := recs["message/80"]
	atts := attachmentsOf(t, r80)
	if len(atts) != 1 || atts[0]["artifact_id"] != b.ByPath[pathA].ID || atts[0]["artifact_sha256"] != b.ByPath[pathA].SHA256 {
		t.Fatalf("message 80: attachments %v, want one linked to %s", atts, pathA)
	}
	a := atts[0]
	if a["name"] != "IMG-A.jpg" || a["mime"] != "image/jpeg" || fmt.Sprint(a["size"]) != "12345" || a["source_ref"] != "Media/WhatsApp Images/IMG-A.jpg" {
		t.Errorf("message 80: attachment %v", a)
	}
	raw := rawMap(t, r80)
	if counter(t, r80, "attachments_resolved") != "1" || counter(t, r80, "attachments_unresolved") != "0" || raw["attachments_ambiguous"] != nil {
		t.Errorf("message 80: raw %v", raw)
	}
	// R6: the stored hash is raw as stored, and differs from the file's own hash without blocking the link
	if raw["file_hash"] != "hash-IMG-A.jpg" || b.ByPath[pathA].SHA256 == "hash-IMG-A.jpg" {
		t.Errorf("message 80: raw.file_hash %v", raw["file_hash"])
	}
	if raw["media_key_present"] != true || fmt.Sprint(raw["file_length"]) != "12346" || fmt.Sprint(raw["transferred"]) != "1" || raw["direct_path"] != "/v/t62/IMG-A.jpg" {
		t.Errorf("message 80: raw %v", raw)
	}
	for _, v := range raw {
		if s, ok := v.(string); ok && s == base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}) {
			t.Errorf("the media key bytes are in raw")
		}
	}

	// 81: a legacy and an Android 11 copy of the same user: never a choice
	r81 := recs["message/81"]
	if a := attachmentsOf(t, r81); len(a) != 1 || a[0]["artifact_id"] != nil || counter(t, r81, "attachments_ambiguous") != "1" || counter(t, r81, "attachments_resolved") != "0" {
		t.Errorf("message 81: attachments %v raw %v", a, rawMap(t, r81))
	}
	// 82: the only copy came from another device
	r82 := recs["message/82"]
	if a := attachmentsOf(t, r82); len(a) != 1 || a[0]["artifact_id"] != nil || counter(t, r82, "attachments_other_source") != "1" {
		t.Errorf("message 82: attachments %v raw %v", a, rawMap(t, r82))
	}
	// 86: an absolute path under a media root of the database's own user
	r86 := recs["message/86"]
	if a := attachmentsOf(t, r86); len(a) != 1 || a[0]["artifact_id"] != b.ByPath[pathG].ID || a[0]["source_ref"] != pathG {
		t.Errorf("message 86: attachments %v", a)
	}
	// 87: a path with .. cannot be looked up: source_ref only, counted
	r87 := recs["message/87"]
	if a := attachmentsOf(t, r87); len(a) != 1 || a[0]["artifact_id"] != nil || counter(t, r87, "attachments_unresolved") != "1" {
		t.Errorf("message 87: attachments %v raw %v", a, rawMap(t, r87))
	}
	// 88: a media row that holds nothing to name: no attachment, the stored size 0 stays in raw
	r88 := recs["message/88"]
	if a := attachmentsOf(t, r88); len(a) != 0 || fmt.Sprint(rawMap(t, r88)["file_size"]) != "0" || rawMap(t, r88)["attachments_resolved"] != nil {
		t.Errorf("message 88: attachments %v raw %v", a, rawMap(t, r88))
	}
	// a media message with no message_media row states it
	if raw := rawMap(t, recs["message/4"]); raw["media_state"] != "no-message_media-row" || len(attachmentsOf(t, recs["message/4"])) != 0 {
		t.Errorf("message 4 (an image with no media row): raw %v", raw)
	}
	// a contact card and a location are never media files
	for _, k := range []string{"message/7", "message/8"} {
		if raw := rawMap(t, recs[k]); raw["media_state"] != nil || len(attachmentsOf(t, recs[k])) != 0 {
			t.Errorf("%s: raw %v attachments %v", k, raw, attachmentsOf(t, recs[k]))
		}
	}
	// a revoke is a tombstone: no attachment
	for _, k := range []string{"message/30", "message/31"} {
		if len(attachmentsOf(t, recs[k])) != 0 {
			t.Errorf("%s: a tombstone has attachments", k)
		}
	}
}

// R5, Review Focus: a database never links a file of the other app, and the other app's copy is neither ambiguous nor
// counted as another user: it is simply not a candidate.
func TestWhatsAppAttachmentNeverCrossesApps(t *testing.T) {
	recs, _, _ := parseWithBundle(t, "android-modern-2026", remoteUser10, nil)
	r83 := recs["message/83"]
	raw := rawMap(t, r83)
	if a := attachmentsOf(t, r83); len(a) != 1 || a[0]["artifact_id"] != nil {
		t.Errorf("message 83: attachments %v, the file exists only under WhatsApp Business", a)
	}
	if counter(t, r83, "attachments_unresolved") != "1" || raw["attachments_ambiguous"] != nil || raw["attachments_other_user"] != nil || raw["attachments_other_source"] != nil || raw["attachments_user_undecided"] != nil {
		t.Errorf("message 83: raw %v", raw)
	}
}

// R5: a database links only media of its own Android user: a copy only under another user is counted and not linked.
func TestWhatsAppAttachmentOnlySameAndroidUser(t *testing.T) {
	recs, _, _ := parseWithBundle(t, "android-modern-2026", remoteUser10, nil)
	r84 := recs["message/84"]
	if a := attachmentsOf(t, r84); len(a) != 1 || a[0]["artifact_id"] != nil || counter(t, r84, "attachments_other_user") != "1" || counter(t, r84, "attachments_resolved") != "0" {
		t.Errorf("message 84: attachments %v raw %v", a, rawMap(t, r84))
	}
	// the same database at user 0 links the user 0 copy and counts the user 10 ones as another user
	recs0, b0, _ := parseWithBundle(t, "android-modern-2026", remoteLive, nil)
	pathE := "/storage/emulated/0" + imgDir + "IMG-E.jpg"
	if a := attachmentsOf(t, recs0["message/84"]); len(a) != 1 || a[0]["artifact_id"] != b0.ByPath[pathE].ID {
		t.Errorf("user 0, message 84: attachments %v", a)
	}
	if r := recs0["message/80"]; linked(t, r) != "" || counter(t, r, "attachments_other_user") != "1" {
		t.Errorf("user 0, message 80: attachments %v raw %v", attachmentsOf(t, r), rawMap(t, r))
	}
}

// R5a: a media path that names no user (/sdcard) pairs with the database only when the case holds exactly one live
// database of the app on the device; a second database (another user), a second acquisition of the same path, or no way
// to show it leaves the file unlinked and counted.
func TestWhatsAppAttachmentUserUndecidedNotLinked(t *testing.T) {
	// (a) the only live database: linked
	recs, b, _ := parseWithBundle(t, "android-modern-2026", remoteLive, nil)
	if a := attachmentsOf(t, recs["message/85"]); len(a) != 1 || a[0]["artifact_id"] != b.ByPath[pathF].ID {
		t.Errorf("(a) message 85: attachments %v, want the /sdcard file linked (the only live database)", a)
	}
	// the same database at user 10 also links it: the rule does not depend on the user
	recs10, b10, _ := parseWithBundle(t, "android-modern-2026", remoteUser10, nil)
	if a := attachmentsOf(t, recs10["message/85"]); len(a) != 1 || a[0]["artifact_id"] != b10.ByPath[pathF].ID {
		t.Errorf("user 10, message 85: attachments %v", a)
	}

	undecided := func(name string, tweak func(*parsertest.Fixture)) {
		t.Helper()
		recs, _, _ := parseWithBundle(t, "android-modern-2026", remoteLive, tweak)
		r := recs["message/85"]
		if a := attachmentsOf(t, r); len(a) != 1 || a[0]["artifact_id"] != nil || counter(t, r, "attachments_user_undecided") != "1" || counter(t, r, "attachments_resolved") != "0" {
			t.Errorf("%s: attachments %v raw %v, want unlinked and counted as user-undecided", name, a, rawMap(t, r))
		}
		// a path that names its user is not affected by the second database
		if r := recs["message/84"]; linked(t, r) == "" {
			t.Errorf("%s: message 84 (user 0 media of a user 0 database) lost its link", name)
		}
	}
	db := base64.StdEncoding.EncodeToString([]byte("another database"))
	// (b) a live database of the same app for another user
	undecided("(b) a second user's database", func(fx *parsertest.Fixture) {
		fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles, parsertest.ExtraFile{RemotePath: remoteUser10, ContentB64: db})
	})
	// (c) a second acquisition holding the same database path
	undecided("(c) a second acquisition", func(fx *parsertest.Fixture) {
		fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles, parsertest.ExtraFile{RemotePath: remoteLive, ContentB64: db, Acquisition: "acq2"})
	})
	// a database of the Business app does not count: the app is the package
	recsB, bB, _ := parseWithBundle(t, "android-modern-2026", remoteLive, func(fx *parsertest.Fixture) {
		fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles, parsertest.ExtraFile{RemotePath: remoteBusiness, ContentB64: db})
	})
	if a := attachmentsOf(t, recsB["message/85"]); len(a) != 1 || a[0]["artifact_id"] != bB.ByPath[pathF].ID {
		t.Errorf("a Business database beside it: attachments %v, want the link kept", a)
	}
}

// R6: the stored hash never decides a link: row 80's file_hash is not the hash of the file it links, and the link is made;
// the hash is raw as stored on every media row, whether or not the file was found.
func TestWhatsAppStoredHashNeverDecidesLink(t *testing.T) {
	recs, b, _ := parseWithBundle(t, "android-modern-2026", remoteUser10, nil)
	r80 := recs["message/80"]
	if linked(t, r80) != b.ByPath[pathA].ID || rawMap(t, r80)["file_hash"] != "hash-IMG-A.jpg" || b.ByPath[pathA].SHA256 == "hash-IMG-A.jpg" {
		t.Errorf("message 80: linked %q raw.file_hash %v", linked(t, r80), rawMap(t, r80)["file_hash"])
	}
	for _, id := range []string{"81", "82", "83", "84", "87"} {
		r := recs["message/"+id]
		if want := "hash-" + attachmentsOf(t, r)[0]["name"].(string); rawMap(t, r)["file_hash"] != want {
			t.Errorf("message %s: raw.file_hash %v, want %s as stored", id, rawMap(t, r)["file_hash"], want)
		}
	}
	// the legacy columns: media_hash is raw.file_hash as stored, and a legacy attachment has no path to link
	legacy, _ := recordsOf(t, "android-legacy-2.11")
	l4 := legacy["messages/4"]
	if a := attachmentsOf(t, l4); len(a) != 1 || a[0]["source_ref"] != nil || a[0]["artifact_id"] != nil || a[0]["name"] != "WA0004" || fmt.Sprint(a[0]["size"]) != "1004" {
		t.Errorf("legacy message 4: attachments %v", a)
	}
	if raw := rawMap(t, l4); raw["file_hash"] != "H4" || raw["media_url"] != "https://mmg.example/4" || counter(t, l4, "attachments_unresolved") != "1" {
		t.Errorf("legacy message 4: raw %v", raw)
	}
	// a legacy text row, contact card or location has no attachment
	for _, id := range []string{"2", "7", "8"} {
		if a := attachmentsOf(t, legacy["messages/"+id]); len(a) != 0 {
			t.Errorf("legacy message %s: attachments %v", id, a)
		}
	}
}
