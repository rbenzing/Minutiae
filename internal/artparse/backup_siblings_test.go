package artparse_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
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
	t.Run("a held file the manifest does not list is reported, the rest still pairs", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL", unlisted: true},
			{domain: recBackupDomain, rel: recBackupRel + "-journal", data: "JRN"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Named || got.WAL != "" || got.Journal != a.file(recBackupDomain, recBackupRel+"-journal").ID {
			t.Errorf("got %+v, want Named, no wal (unnamed), the journal paired", got)
		}
		if got.UnmappedHeld != 1 {
			t.Errorf("UnmappedHeld = %d, want 1", got.UnmappedHeld)
		}
		if w := strings.Join(got.Warnings(), "\n"); !strings.Contains(w, "1 backup files of this backup have no logical path; a companion may be among them") {
			t.Errorf("warnings %q lack the unmapped-files warning", w)
		}
	})
	t.Run("a companion listed but not held is reported", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{
			files: []bfile{{domain: recBackupDomain, rel: recBackupRel, data: "DB"}},
			rows: []frow{
				{id: parsertest.BackupFileID(recBackupDomain, recBackupRel+"-wal"), domain: recBackupDomain, rel: recBackupRel + "-wal", flags: 1},
				{id: parsertest.BackupFileID(recBackupDomain, recBackupRel+"-journal"), domain: recBackupDomain, rel: recBackupRel + "-journal", flags: 1},
			},
		})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Named || got.WAL != "" || got.Journal != "" || !got.WalListedNotHeld || !got.JournalListedNotHeld {
			t.Errorf("got %+v, want Named, nothing paired, both listed-not-held", got)
		}
		w := strings.Join(got.Warnings(), "\n")
		if !strings.Contains(w, recBackupLogic+"-wal") || !strings.Contains(w, recBackupLogic+"-journal") || !strings.Contains(w, "case holds no such file") {
			t.Errorf("warnings %q do not name the listed-but-not-held companions", w)
		}
		// a database whose companions are not listed has no such warning
		bc2 := newBackupCase(t)
		a2 := bc2.add(bspec{files: []bfile{{domain: recBackupDomain, rel: recBackupRel, data: "DB"}}})
		got2, err := siblingsOf(t, bc2, a2.file(recBackupDomain, recBackupRel).ID)
		if err != nil || got2.WalListedNotHeld || got2.JournalListedNotHeld || len(got2.Warnings()) != 0 {
			t.Errorf("got %+v err %v, want a clean answer", got2, err)
		}
	})
	t.Run("two files listed under the same companion path are ambiguous", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL1"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal2", data: "WAL2", asRel: recBackupRel + "-wal"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.WAL != "" || !strings.Contains(got.Reason, "ambiguous") || !strings.Contains(got.Reason, "-wal") {
			t.Errorf("got %+v, want not Named, reason ambiguous naming the -wal", got)
		}
	})
	t.Run("an ambiguous journal pairs neither side", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{files: []bfile{
			{domain: recBackupDomain, rel: recBackupRel, data: "DB"},
			{domain: recBackupDomain, rel: recBackupRel + "-wal", data: "WAL"},
			{domain: recBackupDomain, rel: recBackupRel + "-journal", data: "J1"},
			{domain: recBackupDomain, rel: recBackupRel + "-journal2", data: "J2", asRel: recBackupRel + "-journal"},
		}})
		got, err := siblingsOf(t, bc, a.file(recBackupDomain, recBackupRel).ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Named || got.WAL != "" || got.Journal != "" || !strings.Contains(got.Reason, "ambiguous") || !strings.Contains(got.Reason, "-journal") {
			t.Errorf("got %+v, want not Named, nothing paired (not even the unique -wal), reason naming the -journal", got)
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
