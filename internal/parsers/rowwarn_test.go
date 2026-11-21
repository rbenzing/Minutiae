package parsers

import (
	"net/url"
	"strings"
	"testing"
)

// rowWarningCounts counts, per lower-cased table, the warnings that stand in for a row that produced no record. A
// warning counts only when its locator names a row (a rowid) and no record carries that same locator: a parser may
// warn about a row it still emitted (androidtelegram: a users row with no name, phone or username), and that row is
// already counted as a record. A table-level warning (locator "sqlite:table=<t>", no rowid) is a summary, not a row,
// and is not counted; a row of a WITHOUT ROWID table has no rowid, so rowrun's unmapped warning for it is
// indistinguishable from a summary here and is not counted (the oracle's tables all have rowids). The real-data
// oracle (realdata_test.go) uses it; it carries no build tag so a unit test pins it.
func rowWarningCounts(locators, recordLocators []string) map[string]int64 {
	emitted := make(map[string]struct{}, len(recordLocators))
	for _, l := range recordLocators {
		emitted[l] = struct{}{}
	}
	out := map[string]int64{}
	for _, loc := range locators {
		if _, ok := emitted[loc]; ok {
			continue
		}
		rest, ok := strings.CutPrefix(loc, "sqlite:")
		if !ok {
			continue
		}
		table, hasRowid := "", false
		for _, f := range strings.Split(rest, ";") {
			if v, ok := strings.CutPrefix(f, "table="); ok {
				table = v
				if u, err := url.PathUnescape(v); err == nil {
					table = u
				}
			} else if _, ok := strings.CutPrefix(f, "rowid="); ok {
				hasRowid = true
			}
		}
		if hasRowid && table != "" {
			out[strings.ToLower(table)]++
		}
	}
	return out
}

func TestRowWarningCountsSkipsTableLevel(t *testing.T) {
	got := rowWarningCounts([]string{
		"sqlite:table=messages",                     // table-level summary: not a row
		"sqlite:table=Messages;rowid=7",             // a row
		"sqlite:table=messages;rowid=8;wal_frame=2", // a row in the WAL
		"sqlite:table=users",                        // table-level
		"other:thing",                               // not a sqlite locator
		"",
	}, nil)
	if len(got) != 1 || got["messages"] != 2 {
		t.Fatalf("rowWarningCounts = %v, want messages:2 only", got)
	}
}

func TestRowWarningCountsSkipsWarningAtRecordLocator(t *testing.T) {
	got := rowWarningCounts([]string{
		"sqlite:table=users;rowid=3", // warns about a row that still emitted a record: not counted
		"sqlite:table=users;rowid=4", // a row with no record: counted
		"sqlite:table=users",         // table-level
	}, []string{"sqlite:table=users;rowid=3", "sqlite:table=users;rowid=9"})
	if len(got) != 1 || got["users"] != 1 {
		t.Fatalf("rowWarningCounts = %v, want users:1 only", got)
	}
}
