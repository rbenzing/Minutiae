package iosaddressbook_test

import (
	"bytes"
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
	remote      = "/private/var/mobile/Library/AddressBook/AddressBook.sqlitedb"
	relative    = "Library/AddressBook/AddressBook.sqlitedb"
	logical     = "ios:HomeDomain/" + relative
	imagesRel   = "Library/AddressBook/AddressBookImages.sqlitedb"
	schemasPath = "../../../tools/fixtures/schemas/ios"
)

var variants = []string{"current", "with-images", "older-schema", "huge-contact", "deleted"}

func ddlOf(variant string) string {
	if variant == "older-schema" {
		return "addressbook-older.sql"
	}
	return "addressbook-current.sql"
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

// Every fixture was generated from the DDL snapshot of Task 4 it names; the images database of with-images
// from addressbookimages.sql, and only that variant has one.
func TestFixturesNameAnExistingDDLSnapshot(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if fx.Expect.DDL != ddlOf(v) {
			t.Errorf("%s: ddl %q, want %q", v, fx.Expect.DDL, ddlOf(v))
		}
		if _, err := os.Stat(filepath.Join(schemasPath, fx.Expect.DDL)); err != nil {
			t.Error(err)
		}
		img, has := fx.Files["images"]
		if has != (v == "with-images") {
			t.Errorf("%s: images role present %v", v, has)
		}
		if has && !bytes.HasPrefix(img, []byte("SQLite format 3\x00")) {
			t.Errorf("%s: the images file is not a SQLite database", v)
		}
		if _, has := fx.Files["images_wal"]; has {
			t.Errorf("%s: the scenario has no images WAL", v)
		}
	}
	if _, err := os.Stat(filepath.Join(schemasPath, "addressbookimages.sql")); err != nil {
		t.Error(err)
	}
}

// Every variant bundles as an iOS artifact at the address book path, as a file acquisition and as a backup;
// with-images also places the images database beside it, in the same backup directory.
func TestFixturesBundleAsIOSArtifactsAtTheAddressBookPaths(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := fx.BundleIOS(parsertest.NewHarness(t), remote, logical)
		if b.Primary.Platform != "ios" || b.Primary.Logical != logical || b.Primary.Source.RemotePath != remote {
			t.Errorf("%s: primary %+v", v, b.Primary)
		}
		bb := fx.BundleIOSBackup(parsertest.NewHarness(t), "HomeDomain", relative)
		if bb.Primary.Logical != logical || bb.Primary.Source.Kind != "backup" {
			t.Errorf("%s: backup primary %+v", v, bb.Primary)
		}
		img, has := bb.ByRole["images"]
		if has != (v == "with-images") {
			t.Errorf("%s: backup images artifact present %v", v, has)
			continue
		}
		if has {
			wantRemote := parsertest.BackupRemotePath(parsertest.IOSBackupDevice, "HomeDomain", imagesRel)
			if img.Logical != "ios:HomeDomain/"+imagesRel || img.Source.RemotePath != wantRemote {
				t.Errorf("%s: images artifact %q at %q, want %q at %q", v, img.Logical, img.Source.RemotePath, "ios:HomeDomain/"+imagesRel, wantRemote)
			}
		}
	}
}
