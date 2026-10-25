package safari_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	remoteSafari  = "/private/var/mobile/Library/Safari/History.db"
	logicalSafari = "ios:HomeDomain/Library/Safari/History.db"
)

var variants = []string{"current", "current-wal", "no-tombstones"}

const ddlDir = "../../../tools/fixtures/schemas/safari"

func TestFixturesArePinnedByHash(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		for role, data := range fx.Files {
			sum := sha256.Sum256(data)
			key := v + "." + role
			seen[key] = true
			if want, ok := fixturePins[key]; !ok || want != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: sha256 %x is not the pinned value %q (regenerate image, oracle and pin together)", key, sum, want)
			}
		}
	}
	for key := range fixturePins {
		if !seen[key] {
			t.Errorf("pin %s has no fixture file", key)
		}
	}
}

func TestEveryCommittedFixtureIsInTheVariantList(t *testing.T) {
	m, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != len(variants) {
		t.Errorf("%d fixtures on disk, %d in the variant list", len(m), len(variants))
	}
	for _, p := range m {
		name := strings.TrimSuffix(filepath.Base(p), ".expect.json")
		if !slices.Contains(variants, name) {
			t.Errorf("testdata/%s is not in the variant list", name)
		}
	}
}

// No public source defines Safari's History.db (4F-R12): both snapshots say so in their header, and
// every fixture was generated from one of them.
func TestDDLSnapshotsNameTheirVerdictAndSource(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(ddlDir, "*.sql"))
	if err != nil || len(files) != 2 {
		t.Fatalf("%d snapshots in %s (want 2): %v", len(files), ddlDir, err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) < 3 || lines[0] != "-- verdict no-public-source" ||
			lines[1] != "-- source memory (no public schema; plan 4F ruling 4F-R12)" {
			t.Errorf("%s: header %q, want the verdict no-public-source and the source memory lines", f, lines[:min(2, len(lines))])
		}
	}
	for _, v := range variants {
		if ddl := parsertest.LoadFixture(t, "testdata", v).Expect.DDL; !slices.Contains(names, ddl) {
			t.Errorf("%s: ddl %q is not one of %v", v, ddl, names)
		}
	}
}

// Every variant bundles as an iOS artifact at the Safari paths; the WAL of current-wal sits beside the database.
func TestFixturesBundleAsIOSArtifactsAtTheSafariPaths(t *testing.T) {
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := fx.BundleIOS(h, remoteSafari, logicalSafari)
		if b.Primary.Platform != "ios" || b.Primary.Logical != logicalSafari || b.Primary.Source.RemotePath != remoteSafari {
			t.Errorf("%s: primary %+v", v, b.Primary)
		}
		if wal, ok := b.ByRole["wal"]; ok != (v == "current-wal") || (ok && wal.Logical != logicalSafari+"-wal") {
			t.Errorf("%s: wal artifact %v present %v", v, wal.Logical, ok)
		}
	}
}
