package ioscalendar_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every fixture variant is parsed with a stated outcome: the variants with records match their literal oracle
// (TestIOSCalendarMatchesOracle), and calendar-ios-oldlayout (the older Event layout at the system path) fails with
// ErrNoCalendarItemTable and no record. Every DDL snapshot of the calendar-ios directory is used by a fixture, and
// every fixture names one or states that it has none (oldlayout's Event table is written by the generator).
func TestParserCoversEveryVariant(t *testing.T) {
	refused := map[string]error{"calendar-ios-oldlayout": ioscalendar.ErrNoCalendarItemTable}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		want, isRefused := refused[v]
		switch {
		case slices.Contains(recordedVariants(), v) && !isRefused:
			if len(fx.Expect.Records) == 0 {
				t.Errorf("%s: a recorded variant whose oracle states no record", v)
			}
		case isRefused:
			res := parseVariant(t, v)
			if !errors.Is(res.Err, want) || len(res.Records) != 0 || len(res.Violations) != 0 {
				t.Errorf("%s: err %v, %d records, violations %v; want %v and none", v, res.Err, len(res.Records), res.Violations, want)
			}
		default:
			t.Errorf("%s is parsed by no test with a stated outcome", v)
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "calendar-ios", "*.sql"))
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

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "calendar-ios"), "calendar-ios")
	p := ioscalendar.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}
