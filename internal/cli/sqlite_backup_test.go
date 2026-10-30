package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const backupPlistYes = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>IsEncrypted</key><true/></dict></plist>`

// walBackup is a case holding the Safari current-wal fixture (a database and its WAL) in one iOS backup, plus
// an unrelated -wal file of the same backup that is nobody's companion.
type walBackup struct {
	c              *evidence.Case
	dir            string
	db, wal, other evidence.ManifestRecord
	manifest       evidence.ManifestRecord
}

func newWALBackup(t *testing.T, plist string) *walBackup {
	t.Helper()
	base := filepath.Join("..", "parsers", "safari", "testdata", "current-wal")
	db, ok := readGzFixture(t, base+".db.gz")
	if !ok {
		t.Fatal("the safari current-wal database is missing")
	}
	wal, ok := readGzFixture(t, base+".db-wal.gz")
	if !ok {
		t.Fatal("the safari current-wal WAL is missing")
	}
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "ios.backup"}); err != nil {
		t.Fatal(err)
	}
	recs, manifest := addIOSBackupPlist(t, c, "A1", "UDID1", []iosBackupFile{
		{"HomeDomain", safariRelative, db},
		{"HomeDomain", safariRelative + "-wal", wal},
		{"HomeDomain", "Library/Other/unrelated.db-wal", wal},
	}, plist)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return &walBackup{
		c: c, dir: c.Dir, db: recs["HomeDomain/"+safariRelative], wal: recs["HomeDomain/"+safariRelative+"-wal"],
		other: recs["HomeDomain/Library/Other/unrelated.db-wal"], manifest: manifest,
	}
}

type backupSiblingsReport struct {
	Siblings map[string]struct {
		Status     string `json:"status"`
		ArtifactID string `json:"artifact_id"`
	} `json:"siblings"`
	BackupSiblings *struct {
		Named      bool   `json:"named"`
		WAL        string `json:"wal"`
		Journal    string `json:"journal"`
		ManifestDB *struct {
			ArtifactID string `json:"artifact_id"`
			SHA256     string `json:"sha256"`
		} `json:"manifest_db"`
		Reason string `json:"reason"`
	} `json:"backup_siblings"`
	Typed *typedJSON `json:"typed"`
}

func (w *walBackup) report(t *testing.T, wantCode int, args ...string) (backupSiblingsReport, map[string]json.RawMessage) {
	t.Helper()
	code, out, errOut := runP(t, nil, append([]string{"sqlite", "recover", "--case", w.dir, "--json", w.db.ID}, args...)...)
	if code != wantCode {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, wantCode, out, errOut)
	}
	var r backupSiblingsReport
	if err := json.Unmarshal(jsonPart(out), &r); err != nil {
		t.Fatalf("report json: %v\n%s", err, out)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &raw); err != nil {
		t.Fatal(err)
	}
	return r, raw
}

// The Manifest.db of the backup names the database's -wal: the generic run reads it as an explicit companion, and
// the typed job reads the same bundle (it is NOT bundle-differs, which honors no claim).
func TestSQLiteRecoverPairsBackupSiblingsByManifest(t *testing.T) {
	w := newWALBackup(t, backupPlistNo)
	rep, _ := w.report(t, 0)
	bs := rep.BackupSiblings
	if bs == nil || !bs.Named || bs.WAL != w.wal.ID || bs.Journal != "" || bs.Reason != "" {
		t.Fatalf("backup_siblings = %+v, want named with the -wal %s", bs, w.wal.ID)
	}
	if bs.ManifestDB == nil || bs.ManifestDB.ArtifactID != w.manifest.ID || bs.ManifestDB.SHA256 != w.manifest.SHA256 {
		t.Errorf("manifest_db = %+v, want %s sha256 %s", bs.ManifestDB, w.manifest.ID, w.manifest.SHA256)
	}
	if got := rep.Siblings["wal"].ArtifactID; got != w.wal.ID {
		t.Errorf("the generic run's siblings.wal = %q, want the manifest-named %s", got, w.wal.ID)
	}
	if rep.Typed == nil || len(rep.Typed.Jobs) != 1 || rep.Typed.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("typed %+v, want one complete job (the typed bundle equals the generic one)", rep.Typed)
	}
	if strings.Contains(rep.Typed.Jobs[0].Reason, "bundle-differs") {
		t.Errorf("typed job reason %q", rep.Typed.Jobs[0].Reason)
	}
	// the typed job's bundle holds the same WAL
	var seen bool
	for _, e := range auditOf(t, w.dir) {
		if e.Action != artparse.ActionParseJobStart {
			continue
		}
		js, err := evidence.DecodeDetails[artparse.JobStart](e.Details)
		if err != nil {
			t.Fatal(err)
		}
		if js.RecoverJobStart != nil {
			seen = true
			if got := js.Bundle["wal"].ArtifactID; got != w.wal.ID {
				t.Errorf("typed job wal %q, want %q", got, w.wal.ID)
			}
		}
	}
	if !seen {
		t.Error("no typed recovery job in the audit")
	}
}

func TestSQLiteRecoverPairsBackupSiblingsInTheTextAndTheAssessment(t *testing.T) {
	w := newWALBackup(t, backupPlistNo)
	want := "siblings named by backup Manifest.db " + w.manifest.ID + " (sha256 " + w.manifest.SHA256 + ")"
	for _, extra := range []string{"--list", ""} {
		args := []string{"sqlite", "recover", "--case", w.dir, w.db.ID}
		if extra != "" {
			args = append(args, extra)
		}
		code, out, errOut := runP(t, nil, args...)
		if code != 0 {
			t.Fatalf("%q: exit %d\n%s\n%s", extra, code, out, errOut)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%q: output lacks %q:\n%s", extra, want, out)
		}
		if !strings.Contains(out, "wal: found by manifest identity") && !strings.Contains(out, w.wal.ID) {
			t.Errorf("%q: the WAL %s is not reported:\n%s", extra, w.wal.ID, out)
		}
	}
}

// A Manifest.db that fails its hash check ends the command with exit 4 before any run: nothing is ingested.
func TestSQLiteRecoverBackupManifestTamperedExits4(t *testing.T) {
	w := newWALBackup(t, backupPlistNo)
	tamperFile(t, w.c, w.manifest)
	before := len(auditOf(t, w.dir))
	code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", w.dir, w.db.ID)
	if code != 4 {
		t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
	}
	for _, e := range auditOf(t, w.dir)[before:] {
		if e.Action == "records.ingest.start" {
			t.Errorf("an ingest started after a tampered Manifest.db: %+v", e)
		}
	}
	if recs := listAllRecords(t, w.dir, "--recovered", "only"); len(recs) != 0 {
		t.Errorf("%d recovered records were written", len(recs))
	}
}

// An encrypted backup names no sibling and says why; the command's exit is what it was without the pairing.
func TestSQLiteRecoverEncryptedBackupNamesNoSiblings(t *testing.T) {
	plain := newWALBackup(t, backupPlistYes)
	wantCode, _, _ := runP(t, nil, "sqlite", "recover", "--case", plain.dir, "--no-siblings", plain.db.ID)

	w := newWALBackup(t, backupPlistYes)
	code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", w.dir, "--json", w.db.ID)
	if code != wantCode {
		t.Fatalf("exit %d, want %d (the exit without sibling pairing)\n%s\n%s", code, wantCode, out, errOut)
	}
	var rep backupSiblingsReport
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
		t.Fatalf("report json: %v\n%s", err, out)
	}
	bs := rep.BackupSiblings
	if bs == nil || bs.Named || bs.WAL != "" || bs.Journal != "" || bs.ManifestDB != nil || !strings.Contains(bs.Reason, "encrypted") {
		t.Fatalf("backup_siblings = %+v, want not named with an encrypted reason", bs)
	}
	w2 := newWALBackup(t, backupPlistYes)
	_, text, _ := runP(t, nil, "sqlite", "recover", "--case", w2.dir, "--list", w2.db.ID)
	if !strings.Contains(text, "backup siblings not named: ") || !strings.Contains(text, "encrypted") {
		t.Errorf("text lacks the not-named line:\n%s", text)
	}
}

// An explicit --wal (or --no-siblings) is the examiner's decision: the Manifest.db is not consulted.
func TestSQLiteRecoverExplicitWALWinsOverTheManifest(t *testing.T) {
	w := newWALBackup(t, backupPlistNo)
	rep, raw := w.report(t, 0, "--wal", w.other.ID)
	if string(raw["backup_siblings"]) != "null" {
		t.Errorf("backup_siblings = %s, want null (the manifest is not consulted)", raw["backup_siblings"])
	}
	if got := rep.Siblings["wal"].ArtifactID; got != w.other.ID {
		t.Errorf("siblings.wal = %q, want the given %s", got, w.other.ID)
	}
	_, raw = newWALBackup(t, backupPlistNo).report(t, 0, "--no-siblings")
	if string(raw["backup_siblings"]) != "null" {
		t.Errorf("--no-siblings: backup_siblings = %s, want null", raw["backup_siblings"])
	}
}

// A database that is not a backup file has a null backup_siblings.
func TestSQLiteRecoverNonBackupDatabaseHasNullBackupSiblings(t *testing.T) {
	e := newTypedEnv(t, typedVariants[0], false)
	code, out, errOut := e.recover(t, nil, "--json", e.db.ID)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(jsonPart(out), &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["backup_siblings"]; !ok || string(v) != "null" {
		t.Errorf("backup_siblings = %s (present %v), want null", v, ok)
	}
}
