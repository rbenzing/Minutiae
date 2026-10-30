package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// newTwoBackupCase is a case holding two unencrypted iOS backups of one acquisition: the first holds the Safari
// `deleted` fixture, the second an unrelated file. When tamperSecond is set the second backup's Manifest.db is
// altered on disk after capture. It returns the case directory and the Safari database's artifact id.
func newTwoBackupCase(t *testing.T, tamperSecond bool) (dir, safari, secondManifest string) {
	t.Helper()
	db, ok := readGzFixture(t, filepath.Join("..", "parsers", "safari", "testdata", "deleted.db.gz"))
	if !ok {
		t.Fatal("the safari deleted fixture is missing")
	}
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "ios.backup"}); err != nil {
		t.Fatal(err)
	}
	recs, _ := addIOSBackup(t, c, "A1", "UDID1", []iosBackupFile{{"HomeDomain", safariRelative, db}})
	_, second := addIOSBackup(t, c, "A1", "UDID2", []iosBackupFile{{"HomeDomain", "Library/Other.db", []byte("OTHER")}})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if tamperSecond {
		tamperFile(t, c, second)
	}
	return c.Dir, recs["HomeDomain/"+safariRelative].ID, second.ID
}

func tamperFile(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) {
	t.Helper()
	file := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Fatal(err)
	}
}

// A typed recovery resolves every backup of the case: a tampered Manifest.db of a backup that holds nothing of
// the database being recovered still ends the run with exit 4 (never masked by the first backup's success).
func TestRecoverTypedPropagatesAnUnrelatedBackupIntegrityFailureInTheCLI(t *testing.T) {
	t.Run("parse recover", func(t *testing.T) {
		dir, _, tampered := newTwoBackupCase(t, true)
		code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir)
		if code != 4 {
			t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
		}
		requireVerifyOnlyNamesTheTamperedFile(t, dir, tampered)
	})
	t.Run("sqlite recover", func(t *testing.T) {
		dir, safari, tampered := newTwoBackupCase(t, true)
		code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", dir, safari)
		if code != 4 {
			t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
		}
		// the typed pass ran as a parse run does (its records are kept and flagged by their own ingest); the integrity
		// failure ends the command before the generic run starts, so no db_row exists
		requireVerifyOnlyNamesTheTamperedFile(t, dir, tampered)
		for _, r := range listAllRecords(t, dir, "--recovered", "only") {
			if r.Type == "db_row" {
				t.Errorf("db_row %s recorded: the generic run started after an integrity failure", r.Locator)
			}
		}
	})
	t.Run("untampered is exit 0", func(t *testing.T) {
		dir, safari, _ := newTwoBackupCase(t, false)
		code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", dir, safari)
		if code != 0 {
			t.Fatalf("exit %d, want 0\n%s\n%s", code, out, errOut)
		}
	})
}

// requireVerifyOnlyNamesTheTamperedFile runs `case verify` over a case after a recover run that ended with exit 4: the
// one expected finding is the tampered Manifest.db itself (its bytes no longer match the manifest). The run's own
// audit (analysis, jobs, ingests) must raise no problem: the run was recorded and concluded consistently.
func requireVerifyOnlyNamesTheTamperedFile(t *testing.T, dir, tamperedID string) {
	t.Helper()
	code, out, errOut := runP(t, nil, "case", "verify", "--case", dir)
	all := out + errOut
	if code != 4 {
		t.Fatalf("case verify exit %d, want 4 (the tampered artifact)\n%s", code, all)
	}
	if !strings.Contains(all, tamperedID) {
		t.Errorf("case verify does not name the tampered artifact %s:\n%s", tamperedID, all)
	}
	problems := 0
	for _, l := range strings.Split(all, "\n") {
		if strings.Contains(l, "PROBLEM:") {
			problems++
			if !strings.Contains(l, tamperedID) {
				t.Errorf("case verify reports a problem besides the tampered artifact: %q\n%s", l, all)
			}
		}
	}
	if problems != 1 {
		t.Errorf("case verify lists %d problems, want exactly the tampered artifact\n%s", problems, all)
	}
}

// Integrity wins over a cancel: a run that found a refused backup and was cancelled afterwards still maps to exit 4.
func TestRunOutcomeIntegrityBeatsCancel(t *testing.T) {
	sum := artparse.RunSummary{
		Cancelled: true, Stopped: artparse.StoppedCancelled,
		Backups: []artparse.BackupResolution{{Acquisition: "artifacts/D1/A1", Dir: "UDID2", Status: "refused", Integrity: true, Reason: "integrity: Manifest.db"}},
	}
	err := runOutcome(Deps{Err: io.Discard}, sum, context.Canceled)
	if err == nil || ExitCode(err) != 4 {
		t.Fatalf("exit %d (err %v), want 4", ExitCode(err), err)
	}
}
