package ioscalls_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every DDL snapshot is the schema of at least one fixture, every fixture names a call history snapshot and
// parses without a warning or a refused record.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "ios", "callhistory-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		h := parsertest.NewHarness(t)
		remote, logical := remotesFor(v)
		b := fx.BundleIOS(h, remote, logical)
		p := ioscalls.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
			t.Errorf("%s: err %v, violations %v, %d records", v, res.Err, res.Violations, len(res.Records))
		}
		for _, w := range res.Warnings {
			t.Errorf("%s: a clean fixture gave a warning: %+v", v, w)
		}
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "callhistory-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		remote, logical := remotesFor(v)
		b := parsertest.LoadFixture(t, "testdata", v).BundleIOS(h, remote, logical)
		p := ioscalls.Parser{}
		parsertest.AssertDeterministic(t, func() parsertest.Result {
			res := h.RunLenient(p, b.Input(h, p, false))
			if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
				t.Errorf("%s: err %v, violations %v, %d records", v, res.Err, res.Violations, len(res.Records))
			}
			return res
		})
	}
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "coredata-current").BundleIOS(h, remoteCore, logicalCore)
	p := ioscalls.Parser{}
	res := h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	// a parser that ignored its context would still emit every row of a small database
	if want := len(parsertest.LoadFixture(t, "testdata", "coredata-current").Expect.Records); len(res.Records) >= want {
		t.Errorf("%d records on an already cancelled context, the database holds %d", len(res.Records), want)
	}
}
