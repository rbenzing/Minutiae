package f2fs

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// parseWarningAssumptions and clipString (3H Task 6 follow-ups).

func checkAssumptions(t *testing.T, as []string) {
	t.Helper()
	c := filesys.Candidate{Method: MethodRollForward, Assumptions: as}
	if _, err := filesys.CheckCandidate(c, 1<<20); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
	for _, a := range as {
		if !utf8.ValidString(a) || len(a) > 256 {
			t.Errorf("assumption %q: valid UTF-8 %v, %d bytes", a, utf8.ValidString(a), len(a))
		}
	}
}

func TestParseWarningAssumptionsCapsAtEight(t *testing.T) {
	var ws []string
	for i := range 10 {
		ws = append(ws, fmt.Sprintf("warning number %d", i))
	}
	ws = append(ws, ws[0]) // an exact duplicate is not counted again
	got := parseWarningAssumptions(ws)
	if len(got) != 9 || got[0] != "inode-parse-warning=warning number 0" || got[7] != "inode-parse-warning=warning number 7" ||
		got[8] != "inode-parse-warning=2 more not listed" {
		t.Errorf("got %q, want 8 entries and %q", got, "inode-parse-warning=2 more not listed")
	}
	checkAssumptions(t, got)
}

func TestParseWarningAssumptionsClipsLongWarnings(t *testing.T) {
	got := parseWarningAssumptions([]string{strings.Repeat("x", 1000)})
	if len(got) != 1 || len(got[0]) > 256 || !strings.HasSuffix(got[0], "...") {
		t.Fatalf("got %d entries, first of %d bytes, want one of at most 256 bytes ending in ...", len(got), len(got[0]))
	}
	checkAssumptions(t, got)
}

func TestParseWarningAssumptionsKeepsRunesWhole(t *testing.T) {
	const prefix = "inode-parse-warning="
	// A 3-byte rune whose bytes straddle the cut at every position.
	for pad := range 4 {
		w := strings.Repeat("a", 256-len(prefix)-3-pad) + strings.Repeat("€", 20)
		got := parseWarningAssumptions([]string{w})
		if len(got) != 1 || !strings.HasSuffix(got[0], "...") {
			t.Fatalf("pad %d: got %q", pad, got)
		}
		checkAssumptions(t, got)
		if body := strings.TrimSuffix(strings.TrimPrefix(got[0], prefix), "..."); strings.ContainsRune(body, utf8.RuneError) {
			t.Errorf("pad %d: a broken rune survived: %q", pad, got[0])
		}
	}
}

// Invalid UTF-8 earlier in the string is the parser's text, not a partial rune at the cut: only
// the trailing partial rune is trimmed, the rest of the warning stays.
func TestClipStringTrimsOnlyTheTrailingPartialRune(t *testing.T) {
	s := "keep \xff this text " + strings.Repeat("b", 100)
	if got := clipString(s, 50); got != s[:47]+"..." {
		t.Errorf("clipString = %q, want %q", got, s[:47]+"...")
	}
	// a partial rune at the cut is dropped whole
	e := "ab" + strings.Repeat("€", 10)
	if got := clipString(e, 10); got != "ab€"+"..." { // 7 bytes: "ab" + one 3-byte rune + 2 bytes of the next
		t.Errorf("clipString = %q, want the partial rune dropped", got)
	}
	if got := clipString("short", 50); got != "short" {
		t.Errorf("clipString = %q, want the string unchanged", got)
	}
}
