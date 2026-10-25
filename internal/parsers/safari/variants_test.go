package safari_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
)

// Every DDL snapshot is the schema of at least one fixture, and every fixture names a Safari snapshot.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(ddlDir, "safari-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		used[parsertest.LoadFixture(t, "testdata", v).Expect.DDL] = true
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "safari-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "current").BundleIOS(h, remoteSafari, logicalSafari)
	p := safari.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result {
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
			t.Errorf("err %v, violations %v, %d records", res.Err, res.Violations, len(res.Records))
		}
		return res
	})
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "current").BundleIOS(h, remoteSafari, logicalSafari)
	p := safari.Parser{}
	res := h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
}
