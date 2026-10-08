package records_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
)

func snippetText(s *records.Snippet) string {
	var b strings.Builder
	for _, sp := range s.Spans {
		b.WriteString(sp.Text)
	}
	return b.String()
}

func snippetMatches(s *records.Snippet) []string {
	var out []string
	for _, sp := range s.Spans {
		if sp.Match {
			out = append(out, sp.Text)
		}
	}
	return out
}

// checkSnippetShape asserts what every snippet promises: non-empty spans that alternate between
// match and plain text, and (for valid text) valid UTF-8 spans.
func checkSnippetShape(t *testing.T, text string, s *records.Snippet) {
	t.Helper()
	if s == nil {
		t.Fatalf("no snippet for %q", text)
	}
	for i, sp := range s.Spans {
		if sp.Text == "" {
			t.Errorf("span %d is empty: %+v", i, s)
		}
		if i > 0 && s.Spans[i-1].Match == sp.Match {
			t.Errorf("spans %d and %d have the same Match %v: %+v", i-1, i, sp.Match, s)
		}
		if utf8.ValidString(text) && !utf8.ValidString(sp.Text) {
			t.Errorf("span %d is not valid UTF-8: %q", i, sp.Text)
		}
	}
}

// TestSnippetSpansConcatenateToOriginalSubstring: the spans always read as one contiguous piece of the
// original text, and the cut flags say whether text lies before and after it.
func TestSnippetSpansConcatenateToOriginalSubstring(t *testing.T) {
	var plain, accented []string
	for i := 0; i < 3000; i++ {
		plain = append(plain, fmt.Sprintf("w%04d", i))
		accented = append(accented, fmt.Sprintf("caf%s%04d", cp(0xe9), i))
	}
	texts := map[string]string{"ascii": strings.Join(plain, " "), "accented": strings.Join(accented, ", ")}
	for name, text := range texts {
		for _, tc := range []struct {
			query string
			opt   records.TextOptions
		}{
			{"w0000", records.TextOptions{}},
			{"w1500", records.TextOptions{}},
			{"w2999", records.TextOptions{}},
			{"w15*", records.TextOptions{}},
			{"w1499 w1500", records.TextOptions{}},
			{`"w1500 w1501"`, records.TextOptions{}},
			{"w1500 w1501", records.TextOptions{Substring: true}},
			{"w1500", records.TextOptions{Substring: true}},
			{"cafe1500", records.TextOptions{}},
			{"cafe0000", records.TextOptions{}},
			{"cafe2999", records.TextOptions{Substring: true}},
			{"zzzz", records.TextOptions{}},
		} {
			q := mustCompile(t, tc.query, tc.opt)
			for _, width := range []int{0, 20, 21, 50, 120, 400} {
				s := records.SnippetFor(text, q, width)
				checkSnippetShape(t, text, s)
				got := snippetText(s)
				at := strings.Index(text, got)
				if at < 0 {
					t.Fatalf("%s %q width %d: %q is not a substring of the text", name, tc.query, width, got)
				}
				if s.LeadingCut != (at > 0) || s.TrailingCut != (at+len(got) < len(text)) {
					t.Errorf("%s %q width %d: cuts (%v,%v), the window is at [%d,%d) of %d", name, tc.query, width, s.LeadingCut, s.TrailingCut, at, at+len(got), len(text))
				}
				w := width
				switch {
				case w == 0:
					w = 120
				case w < 20:
					w = 20
				case w > 400:
					w = 400
				}
				if n := utf8.RuneCountInString(got); n > w {
					t.Errorf("%s %q width %d: %d runes shown", name, tc.query, width, n)
				}
			}
		}
	}
	// a short text is shown whole, uncut
	s := records.SnippetFor("one two three", mustCompile(t, "two", records.TextOptions{}), 120)
	if snippetText(s) != "one two three" || s.LeadingCut || s.TrailingCut {
		t.Errorf("short text: %+v", s)
	}
}

// TestSnippetHighlightsFoldedMatch: the match is found in the folded text and highlighted in the
// original spelling; a composed character is one unit.
func TestSnippetHighlightsFoldedMatch(t *testing.T) {
	sharp := "Stra" + cp(0xdf) + "e"
	nfd := "caf" + "e" + cp(0x301)
	for _, tc := range []struct {
		name, text, query string
		opt               records.TextOptions
		want              []string
	}{
		{"strasse highlights the sharp s", "Die " + sharp + " ist lang", "strasse", records.TextOptions{}, []string{sharp}},
		{"substring through the sharp s", "Die " + sharp + " ist lang", "strass", records.TextOptions{Substring: true}, []string{"Stra" + cp(0xdf)}},
		{"an NFD e acute is one match (word)", nfd + " au lait", "cafe", records.TextOptions{}, []string{nfd}},
		{"an NFD e acute is one match (prefix)", nfd + " au lait", "caf*", records.TextOptions{}, []string{nfd}},
		{"an NFD e acute is one match (substring)", nfd + " au lait", "cafe", records.TextOptions{Substring: true}, []string{nfd}},
		{"the precomposed form is found by the plain spelling", "caf" + cp(0xe9) + " au lait", "cafe", records.TextOptions{}, []string{"caf" + cp(0xe9)}},
		{"the plain form is found by the accented query", "cafe au lait", "caf" + cp(0xe9), records.TextOptions{}, []string{"cafe"}},
		{"upper case", "An Apple a day", "apple", records.TextOptions{}, []string{"Apple"}},
		{"dotted capital I", cp(0x130) + "stanbul is big", "istanbul", records.TextOptions{}, []string{cp(0x130) + "stanbul"}},
		{"dotless i", "in " + cp(0x131) + "sp" + cp(0x131) + "rta now", "ispirta", records.TextOptions{}, []string{cp(0x131) + "sp" + cp(0x131) + "rta"}},
		{"fullwidth", "x " + cp(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13) + " y", "abc123", records.TextOptions{}, []string{cp(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13)}},
		{"a bidi override inside the word", "my pass" + cp(0x202e) + "word is", "password", records.TextOptions{}, []string{"pass" + cp(0x202e) + "word"}},
		{"a zero width space separates words", "my pass" + cp(0x200b) + "word is", `"pass word"`, records.TextOptions{}, []string{"pass" + cp(0x200b) + "word"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := records.SnippetFor(tc.text, mustCompile(t, tc.query, tc.opt), 120)
			checkSnippetShape(t, tc.text, s)
			if got := snippetMatches(s); !slices.Equal(got, tc.want) {
				t.Fatalf("matches %q, want %q (spans %+v)", got, tc.want, s.Spans)
			}
			if snippetText(s) != tc.text {
				t.Errorf("short text not shown whole: %q", snippetText(s))
			}
		})
	}
}

// TestSnippetModes: word, prefix, phrase (across punctuation), several words, negation and
// substring queries highlight what they match.
func TestSnippetModes(t *testing.T) {
	sub := records.TextOptions{Substring: true}
	for _, tc := range []struct {
		name, text, query string
		opt               records.TextOptions
		want              []string
	}{
		{"word", "red apple pie", "apple", records.TextOptions{}, []string{"apple"}},
		{"a word is not a part of a longer word", "pineapple pie", "apple", records.TextOptions{}, nil},
		{"prefix", "apple and application", "app*", records.TextOptions{}, []string{"apple", "application"}},
		{"phrase across punctuation", "a red-apple, pie", `"red apple"`, records.TextOptions{}, []string{"red-apple"}},
		{"phrase across several blanks", "a red   apple pie", `"red apple"`, records.TextOptions{}, []string{"red   apple"}},
		{"phrase order", "apple red", `"red apple"`, records.TextOptions{}, nil},
		{"two words", "red apple pie", "red pie", records.TextOptions{}, []string{"red", "pie"}},
		{"negated word is not highlighted", "apple pie red", "apple -red", records.TextOptions{}, []string{"apple"}},
		{"or", "red apple pie", "pie OR red", records.TextOptions{}, []string{"red", "pie"}},
		{"every occurrence", "go go go", "go", records.TextOptions{}, []string{"go", "go", "go"}},
		{"substring", "call +1 (555) 123-4567 now", "555) 123", sub, []string{"555) 123"}},
		{"substring inside a word", "pineapple pie", "neap", sub, []string{"neap"}},
		{"substring across words", "red apple pie", "d app", sub, []string{"d app"}},
		{"substring url", "see https://example.com/path?x=1 ok", "ample.com/pa", sub, []string{"ample.com/pa"}},
		{"substring cjk", cp(0x4f60, 0x597d, 0x4e16, 0x754c, 0x20, 0x61), cp(0x597d, 0x4e16, 0x754c), sub, []string{cp(0x597d, 0x4e16, 0x754c)}},
		{"cjk run word", "x " + cp(0x4f60, 0x597d, 0x4e16, 0x754c) + " y", cp(0x4f60, 0x597d, 0x4e16, 0x754c), records.TextOptions{}, []string{cp(0x4f60, 0x597d, 0x4e16, 0x754c)}},
		{"digits are word characters", "room 42b and 42", "42", records.TextOptions{}, []string{"42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := records.SnippetFor(tc.text, mustCompile(t, tc.query, tc.opt), 120)
			checkSnippetShape(t, tc.text, s)
			if got := snippetMatches(s); !slices.Equal(got, tc.want) {
				t.Fatalf("matches %q, want %q (spans %+v)", got, tc.want, s.Spans)
			}
		})
	}
}

// TestSnippetBounded: a text is scanned only up to snippetScanBytes, whatever its size; the width is
// honoured and clamped; the cut flags tell.
func TestSnippetBounded(t *testing.T) {
	const mib4 = 4 << 20
	filler := strings.Repeat("lorem ipsum ", mib4/12+1)
	build := func(at int, needle string) string { return filler[:at] + " " + needle + " " + filler[at+1:mib4] }
	q := mustCompile(t, "needleword", records.TextOptions{})

	if records.SnippetScanBytes != 256<<10 {
		t.Fatalf("scan limit %d, want 256 KiB", records.SnippetScanBytes)
	}
	near := build(100<<10, "needleword")
	s := records.SnippetFor(near, q, 60)
	if got := snippetMatches(s); !slices.Equal(got, []string{"needleword"}) || !s.LeadingCut || !s.TrailingCut || utf8.RuneCountInString(snippetText(s)) != 60 {
		t.Errorf("a match inside the scan limit: %+v", s)
	}
	for _, at := range []int{300 << 10, 1 << 20, mib4 - 100} {
		s := records.SnippetFor(build(at, "needleword"), q, 60)
		checkSnippetShape(t, near, s)
		if len(snippetMatches(s)) != 0 || s.LeadingCut || !s.TrailingCut || !strings.HasPrefix(snippetText(s), "lorem ipsum") {
			t.Errorf("a match at %d is past the scan limit, the head is shown unhighlighted: %+v", at, s)
		}
	}
	// a word that straddles the limit is not found either
	straddle := records.SnippetScanBytes - 5
	if s := records.SnippetFor(build(straddle, "needleword"), q, 60); len(snippetMatches(s)) != 0 || !s.TrailingCut {
		t.Errorf("a straddling match: %+v", s)
	}
	// the word that ends exactly at the limit is found
	end := records.SnippetScanBytes - len("needleword") - 1
	if s := records.SnippetFor(build(end, "needleword"), q, 60); !slices.Equal(snippetMatches(s), []string{"needleword"}) {
		t.Errorf("a match just inside the limit: %+v", s)
	}
	// multi-byte text is cut on a rune boundary
	wide := strings.Repeat(cp(0xe9), mib4/2)
	s = records.SnippetFor(wide, q, 100)
	checkSnippetShape(t, wide, s)
	if n := utf8.RuneCountInString(snippetText(s)); n != 100 || !s.TrailingCut {
		t.Errorf("wide text: %d runes, %+v", n, s.TrailingCut)
	}
	// width: 0 is 120, below 20 is 20, above 400 is 400
	for _, tc := range []struct{ width, want int }{{0, 120}, {5, 20}, {-3, 120}, {20, 20}, {37, 37}, {400, 400}, {1000, 400}} {
		s := records.SnippetFor(near, q, tc.width)
		if n := utf8.RuneCountInString(snippetText(s)); n != tc.want {
			t.Errorf("width %d: %d runes, want %d", tc.width, n, tc.want)
		}
	}
}

// TestSnippetNoMatchFallsBackToHead: when nothing is found the head of the text is shown, unhighlighted.
func TestSnippetNoMatchFallsBackToHead(t *testing.T) {
	text := strings.Repeat("alpha beta gamma ", 40)
	for name, q := range map[string]*records.TextQuery{
		"no match": mustCompile(t, "zzzz", records.TextOptions{}), "nil query": nil,
		"substring no match": mustCompile(t, "qqq", records.TextOptions{Substring: true}),
	} {
		s := records.SnippetFor(text, q, 50)
		checkSnippetShape(t, text, s)
		if len(s.Spans) != 1 || s.Spans[0].Match || s.LeadingCut || !s.TrailingCut || snippetText(s) != text[:50] {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if s := records.SnippetFor("short", mustCompile(t, "zzzz", records.TextOptions{}), 50); snippetText(s) != "short" || s.TrailingCut || s.LeadingCut {
		t.Errorf("short text: %+v", s)
	}
	if s := records.SnippetFor("", mustCompile(t, "zzzz", records.TextOptions{}), 50); s != nil {
		t.Errorf("empty text: %+v, want nil", s)
	}
}
