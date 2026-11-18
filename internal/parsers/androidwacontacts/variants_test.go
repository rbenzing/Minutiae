package androidwacontacts_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every DDL snapshot of wa.db is used by a fixture, and every fixture names a wa DDL.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "whatsapp-android", "wa-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		if ddl := parsertest.LoadFixture(t, "testdata", v).Expect.DDL; ddl != "" {
			used[ddl] = true
		}
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "wa-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "wa-current").Bundle(h, remoteCurrent)
	p := androidwacontacts.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "wa-deleted").Bundle(h, remoteCurrent)
	p := androidwacontacts.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}
