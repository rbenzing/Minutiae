package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// newTwoBackupCase is a case holding two unencrypted iOS backups of one acquisition: the first holds the Safari
// `deleted` fixture, the second an unrelated file. When tamperSecond is set the second backup's Manifest.db is
// altered on disk after capture. It returns the case directory and the Safari database's artifact id.
func newTwoBackupCase(t *testing.T, tamperSecond bool) (dir, safari string) {
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
	return c.Dir, recs["HomeDomain/"+safariRelative].ID
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
		dir, _ := newTwoBackupCase(t, true)
		code, out, errOut := runP(t, nil, "parse", "recover", "--case", dir)
		if code != 4 {
			t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("sqlite recover", func(t *testing.T) {
		dir, safari := newTwoBackupCase(t, true)
		code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", dir, safari)
		if code != 4 {
			t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
		}
		// the typed pass ran as a parse run does (its records are kept and flagged by their own ingest); the integrity
		// failure ends the command before the generic run starts, so no db_row exists
		for _, r := range listAllRecords(t, dir, "--recovered", "only") {
			if r.Type == "db_row" {
				t.Errorf("db_row %s recorded: the generic run started after an integrity failure", r.Locator)
			}
		}
	})
	t.Run("untampered is exit 0", func(t *testing.T) {
		dir, safari := newTwoBackupCase(t, false)
		code, out, errOut := runP(t, nil, "sqlite", "recover", "--case", dir, safari)
		if code != 0 {
			t.Fatalf("exit %d, want 0\n%s\n%s", code, out, errOut)
		}
	})
}
