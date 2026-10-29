package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// iosE2EFile is one file of a backup built for the end-to-end test.
type iosE2EFile struct{ domain, rel string }

// iosBackupBuilder adds the backups of one acquisition to a case.
type iosBackupBuilder struct {
	t   *testing.T
	c   *evidence.Case
	acq string
}

func (b *iosBackupBuilder) put(udid, name string, data []byte) evidence.ManifestRecord {
	b.t.Helper()
	rec, err := b.c.Capture(udid, b.acq, "backup/"+udid+"/"+name,
		evidence.Source{Kind: "backup", DeviceID: udid, RemotePath: udid + "/" + name},
		func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		b.t.Fatal(err)
	}
	return rec
}

// backup stores the files (domain, relative path, content) of one backup directory udid with a Manifest.db that
// names exactly them (the fileID is the sha1 rule) and the given Manifest.plist. It returns the manifest records
// of the data files by their logical path.
func (b *iosBackupBuilder) backup(udid, plist string, files map[iosE2EFile][]byte) map[string]evidence.ManifestRecord {
	b.t.Helper()
	if _, err := b.c.Audit.Append("acquire.start", udid, map[string]any{"acquisition_id": b.acq, "type": "ios.backup"}); err != nil {
		b.t.Fatal(err)
	}
	keys := make([]iosE2EFile, 0, len(files))
	for f := range files {
		keys = append(keys, f)
	}
	slices.SortFunc(keys, func(x, y iosE2EFile) int { return strings.Compare(x.domain+"/"+x.rel, y.domain+"/"+y.rel) })
	out := map[string]evidence.ManifestRecord{}
	sb := sqlitetest.New(sqlitetest.Options{})
	tbl := sb.CreateTable("Files", "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)")
	for i, f := range keys {
		id := parsertest.BackupFileID(f.domain, f.rel)
		tbl.Insert(int64(i+1), id, f.domain, f.rel, int64(1), nil)
		out["ios:"+f.domain+"/"+f.rel] = b.put(udid, id[:2]+"/"+id, files[f])
	}
	b.put(udid, "Manifest.db", sb.Bytes())
	b.put(udid, "Manifest.plist", []byte(plist))
	b.put(udid, "Status.plist", []byte("status"))
	return out
}

type iosE2ERun struct {
	Code    int
	Out     string
	Err     string
	Summary struct {
		Class   string    `json:"class"`
		Records int64     `json:"records"`
		Jobs    []jobJSON `json:"jobs"`
	}
}

// parseRunReal runs `parse run --json` with the REAL registry (no injected parsers).
func parseRunReal(t *testing.T, dir string, extra ...string) iosE2ERun {
	t.Helper()
	var r iosE2ERun
	r.Code, r.Out, r.Err = runP(t, nil, append([]string{"parse", "run", "--case", dir, "--json"}, extra...)...)
	lines := strings.Split(strings.TrimSpace(r.Out), "\n")
	var ev struct {
		Event string `json:"event"`
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if json.Unmarshal([]byte(lines[i]), &ev) == nil && ev.Event == "summary" {
			if err := json.Unmarshal([]byte(lines[i]), &r.Summary); err != nil {
				t.Fatal(err)
			}
			return r
		}
	}
	t.Fatalf("no summary event (exit %d):\n%s\n%s", r.Code, r.Out, r.Err)
	return r
}

func extraFilesOf(t *testing.T, fx parsertest.Fixture, device string) map[iosE2EFile][]byte {
	t.Helper()
	out := map[iosE2EFile][]byte{}
	for _, x := range fx.Expect.ExtraFiles {
		if x.Device != device {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(x.ContentB64)
		if err != nil {
			t.Fatal(err)
		}
		out[iosE2EFile{x.Domain, x.RelativePath}] = data
	}
	return out
}

var hashedRemote = regexp.MustCompile(`^UDID[0-9]/[0-9a-f]{2}/[0-9a-f]{40}$`)

type e2eMessage struct {
	Guid        string `json:"guid"`
	Attachments []struct {
		ArtifactID string `json:"artifact_id"`
		SHA256     string `json:"artifact_sha256"`
		Name       string `json:"name"`
	} `json:"attachments"`
}

// A parse run over an unencrypted iOS backup names the files from the backup's Manifest.db and runs the three iOS
// parsers over them (plus a second sms.db that comes with its -wal): the records of every committed oracle are
// present, an attachment is found through the host's Lookup and carries the attachment artifact's id, a
// SourcePath is the hashed remote path, the audit names the ios-backup namer, a second run skips everything, and
// a second, encrypted acquisition is reported unparsed (exit 1) without touching the first.
func TestIOSBackupEndToEnd(t *testing.T) {
	smsDir := filepath.Join("..", "parsers", "iossms", "testdata")
	nanos := parsertest.LoadFixture(t, smsDir, "ios17-nanos")
	walfx := parsertest.LoadFixture(t, smsDir, "ios17-wal")
	calls := parsertest.LoadFixture(t, filepath.Join("..", "parsers", "ioscalls", "testdata"), "coredata-current")
	ab := parsertest.LoadFixture(t, filepath.Join("..", "parsers", "iosaddressbook", "testdata"), "with-images")

	c := recordstest.NewCase(t)
	b := &iosBackupBuilder{t: t, c: c, acq: "A1"}
	main := extraFilesOf(t, nanos, "")
	main[iosE2EFile{"HomeDomain", "Library/SMS/sms.db"}] = nanos.Files["db"]
	main[iosE2EFile{"HomeDomain", "Library/CallHistoryDB/CallHistory.storedata"}] = calls.Files["db"]
	main[iosE2EFile{"HomeDomain", "Library/AddressBook/AddressBook.sqlitedb"}] = ab.Files["db"]
	main[iosE2EFile{"HomeDomain", "Library/AddressBook/AddressBookImages.sqlitedb"}] = ab.Files["images"]
	// UDID1 is the backup directory the oracles use, so their literal SourcePaths apply
	mainRecs := b.backup("UDID1", cliPlistNo, main)
	walRecs := b.backup("UDID3", cliPlistNo, map[iosE2EFile][]byte{
		{"HomeDomain", "Library/SMS/sms.db"}:     walfx.Files["db"],
		{"HomeDomain", "Library/SMS/sms.db-wal"}: walfx.Files["wal"],
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	first := parseRunReal(t, dir)
	if first.Code != 0 || first.Summary.Class != "ok" {
		t.Fatalf("exit %d class %q:\n%s\n%s", first.Code, first.Summary.Class, first.Out, first.Err)
	}
	wantJobs := map[string]int64{"ios.sms": int64(len(nanos.Expect.Records)) + int64(len(walfx.Expect.Records)), "ios.calls": int64(len(calls.Expect.Records)), "ios.addressbook": int64(len(ab.Expect.Records))}
	gotJobs := map[string]int64{}
	for _, j := range first.Summary.Jobs {
		if j.Outcome != "complete" {
			t.Errorf("job %s on %s: outcome %q (%s)", j.Parser, j.Logical, j.Outcome, j.Reason)
		}
		gotJobs[j.Parser] += j.Records
	}
	if len(first.Summary.Jobs) != 4 || !maps.Equal(gotJobs, wantJobs) {
		t.Errorf("%d jobs, records by parser %v, want 4 jobs and %v", len(first.Summary.Jobs), gotJobs, wantJobs)
	}

	// the records, read back from the case
	rc, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := records.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string][]records.Full{} // by parser name
	for cursor := ""; ; {
		res, err := rd.List(context.Background(), records.Filter{}, records.Page{Limit: records.MaxLimit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range res.Rows {
			f, err := rd.Get(context.Background(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			all[row.Parser.Name] = append(all[row.Parser.Name], f)
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	for name, want := range wantJobs {
		if int64(len(all[name])) != want {
			t.Errorf("%s: %d records in the case, the oracles hold %d", name, len(all[name]), want)
		}
	}
	for name, fs := range all {
		for _, f := range fs {
			if !hashedRemote.MatchString(f.SourcePath) {
				t.Errorf("%s record %d: SourcePath %q is not a hashed backup remote path", name, f.ID, f.SourcePath)
			}
			if f.Recovered {
				t.Errorf("%s record %d is flagged recovered in a live backup parse", name, f.ID)
			}
		}
	}

	// every record of the nanos oracle, by guid and source path; the attachments through the host's Lookup
	byGuid := map[string]records.Full{}
	walSMS := 0
	for _, f := range all["ios.sms"] {
		var p e2eMessage
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			t.Fatal(err)
		}
		byGuid[f.SourcePath+"|"+p.Guid] = f
		if strings.HasPrefix(f.SourcePath, "UDID3/") {
			walSMS++
		}
	}
	attached := 0
	for _, want := range nanos.Expect.Records {
		g, _ := want.Payload["guid"].(string)
		f, ok := byGuid[want.SourcePath+"|"+g]
		if !ok {
			t.Errorf("oracle record %s (rowid %d, %s) is not in the case", g, want.Rowid, want.SourcePath)
			continue
		}
		var p e2eMessage
		_ = json.Unmarshal(f.Payload, &p)
		wantAtt, _ := want.Payload["attachments"].([]any)
		if len(wantAtt) != len(p.Attachments) {
			t.Errorf("%s: %d attachments, oracle %d", g, len(p.Attachments), len(wantAtt))
			continue
		}
		for i, wa := range wantAtt {
			m, _ := wa.(map[string]any)
			ph, _ := m["artifact_id"].(string)
			if ph == "" {
				if p.Attachments[i].ArtifactID != "" {
					t.Errorf("%s: attachment %s resolved to %q, the oracle has no artifact", g, p.Attachments[i].Name, p.Attachments[i].ArtifactID)
				}
				continue
			}
			logical := strings.TrimPrefix(ph, "@id:")
			art, ok := mainRecs[logical]
			if !ok {
				t.Fatalf("the oracle names %s, which the test did not store", logical)
			}
			if p.Attachments[i].ArtifactID != art.ID || p.Attachments[i].SHA256 != art.SHA256 {
				t.Errorf("%s: attachment %s is %s/%s, want the artifact %s/%s that the case holds at %s",
					g, p.Attachments[i].Name, p.Attachments[i].ArtifactID, p.Attachments[i].SHA256, art.ID, art.SHA256, logical)
			}
			attached++
		}
	}
	if attached == 0 {
		t.Error("no resolved attachment was checked: the test would pass vacuously")
	}
	// the backup with a -wal: the records of its oracle, from the database AND the -wal its Manifest.db names
	if walSMS != len(walfx.Expect.Records) || walRecs["ios:HomeDomain/Library/SMS/sms.db-wal"].ID == "" {
		t.Errorf("the backup with a -wal holds %d sms records, oracle %d", walSMS, len(walfx.Expect.Records))
	}

	// the audit: every member of a job was named by ios-backup from a Manifest.db
	entries, err := rc.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range entries {
		if e.Action != artparse.ActionParseJobStart {
			continue
		}
		starts++
		js, err := evidence.DecodeDetails[artparse.JobStart](e.Details)
		if err != nil {
			t.Fatal(err)
		}
		for role, m := range js.Bundle {
			if m.Namer != "ios-backup" || m.NamerSource == nil {
				t.Errorf("job %d member %s: namer %q source %v, want ios-backup with its Manifest.db", js.Job, role, m.Namer, m.NamerSource)
			}
		}
	}
	if starts != 4 {
		t.Errorf("%d parse.job.start entries, want 4", starts)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	// a second run: everything skipped
	second := parseRunReal(t, dir)
	if second.Code != 0 || len(second.Summary.Jobs) != 4 {
		t.Fatalf("second run: exit %d, %d jobs:\n%s\n%s", second.Code, len(second.Summary.Jobs), second.Out, second.Err)
	}
	for _, j := range second.Summary.Jobs {
		if j.Outcome != "skipped" {
			t.Errorf("second run: job %s is %q, want skipped", j.Parser, j.Outcome)
		}
	}

	// a second acquisition whose Manifest.plist says encrypted
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b2 := &iosBackupBuilder{t: t, c: c2, acq: "A2"}
	b2.backup("UDID9", cliPlistYes, map[iosE2EFile][]byte{{"HomeDomain", "Library/SMS/sms.db"}: nanos.Files["db"]})
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	third := parseRunReal(t, dir)
	if third.Code != 1 || !strings.Contains(third.Out+third.Err, cliEncReason) {
		t.Fatalf("third run: exit %d, want 1 naming %q:\n%s\n%s", third.Code, cliEncReason, third.Out, third.Err)
	}
	if third.Summary.Class != "partial" || third.Summary.Records != 0 {
		t.Errorf("third run: class %q records %d, want partial and 0 new records", third.Summary.Class, third.Summary.Records)
	}
	unparsed := 0
	for _, j := range third.Summary.Jobs {
		switch {
		case strings.Contains(j.Reason, cliEncReason):
			unparsed++
			if j.Outcome != "unparsed" {
				t.Errorf("third run: the encrypted backup's job is %q, want unparsed", j.Outcome)
			}
		case j.Outcome != "skipped":
			t.Errorf("third run: job %s on %s is %q: the first acquisition's jobs must be unaffected", j.Parser, j.Logical, j.Outcome)
		}
	}
	if unparsed != 1 || len(third.Summary.Jobs) != 5 {
		t.Errorf("third run: %d jobs, %d unparsed for the encrypted backup, want 5 and 1", len(third.Summary.Jobs), unparsed)
	}
}
