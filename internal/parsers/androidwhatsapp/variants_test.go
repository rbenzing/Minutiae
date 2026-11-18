package androidwhatsapp_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every msgstore DDL snapshot is used by a fixture, and every fixture names a snapshot of this directory (the wa.db
// snapshots are the contacts parser's).
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "whatsapp-android", "*.sql"))
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
		base := filepath.Base(f)
		if strings.HasPrefix(base, "wa-") {
			continue
		}
		if !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if strings.HasPrefix(ddl, "wa-") {
			t.Errorf("a msgstore fixture names the wa.db snapshot %q", ddl)
		}
	}
}

func TestParseIsDeterministic(t *testing.T) {
	for _, v := range []string{"android-modern-2026", "android-transition-legacydata"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			p := androidwhatsapp.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
		})
	}
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "android-modern-2026").Bundle(h, remoteFor("android-modern-2026"))
	p := androidwhatsapp.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}
