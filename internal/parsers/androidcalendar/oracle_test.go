package androidcalendar_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// childKeys are the payload and raw keys of the child joins and raw-only child tables, which Task 5 maps: the oracle
// states them and this task does not emit them yet, so they are filtered from the expected records (a filter Task 5
// removes).
var (
	childPayloadKeys = []string{"attendees", "attendees_total", "attendees_truncated", "reminders", "reminders_total", "reminders_truncated"}
	childRawKeys     = []string{
		"attendees", "attendees_unnamed", "reminders", "reminders_state", "alerts", "alerts_total", "alerts_truncated",
		"extended_properties", "extended_properties_total", "extended_properties_truncated", "raw_times",
		"attendees_state", "alerts_state", "extended_properties_state", "raw_times_state",
	}
)

// withoutChildren removes the Task 5 fields from an expected payload: the child keys, an "attendees" entry of
// raw.text_flags and the Attendees.* entries of raw.unknown_columns.
func withoutChildren(p map[string]any) {
	for _, k := range childPayloadKeys {
		delete(p, k)
	}
	raw, _ := p["raw"].(map[string]any)
	for _, k := range childRawKeys {
		delete(raw, k)
	}
	if tf, ok := raw["text_flags"].(string); ok {
		fl := slices.DeleteFunc(strings.Split(tf, ","), func(s string) bool { return s == "attendees" })
		if len(fl) == 0 {
			delete(raw, "text_flags")
		} else {
			raw["text_flags"] = strings.Join(fl, ",")
		}
	}
	if uc, ok := raw["unknown_columns"].(map[string]any); ok {
		for k := range uc {
			if strings.HasPrefix(k, "Attendees.") || strings.HasPrefix(k, "Reminders.") {
				delete(uc, k)
			}
		}
		if len(uc) == 0 {
			delete(raw, "unknown_columns")
		}
	}
}

// Parent spec 10 and plan 4I3 Task 4: every variant with records parses to exactly the Events records and warnings
// its literal oracle states (the child fields filtered until Task 5), with no contract violation.
func TestAndroidCalendarMatchesOracle(t *testing.T) {
	for _, v := range recordedVariants {
		t.Run(v, func(t *testing.T) {
			fx := parsertest.LoadFixture(t, "testdata", v)
			for i := range fx.Expect.Records {
				withoutChildren(fx.Expect.Records[i].Payload)
			}
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteProvider)
			p := androidcalendar.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect, "Events")
		})
	}
}
