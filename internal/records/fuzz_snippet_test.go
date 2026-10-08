package records_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
)

// FuzzSnippetFor: any text, needle and width: no panic, no empty span, the spans read as a contiguous
// substring of the scanned prefix of the text, valid text gives valid spans, the width bounds the runes.
func FuzzSnippetFor(f *testing.F) {
	for _, s := range []struct {
		text, needle string
		mode         uint8
		width        int
	}{
		{"red apple pie", "apple", 0, 120},
		{"Stra" + cp(0xdf) + "e", "strasse", 0, 20},
		{"cafe" + cp(0x301) + " au lait", "caf*", 0, 30},
		{"call +1 (555) 123-4567", "555) 123", 1, 400},
		{"a\xffb c", "b", 0, 0},
		{cp(0xff9e, 0xff9f, 0x1100, 0x1161, 0x11a8) + " x", "x", 0, 21},
		{"pass" + cp(0x200b, 0x202e) + "word", `"pass word"`, 0, 50},
		{strings.Repeat(cp(0x301), 500) + "a", "a", 1, 20},
		{strings.Repeat("ab ", 5000), "ab", 0, 77},
		{"", "x", 0, 10},
	} {
		f.Add(s.text, s.needle, s.mode, s.width)
	}
	f.Fuzz(func(t *testing.T, text, needle string, mode uint8, width int) {
		q, err := records.CompileQuery(needle, records.TextOptions{Substring: mode&1 == 1})
		if err != nil {
			q = nil // a refused query still has a snippet: the head
		}
		s := records.SnippetFor(text, q, width)
		if text == "" {
			if s != nil {
				t.Fatalf("empty text gave %+v", s)
			}
			return
		}
		if s == nil {
			t.Fatalf("no snippet for %q", text)
		}
		scanned := text
		if len(scanned) > records.SnippetScanBytes {
			scanned = scanned[:records.SnippetScanBytes]
		}
		var b strings.Builder
		for i, sp := range s.Spans {
			if sp.Text == "" {
				t.Fatalf("empty span %d", i)
			}
			if utf8.ValidString(text) && !utf8.ValidString(sp.Text) {
				t.Fatalf("span %d is not valid UTF-8: %q", i, sp.Text)
			}
			b.WriteString(sp.Text)
		}
		got := b.String()
		if got == "" || !strings.Contains(scanned, got) {
			t.Fatalf("%q is not a non-empty substring of the scanned text", got)
		}
		w := width
		switch {
		case w <= 0:
			w = 120
		case w < 20:
			w = 20
		case w > 400:
			w = 400
		}
		if n := utf8.RuneCountInString(got); n > w {
			t.Fatalf("%d runes shown, width %d", n, w)
		}
	})
}
