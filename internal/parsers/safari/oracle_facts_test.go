package safari_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func rawOf(r parsertest.ExpRecord) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// An application tombstone is the application's own deletion marker (4F-R13): deleted, no visit time
// (the instant of the deleted visit is unknown), and only the deleted range as Times.
func TestOracleStatesTombstonesAsDeletedWithoutVisitTime(t *testing.T) {
	for _, v := range []string{"current", "current-wal"} {
		fx := parsertest.LoadFixture(t, "testdata", v)
		n := 0
		for _, r := range fx.Expect.Records {
			if r.Table != "history_tombstones" {
				continue
			}
			n++
			del, _ := r.Payload["deleted"].(map[string]any)
			if !r.Deleted || r.Time != nil || r.TimeEnd != nil || del["source"] != "history_tombstones" || len(del) != 1 {
				t.Errorf("%s tombstone %d: deleted %v time %v time_end %v payload.deleted %v", v, r.Rowid, r.Deleted, r.Time, r.TimeEnd, del)
			}
			for k := range r.Times {
				if k != "deleted_range_start" && k != "deleted_range_end" {
					t.Errorf("%s tombstone %d: Times kind %q", v, r.Rowid, k)
				}
			}
			if r.Type != "web_visit" || r.Payload["url"] == nil {
				t.Errorf("%s tombstone %d: type %q url %v", v, r.Rowid, r.Type, r.Payload["url"])
			}
		}
		if n == 0 {
			t.Errorf("%s: no tombstone records", v)
		}
		if got := fx.Expect.Notes["tombstones_without_url"]; got != "2" {
			t.Errorf("%s: note tombstones_without_url = %q, want 2 (one NULL url, one empty)", v, got)
		}
	}
	fx := parsertest.LoadFixture(t, "testdata", "no-tombstones")
	if fx.Expect.Notes["table.history_tombstones"] != "absent" {
		t.Errorf("no-tombstones notes: %v", fx.Expect.Notes)
	}
	for _, r := range fx.Expect.Records {
		if r.Table == "history_tombstones" {
			t.Errorf("no-tombstones has a tombstone record")
		}
	}
}

// The Cocoa instant is rounded half away from zero to the nanosecond; the Python half of that rule
// is checked here against the Go converter, which the generator shares no code with.
func TestOracleStatesNanosecondRounding(t *testing.T) {
	const stored = 704714400.123456789
	fx := parsertest.LoadFixture(t, "testdata", "current")
	want, ok := ts.CocoaSeconds(stored)
	if !ok {
		t.Fatal("ts.CocoaSeconds refuses the rounding probe")
	}
	found := false
	for _, r := range fx.Expect.Records {
		n, isNum := rawOf(r)["visit_time"].(json.Number)
		if f, err := strconv.ParseFloat(string(n), 64); !isNum || err != nil || f != stored {
			continue
		}
		found = true
		if r.Time == nil {
			t.Fatalf("visit %d: no time", r.Rowid)
		}
		got, err := time.Parse(time.RFC3339Nano, *r.Time)
		if err != nil || !got.Equal(want) {
			t.Errorf("visit %d: oracle time %s (%v), ts.CocoaSeconds gives %s", r.Rowid, *r.Time, err, want.Format(time.RFC3339Nano))
		}
		if want.Nanosecond()%1000 == 0 {
			t.Errorf("the probe lost its sub-microsecond digits: %s", want.Format(time.RFC3339Nano))
		}
	}
	if !found {
		t.Error("no visit with visit_time 704714400.123456789")
	}
}

// Literal facts of the scenario, written from the rules (4F-R9, R14, R24), not read back from the generator.
func TestOracleStatesTheScenarioRules(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current")
	statuses := map[string]int{}
	var visitless []int64
	visits := 0
	for _, r := range fx.Expect.Records {
		switch r.Table {
		case "history_visits":
			visits++
			if s, ok := rawOf(r)["visit_time_status"].(string); ok {
				statuses[s]++
				if r.Time != nil {
					t.Errorf("visit %d has status %s and a time", r.Rowid, s)
				}
			}
		case "history_items":
			visitless = append(visitless, r.Rowid)
			if rawOf(r)["visitless"] != true || r.Payload["visit_id"] != nil || r.Time != nil {
				t.Errorf("item %d: raw %v visit_id %v time %v", r.Rowid, rawOf(r), r.Payload["visit_id"], r.Time)
			}
		}
		if r.Payload["browser"] != "safari" {
			t.Errorf("%s %d: browser %v", r.Table, r.Rowid, r.Payload["browser"])
		}
	}
	// 0 and -1 are the sentinels, 4e9 is after 2100.
	if statuses["sentinel"] != 2 || statuses["invalid"] != 1 {
		t.Errorf("visit_time statuses %v, want 2 sentinel and 1 invalid", statuses)
	}
	if !slices.Equal(visitless, []int64{2, 3, 4}) {
		t.Errorf("visitless items %v, want items 2, 3 and 4", visitless)
	}
	// the dangling visit (history_item 999) gives a warning and no record; the count is a note.
	if len(fx.Expect.Warnings) != 1 || fx.Expect.Warnings[0].Table != "history_visits" || fx.Expect.Notes["unmapped_rows.history_visits"] != "1" {
		t.Errorf("warnings %v notes %v", fx.Expect.Warnings, fx.Expect.Notes)
	}
	if visits == 0 {
		t.Error("no visit records")
	}
}
