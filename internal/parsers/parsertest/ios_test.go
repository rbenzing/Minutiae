package parsertest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"

	"github.com/rbenzing/minutiae/internal/parse"
)

const (
	testIOSRemote  = "/private/var/mobile/Library/Safari/History.db"
	testIOSLogical = "ios:HomeDomain/Library/Safari/History.db"
)

func TestBundleIOSMakesAnIOSArtifact(t *testing.T) {
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("main-db"), "wal": []byte("wal-bytes"), "journal": []byte("journal-bytes")}}
	b := fx.BundleIOS(NewHarness(t), testIOSRemote, testIOSLogical)

	for role, want := range map[string]struct{ remote, logical, data string }{
		"db":      {testIOSRemote, testIOSLogical, "main-db"},
		"wal":     {testIOSRemote + "-wal", testIOSLogical + "-wal", "wal-bytes"},
		"journal": {testIOSRemote + "-journal", testIOSLogical + "-journal", "journal-bytes"},
	} {
		a, ok := b.ByRole[role]
		if !ok {
			t.Errorf("%s: no artifact in ByRole", role)
			continue
		}
		if a.Platform != parse.PlatformIOS || a.Logical != want.logical || a.Source.RemotePath != want.remote {
			t.Errorf("%s: platform %q logical %q remote %q, want ios %q %q", role, a.Platform, a.Logical, a.Source.RemotePath, want.logical, want.remote)
		}
		if a.Source.Kind != "file" || a.Source.DeviceID != "IOS1" || a.Acquisition == "" {
			t.Errorf("%s: source %+v acquisition %q", role, a.Source, a.Acquisition)
		}
		got := make([]byte, a.Size)
		if n, err := a.R.ReadAt(got, 0); err != nil && (!errors.Is(err, io.EOF) || n != len(got)) {
			t.Errorf("%s: read: %v", role, err)
		} else if string(got) != want.data {
			t.Errorf("%s: bytes %q, want %q", role, got, want.data)
		}
	}
	if b.Primary.ID != b.ByRole["db"].ID || b.Others["wal"].ID != b.ByRole["wal"].ID || b.Others["journal"].ID != b.ByRole["journal"].ID {
		t.Errorf("Primary and Others do not point at the ByRole artifacts")
	}
	if len(b.Others) != 2 {
		t.Errorf("Others holds %d artifacts, want wal and journal", len(b.Others))
	}
}

func TestBundleIOSWithoutCompanionsHasNone(t *testing.T) {
	b := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}}.BundleIOS(NewHarness(t), testIOSRemote, testIOSLogical)
	if len(b.Others) != 0 || len(b.ByRole) != 1 {
		t.Errorf("Others %v ByRole %v, want only the database", b.Others, b.ByRole)
	}
}

func TestBuildBundleIOSRefusesAnEmptyRemotePathAndExtraFiles(t *testing.T) {
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}}
	if _, err := fx.BuildBundleIOS(NewHarness(t), "", testIOSLogical); !errors.Is(err, ErrEmptyRemotePath) {
		t.Errorf("empty remote path: err = %v, want ErrEmptyRemotePath", err)
	}
	fx.Expect.ExtraFiles = []ExtraFile{{RemotePath: "/x", ContentB64: "AA=="}}
	if _, err := fx.BuildBundleIOS(NewHarness(t), testIOSRemote, testIOSLogical); err == nil {
		t.Error("extra files were silently dropped")
	}
}

func TestIOSArtifactIsAnIOSArtifactOfTheManifestRecord(t *testing.T) {
	h := NewHarness(t)
	m := AddAndroidFile(t, h.Case, "acq1", "/private/var/x.db", []byte("zz"))
	a := h.IOSArtifact(m, []byte("zz"), "ios:HomeDomain/x.db")
	if a.Platform != parse.PlatformIOS || a.Logical != "ios:HomeDomain/x.db" || a.ID != m.ID || a.SHA256 != m.SHA256 || a.Size != 2 {
		t.Errorf("artifact %+v", a)
	}
}

const (
	testBackupDomain  = "HomeDomain"
	testBackupRel     = "Library/SMS/sms.db"
	testAttDomain     = "MediaDomain"
	testAttRel        = "Library/SMS/Attachments/aa/x.jpg"
	testAttLogical    = "ios:MediaDomain/Library/SMS/Attachments/aa/x.jpg"
	testBackupLogical = "ios:HomeDomain/Library/SMS/sms.db"
)

func backupFixture() Fixture {
	return Fixture{
		Name:  "x",
		Files: map[string][]byte{"db": []byte("main-db"), "wal": []byte("wal-bytes"), "images": []byte("images-db")},
		Expect: Expect{ExtraFiles: []ExtraFile{
			{Domain: testAttDomain, RelativePath: testAttRel, ContentB64: base64.StdEncoding.EncodeToString([]byte("jpeg"))},
		}},
	}
}

func TestBackupFileIDIsSHA1OfDomainAndPath(t *testing.T) {
	const id = "3d0d7e5fb2ce288813306e4d4636395e047a3d28"
	if got := BackupFileID("HomeDomain", "Library/SMS/sms.db"); got != id {
		t.Errorf("BackupFileID = %s, want %s", got, id)
	}
	if got, want := BackupRemotePath("UDID1", "HomeDomain", "Library/SMS/sms.db"), "UDID1/3d/"+id; got != want {
		t.Errorf("BackupRemotePath = %s, want %s", got, want)
	}
}

func TestBundleIOSBackupPlacesFilesAsABackup(t *testing.T) {
	b := backupFixture().BundleIOSBackup(NewHarness(t), testBackupDomain, testBackupRel)
	p := b.Primary
	if p.Source.Kind != "backup" || p.Source.DeviceID != IOSBackupDevice ||
		p.Source.RemotePath != BackupRemotePath(IOSBackupDevice, testBackupDomain, testBackupRel) ||
		p.Logical != testBackupLogical || p.Platform != parse.PlatformIOS || p.Acquisition == "" {
		t.Errorf("primary %+v logical %q platform %q acquisition %q", p.Source, p.Logical, p.Platform, p.Acquisition)
	}
	if b.Others["wal"].Logical != p.Logical+"-wal" {
		t.Errorf("wal logical %q", b.Others["wal"].Logical)
	}
	if _, ok := b.Others["images"]; !ok {
		t.Error("no images artifact")
	}
	if len(b.Others) != 3 { // wal, images, extra0
		t.Errorf("Others holds %d artifacts", len(b.Others))
	}
	for name, a := range b.Others {
		if a.Acquisition != p.Acquisition || a.Acquisition == "" {
			t.Errorf("%s: acquisition %q, primary %q", name, a.Acquisition, p.Acquisition)
		}
		if a.Source.Kind != "backup" || a.Platform != parse.PlatformIOS {
			t.Errorf("%s: %+v platform %q", name, a.Source, a.Platform)
		}
	}
}

func TestBundleIOSBackupFillsARoleAndSplitsDevices(t *testing.T) {
	fx := backupFixture()
	fx.Files = map[string][]byte{"db": []byte("d")}
	fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles,
		ExtraFile{Domain: "HomeDomain", RelativePath: "Library/AddressBook/AddressBookImages.sqlitedb", Role: "images", ContentB64: "YQ=="},
		ExtraFile{Domain: "HomeDomain", RelativePath: "Library/Other/o.dat", Device: "UDID2", ContentB64: "Yg=="})
	b := fx.BundleIOSBackup(NewHarness(t), testBackupDomain, testBackupRel)
	img, ok := b.Others["images"]
	if !ok || b.ByRole["images"].ID != img.ID || img.Logical != "ios:HomeDomain/Library/AddressBook/AddressBookImages.sqlitedb" {
		t.Errorf("images role: %+v %v", img, ok)
	}
	o := b.ByPath["ios:HomeDomain/Library/Other/o.dat"]
	if o.Source.DeviceID != "UDID2" || o.Acquisition == b.Primary.Acquisition || o.Acquisition == "" {
		t.Errorf("other device: %+v acquisition %q primary %q", o.Source, o.Acquisition, b.Primary.Acquisition)
	}
	if o.Source.RemotePath != BackupRemotePath("UDID2", "HomeDomain", "Library/Other/o.dat") {
		t.Errorf("other device path %q", o.Source.RemotePath)
	}
}

func TestBundleIOSBackupRefusesAnExtraWithoutDomainOrPath(t *testing.T) {
	for name, x := range map[string]ExtraFile{
		"no domain": {RelativePath: "a/b", ContentB64: "YQ=="},
		"no path":   {Domain: "HomeDomain", ContentB64: "YQ=="},
	} {
		fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}, Expect: Expect{ExtraFiles: []ExtraFile{x}}}
		if _, err := fx.BuildIOSBackupBundle(NewHarness(t), testBackupDomain, testBackupRel); !errors.Is(err, ErrEmptyRemotePath) {
			t.Errorf("%s: err = %v, want ErrEmptyRemotePath", name, err)
		}
	}
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}}
	if _, err := fx.BuildIOSBackupBundle(NewHarness(t), "", testBackupRel); !errors.Is(err, ErrEmptyRemotePath) {
		t.Errorf("empty database domain: err = %v", err)
	}
}

func TestLoadFixtureReadsTheImagesRoles(t *testing.T) {
	db, img := []byte("SQLite format 3 db body"), []byte("SQLite format 3 images body")
	hex256 := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	write := func(dir, imgPin string) {
		exp := `{"generator":{"tool":"t","python":"3","sqlite":"3","files":{"db":"` + hex256(db) + `","images":"` + imgPin + `"}},"family":"f","variant":"v","records":[],"warnings":[],"notes":{}}`
		for name, data := range map[string][]byte{"x.db.gz": gz(db), "x.images.db.gz": gz(img), "x.expect.json": []byte(exp)} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir := t.TempDir()
	write(dir, hex256(img))
	if f := LoadFixture(t, dir, "x"); !bytes.Equal(f.Files["images"], img) {
		t.Errorf("images = %q", f.Files["images"])
	}
	bad := t.TempDir()
	write(bad, strings.Repeat("0", 64))
	rec := &recorder{TB: t}
	LoadFixture(rec, bad, "x")
	if !rec.Failed() || !strings.Contains(strings.Join(rec.errs, " "), "images") {
		t.Errorf("a wrong images pin was accepted: %v", rec.errs)
	}
}

func TestBundleIOSBackupKeysExtrasByLogicalPath(t *testing.T) {
	b := backupFixture().BundleIOSBackup(NewHarness(t), testBackupDomain, testBackupRel)
	att, ok := b.ByPath[testAttLogical]
	if !ok {
		t.Fatalf("ByPath has no %s: %v", testAttLogical, b.ByPath)
	}
	if _, wrong := b.ByPath[BackupRemotePath(IOSBackupDevice, testAttDomain, testAttRel)]; wrong {
		t.Error("ByPath also keys the extra by its remote path")
	}
	rec := records.Record{
		Type: "message", SourcePath: b.Primary.Source.RemotePath, ArtifactID: b.Primary.ID, Locator: "sqlite:table=sms;rowid=1",
		Range: &records.Range{Offset: 1, Length: 2}, Summary: "s",
		Payload: map[string]any{"artifact_id": att.ID},
	}
	exp := Expect{Records: []ExpRecord{{
		Table: "sms", Rowid: 1, File: "db", Type: "message", SourcePath: b.Primary.Source.RemotePath, Summary: sp("s"),
		Payload: map[string]any{"artifact_id": "@id:" + testAttLogical},
	}}}
	r := &recorder{TB: t}
	AssertMatchesOracle(r, Result{Records: []records.Record{rec}}, b, exp)
	if r.Failed() {
		t.Errorf("the placeholder on a logical path was not resolved: %v", r.errs)
	}
	rec.Payload = map[string]any{"artifact_id": "another"}
	r = &recorder{TB: t}
	AssertMatchesOracle(r, Result{Records: []records.Record{rec}}, b, exp)
	if !r.Failed() {
		t.Error("a wrong artifact id matched the placeholder")
	}
}

func TestHarnessLookupFindsIOSExtras(t *testing.T) {
	h := NewHarness(t)
	b := backupFixture().BundleIOSBackup(h, testBackupDomain, testBackupRel)
	in := b.Input(h, WellBehaved{Name: "ios", Version: "1.0.0"}, false)
	got := in.Lookup.Find(testAttLogical)
	att := b.ByPath[testAttLogical]
	if len(got) != 1 || got[0].ID != att.ID || got[0].SHA256 != att.SHA256 || got[0].SHA256 == "" {
		t.Errorf("Find = %+v, want the extra %s", got, att.ID)
	}
}
