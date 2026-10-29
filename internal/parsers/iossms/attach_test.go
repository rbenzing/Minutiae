package iossms_test

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// attach adds attachment row id (named n<id>, image/jpeg, 100 bytes) with the given filename to message msg.
func (d *memDB) attach(msg, id int64, filename any) {
	d.add("attachment", id, map[string]any{
		"guid": fmt.Sprintf("A%d", id), "filename": filename, "transfer_name": fmt.Sprintf("n%d", id),
		"mime_type": "image/jpeg", "total_bytes": int64(100),
	})
	d.join("message_attachment_join", map[string]any{"message_id": msg, "attachment_id": id})
}

// attachFixture is a bundle of the database in the backup of UDID1 (acquisition acq1) and a runner.
type attachFixture struct {
	t *testing.T
	h *parsertest.Harness
	b parsertest.Bundle
	n int
}

func newAttachFixture(t *testing.T, d *memDB) *attachFixture {
	h := parsertest.NewHarness(t)
	fx := parsertest.Fixture{Name: "mem", Files: map[string][]byte{"db": d.b.Bytes()}}
	return &attachFixture{t: t, h: h, b: fx.BundleIOSBackup(h, domain, rel)}
}

// add registers an extra artifact of the bundle under the next free role.
func (f *attachFixture) add(a parse.Artifact) parse.Artifact {
	f.n++
	f.b.Others[fmt.Sprintf("extra%d", f.n)] = a
	return a
}

// file adds a backup file of acquisition acq in the backup directory udid (the device of the artifact too).
func (f *attachFixture) file(acq, udid, dom, relPath string) parse.Artifact {
	f.t.Helper()
	data := []byte("bytes of " + udid + "/" + dom + "/" + relPath)
	return f.add(f.h.IOSArtifact(parsertest.AddIOSBackupFile(f.t, f.h.Case, acq, udid, dom, relPath, data), data, "ios:"+dom+"/"+relPath))
}

// fileInDir adds a backup file of device UDID1 and acquisition acq1 whose backup directory is dir (isolating
// the backup directory from the device and acquisition rules, 4E-R34).
func (f *attachFixture) fileInDir(dir, dom, relPath string) parse.Artifact {
	f.t.Helper()
	data := []byte("bytes in " + dir)
	id := parsertest.BackupFileID(dom, relPath)
	remote := dir + "/" + id[:2] + "/" + id
	src := evidence.Source{Kind: "backup", DeviceID: parsertest.IOSBackupDevice, RemotePath: remote}
	m, err := f.h.Case.Capture(parsertest.IOSBackupDevice, "acq1", "backup/"+remote, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(f.h.IOSArtifact(m, data, "ios:"+dom+"/"+relPath))
}

func (f *attachFixture) run() parsertest.Result {
	f.t.Helper()
	p := iossms.Parser{}
	res := f.h.RunLenient(p, f.b.Input(f.h, p, false))
	for _, v := range res.Violations {
		f.t.Errorf("contract violation: %v", v)
	}
	return res
}

func attOf(t *testing.T, r records.Record, i int) map[string]any {
	t.Helper()
	l, _ := r.Payload["attachments"].([]any)
	if i >= len(l) {
		t.Fatalf("attachment %d of %v", i, r.Payload["attachments"])
	}
	return l[i].(map[string]any)
}

const att = "Library/SMS/Attachments/"

// 4E-R19: only the message's own backup links; the filename forms, the inexpressible names and the counters.
func TestAttachmentLinksTheFileOfTheMessagesOwnBackupOnly(t *testing.T) {
	d := newSMS(t)
	d.dims()
	forms := []struct {
		filename any
		linked   bool
	}{
		{"~/" + att + "ab/12/a.jpeg", true},                  // present
		{"/var/mobile/" + att + "cd/34/b.heic", true},        // present, /var/mobile form
		{"/private/var/mobile/" + att + "ef/56/c.png", true}, // present as an extract-named copy (HomeDomain)
		{"~/" + att + "bb/22/missing.jpeg", false},           // missing
		{"~/" + att + "aa/11/a*b.png", false},                // '*' is inexpressible, though the file exists
		{"/etc/passwd", false},                               // another prefix
		{"~/" + att + "../x.png", false},                     // '..'
		{"~/" + att + "a//b.png", false},                     // an empty element
		{"~/" + strings.Repeat("e/", 70) + "x.png", false},   // over 64 elements
		{nil, false}, // no filename
	}
	for i := range forms {
		id := int64(i + 1)
		d.msg(id, nil)
		d.attach(id, id, forms[i].filename)
	}
	f := newAttachFixture(t, d)
	a1 := f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
	a2 := f.file("acq1", "UDID1", "MediaDomain", att+"cd/34/b.heic")
	a3 := f.file("acq1", "UDID1", "HomeDomain", att+"ef/56/c.png")
	f.file("acq1", "UDID1", "MediaDomain", att+"aa/11/a*b.png")
	res := f.run()

	want := []parse.Artifact{a1, a2, a3}
	for i, c := range forms {
		r := rec(t, res, int64(i+1))
		a := attOf(t, r, 0)
		raw := rawOfRec(r)
		if a["name"] != fmt.Sprintf("n%d", i+1) || a["mime"] != "image/jpeg" || !is(a["size"], 100) {
			t.Errorf("row %d: attachment %v", i+1, a)
		}
		if c.filename != nil && a["source_ref"] != c.filename {
			t.Errorf("row %d: source_ref %v, want %v", i+1, a["source_ref"], c.filename)
		}
		if c.linked {
			if a["artifact_id"] != want[i].ID || a["artifact_sha256"] != want[i].SHA256 {
				t.Errorf("row %d: linked %v/%v, want %s/%s", i+1, a["artifact_id"], a["artifact_sha256"], want[i].ID, want[i].SHA256)
			}
			if !is(raw["attachments_resolved"], 1) || !is(raw["attachments_unresolved"], 0) {
				t.Errorf("row %d: counters %v", i+1, raw)
			}
			continue
		}
		if a["artifact_id"] != nil || a["artifact_sha256"] != nil {
			t.Errorf("row %d (%v): linked to %v", i+1, c.filename, a["artifact_id"])
		}
		if !is(raw["attachments_resolved"], 0) || !is(raw["attachments_unresolved"], 1) {
			t.Errorf("row %d: counters %v", i+1, raw)
		}
		if raw["attachments_other_source"] != nil || raw["attachments_ambiguous"] != nil {
			t.Errorf("row %d: counters %v", i+1, raw)
		}
	}
	// a message with an attachment and no text is media; with a text it stays text
	d2 := newSMS(t)
	d2.msg(1, map[string]any{"text": nil})
	d2.msg(2, nil)
	d2.attach(1, 1, "~/"+att+"x")
	d2.attach(2, 2, "~/"+att+"y")
	res2 := newAttachFixture(t, d2).run()
	if k := rec(t, res2, 1).Payload["kind"]; k != "media" {
		t.Errorf("attachment without text: kind %v", k)
	}
	if k := rec(t, res2, 2).Payload["kind"]; k != "text" {
		t.Errorf("attachment with text: kind %v", k)
	}
}

// Review focus 2 (rulings T19, T25, T46): a file of another backup of the same device, of another device, or of
// an unknown acquisition is never the attachment of this message.
func TestAttachmentOfAnotherBackupOfTheSameDeviceIsNeverLinked(t *testing.T) {
	name := "~/" + att + "ab/12/a.jpeg"
	cases := map[string]struct {
		build func(f *attachFixture)
		want  string // the counter that states the unlinked attachment
	}{
		"second acquisition of the device": {func(f *attachFixture) { f.file("acq2", "UDID1", "MediaDomain", att+"ab/12/a.jpeg") }, "attachments_other_source"},
		"another device":                   {func(f *attachFixture) { f.file("acq-UDID2", "UDID2", "MediaDomain", att+"ab/12/a.jpeg") }, "attachments_other_source"},
		"two copies in the own backup": {func(f *attachFixture) {
			f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
			f.file("acq1", "UDID1", "HomeDomain", att+"ab/12/a.jpeg")
		}, "attachments_ambiguous"},
		"attachment acquisition unknown": {func(f *attachFixture) {
			a := f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
			a.Acquisition = ""
			f.b.Others["extra1"] = a
		}, "attachments_other_source"},
		"both acquisitions unknown": {func(f *attachFixture) {
			a := f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
			a.Acquisition = ""
			f.b.Others["extra1"] = a
			f.b.Primary.Acquisition = ""
		}, "attachments_other_source"},
		"database acquisition unknown": {func(f *attachFixture) {
			f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
			f.b.Primary.Acquisition = ""
		}, "attachments_other_source"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			d := newSMS(t)
			d.msg(1, nil)
			d.attach(1, 1, name)
			f := newAttachFixture(t, d)
			c.build(f)
			r := rec(t, f.run(), 1)
			if a := attOf(t, r, 0); a["artifact_id"] != nil || a["artifact_sha256"] != nil {
				t.Errorf("linked to %v", a["artifact_id"])
			}
			raw := rawOfRec(r)
			if !is(raw[c.want], 1) || !is(raw["attachments_resolved"], 0) || !is(raw["attachments_unresolved"], 0) {
				t.Errorf("raw %v, want %s = 1 and nothing resolved or unresolved", raw, c.want)
			}
		})
	}
	// with the own copy and one of another acquisition, only the own one links
	d := newSMS(t)
	d.msg(1, nil)
	d.attach(1, 1, name)
	f := newAttachFixture(t, d)
	f.file("acq2", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
	own := f.file("acq1", "UDID1", "HomeDomain", att+"ab/12/a.jpeg")
	if a := attOf(t, rec(t, f.run(), 1), 0); a["artifact_id"] != own.ID {
		t.Errorf("linked %v, want the own acquisition's %s", a["artifact_id"], own.ID)
	}
}

// 4E-R34: inside one acquisition and one device, the backup directory decides.
func TestAttachmentOfAnotherBackupDirIsNeverLinked(t *testing.T) {
	name := "~/" + att + "ab/12/a.jpeg"
	d := newSMS(t)
	d.msg(1, nil)
	d.attach(1, 1, name)

	f := newAttachFixture(t, d)
	other := f.fileInDir("UDID2", "MediaDomain", att+"ab/12/a.jpeg")
	own := f.fileInDir("UDID1", "HomeDomain", att+"ab/12/a.jpeg")
	if a := attOf(t, rec(t, f.run(), 1), 0); a["artifact_id"] != own.ID {
		t.Errorf("linked %v, want the copy of the database's backup dir %s (not %s)", a["artifact_id"], own.ID, other.ID)
	}

	f = newAttachFixture(t, d)
	f.fileInDir("UDID2", "MediaDomain", att+"ab/12/a.jpeg")
	r := rec(t, f.run(), 1)
	if a := attOf(t, r, 0); a["artifact_id"] != nil {
		t.Errorf("linked %v from another backup dir", a["artifact_id"])
	}
	if raw := rawOfRec(r); !is(raw["attachments_other_source"], 1) {
		t.Errorf("raw %v", raw)
	}

	// a file that is not a backup file next to a backup database is another kind of source
	f = newAttachFixture(t, d)
	a := f.file("acq1", "UDID1", "MediaDomain", att+"ab/12/a.jpeg")
	a.Source.Kind = "file"
	f.b.Others["extra1"] = a
	if a := attOf(t, rec(t, f.run(), 1), 0); a["artifact_id"] != nil {
		t.Errorf("a non-backup artifact linked to a backup database: %v", a["artifact_id"])
	}
}

// An undecidable join is unknown: the table is named, no attachment is invented and the kind is not media.
func TestUndecidableAttachmentJoinIsUnknownNotEmpty(t *testing.T) {
	const rows = 800
	d := newSMS(t)
	for i := int64(1); i <= rows; i++ {
		d.msg(i, map[string]any{"text": nil})
		d.attach(i, i, "~/"+att+"x")
	}
	leaves := d.tbl["message_attachment_join"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	data := d.b.Bytes()
	ps := d.b.PageSize()
	page := data[int(leaves[1]-1)*ps:][:ps]
	page[3], page[4] = 0, 0
	res := runBytes(t, data)
	if len(res.Records) != rows {
		t.Fatalf("%d records, want all %d", len(res.Records), rows)
	}
	for i := int64(1); i <= rows; i++ {
		r := rec(t, res, i)
		if !slices.Equal(anyStrs(rawOfRec(r)["join_undecided"]), []string{"message_attachment_join"}) {
			t.Errorf("row %d: join_undecided = %v", i, rawOfRec(r)["join_undecided"])
		}
		if r.Payload["kind"] == "media" {
			t.Errorf("row %d: kind media from an undecided join", i)
		}
		if l, ok := r.Payload["attachments"].([]any); ok && len(l) == 0 {
			t.Errorf("row %d: an empty attachments list from an undecided join", i)
		}
	}
}

// A missing join or attachment table, and a join row without an attachment row, are stated.
func TestAttachmentGapsAreStated(t *testing.T) {
	d := newSMS(t, "message", "handle")
	d.msg(1, nil)
	raw := rawOfRec(rec(t, d.run(t), 1))
	if raw["attachments_state"] != "no-join-table" {
		t.Errorf("no join table: raw %v", raw)
	}

	d = newSMS(t, "message", "message_attachment_join")
	d.msg(1, nil)
	d.join("message_attachment_join", map[string]any{"message_id": int64(1), "attachment_id": int64(9)})
	raw = rawOfRec(rec(t, d.run(t), 1))
	if raw["attachments_state"] != "no-attachment-table" {
		t.Errorf("no attachment table: raw %v", raw)
	}

	d = newSMS(t)
	d.msg(1, nil)
	d.join("message_attachment_join", map[string]any{"message_id": int64(1), "attachment_id": int64(9)})
	r := rec(t, d.run(t), 1)
	raw = rawOfRec(r)
	if !is(raw["attachments_no_row"], 1) || r.Payload["attachments"] != nil {
		t.Errorf("dangling attachment id: payload %v", r.Payload)
	}
}
