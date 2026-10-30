package iossms_test

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
	domain  = "HomeDomain"
	rel     = "Library/SMS/sms.db"
	logical = "ios:HomeDomain/Library/SMS/sms.db"
	// backupPath is the hashed remote path of the database in the backup of UDID1: sha1("HomeDomain-Library/SMS/sms.db")
	// is the well-known file ID 3d0d7e5f..., written here as a literal, not computed.
	backupPath = "UDID1/3d/3d0d7e5fb2ce288813306e4d4636395e047a3d28"
)

var variants = []string{"ios10-seconds", "ios14-attributed", "ios17-nanos", "ios17-wal", "ios17-deleted"}

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
	all, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var m []string
	for _, p := range all {
		if !strings.HasSuffix(p, ".recover.expect.json") { // the typed recovery oracles are checked by TestEveryRecoverOracleHasAFixture
			m = append(m, p)
		}
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

// Every fixture was generated from one of the three DDL snapshots of Task 4.
func TestFixturesNameAnExistingDDLSnapshot(t *testing.T) {
	want := map[string]string{"ios10-seconds": "sms-ios10.sql", "ios14-attributed": "sms-ios14.sql", "ios17-nanos": "sms-ios17.sql", "ios17-wal": "sms-ios17.sql", "ios17-deleted": "sms-ios17.sql"}
	for _, v := range variants {
		ddl := parsertest.LoadFixture(t, "testdata", v).Expect.DDL
		if ddl != want[v] {
			t.Errorf("%s: ddl %q, want %q", v, ddl, want[v])
		}
		if _, err := os.Stat(filepath.Join("../../../tools/fixtures/schemas/ios", ddl)); err != nil {
			t.Error(err)
		}
	}
}

// Every variant bundles as a backup of UDID1 holding HomeDomain/Library/SMS/sms.db; the WAL of ios17-wal sits
// beside it; ios17-nanos carries its attachment files in the backups of UDID1 and UDID2.
func TestFixturesBundleAsAnIOSBackupAtTheSMSPath(t *testing.T) {
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := fx.BundleIOSBackup(h, domain, rel)
		p := b.Primary
		if p.Platform != "ios" || p.Logical != logical || p.Source.Kind != "backup" || p.Source.DeviceID != "UDID1" || p.Source.RemotePath != backupPath {
			t.Errorf("%s: primary %+v", v, p)
		}
		wal, hasWAL := b.ByRole["wal"]
		if hasWAL != (v == "ios17-wal") || (hasWAL && wal.Logical != logical+"-wal") {
			t.Errorf("%s: wal artifact %q present %v", v, wal.Logical, hasWAL)
		}
		if v == "ios17-wal" && !strings.HasPrefix(wal.Source.RemotePath, "UDID1/") {
			t.Errorf("%s: wal remote path %q", v, wal.Source.RemotePath)
		}
		if (len(fx.Expect.ExtraFiles) > 0) != (v == "ios17-nanos") {
			t.Errorf("%s: %d extra files", v, len(fx.Expect.ExtraFiles))
		}
	}
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "ios17-nanos").BundleIOSBackup(h, domain, rel)
	own := b.ByPath["ios:MediaDomain/Library/SMS/Attachments/ab/12/IMG_0001.jpeg"]
	other := b.ByPath["ios:MediaDomain/Library/SMS/Attachments/ef/56/other.jpeg"]
	if own.ID == "" || own.Source.DeviceID != "UDID1" || own.Acquisition != b.Primary.Acquisition {
		t.Errorf("own attachment %+v", own)
	}
	if other.ID == "" || other.Source.DeviceID != "UDID2" || other.Acquisition == b.Primary.Acquisition {
		t.Errorf("other-device attachment %+v", other)
	}
}
