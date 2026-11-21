package androidgooglemessages_test

import (
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

// at renders the time of a record's Time (nil: "").
func timeText(tm *records.Time) string {
	if tm == nil {
		return ""
	}
	return tm.T.UTC().Format(time.RFC3339)
}

// kindTimeOf is the text of the named time of a record ("" when the record has none of that kind).
func kindTimeOf(r records.Record, kind string) string {
	for _, nt := range r.Times {
		if nt.Kind == kind {
			return timeText(&nt.Time)
		}
	}
	return ""
}

// M22 (spec 6.6, R14): a stored 0 is no time, never 1970: no Times.sent, the status states the sentinel. B5: with both
// columns 0 there is no Time at all.
func TestGoogleMessagesZeroTimeIsAbsent(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	m22 := recs[22]
	if m22.Time != nil || kindTimeOf(m22, "sent") != "" || kindTimeOf(m22, "received") != "" || len(m22.Times) != 0 {
		t.Errorf("M22: Time %v, Times %v", m22.Time, m22.Times)
	}
	if rawOf(m22)["received_timestamp"] != int64(0) || rawOf(m22)["received_timestamp_status"] != "sentinel" ||
		rawOf(m22)["sent_timestamp_status"] != "sentinel" || rawOf(m22)["time_basis"] != "absent" {
		t.Errorf("M22 raw: %v", rawOf(m22))
	}
	// a Go-built incoming row with a valid received time and sent_timestamp 0
	_, got := parsedRows(t, []mrow{{1, map[string]any{"sent_timestamp": int64(0)}}})
	r := got[1]
	if kindTimeOf(r, "sent") != "" || rawOf(r)["sent_timestamp_status"] != "sentinel" || rawOf(r)["sent_timestamp"] != int64(0) ||
		kindTimeOf(r, "received") != "2023-11-14T22:13:21Z" || timeText(r.Time) != "2023-11-14T22:13:21Z" || rawOf(r)["time_basis"] != "received" {
		t.Errorf("Go-built: Time %v, Times %v, raw %v", r.Time, r.Times, rawOf(r))
	}
}

// Spec 6.6 (the 1e11 floor of rowrun.MillisFrom1973): a value below 1e11 is invalid (a seconds value in a ms column,
// negative, beyond 2100), never rescaled; 1e11 is the first valid millisecond.
func TestGoogleMessagesTimeFloor(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	for _, n := range []int64{23, 24, 25} {
		r := recs[n]
		if r.Time != nil || rawOf(r)["received_timestamp_status"] != "invalid" || rawOf(r)["time_basis"] != "invalid" {
			t.Errorf("M%d: Time %v, raw %v", n, r.Time, rawOf(r))
		}
	}
	_, got := parsedRows(t, []mrow{
		{1, map[string]any{"received_timestamp": int64(99_999_999_999)}},
		{2, map[string]any{"received_timestamp": int64(100_000_000_000)}},
	})
	if r := got[1]; r.Time != nil || rawOf(r)["received_timestamp_status"] != "invalid" || rawOf(r)["received_timestamp"] != int64(99_999_999_999) {
		t.Errorf("99,999,999,999: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := got[2]; timeText(r.Time) != "1973-03-03T09:46:40Z" || has(r, "received_timestamp_status") || rawOf(r)["time_basis"] != "received" {
		t.Errorf("100,000,000,000: Time %v, raw %v", r.Time, rawOf(r))
	}
}

// B5, 4I5-P2, ruling 4I5-T4c1: the sent time is the Time only when received_timestamp is a stored 0 (or NULL, no value); a non-zero
// received that is invalid or unreadable gives no Time and no fallback.
func TestGoogleMessagesTimeFallsBackToSentOnlyOnZeroOrNull(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	if r := recs[21]; timeText(r.Time) != "2023-11-14T22:13:41Z" || rawOf(r)["time_basis"] != "sent" {
		t.Errorf("M21: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := recs[22]; r.Time != nil || rawOf(r)["time_basis"] != "absent" {
		t.Errorf("M22: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := recs[23]; r.Time != nil || rawOf(r)["time_basis"] != "invalid" || rawOf(r)["sent_timestamp"] != int64(1700000022500) {
		t.Errorf("M23 (a valid-looking sent time must not be used): Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := recs[1]; timeText(r.Time) != "2023-11-14T22:13:21Z" || rawOf(r)["time_basis"] != "received" {
		t.Errorf("M1: Time %v, raw %v", r.Time, rawOf(r))
	}
	_, got := parsedRows(t, []mrow{
		{1, map[string]any{"received_timestamp": "1700000001000"}},
		{2, map[string]any{"received_timestamp": int64(0), "sent_timestamp": int64(1700000000)}},
		{3, map[string]any{"received_timestamp": nil}}, // the oracle's open choice: a NULL is no value, B5's "else" applies
		{4, map[string]any{"received_timestamp": int64(-5)}},
	})
	if r := got[1]; r.Time != nil || rawOf(r)["time_basis"] != "undecided" || unknownOf(r)["received_timestamp"] != "type-mismatch" || has(r, "received_timestamp") {
		t.Errorf("received TEXT with a valid sent: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := got[2]; r.Time != nil || rawOf(r)["time_basis"] != "invalid" || rawOf(r)["sent_timestamp_status"] != "invalid" {
		t.Errorf("received 0, sent in seconds: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := got[3]; timeText(r.Time) != "2023-11-14T22:13:20Z" || rawOf(r)["time_basis"] != "sent" || has(r, "received_timestamp") {
		t.Errorf("received NULL: Time %v, raw %v", r.Time, rawOf(r))
	}
	if r := got[4]; r.Time != nil || rawOf(r)["time_basis"] != "invalid" {
		t.Errorf("received -5: Time %v, raw %v", r.Time, rawOf(r))
	}
}
