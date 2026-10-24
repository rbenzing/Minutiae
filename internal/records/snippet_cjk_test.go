package records

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestFoldIndexSegmentsLikeNormalizeText (M11): the fold index of a text holds exactly what the
// index holds, the space between two CJK letters included.
func TestFoldIndexSegmentsLikeNormalizeText(t *testing.T) {
	for _, s := range []string{
		string([]rune{0x65e5, 0x672c, 0x8a9e}),
		string([]rune{0x6771, 0x4eac}) + "tower",
		string([]rune{0x30a2, 0x309a, 0x30a4}),
		string([]rune{0x3300}),
		"x " + string([]rune{0x4eba, 0x3005, ' ', 0x65e5}) + " y",
	} {
		if got, want := buildFoldIndex(s).cat, evidence.FTSFoldDiacritics(evidence.NormalizeText(s)); got != want {
			t.Errorf("buildFoldIndex(%q).cat = %q, want %q", s, got, want)
		}
	}
}

func cps(rs ...rune) string { return string(rs) }

// TestSnippetHighlightsCJKWordInsideARun (Review Focus 4): a word inside a CJK run is highlighted
// as a whole, and only its own letters.
func TestSnippetHighlightsCJKWordInsideARun(t *testing.T) {
	spans := func(text, query string, o TextOptions) []Span {
		t.Helper()
		q, err := CompileQuery(query, o)
		if err != nil {
			t.Fatal(err)
		}
		s := SnippetFor(text, q, 120)
		if s == nil {
			t.Fatalf("no snippet for %q", text)
		}
		return s.Spans
	}
	got := spans(cps(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8), cps(0x672c, 0x8a9e), TextOptions{})
	want := []Span{{Text: cps(0x65e5)}, {Text: cps(0x672c, 0x8a9e), Match: true}, {Text: cps(0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8)}}
	if !slices.Equal(got, want) {
		t.Errorf("word in a run: %v, want %v", got, want)
	}
	got = spans(cps(0x6771, 0x4eac, 0x90fd, 0x5e81), cps(0x6771, 0x4eac), TextOptions{Substring: true})
	if len(got) < 1 || got[0] != (Span{Text: cps(0x6771, 0x4eac), Match: true}) {
		t.Errorf("substring: %v, want the first span to be the match", got)
	}
	got = spans(cps(0x6771, 0x4eac)+"tower "+cps(0x3068, ' ', 0x4eac, 0x90fd), cps(0x4eac, 0x90fd), TextOptions{})
	var matched string
	for _, s := range got {
		if s.Match {
			matched += s.Text
		}
	}
	if matched != cps(0x4eac, 0x90fd) || got[0].Match {
		t.Errorf("word beside Latin: %v, want only the last two letters matched", got)
	}
}
