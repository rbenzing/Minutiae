package ioswacalls_test

import (
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every CallHistory DDL snapshot is used by a fixture (the chat and contacts snapshots are the other parsers'), every
// variant with records parses to exactly the records and warnings its oracle states with no contract violation, and every
// variant without records is one Probe does not accept.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "whatsapp-ios", "callhistory*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if fx.Expect.DDL != "" {
			used[fx.Expect.DDL] = true
		}
		h := parsertest.NewHarness(t)
		b := bundleOf(h, fx, v)
		p := ioswacalls.Parser{}
		if len(fx.Expect.Records) == 0 {
			got, err := p.Probe(t.Context(), b.Input(h, p, true))
			if err != nil || got.Status == parse.Applicable {
				t.Errorf("%s: no records in the oracle, but Probe = %+v, %v", v, got, err)
			}
			continue
		}
		res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied")
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) != len(fx.Expect.Records) {
			t.Errorf("%s: err %v, violations %v, %d records, oracle %d", v, res.Err, res.Violations, len(res.Records), len(fx.Expect.Records))
		}
		if len(res.Warnings) != len(fx.Expect.Warnings) {
			t.Errorf("%s: %d warnings, oracle %d: %+v", v, len(res.Warnings), len(fx.Expect.Warnings), res.Warnings)
		}
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
}

// Two runs of the parser over one bundle give the same records, warnings, notes and error, sixteen times over (map order, a
// stateful parser and a clock all fail this).
func TestParseIsDeterministic(t *testing.T) {
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			p := ioswacalls.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result {
				res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied")
				if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
					t.Errorf("%s: err %v, violations %v, %d records", v, res.Err, res.Violations, len(res.Records))
				}
				return res
			})
		})
	}
}
