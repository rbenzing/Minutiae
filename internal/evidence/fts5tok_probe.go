package evidence

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// ftsProbeCorpus is the fixed text the self-test indexes into a fresh SQLite to prove, at every verify, that
// the tokenizer replicas still agree with the SQLite that built the index. It is built from the generated
// tables: the first and the last code point of every range of fts5tok_tables.go and the code point on each
// side of it (a boundary is where a wrong table would differ), mixed with ASCII letters in framings that make
// separators, token characters, marks and trigram windows all matter. It also holds a token longer than
// fts5MaxTokenSize (the cut), runs of 3-byte characters across the cut, and the shapes the full-text index
// sees most. Every string is valid UTF-8 without NUL. The corpus is a pure function of the committed tables.
var ftsProbeCorpus = buildFTSProbeCorpus()

func buildFTSProbeCorpus() []string {
	edge := map[rune]bool{}
	add := func(c rune) {
		if c > 0 && c <= utf8.MaxRune && (c < 0xD800 || c > 0xDFFF) {
			edge[c] = true
		}
	}
	for _, r := range fts5TokKind {
		add(r.lo - 1)
		add(r.lo)
		add(r.hi)
		add(r.hi + 1)
		add(r.lo + (r.hi-r.lo)/2)
	}
	for _, r := range fts5TokFold {
		add(r.lo - 1)
		add(r.lo)
		add(r.hi)
		add(r.hi + 1)
		add(r.lo + (r.hi-r.lo)/2)
	}
	for _, c := range "\x01\t\n 09AZaz{~\x7f" {
		add(c)
	}
	for _, c := range []rune{0xFFFD, 0xFFFE, 0xFFFF, 0x10000, 0x10400, 0x10428, 0x1F600, 0xE000, 0xF8FF, 0xF0000, 0x10FFFF} {
		add(c)
	}
	cps := make([]rune, 0, len(edge))
	for c := range edge {
		cps = append(cps, c)
	}
	slices.Sort(cps)

	// every edge code point, in four framings, six to a string
	frames := []func(string) string{
		func(c string) string { return "a" + c + "b" },
		func(c string) string { return c },
		func(c string) string { return "xw" + c + "yz" },
		func(c string) string { return c + "q" },
	}
	var out []string
	for i := 0; i < len(cps); i += 6 {
		pack := cps[i:min(i+6, len(cps))]
		for _, frame := range frames {
			parts := make([]string, len(pack))
			for j, c := range pack {
				parts[j] = frame(string(c))
			}
			out = append(out, strings.Join(parts, " "))
		}
	}
	// the same code points adjacent to each other, so one run holds many characters
	var run strings.Builder
	for i, c := range cps {
		run.WriteRune(c)
		if i%24 == 23 || i == len(cps)-1 {
			out = append(out, run.String())
			run.Reset()
		}
	}
	return append(out,
		"",
		"a",
		"ab",
		"abc",
		"Hello, World 123",
		"caf\u00e9 CAF\u00c9 cafe\u0301 \u00e0\u00e8\u00ec\u00f2\u00f9",
		"o\u0323\u0302 e\u0301\u0302 a\u0345 \u0300x",
		"\u0e17\u0e14\u0e2a\u0e2d\u0e1a \u0e44\u0e17\u0e22",
		"\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8 \ud55c\uad6d\uc5b4",
		"x\ufffey\uffffz \ufffd",
		"\U0001f600 smile \U0001f1fa\U0001f1f8",
		"12ab34cd 5e6",
		strings.Repeat("a", fts5MaxTokenSize+232),        // longer than any token FTS5 keeps
		strings.Repeat("\u4e16", fts5MaxTokenSize/3+800), // 3-byte characters across the cut
		"tail "+strings.Repeat("b", fts5MaxTokenSize)+" tail",
	)
}
