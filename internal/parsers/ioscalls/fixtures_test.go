package ioscalls_test

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
	remoteCore    = "/private/var/mobile/Library/CallHistoryDB/CallHistory.storedata"
	logicalCore   = "ios:HomeDomain/Library/CallHistoryDB/CallHistory.storedata"
	remoteLegacy  = "/private/var/mobile/Library/CallHistory/call_history.db"
	logicalLegacy = "ios:HomeDomain/Library/CallHistory/call_history.db"
)

var variants = []string{"coredata-current", "coredata-wal", "legacy-call-history"}

// remotesFor is where the variant's database sits on the device and its logical path.
func remotesFor(variant string) (remote, logical string) {
	if variant == "legacy-call-history" {
		return remoteLegacy, logicalLegacy
	}
	return remoteCore, logicalCore
}

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

// Every fixture was generated from one of the two DDL snapshots of Task 4.
func TestFixturesNameAnExistingDDLSnapshot(t *testing.T) {
	for _, v := range variants {
		ddl := parsertest.LoadFixture(t, "testdata", v).Expect.DDL
		want := "callhistory-coredata.sql"
		if v == "legacy-call-history" {
			want = "callhistory-legacy.sql"
		}
		if ddl != want {
			t.Errorf("%s: ddl %q, want %q", v, ddl, want)
		}
		if _, err := os.Stat(filepath.Join("../../../tools/fixtures/schemas/ios", ddl)); err != nil {
			t.Error(err)
		}
	}
}

// Every variant bundles as an iOS artifact at the call history paths, as a file acquisition and as a
// backup; the WAL of coredata-wal sits beside the database in both.
func TestFixturesBundleAsIOSArtifactsAtTheCallHistoryPaths(t *testing.T) {
	for _, v := range variants {
		remote, logical := remotesFor(v)
		wantWAL := v == "coredata-wal"
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := fx.BundleIOS(h, remote, logical)
		if b.Primary.Platform != "ios" || b.Primary.Logical != logical || b.Primary.Source.RemotePath != remote {
			t.Errorf("%s: primary %+v", v, b.Primary)
		}
		if wal, ok := b.ByRole["wal"]; ok != wantWAL || (ok && wal.Logical != logical+"-wal") {
			t.Errorf("%s: wal artifact %v present %v", v, wal.Logical, ok)
		}
		h2 := parsertest.NewHarness(t)
		bb := fx.BundleIOSBackup(h2, "HomeDomain", strings.TrimPrefix(logical, "ios:HomeDomain/"))
		if bb.Primary.Logical != logical || bb.Primary.Source.Kind != "backup" {
			t.Errorf("%s: backup primary %+v", v, bb.Primary)
		}
		if _, ok := bb.ByRole["wal"]; ok != wantWAL {
			t.Errorf("%s: backup wal present %v", v, ok)
		}
	}
}
