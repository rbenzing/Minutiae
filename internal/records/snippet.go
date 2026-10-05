package records

import (
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Snippet widths in runes.
const (
	defaultSnippetWidth = 120
	minSnippetWidth     = 20
	maxSnippetWidth     = 400
	// maxSnippetMatches bounds the matches one snippet looks at.
	maxSnippetMatches = 4096
	// maxMemoUnitBytes and maxMemoUnits bound the cache of normalized units.
	maxMemoUnitBytes = 16
	maxMemoUnits     = 1 << 14
)

func clampSnippetWidth(w int) int {
	switch {
	case w <= 0:
		return defaultSnippetWidth
	case w < minSnippetWidth:
		return minSnippetWidth
	case w > maxSnippetWidth:
		return maxSnippetWidth
	}
	return w
}

// scanPrefix returns the part of text a snippet is built from: at most snippetScanBytes, cut on a
// rune boundary.
func scanPrefix(text string) (scanned string, cut bool) {
	if len(text) <= snippetScanBytes {
		return text, false
	}
	i := snippetScanBytes
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	return text[:i], true
}

// attaches reports whether r continues the unit (the grapheme cluster approximation) of the rune
// before it: a combining mark, a joiner, a Hangul vowel or final jamo, a halfwidth voiced-sound mark.
// Normalization composes only inside such a unit, so a unit can be normalized on its own.
func attaches(r rune) bool {
	return r == 0x200C || r == 0x200D || r == 0xFF9E || r == 0xFF9F ||
		(r >= 0x1160 && r <= 0x11FF) || (r >= 0xD7B0 && r <= 0xD7FF) || unicode.Is(unicode.M, r)
}

// foldString is the display-time comparison form of normalized text: the combining diacritical marks
// U+0300 to U+036F are dropped (the tokenizers of the index drop them too).
func foldString(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	d := norm.NFD.String(s)
	return strings.Map(func(r rune) rune {
		if r >= 0x300 && r <= 0x36F {
			return -1
		}
		return r
	}, d)
}

// foldUnit is the folded normalized form of one unit of the original text, as the index sees it:
// "" for a unit the normalization drops, " " for a blank.
func foldUnit(unit string, memo map[string]string) string {
	if len(unit) == 1 && unit[0] < utf8.RuneSelf {
		c := unit[0]
		switch {
		case c <= ' ' || c == 0x7F:
			return " "
		case c >= 'A' && c <= 'Z':
			return string(c + ('a' - 'A'))
		}
		return unit
	}
	if s, ok := memo[unit]; ok {
		return s
	}
	// the sentinels keep NormalizeText from trimming and collapsing blanks of the unit
	s := evidence.NormalizeText("|" + unit + "|")
	out := ""
	if len(s) >= 2 && s[0] == '|' && s[len(s)-1] == '|' {
		out = foldString(s[1 : len(s)-1])
	}
	if len(unit) <= maxMemoUnitBytes && len(memo) < maxMemoUnits {
		memo[unit] = out
	}
	return out
}

// foldIndex maps the folded text back to the original through its units.
type foldIndex struct {
	units []int   // original start offset of each unit, then len(text)
	cat   string  // the folded, normalized text
	owner []int32 // the unit of every byte of cat
}

func buildFoldIndex(text string) *foldIndex {
	var cat strings.Builder
	fx := &foldIndex{units: make([]int, 0, len(text)/2+1), owner: make([]int32, 0, len(text))}
	memo := map[string]string{}
	lastBlank := false
	for i := 0; i < len(text); {
		start := i
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
		for i < len(text) {
			r, s := utf8.DecodeRuneInString(text[i:])
			if !attaches(r) {
				break
			}
			i += s
		}
		u := int32(len(fx.units))
		fx.units = append(fx.units, start)
		for _, r := range foldUnit(text[start:i], memo) {
			// leading blanks are trimmed and runs of blanks collapsed, as NormalizeText does
			if r == ' ' && (cat.Len() == 0 || lastBlank) {
				continue
			}
			lastBlank = r == ' '
			n, _ := cat.WriteRune(r)
			for k := 0; k < n; k++ {
				fx.owner = append(fx.owner, u)
			}
		}
	}
	fx.units = append(fx.units, len(text))
	fx.cat = cat.String()
	return fx
}

// origRange maps the bytes [a, b) of the folded text to the bytes of the original text, whole units.
func (fx *foldIndex) origRange(a, b int) (int, int) {
	return fx.units[fx.owner[a]], fx.units[fx.owner[b-1]+1]
}

type token struct{ start, end int }

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r)
}

// tokenize splits s into runs of letters and digits.
func tokenize(s string) []token {
	var out []token
	start := -1
	for i, r := range s {
		if isTokenRune(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			out = append(out, token{start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, token{start, len(s)})
	}
	return out
}

func tokenTexts(s string) []string {
	toks := tokenize(s)
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = s[t.start:t.end]
	}
	return out
}

type byteRange struct{ start, end int }

// find returns the ranges of the original text the positive needles match, merged, ascending.
func (fx *foldIndex) find(q *TextQuery) []byteRange {
	if q == nil || fx.cat == "" {
		return nil
	}
	toks := tokenize(fx.cat)
	var found []byteRange
	add := func(a, b int) {
		s, e := fx.origRange(a, b)
		found = append(found, byteRange{s, e})
	}
	for _, nd := range q.needles {
		if len(found) >= maxSnippetMatches {
			break
		}
		needle := foldString(nd.Text)
		if nd.Kind == NeedleSubstring {
			if needle == "" {
				continue
			}
			for from := 0; len(found) < maxSnippetMatches; {
				at := strings.Index(fx.cat[from:], needle)
				if at < 0 {
					break
				}
				add(from+at, from+at+len(needle))
				from += at + len(needle)
			}
			continue
		}
		words := tokenTexts(needle)
		if len(words) == 0 {
			continue
		}
		for i := 0; i+len(words) <= len(toks) && len(found) < maxSnippetMatches; i++ {
			ok := true
			for j, w := range words {
				tk := fx.cat[toks[i+j].start:toks[i+j].end]
				if j == len(words)-1 && nd.Kind == NeedlePrefix {
					ok = strings.HasPrefix(tk, w)
				} else {
					ok = tk == w
				}
				if !ok {
					break
				}
			}
			if ok {
				add(toks[i].start, toks[i+len(words)-1].end)
			}
		}
	}
	slices.SortFunc(found, func(a, b byteRange) int { return a.start - b.start })
	var merged []byteRange
	for _, r := range found {
		if n := len(merged); n > 0 && r.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, r.end)
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// SnippetFor builds the snippet of text for the positive terms of q: a window of at most width runes
// (0 = 120, clamped to 20..400) around the first match, with every match in the window marked. The
// spans concatenate to a contiguous substring of the original text, cut at rune boundaries, and a
// match is highlighted in its original spelling: the matches are found in the normalized, folded text
// (letters and digits form the tokens) and mapped back through the units (grapheme-cluster
// approximations) of the original, so a folded match covers whole characters. When nothing is found
// the head of the text is shown unmarked. Only the first snippetScanBytes of text are read. It
// returns nil for an empty text and never panics.
func SnippetFor(text string, q *TextQuery, width int) *Snippet {
	if text == "" {
		return nil
	}
	width = clampSnippetWidth(width)
	scanned, cut := scanPrefix(text)
	fx := buildFoldIndex(scanned)
	matches := fx.find(q)

	ws := 0
	if len(matches) > 0 {
		ws = matches[0].start
		for lead := width / 4; lead > 0 && ws > 0; lead-- {
			_, size := utf8.DecodeLastRuneInString(scanned[:ws])
			ws -= size
		}
		// start on a unit boundary
		u := sort.Search(len(fx.units), func(i int) bool { return fx.units[i] > ws }) - 1
		if u >= 0 {
			ws = fx.units[u]
		}
	}
	we := ws
	for n := 0; n < width && we < len(scanned); n++ {
		_, size := utf8.DecodeRuneInString(scanned[we:])
		we += size
	}

	s := &Snippet{LeadingCut: ws > 0, TrailingCut: we < len(scanned) || cut}
	pos := ws
	plain := func(to int) {
		if to > pos {
			s.Spans = append(s.Spans, Span{Text: scanned[pos:to]})
			pos = to
		}
	}
	for _, m := range matches {
		a, b := max(m.start, ws), min(m.end, we)
		if a >= b {
			continue
		}
		plain(a)
		s.Spans = append(s.Spans, Span{Text: scanned[a:b], Match: true})
		pos = b
	}
	plain(we)
	return s
}
