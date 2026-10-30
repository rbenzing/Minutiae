package artparse_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func siblingsOf(t *testing.T, bc *bcase, id string) (artparse.BackupSiblings, error) {
	t.Helper()
	return bc.host(nil).BackupSiblings(context.Background(), id)
}

// The Manifest.db of the backup names the -wal and -journal of a database file; BackupSiblings returns their
// artifact ids and the hash-checked Manifest.db that said so (4h-R8: identity from structure, never a fileID guess).
func TestBackupSiblingsNamesTheManifestCompanions(t *testing.T) {
	t.Run("wal and journal", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL"},
			{domain: recBackupDomain, rel: recBackupRel + "-journal", data: "JRN"},
			{domain: recBackupDomain, rel: "Library/Safari/Other.db-wal", data: "OTHERWAL"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Named || got.Reason != "" {
			t.Fatalf("not named: %+v", got)
		}
		if got.WAL != a.file(recBackupDomain, recBackupRel+"-wal").ID || got.Journal != a.file(recBackupDomain, recBackupRel+"-journal").ID {
			t.Errorf("WAL %q Journal %q, want the -wal and -journal of the same file", got.WAL, got.Journal)
		}
		if got.ManifestDB == nil || got.ManifestDB.ArtifactID != a.db.ID || got.ManifestDB.SHA256 != a.db.SHA256 {
			t.Errorf("ManifestDB %+v, want the backup's Manifest.db %s sha256 %s", got.ManifestDB, a.db.ID, a.db.SHA256)
		}
	})
	t.Run("only a wal", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Named || got.WAL != a.file(recBackupDomain, recBackupRel+"-wal").ID || got.Journal != "" {
			t.Errorf("got %+v, want Named with the wal and no journal", got)
		}
	})
	t.Run("a file the manifest does not list is no sibling", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL", unlisted: true},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Named || got.WAL != "" {
			t.Errorf("got %+v, want Named with no wal: the Manifest.db does not name that file", got)
		}
	})
	t.Run("a file kind database", func(t *testing.T) {
		bc := newBackupCase(t)
		acquireStart(t, bc.c, "UDID1", "acq1", "logical")
		rec, err := bc.c.Capture("UDID1", "acq1", "files/main.db", evidence.Source{Kind: "file", DeviceID: "UDID1", RemotePath: "/data/main.db"}, func(w io.Writer) error {
			_, err := io.WriteString(w, "DB")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := siblingsOf(t, bc, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.Reason == "" || got.WAL != "" || got.Journal != "" || got.ManifestDB != nil {
			t.Errorf("got %+v, want not Named with a reason", got)
		}
	})
	t.Run("a Manifest.db is not a database of the backup", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{{domain: recBackupDomain, rel: recBackupRel, data: "DB"}}})
		got, err := siblingsOf(t, bc, a.db.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.Reason == "" {
			t.Errorf("got %+v, want not Named with a reason (a backup metadata file has no logical path)", got)
		}
	})
	t.Run("encrypted backup", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{plist: plistYes, files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.WAL != "" || !strings.Contains(got.Reason, "encrypted") {
			t.Errorf("got %+v, want not Named, reason encrypted", got)
		}
	})
	t.Run("no Manifest.db", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{noManifest: true, files: []bfile{{domain: recBackupDomain, rel: recBackupRel, data: "DB"}}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.Reason == "" {
			t.Errorf("got %+v, want not Named with a reason", got)
		}
	})
	t.Run("unknown artifact", func(t *testing.T) {
		bc := newBackupCase(t)
		bc.add(bspec{files: []bfile{{domain: recBackupDomain, rel: recBackupRel, data: "DB"}}})
		if _, err := siblingsOf(t, bc, "no-such-artifact"); !errors.Is(err, evidence.ErrUnknownArtifact) {
			t.Errorf("err = %v, want evidence.ErrUnknownArtifact", err)
		}
	})
}

// A Manifest.db that fails its hash check names nothing: it is an integrity error, never "no siblings".
func TestBackupSiblingsTamperedManifestIsIntegrity(t *testing.T) {
	setup := func(t *testing.T) (*bcase, *bres, *bres) {
		bc := newBackupCase(t)
		a := bc.add(bspec{acq: "acqA", files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL"},
		}})
		b := bc.add(bspec{dev: "UDID2", acq: "acqB", files: []bfile{{domain: recBackupDomain, rel: "Library/Other.db", data: "OTHER"}}})
		return bc, a, b
	}
	t.Run("its own backup", func(t *testing.T) {
		bc, a, _ := setup(t)
		tamper(t, bc.c, a.db)
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if !errors.Is(err, evidence.ErrIntegrity) {
			t.Fatalf("err = %v (siblings %+v), want evidence.ErrIntegrity", err, got)
		}
		if got.Named || got.WAL != "" {
			t.Errorf("a tampered Manifest.db named siblings: %+v", got)
		}
	})
	t.Run("another backup of the case", func(t *testing.T) {
		bc, a, b := setup(t)
		tamper(t, bc.c, b.db)
		if _, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID); !errors.Is(err, evidence.ErrIntegrity) {
			t.Fatalf("err = %v, want evidence.ErrIntegrity (every backup of the case is resolved, as a recovery does)", err)
		}
	})
}
