package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	cliPlistHead = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict>`
	cliPlistNo   = cliPlistHead + `<key>IsEncrypted</key><false/></dict></plist>`
	cliPlistYes  = cliPlistHead + `<key>IsEncrypted</key><true/></dict></plist>`
	cliEncReason = "encrypted backup (sub-project 10)"
)

// iosCliParser is a well-behaved fake over an iOS sms.db.
type iosCliParser struct{ parsertest.WellBehaved }

func (p iosCliParser) Meta() parse.Meta {
	m := parsertest.FakeMeta("ios-sms-cli", "1.0.0")
	m.Platforms = []string{parse.PlatformIOS}
	m.Inputs[0].Globs = []string{"ios:*/Library/SMS/sms.db"}
	return m
}

type cliBackup struct {
	dir   string
	db    evidence.ManifestRecord
	plist evidence.ManifestRecord
	reg   regFn
}

// newCliBackup builds a case holding one iOS backup: sms.db, a Manifest.db listing it (or noise when
// noise is set), Manifest.plist (plist "" = none) and Status.plist, all under the backup directory udid.
func newCliBackup(t *testing.T, udid, plist string, noise bool) *cliBackup {
	t.Helper()
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "ios.backup"}); err != nil {
		t.Fatal(err)
	}
	put := func(name, remote, data string) evidence.ManifestRecord {
		rec, err := c.Capture("D1", "A1", "backup/"+name, evidence.Source{Kind: "backup", DeviceID: "D1", RemotePath: remote}, func(w io.Writer) error {
			_, err := io.WriteString(w, data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	fid := parsertest.BackupFileID("HomeDomain", "Library/SMS/sms.db")
	b := &cliBackup{}
	put("sms", udid+"/"+fid[:2]+"/"+fid, "SMSDB")
	var manifest []byte
	if noise {
		manifest = make([]byte, 4096)
		x := uint32(12345)
		for i := range manifest {
			x = x*1664525 + 1013904223
			manifest[i] = byte(x >> 24)
		}
	} else {
		sb := sqlitetest.New(sqlitetest.Options{})
		sb.CreateTable("Files", "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)").
			Insert(int64(1), fid, "HomeDomain", "Library/SMS/sms.db", int64(1), nil)
		manifest = sb.Bytes()
	}
	b.db = put("mdb", udid+"/Manifest.db", string(manifest))
	if plist != "" {
		b.plist = put("mpl", udid+"/Manifest.plist", plist)
	}
	put("stat", udid+"/Status.plist", "status")
	b.dir = c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	b.reg = registry(t, "", iosCliParser{parsertest.WellBehaved{Name: "ios-sms-cli", Version: "1.0.0"}})
	return b
}

type backupJSONRow struct {
	Acquisition string `json:"acquisition"`
	Dir         string `json:"dir"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	Encryption  string `json:"encryption"`
	ManifestDB  *struct {
		ArtifactID string `json:"artifact_id"`
		SHA256     string `json:"sha256"`
	} `json:"manifest_db"`
	ManifestPlist *struct {
		ArtifactID string `json:"artifact_id"`
	} `json:"manifest_plist"`
	Integrity bool `json:"integrity"`
}

func TestParsePlanListsBackups(t *testing.T) {
	b := newCliBackup(t, "UDID1", cliPlistNo, false)
	code, out, errOut := runP(t, b.reg, "parse", "plan", "--case", b.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "BACKUP ") {
			line = l
		}
	}
	for _, want := range []string{"/UDID1", "mapped", "encryption=not-encrypted", "manifest=" + b.db.ID, "mapped=", "unmapped=", "fileid_mismatch=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("BACKUP line lacks %q: %q\nfull:\n%s", want, line, out)
		}
	}
	code, out, _ = runP(t, b.reg, "parse", "plan", "--case", b.dir, "--json")
	var pl struct {
		Backups []backupJSONRow `json:"backups"`
	}
	if err := json.Unmarshal([]byte(out), &pl); code != 0 || err != nil || len(pl.Backups) != 1 {
		t.Fatalf("exit %d err %v:\n%s", code, err, out)
	}
	bk := pl.Backups[0]
	if bk.Status != "mapped" || bk.Encryption != "not-encrypted" || bk.Dir != "UDID1" || bk.ManifestDB == nil || bk.ManifestDB.ArtifactID != b.db.ID ||
		bk.ManifestDB.SHA256 == "" || bk.ManifestPlist == nil || bk.ManifestPlist.ArtifactID != b.plist.ID || bk.Integrity {
		t.Errorf("backups[0] = %+v", bk)
	}
	for _, key := range []string{`"fileid_mismatch"`, `"mapped"`, `"unmapped"`, `"integrity"`, `"reason"`} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON lacks the key %s:\n%s", key, out)
		}
	}
}

func TestParsePlanBackupWithoutPlistSaysEncryptionUnknown(t *testing.T) {
	b := newCliBackup(t, "UDID1", "", false)
	code, out, errOut := runP(t, b.reg, "parse", "plan", "--case", b.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	textOut := out
	if !strings.Contains(out, "encryption=unknown note=") {
		t.Errorf("no `encryption=unknown note=` in:\n%s", out)
	}
	code, out, _ = runP(t, b.reg, "parse", "plan", "--case", b.dir, "--json")
	var pl struct {
		Backups []backupJSONRow `json:"backups"`
	}
	if err := json.Unmarshal([]byte(out), &pl); code != 0 || err != nil || len(pl.Backups) != 1 {
		t.Fatalf("exit %d err %v:\n%s", code, err, out)
	}
	if !strings.Contains(textOut, "encryption=unknown note="+pl.Backups[0].Reason) {
		t.Errorf("the text note is not the JSON reason %q:\n%s", pl.Backups[0].Reason, textOut)
	}
	if pl.Backups[0].Encryption != "unknown" || pl.Backups[0].Reason == "" || pl.Backups[0].ManifestPlist != nil {
		t.Errorf("backups[0] = %+v", pl.Backups[0])
	}
	if !strings.Contains(out, `"manifest_plist": null`) {
		t.Errorf("manifest_plist is not an explicit null:\n%s", out)
	}
}

func TestParsePlanWithoutBackupsHasNoSectionAndAnEmptyArray(t *testing.T) {
	p := newPfx(t, "a")
	reg := registry(t, "", good("a"))
	_, out, _ := runP(t, reg, "parse", "plan", "--case", p.dir)
	if strings.Contains(out, "BACKUP ") {
		t.Errorf("a case without backups prints a BACKUP line:\n%s", out)
	}
	_, out, _ = runP(t, reg, "parse", "plan", "--case", p.dir, "--json")
	if !strings.Contains(out, `"backups": []`) {
		t.Errorf("JSON lacks an empty backups array:\n%s", out)
	}
}

func TestParseRunEncryptedBackupExits1(t *testing.T) {
	for _, tc := range []struct {
		name  string
		noise bool
	}{{"readable Manifest.db", false}, {"high entropy Manifest.db", true}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newCliBackup(t, "UDID1", cliPlistYes, tc.noise)
			code, out, errOut := runP(t, b.reg, "parse", "run", "--case", b.dir)
			if code != 1 {
				t.Fatalf("exit %d, want 1:\n%s\n%s", code, out, errOut)
			}
			if !strings.Contains(out+errOut, cliEncReason) {
				t.Errorf("output does not name %q:\n%s\n%s", cliEncReason, out, errOut)
			}
			if !strings.Contains(out, "BACKUP ") || !strings.Contains(out, "encryption=encrypted") {
				t.Errorf("summary lacks the BACKUP line:\n%s", out)
			}
			if tc.noise {
				if !strings.Contains(out, "1 backup(s) not parsed") {
					t.Errorf("summary lacks `1 backup(s) not parsed`:\n%s", out)
				}
				if !strings.Contains(errOut, "backup") {
					t.Errorf("the exit line does not mention the unparsed backup: %q", errOut)
				}
			}
			code, out, _ = runP(t, b.reg, "parse", "run", "--case", b.dir, "--json", "--reparse")
			if code != 1 {
				t.Fatalf("json exit %d:\n%s", code, out)
			}
			var sum struct {
				Class   string          `json:"class"`
				Backups []backupJSONRow `json:"backups"`
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &sum); err != nil {
				t.Fatalf("%v:\n%s", err, out)
			}
			if sum.Class != "partial" || len(sum.Backups) != 1 || sum.Backups[0].Encryption != "encrypted" {
				t.Errorf("summary event = %+v", sum)
			}
		})
	}
}

func TestParseBackupIntegrityExits4(t *testing.T) {
	b := newCliBackup(t, "UDID1", cliPlistNo, false)
	path := filepath.Join(b.dir, filepath.FromSlash(b.db.Path))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Fatal(err)
	}
	for _, args := range [][]string{{"parse", "plan"}, {"parse", "run"}, {"parse", "plan", "--json"}} {
		code, out, errOut := runP(t, b.reg, append(args, "--case", b.dir)...)
		if code != 4 {
			t.Errorf("%v: exit %d, want 4:\n%s\n%s", args, code, out, errOut)
		}
		if !strings.Contains(errOut, b.db.ID) {
			t.Errorf("%v: stderr does not name the Manifest.db artifact %s: %q", args, b.db.ID, errOut)
		}
		if !strings.Contains(out, "refused") {
			t.Errorf("%v: stdout does not show the refused backup:\n%s", args, out)
		}
	}
}

func TestParseBackupIntegrityIsNeverMaskedByACancelOrPlanError(t *testing.T) {
	bk := []artparse.BackupResolution{{
		Acquisition: "artifacts/D1/A1", Dir: "UDID1", Status: artparse.BackupRefused, Integrity: true,
		ManifestDB: &artparse.ArtifactRef{ArtifactID: "MDB1"}, Reason: "integrity: hash mismatch",
	}}
	var o strings.Builder
	d := Deps{Out: &o, Err: &o}
	// a run whose only finding is the backup, under a cancelled context
	err := finishRun(d, &rootOptions{}, artparse.RunSummary{ParseID: "p", Backups: bk, Cancelled: true, Stopped: artparse.StoppedCancelled}, context.Canceled)
	if ExitCode(err) != ExitIntegrity || !strings.Contains(err.Error(), "MDB1") {
		t.Errorf("run: exit %d: %v", ExitCode(err), err)
	}
	// the same finding in a plan that also failed
	err = planOutcome(artparse.Plan{Backups: bk}, context.Canceled, nil)
	if ExitCode(err) != ExitIntegrity || !strings.Contains(err.Error(), "MDB1") {
		t.Errorf("plan: exit %d: %v", ExitCode(err), err)
	}
	// without an integrity finding a plan error stays itself
	err = planOutcome(artparse.Plan{}, errors.New("boom"), nil)
	if err == nil || ExitCode(err) == ExitIntegrity {
		t.Errorf("plan without finding: %v", err)
	}
}

func TestParseCLIEscapesBackupText(t *testing.T) {
	hostile := "UD\x1b[31m\u202eID"
	b := newCliBackup(t, hostile, cliPlistYes, true)
	_, out, errOut := runP(t, b.reg, "parse", "plan", "--case", b.dir)
	if r, bad := rawControl(out + errOut); bad {
		t.Errorf("plan text holds the raw control rune %U:\n%q", r, out)
	}
	if !strings.Contains(out, "BACKUP ") || !strings.Contains(out, `\x1b`) {
		t.Errorf("hostile dir is not shown escaped:\n%q", out)
	}
	_, out, errOut = runP(t, b.reg, "parse", "run", "--case", b.dir)
	if r, bad := rawControl(out + errOut); bad {
		t.Errorf("run text holds the raw control rune %U:\n%q\n%q", r, out, errOut)
	}
	for _, args := range [][]string{{"parse", "plan", "--json"}, {"parse", "run", "--json", "--reparse"}} {
		_, out, errOut = runP(t, b.reg, append(args, "--case", b.dir)...)
		if r, bad := rawControl(out + errOut); bad {
			t.Errorf("%v holds the raw control rune %U:\n%q", args, r, out)
		}
		if strings.ContainsRune(out, '\u202e') {
			t.Errorf("%v holds a raw U+202E:\n%q", args, out)
		}
		if !strings.Contains(out, `UD\u001b[31m\u202eID`) {
			t.Errorf("%v: the dir is not in the JSON as escapes:\n%q", args, out)
		}
	}
}
