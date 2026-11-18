package ioswhatsapp_test

import (
	"encoding/base64"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// countLookup is a Lookuper that records every glob it is asked for.
type countLookup struct {
	parse.Lookuper
	mu    sync.Mutex
	globs []string
}

func (c *countLookup) Find(g string) []parse.Artifact {
	c.mu.Lock()
	c.globs = append(c.globs, g)
	c.mu.Unlock()
	return c.Lookuper.Find(g)
}

func (c *countLookup) asked(sub string) bool {
	for _, g := range c.globs {
		if strings.Contains(g, sub) {
			return true
		}
	}
	return false
}

func extraAt(domain, rel, content, device string) parsertest.ExtraFile {
	return parsertest.ExtraFile{Domain: domain, RelativePath: "Message/" + rel, ContentB64: base64.StdEncoding.EncodeToString([]byte(content)), Device: device}
}

// runMedia parses a Go-built store at the consumer backup domain with the extras beside it and a counting Lookuper.
func runMedia(t *testing.T, db []byte, extras []parsertest.ExtraFile) (map[int64]records.Record, parsertest.Result, parsertest.Bundle, *countLookup) {
	t.Helper()
	h := parsertest.NewHarness(t)
	fx := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}, Expect: parsertest.Expect{ExtraFiles: extras}}
	b := fx.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	in := b.Input(h, p, false)
	cl := &countLookup{Lookuper: in.Lookup}
	in.Lookup = cl
	res := runLenient(t, h, p, in, "whatsapp-ios-join-undecided")
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		out[rowidOf(r.Locator)] = r
	}
	return out, res, b, cl
}

// imageDB builds a store of image messages 1..n whose media rows have the given stored paths (nil: a NULL path).
func imageDB(paths ...any) []byte {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		for i, p := range paths {
			pk := int64(i + 1)
			tb["ZWAMESSAGE"].put(pk, msgRow(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil}))
			tb["ZWAMEDIAITEM"].put(pk, map[string]any{"ZMESSAGE": pk, "ZMEDIALOCALPATH": p, "ZVCARDSTRING": "image/jpeg", "ZFILESIZE": int64(10)})
		}
	})
	return db
}

func attachmentsOf(r records.Record) []any {
	a, _ := r.Payload["attachments"].([]any)
	return a
}

func counter(r records.Record, k string) any { return rawOf(r)[k] }

// parseFixture parses a committed variant with a counting Lookuper.
func parseFixture(t *testing.T, variant string) (map[int64]records.Record, parsertest.Bundle, *countLookup) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	h := parsertest.NewHarness(t)
	b := bundleOf(h, fx, variant)
	p := ioswhatsapp.Parser{}
	in := b.Input(h, p, false)
	cl := &countLookup{Lookuper: in.Lookup}
	in.Lookup = cl
	res := runLenient(t, h, p, in, "wal-frames-not-applied", "whatsapp-ios-join-undecided")
	recs := map[int64]records.Record{}
	for _, r := range res.Records {
		recs[rowidOf(r.Locator)] = r
	}
	return recs, b, cl
}

// 4I2-R6, spec 6.7: a file is linked only by exactly one same-source file at <container>/Message/<ZMEDIALOCALPATH>.
func TestIOSWhatsAppChatsAttachmentLinksExactlyOneSameSourceMatch(t *testing.T) {
	recs, b, _ := parseFixture(t, "ios-chatstorage")
	const img4 = "Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg"
	link, ok := b.ByPath["ios:"+domainConsumer+"/Message/"+img4]
	if !ok {
		t.Fatalf("the fixture's IMG-0004 extra is not in the bundle: %v", b.ByPath)
	}
	a4 := attachmentsOf(recs[4])
	if len(a4) != 1 {
		t.Fatalf("message 4 has %d attachments", len(a4))
	}
	m, _ := a4[0].(map[string]any)
	if m["artifact_id"] != link.ID || m["artifact_sha256"] != link.SHA256 || m["source_ref"] != img4 || m["mime"] != "image/jpeg" {
		t.Errorf("message 4 attachment %v, want a link to %s (%s)", m, link.ID, link.SHA256)
	}
	if _, has := m["name"]; has {
		t.Errorf("an iOS attachment has no name: %v", m)
	}
	if counter(recs[4], "attachments_resolved") != int64(1) || counter(recs[4], "attachments_unresolved") != int64(0) {
		t.Errorf("message 4 counters: %v", rawOf(recs[4]))
	}
	// row 43: the only file at that path is of another device: counted, never linked
	a43, _ := attachmentsOf(recs[43])[0].(map[string]any)
	if _, linked := a43["artifact_id"]; linked || counter(recs[43], "attachments_other_source") != int64(1) || counter(recs[43], "attachments_resolved") != int64(0) {
		t.Errorf("message 43: %v %v", a43, rawOf(recs[43]))
	}
	for _, n := range []int64{9, 28} { // no file at all
		a, _ := attachmentsOf(recs[n])[0].(map[string]any)
		if _, linked := a["artifact_id"]; linked || counter(recs[n], "attachments_unresolved") != int64(1) || counter(recs[n], "attachments_resolved") != int64(0) {
			t.Errorf("message %d: %v %v", n, a, rawOf(recs[n]))
		}
	}
}

// R6: two files of the database's own source at one logical path are counted and never chosen, by the whole parser.
func TestIOSWhatsAppChatsTwoFilesAtOnePathAreNotChosenBetween(t *testing.T) {
	rel := "Media/x/IMG.jpg"
	h := parsertest.NewHarness(t)
	fx := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": imageDB(rel)}, Expect: parsertest.Expect{ExtraFiles: []parsertest.ExtraFile{extraAt(domainConsumer, rel, "one", "")}}}
	b := fx.BundleIOSBackup(h, domainConsumer, dbName)
	// the second file: another capture of the same logical path, from the same backup directory and acquisition
	data := []byte("two")
	src := evidence.Source{Kind: "backup", DeviceID: parsertest.IOSBackupDevice, RemotePath: parsertest.IOSBackupDevice + "/zz/second"}
	m, err := h.Case.Capture(parsertest.IOSBackupDevice, "acq1", "backup/"+src.RemotePath, src, func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		t.Fatal(err)
	}
	b.Others["extraSecond"] = h.IOSArtifact(m, data, "ios:"+domainConsumer+"/Message/"+rel)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false))
	if len(res.Records) != 1 {
		t.Fatalf("%d records", len(res.Records))
	}
	r := res.Records[0]
	a, _ := attachmentsOf(r)[0].(map[string]any)
	if _, linked := a["artifact_id"]; linked || counter(r, "attachments_ambiguous") != int64(1) || counter(r, "attachments_resolved") != int64(0) || counter(r, "attachments_unresolved") != int64(0) {
		t.Errorf("an ambiguous pair: %v %v", a, rawOf(r))
	}
}

// 4I2-R6: the media row of a message whose directions disagree is never taken: no attachment and no media field.
func TestIOSWhatsAppChatsMediaJoinDisagreementIsUndecided(t *testing.T) {
	recs, _, _ := parseFixture(t, "ios-chatstorage")
	r := recs[29]
	raw := rawOf(r)
	if !reflect.DeepEqual(raw["join_undecided"], []any{"ZWAMEDIAITEM"}) || len(attachmentsOf(r)) != 0 {
		t.Errorf("message 29: %v", r.Payload)
	}
	for _, k := range []string{"ZFILESIZE", "ZMETADATA", "media_state", "attachments_resolved", "attachments_unresolved", "media_key_present", "ZTITLE", "body_column"} {
		if _, has := raw[k]; has {
			t.Errorf("message 29 holds %s: %v", k, raw)
		}
	}
	// a database of one such message: exactly one warning, with the plan's text (4I2-P7), and no lookup
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil, "ZMEDIAITEM": int64(2)}))
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": int64(1), "ZMEDIALOCALPATH": "Media/x.jpg", "ZVCARDSTRING": "image/jpeg"})
		tb["ZWAMEDIAITEM"].put(2, map[string]any{"ZMESSAGE": int64(7), "ZMEDIALOCALPATH": "Media/y.jpg", "ZVCARDSTRING": "image/jpeg"})
	})
	got, res, _, cl := runMedia(t, db, []parsertest.ExtraFile{extraAt(domainConsumer, "Media/x.jpg", "x", "")})
	if len(attachmentsOf(got[1])) != 0 || cl.asked("Media/x.jpg") || len(cl.globs) != 0 {
		t.Errorf("disagreeing directions: %v, finds %q", got[1].Payload, cl.globs)
	}
	var join []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "whatsapp-ios-join-undecided") {
			join = append(join, w.Reason)
		}
	}
	if len(join) != 1 || !strings.Contains(join[0], "whatsapp-ios-join-undecided: ZWAMEDIAITEM: 1 records") {
		t.Errorf("join warnings %q", join)
	}
}

// 4I2-R6: a path that cleaning changed (NUL, invalid UTF-8, a cut) or that holds '*' is never looked up, not even as its
// cleaned text: it is counted as undecided, never as unresolved.
func TestIOSWhatsAppChatsMediaPathThatCleaningChangedIsNeverLooked(t *testing.T) {
	cut := "Media/" + strings.Repeat("a", 300<<10) + ".jpg" // over the text cap: cut by cleaning
	db := imageDB("Media/a\x00b.jpg", "Media/a\xffb.jpg", cut, "Media/a*b.jpg", "Media/**/x.jpg")
	recs, _, _, cl := runMedia(t, db, nil)
	if len(cl.globs) != 0 {
		t.Errorf("a Find was made for a path no glob can express: %.200q", cl.globs)
	}
	for n := int64(1); n <= 5; n++ {
		r := recs[n]
		if rawOf(r)["media_state"] != "lookup-path-not-expressible" || counter(r, "attachments_lookup_undecided") != int64(1) ||
			counter(r, "attachments_unresolved") != int64(0) || counter(r, "attachments_resolved") != int64(0) || len(attachmentsOf(r)) != 1 {
			t.Errorf("message %d: %.300v", n, rawOf(r))
		}
	}
	// the committed fixture's row 30 (a path with '*')
	fr, _, fcl := parseFixture(t, "ios-chatstorage")
	if rawOf(fr[30])["media_state"] != "lookup-path-not-expressible" || counter(fr[30], "attachments_lookup_undecided") != int64(1) {
		t.Errorf("message 30: %v", rawOf(fr[30]))
	}
	if fcl.asked("a*b") {
		t.Errorf("row 30's path was looked up: %q", fcl.globs)
	}
}

// Refused paths and a missing path are never looked up; a message without any media row has no attachment.
func TestIOSWhatsAppChatsRefusedPathsAreNeverLooked(t *testing.T) {
	deep := strings.TrimSuffix(strings.Repeat("e/", 64), "/")
	db := imageDB("/var/mobile/x.jpg", "../x.jpg", "Media/./x.jpg", "Media//x.jpg", `Media\x.jpg`, deep, nil, "")
	recs, _, _, cl := runMedia(t, db, nil)
	if len(cl.globs) != 0 {
		t.Errorf("a Find was made for a refused or empty path: %.200q", cl.globs)
	}
	for n := int64(1); n <= 6; n++ {
		r := recs[n]
		if rawOf(r)["media_state"] != "lookup-path-refused" || counter(r, "attachments_unresolved") != int64(1) || counter(r, "attachments_resolved") != int64(0) || counter(r, "attachments_lookup_undecided") != nil {
			t.Errorf("message %d: %.300v", n, rawOf(r))
		}
	}
	for _, n := range []int64{7, 8} {
		r := recs[n]
		if rawOf(r)["media_state"] != "no-local-path" || counter(r, "attachments_unresolved") != int64(1) {
			t.Errorf("message %d (no path): %v", n, rawOf(r))
		}
	}
	c, _, fcl := parseFixture(t, "ios-chatstorage")
	if rawOf(c[31])["media_state"] != "lookup-path-refused" || counter(c[31], "attachments_unresolved") != int64(1) ||
		rawOf(c[32])["media_state"] != "no-local-path" ||
		rawOf(c[33])["media_state"] != "no-ZWAMEDIAITEM-row" || len(attachmentsOf(c[33])) != 0 {
		t.Errorf("fixture rows 31-33: %v %v %v", rawOf(c[31]), rawOf(c[32]), rawOf(c[33]))
	}
	if fcl.asked("escape") {
		t.Errorf("row 31's path was looked up: %q", fcl.globs)
	}
}

// [ ] ? { } are literal to the case glob and are never refused: the file at such a path is found.
func TestIOSWhatsAppChatsOtherGlobCharactersAreLiteral(t *testing.T) {
	rel := "Media/[a]?{b}.jpg"
	recs, _, b, _ := runMedia(t, imageDB(rel), []parsertest.ExtraFile{extraAt(domainConsumer, rel, "x", "")})
	r := recs[1]
	a, _ := attachmentsOf(r)[0].(map[string]any)
	want := b.ByPath["ios:"+domainConsumer+"/Message/"+rel]
	if want.ID == "" || a["artifact_id"] != want.ID || counter(r, "attachments_resolved") != int64(1) || rawOf(r)["media_state"] != nil {
		t.Errorf("attachment %v, raw %v", a, rawOf(r))
	}
}

// Review Focus 1: an attachment resolves only inside its own container.
func TestIOSWhatsAppChatsMediaNeverCrossesContainers(t *testing.T) {
	const rel = "Media/15550000001@s.whatsapp.net/a/b/IMG-0004.jpg"
	// (a) a consumer database; the only copy of the file is under the Business domain of the same backup
	recs, _, _, _ := runMedia(t, imageDB(rel), []parsertest.ExtraFile{extraAt(domainBusiness, rel, "biz", "")})
	r := recs[1]
	a, _ := attachmentsOf(r)[0].(map[string]any)
	if _, linked := a["artifact_id"]; linked || counter(r, "attachments_unresolved") != int64(1) || counter(r, "attachments_resolved") != int64(0) || counter(r, "attachments_other_source") != nil {
		t.Errorf("a file of the Business container: %v %v", a, rawOf(r))
	}
	// (b) two app-group containers, each with the same relative media path: each database links its own file only
	const root = "Containers/Shared/AppGroup/"
	h := parsertest.NewHarness(t)
	put := func(id, rel string, data []byte) parse.Artifact {
		remote := "/private/var/mobile/" + root + id + "/" + rel
		src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: remote}
		m, err := h.Case.Capture("IOS1", "acq1", strings.TrimPrefix(remote, "/"), src, func(w io.Writer) error { _, err := w.Write(data); return err })
		if err != nil {
			t.Fatal(err)
		}
		return h.IOSArtifact(m, data, "ios:HomeDomain/"+root+id+"/"+rel)
	}
	db := imageDB(rel)
	dbA, dbB := put("AAAA", dbName, db), put("BBBB", dbName, db)
	fileA, fileB := put("AAAA", "Message/"+rel, []byte("file of A")), put("BBBB", "Message/"+rel, []byte("file of B"))
	for _, c := range []struct {
		name              string
		primary, other    parse.Artifact
		want, notWantedID parse.Artifact
	}{{"A", dbA, dbB, fileA, fileB}, {"B", dbB, dbA, fileB, fileA}} {
		others := map[string]parse.Artifact{"extra0": fileA, "extra1": fileB, "otherdb": c.other}
		p := ioswhatsapp.Parser{}
		res := runLenient(t, h, p, h.Input(p, c.primary, others, false))
		if len(res.Records) != 1 {
			t.Fatalf("%s: %d records", c.name, len(res.Records))
		}
		got, _ := attachmentsOf(res.Records[0])[0].(map[string]any)
		if got["artifact_id"] != c.want.ID || got["artifact_id"] == c.notWantedID.ID || counter(res.Records[0], "attachments_resolved") != int64(1) || counter(res.Records[0], "attachments_ambiguous") != nil {
			t.Errorf("database %s: attachment %v, want the file %s only (not %s)", c.name, got, c.want.ID, c.notWantedID.ID)
		}
	}
}

// 4I2-P6: only the file types make an attachment; the other types keep their media row's columns in raw.
func TestIOSWhatsAppChatsNonFileTypesHaveNoAttachment(t *testing.T) {
	c := chatFixture(t, "ios-chatstorage")
	for _, n := range []int64{7, 8} {
		r := c[n]
		raw := rawOf(r)
		if len(attachmentsOf(r)) != 0 || raw["attachments_resolved"] != nil || raw["attachments_unresolved"] != nil || raw["media_state"] != nil {
			t.Errorf("message %d: %v", n, r.Payload)
		}
	}
	if rawOf(c[7])["body_column"] != "ZVCARDSTRING" || rawOf(c[8])["location"] == nil {
		t.Errorf("the media raw of rows 7 and 8 is missing: %v %v", rawOf(c[7]), rawOf(c[8]))
	}
	const rel = "Media/link.bin"
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZMESSAGETYPE": int64(7), "ZTEXT": "see this"}))
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": int64(1), "ZMEDIALOCALPATH": rel, "ZVCARDSTRING": "text/x", "ZFILESIZE": int64(99)})
	})
	recs, _, _, cl := runMedia(t, db, []parsertest.ExtraFile{extraAt(domainConsumer, rel, "x", "")})
	r := recs[1]
	raw := rawOf(r)
	if len(attachmentsOf(r)) != 0 || raw["attachments_resolved"] != nil || raw["attachments_unresolved"] != nil || raw["media_state"] != nil || cl.asked(rel) {
		t.Errorf("type 7: %v, finds %q", r.Payload, cl.globs)
	}
	if raw["ZFILESIZE"] != int64(99) {
		t.Errorf("type 7 media raw: %v", raw)
	}
}

// A hostile backup can put the database in an app group whose id holds "**" inside one element: classifyPlace accepts it and
// so does the primary glob, but the case glob of the container's files is not compilable. The lookup is refused (never read as
// "no file", never a Find), counted unresolved as the plan's vocabulary says of a refused lookup.
func TestIOSWhatsAppChatsContainerThatNoGlobCanNameIsRefused(t *testing.T) {
	h := parsertest.NewHarness(t)
	db := imageDB("Media/x/IMG.jpg")
	// the remote path (device metadata) holds the "**"; the file in the case does not need to
	const logical = "ios:HomeDomain/Containers/Shared/AppGroup/x**y/ChatStorage.sqlite"
	src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: "/private/var/mobile/Containers/Shared/AppGroup/x**y/ChatStorage.sqlite"}
	m, err := h.Case.Capture("IOS1", "acq1", "appgroup-xy/ChatStorage.sqlite", src, func(w io.Writer) error { _, err := w.Write(db); return err })
	if err != nil {
		t.Fatal(err)
	}
	primary := h.IOSArtifact(m, db, logical)
	p := ioswhatsapp.Parser{}
	in := h.Input(p, primary, nil, false)
	cl := &countLookup{Lookuper: in.Lookup}
	in.Lookup = cl
	res := runLenient(t, h, p, in, "whatsapp-ios-join-undecided")
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	r := res.Records[0]
	if rawOf(r)["media_state"] != "lookup-path-refused" || counter(r, "attachments_unresolved") != int64(1) || counter(r, "attachments_resolved") != int64(0) || len(attachmentsOf(r)) != 1 {
		t.Errorf("raw %v", rawOf(r))
	}
	if len(cl.globs) != 0 {
		t.Errorf("a Find was made under a container no glob can name: %q", cl.globs)
	}
}
