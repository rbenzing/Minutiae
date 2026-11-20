package androidcalendar_test

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every fixture variant is parsed with a stated outcome: the variants with records match their literal oracle
// (TestAndroidCalendarMatchesOracle), calendar-noevents fails with ErrNoEventsTable and calendar-otherversion with
// ErrUnsupportedVersion, each with no record. Every DDL snapshot of the calendar-android directory is used by a
// fixture, and every fixture names one.
func TestParserCoversEveryVariant(t *testing.T) {
	refused := map[string]error{"calendar-noevents": androidcalendar.ErrNoEventsTable, "calendar-otherversion": androidcalendar.ErrUnsupportedVersion}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		want, isRefused := refused[v]
		switch {
		case slices.Contains(recordedVariants, v) && !isRefused:
			if len(fx.Expect.Records) == 0 {
				t.Errorf("%s: a recorded variant whose oracle states no record", v)
			}
		case isRefused:
			res := parseAt(t, remoteProvider, fx.Files)
			if !errors.Is(res.Err, want) || len(res.Records) != 0 || len(res.Violations) != 0 {
				t.Errorf("%s: err %v, %d records, violations %v; want %v and none", v, res.Err, len(res.Records), res.Violations, want)
			}
		default:
			t.Errorf("%s is parsed by no test with a stated outcome", v)
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "calendar-android", "*.sql"))
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

// The same input gives the same records, warnings and notes run after run, whatever time.Local is: the zone of the
// host never shows in a record (AssertDeterministic varies time.Local).
func TestParseIsDeterministic(t *testing.T) {
	for _, v := range []string{"calendar-601", "calendar-deleted", "calendar-600"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteProvider)
			p := androidcalendar.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
		})
	}
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "calendar-601").Bundle(h, remoteProvider)
	p := androidcalendar.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}
