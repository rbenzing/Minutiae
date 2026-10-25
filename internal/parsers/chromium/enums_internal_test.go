package chromium

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var enumRow = regexp.MustCompile(`^\| (download_state|download_danger|download_interrupt) \| (\d+) \| ([a-z0-9_]+) \|`)

// The names the parser gives are exactly the download rows of the committed cross-check (4F-R27); an integer the
// table does not list is "unknown", never a guessed name.
func TestDownloadEnumNamesFollowTheCrossCheck(t *testing.T) {
	raw, err := os.ReadFile("../../../tools/fixtures/schemas/chromium/CROSSCHECK.md")
	if err != nil {
		t.Fatal(err)
	}
	enums := map[string]func(int64) string{"download_state": stateName, "download_danger": dangerName, "download_interrupt": interruptName}
	listed := map[string]map[int64]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := enumRow.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if listed[m[1]] == nil {
			listed[m[1]] = map[int64]string{}
		}
		listed[m[1]][n] = m[3]
		if got := enums[m[1]](n); got != m[3] {
			t.Errorf("%s(%d) = %q, the cross-check lists %q", m[1], n, got, m[3])
		}
	}
	for e, fn := range enums {
		if len(listed[e]) == 0 {
			t.Fatalf("the cross-check has no %s rows", e)
		}
		for n := int64(-3); n < 300; n++ {
			if _, ok := listed[e][n]; !ok && fn(n) != "unknown" {
				t.Errorf("%s(%d) = %q, an integer the cross-check does not list must be unknown", e, n, fn(n))
			}
		}
	}
	// 32 is obsolete in the source and absent from the table: unknown, no guessed name; state 3 is a real stored value
	if got := interruptName(32); got != "unknown" {
		t.Errorf("interruptName(32) = %q, want unknown", got)
	}
	if got := stateName(3); got != "bug_140687" {
		t.Errorf("stateName(3) = %q, want bug_140687", got)
	}
}
