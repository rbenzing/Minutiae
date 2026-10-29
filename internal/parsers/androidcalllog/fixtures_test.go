package androidcalllog_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	remoteCalllog  = "/data/user_de/0/com.android.providers.contacts/databases/calllog.db"
	remoteContacts = "/data/user_de/0/com.android.providers.contacts/databases/contacts2.db"
)

var variants = []string{"calllog-deleted", "calllog-wal-edit", "features-subid", "in-contacts2", "pre-presentation"}

// remoteFor is where the variant's database sits on the device.
func remoteFor(variant string) string {
	if variant == "in-contacts2" {
		return remoteContacts
	}
	return remoteCalllog
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
		known := false
		for _, v := range variants {
			known = known || v == name
		}
		if !known {
			t.Errorf("testdata/%s is not in the variant list", name)
		}
	}
}
