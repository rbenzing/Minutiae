package parsers

import (
	"net/url"
	"strings"
	"testing"
)

// rowWarningCounts counts, per lower-cased table, the warnings whose locator names a row (a rowid). A table-level
// warning (locator "sqlite:table=<t>", no rowid) is a summary, not a row, and is not counted. The real-data oracle
// (realdata_test.go) uses it; it carries no build tag so a unit test pins it.
func rowWarningCounts(locators []string) map[string]int64 {
	out := map[string]int64{}
	for _, loc := range locators {
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
	})
	if len(got) != 1 || got["messages"] != 2 {
		t.Fatalf("rowWarningCounts = %v, want messages:2 only", got)
	}
}
