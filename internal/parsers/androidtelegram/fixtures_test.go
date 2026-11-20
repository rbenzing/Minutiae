package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// variants are the committed fixtures of plan 4I4 Task 5 (tools/fixtures/telegram_android.py).
var variants = []string{
	"telegram-v181", "telegram-v159", "telegram-v84", "telegram-v84-upgraded-v181", "telegram-v64", "telegram-both",
	"telegram-otherversion", "telegram-oldversion", "telegram-nomessages", "telegram-layers", "telegram-hostile",
	"telegram-deleted",
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
	if len(seen) == 0 {
		t.Error("no fixture was loaded")
	}
}

func TestEveryCommittedFixtureIsInTheVariantList(t *testing.T) {
	all, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var m []string
	for _, p := range all {
		if !strings.HasSuffix(p, ".recover.expect.json") { // the typed recovery oracle of telegram-deleted (Task 14 uses it)
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
