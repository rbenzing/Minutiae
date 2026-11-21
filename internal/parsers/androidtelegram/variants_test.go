package androidtelegram_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8, oracles): each of the twelve variants is parsed by a test with a stated outcome. A variant whose
// oracle states records is matched by TestAndroidTelegramMatchesOracle (and its oracle must state some); the three
// refused variants fail Parse at the app path with their typed error and no record: telegram-otherversion (182) and
// telegram-oldversion (63) with ErrUnsupportedVersion (B4, 4I4-P8), telegram-nomessages with ErrNoMessageTable.
// Every DDL snapshot of the telegram-android directory is used by a fixture, and every fixture names one.
func TestParserCoversEveryVariant(t *testing.T) {
	refused := map[string]error{
		"telegram-otherversion": androidtelegram.ErrUnsupportedVersion,
		"telegram-oldversion":   androidtelegram.ErrUnsupportedVersion,
		"telegram-nomessages":   androidtelegram.ErrNoMessageTable,
	}
	if len(variants) != 12 {
		t.Errorf("%d variants, want the twelve of spec 13", len(variants))
	}
	used := map[string]bool{}
	recorded := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		want, isRefused := refused[v]
		switch {
		case isRefused:
			if len(fx.Expect.Records) != 0 {
				t.Errorf("%s: a refused variant whose oracle states %d records", v, len(fx.Expect.Records))
			}
			res := parseAt(t, remoteApp, fx.Files)
			if !errors.Is(res.Err, want) || len(res.Records) != 0 || len(res.Violations) != 0 {
				t.Errorf("%s: err %v, %d records, violations %v; want %v and none", v, res.Err, len(res.Records), res.Violations, want)
			}
		case len(fx.Expect.Records) > 0:
			recorded++ // TestAndroidTelegramMatchesOracle parses it against its oracle
		default:
			t.Errorf("%s is parsed by no test with a stated outcome", v)
		}
	}
	if recorded != 9 {
		t.Errorf("%d variants with records, want the 9 TestAndroidTelegramMatchesOracle runs", recorded)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "telegram-android", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	for _, f := range files {
		if !used[filepath.Base(f)] {
			t.Errorf("DDL snapshot %s is used by no fixture", filepath.Base(f))
		}
	}
	if used[""] {
		t.Error("a fixture names no DDL snapshot")
	}
}
