package evidence

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// TestNormalizeTable pins every step of the pipeline (plan "NormalizeText", pipeline version 1) and
// the spec §13 cases. The expected values are written as escapes so the source holds no ambiguous
// glyph and no invisible control character.
func TestNormalizeTable(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// the empty string and trivia
		{"empty", "", ""},
		{"ascii lower stays", "plain text 123", "plain text 123"},
		{"ascii upper folds", "Hello WORLD", "hello world"},

		// step 1: invalid UTF-8 and NUL
		{"invalid byte becomes U+FFFD", "a\xffb", "a\ufffdb"},
		{"each invalid byte becomes one U+FFFD", "a\xff\xfeb", "a\ufffd\ufffdb"},
		{"truncated sequence is invalid", "a\xe2\x82", "a\ufffd\ufffd"},
		{"overlong encoding is invalid", "\xc0\xaf", "\ufffd\ufffd"},
		{"encoded surrogate is invalid", "\xed\xa0\x80", "\ufffd\ufffd\ufffd"},
		{"a real U+FFFD stays", "a\ufffdb", "a\ufffdb"},
		{"NUL becomes a space", "a\x00b", "a b"},
		{"NUL run is one space", "a\x00\x00\x00b", "a b"},
		{"only NUL is empty (trimmed)", "\x00", ""},

		// step 2: Cf dropped
		{"bidi override U+202E removed", "ab\u202ecd", "abcd"},
		{"bidi isolate U+2066 and pop U+2069 removed", "a\u2066b\u2069c", "abc"},
		{"left-to-right mark removed", "a\u200eb", "ab"},
		{"zero width space U+200B separates words (R32)", "a\u200bb", "a b"},
		{"zero width space run is one space", "a\u200b\u200b\u200bb", "a b"},
		{"zero width space beside a space is one space", "a \u200bb\u200b c", "a b c"},
		{"zero width space beside a dropped Cf is one space", "a\u2060\u200b\u00adb", "a b"},
		{"zero width non-joiner U+200C dropped (R32)", "a\u200cb", "ab"},
		{"zero width joiner U+200D dropped (R32)", "a\u200db", "ab"},
		{"word joiner U+2060 dropped (R32)", "a\u2060b", "ab"},
		{"leading zero width space is trimmed", "\u200ba", "a"},
		{"trailing zero width space is trimmed", "a\u200b", "a"},
		{"soft hyphen removed", "co\u00adoperate", "cooperate"},
		{"BOM removed", "\ufeffabc", "abc"},
		{"tag character removed", "a\U000e0001b", "ab"},
		{"only Cf gives nothing", "\u202e\ufeff\u200c\u200d\u2060", ""},
		{"only a zero width space is empty (trimmed)", "\u200b\u202e\ufeff", ""},

		// step 2: Cc, Zs, Zl, Zp become one space each, then runs collapse (R14)
		{"newline", "a\nb", "a b"},
		{"tab", "a\tb", "a b"},
		{"carriage return", "a\rb", "a b"},
		{"vertical tab and form feed", "a\vb\fc", "a b c"},
		{"C0 control", "a\x01b", "a b"},
		{"DEL", "a\x7fb", "a b"},
		{"C1 control", "a\u0085b", "a b"},
		{"no-break space", "a\u00a0b", "a b"},
		{"ideographic space", "a\u3000b", "a b"},
		{"en quad and thin space", "a\u2000b\u2009c", "a b c"},
		{"line separator U+2028", "a\u2028b", "a b"},
		{"paragraph separator U+2029", "a\u2029b", "a b"},
		{"CRLF is one space", "a\r\nb", "a b"},
		{"LFLF is one space", "a\n\nb", "a b"},
		{"two tabs are one space", "a\t\tb", "a b"},
		{"mixed whitespace run is one space", "a \t\r\n\u00a0\u2028 b", "a b"},
		{"run of ordinary spaces is one space", "a      b", "a b"},
		{"Cf between spaces joins the run", "a \u200b b", "a b"},
		{"leading run is trimmed", " \n\t a", "a"},
		{"trailing run is trimmed", "a \r\n", "a"},
		{"a query with a trailing newline equals the bare word", "foo\n", "foo"},
		{"a space-wrapped phrase is trimmed on both sides", "  foo bar \t", "foo bar"},
		{"only whitespace is empty (trimmed)", " \t\n ", ""},

		// steps 3-5: NFKC, case folding
		{"NFC e-acute", "caf\u00e9", "caf\u00e9"},
		{"NFD e-acute composes", "cafe\u0301", "caf\u00e9"},
		{"capital E-acute folds", "CAF\u00c9", "caf\u00e9"},
		{"NFD capital E-acute folds and composes", "CAFE\u0301", "caf\u00e9"},
		{"fullwidth letters and digits", "\uff21\uff22\uff23\uff11\uff12\uff13", "abc123"},
		{"fullwidth punctuation", "\uff21\uff01", "a!"},
		{"half-width katakana", "\uff76\uff80\uff76\uff85", "\u30ab\u30bf\u30ab\u30ca"},
		{"half-width katakana with voiced mark composes", "\uff76\uff9e", "\u30ac"},
		{"sharp s", "Stra\u00dfe", "strasse"},
		{"capital sharp s", "STRA\u1e9eE", "strasse"},
		{"fi ligature", "\ufb01sh", "fish"},
		{"ffl ligature", "o\ufb04ce", "offlce"},
		{"long s", "\u017fun", "sun"},
		{"circled digits", "\u2460\u2461", "12"},
		{"superscript two", "x\u00b2", "x2"},
		{"trade mark sign", "a\u2122", "atm"},
		{"vulgar fraction one half uses the fraction slash", "\u00bd", "1\u20442"},
		{"Kelvin sign", "\u212a", "k"},
		{"Angstrom sign", "\u212b", "\u00e5"},
		{"Ohm sign", "\u2126", "\u03c9"},
		{"titlecase dz digraph", "\u01c5", "d\u017e"},
		{"final sigma folds like sigma", "\u03a3\u0391\u03a3 \u03c3\u03b1\u03c2", "\u03c3\u03b1\u03c3 \u03c3\u03b1\u03c3"},
		{"Cyrillic folds", "\u041f\u0420\u0418\u0412\u0415\u0422", "\u043f\u0440\u0438\u0432\u0435\u0442"},
		{"Armenian folds", "\u0531", "\u0561"},
		{"Cherokee capital maps to the small letter", "\u13a0", "\uab70"},
		{"Cherokee small letter stays", "\uab70", "\uab70"},
		{"Cherokee capital last of the range", "\u13ef", "\uabbf"},
		{"Cherokee capital extension", "\u13f0", "\u13f8"},
		{"Cherokee small extension stays", "\u13f8", "\u13f8"},
		{"Hangul jamo compose (NFKC)", "\u1112\u1161\u11ab", "\ud55c"},
		{"compatibility jamo (NFKC)", "\u3131", "\u1100"},
		{"Arabic presentation form", "\ufe8d", "\u0627"},
		{"U+FDFA expands to 18 characters", "\ufdfa", "\u0635\u0644\u0649 \u0627\u0644\u0644\u0647 \u0639\u0644\u064a\u0647 \u0648\u0633\u0644\u0645"},

		// CJK and Hangul are unchanged (apart from NFKC)
		{"CJK unchanged", "\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8", "\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8"},
		{"Hangul syllables unchanged", "\ud55c\uad6d\uc5b4", "\ud55c\uad6d\uc5b4"},
		{"CJK compatibility ideograph maps to its unified form", "\uf900", "\u8c48"},
		{"ideographic space between CJK", "\u65e5\u3000\u672c", "\u65e5 \u672c"},

		// emoji keep their base; joiners are Cf and go
		{"thumbs up with skin tone", "\U0001f44d\U0001f3fd", "\U0001f44d\U0001f3fd"},
		{"ZWJ family loses the joiner, keeps every base", "\U0001f468\u200d\U0001f469\u200d\U0001f467", "\U0001f468\U0001f469\U0001f467"},
		{"flag", "\U0001f1fa\U0001f1f8", "\U0001f1fa\U0001f1f8"},
		{"keycap loses the variation selector", "1\ufe0f\u20e3", "1\u20e3"},
		{"emoji variation selector VS16 dropped", "\u2764\ufe0f", "\u2764"},
		{"text variation selector VS15 dropped", "\u2764\ufe0e", "\u2764"},
		{"VS1 U+FE00 dropped", "a\ufe00b", "ab"},
		{"a lone VS16 is empty", "\ufe0f", ""},
		{"ideographic variation selector U+E0100 dropped", "\u8fbb\U000e0100", "\u8fbb"},
		{"last ideographic variation selector U+E01EF dropped", "\u8fbb\U000e01ef", "\u8fbb"},
		{"VS between a base and a combining mark does not block composition", "e\ufe0f\u0301", "\u00e9"},

		// steps 6-7: the Turkish mappings
		{"dotted capital I", "\u0130stanbul", "istanbul"},
		{"plain capital", "ISTANBUL", "istanbul"},
		{"dotless i", "\u0131spirta", "ispirta"},
		{"dotless i capital partner", "I\u0307", "i"},
		{"i with combining dot", "i\u0307", "i"},
		{"dotless i followed by a combining dot", "\u0131\u0307", "i"},
		{"dotted capital I followed by a combining dot", "\u0130\u0307", "i"},
		{"several combining dots after i", "i\u0307\u0307\u0307", "i"},
		{"combining dot after another letter stays", "a\u0307", "\u0227"},
		{"combining dot after a space stays (the leading space is trimmed)", " \u0307", "\u0307"},
		{"combining dot after an inner space stays", "a \u0307", "a \u0307"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeText(c.in)
			if got != c.want {
				t.Errorf("NormalizeText(%q) = %q (% x), want %q (% x)", c.in, got, got, c.want, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("NormalizeText(%q) is not valid UTF-8", c.in)
			}
		})
	}
	if n := utf8.RuneCountInString(NormalizeText("\ufdfa")); n != 18 {
		t.Errorf("U+FDFA normalizes to %d characters, want 18", n)
	}
}

func TestNormalizeDeterministicAndConcurrent(t *testing.T) {
	inputs := []string{
		"Caf\u00e9 cafe\u0301 \uff21\uff22\uff23 Stra\u00dfe \u0130stanbul \u0131spirta \ufb01 \ufdfa",
		"\u202e\u200b a\r\n\tb \x00 \xff\xfe",
		"\U0001f468\u200d\U0001f469\u200d\U0001f467 \u65e5\u672c\u8a9e \ud55c\uad6d\uc5b4",
		strings.Repeat("\u0130\u0131i\u0307 \u00df\u1e9e ", 500),
	}
	want := make([]string, len(inputs))
	for i, in := range inputs {
		want[i] = NormalizeText(in)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := range 64 {
		wg.Go(func() {
			for round := range 20 {
				i := (g + round) % len(inputs)
				if got := NormalizeText(inputs[i]); got != want[i] {
					errs <- fmt.Sprintf("goroutine %d round %d input %d: output differs", g, round, i)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestNormalizeEquivalentSpellingsMeet: every spelling in a group must normalize to the same text.
func TestNormalizeEquivalentSpellingsMeet(t *testing.T) {
	groups := [][]string{
		{"\u00e9", "e\u0301", "\u00c9", "E\u0301"},
		{"\uff21\uff22\uff23", "abc", "ABC", "Abc"},
		{"\u00df", "ss", "SS", "\u1e9e"},
		{"\u0130stanbul", "ISTANBUL", "istanbul", "\u0131stanbul"},
		{"\ufb01", "fi", "FI"},
		{"\u212a", "K", "k", "\uff2b"},
		{"\u2460", "1", "\uff11"},
		{"a\u200cb", "ab", "a\u00adb", "a\u2060b", "a\u200db"},
		{"\u2764", "\u2764\ufe0f", "\u2764\ufe0e"},
		{"i \u2764\ufe0f you", "i \u2764 you", "I \u2764\ufe0e YOU"},
		{"foo", "foo\n", " foo ", "\tfoo\r\n", "\u200bfoo\u200b"},
		{"a\u200bb", "a b", "a\u200b\u200bb", "a \u200bb"},
		{"a\u00a0b", "a b", "a\tb", "a\nb", "a\r\nb", "a  b", "a\u2028b", "a\u3000b"},
		{"\u03c3", "\u03c2", "\u03a3"},
	}
	for _, g := range groups {
		want := NormalizeText(g[0])
		for _, s := range g[1:] {
			if got := NormalizeText(s); got != want {
				t.Errorf("NormalizeText(%q) = %q, but NormalizeText(%q) = %q: they must meet", s, got, g[0], want)
			}
		}
	}
	// The groups above are deliberate classes; two spellings that are not equivalent stay apart
	// (accents are the tokenizers' job: see TestNormalizeAndTokenizersAgree).
	for _, pair := range [][2]string{{"\u00e9", "e"}, {"ss", "s"}, {"a b", "ab"}, {"a\u200bb", "ab"}, {"\u0131", "\u00ef"}} {
		if NormalizeText(pair[0]) == NormalizeText(pair[1]) {
			t.Errorf("NormalizeText folds %q and %q together; accent folding must stay with the tokenizers", pair[0], pair[1])
		}
	}
}

// TestNormalizeIsIdempotentOnEveryRune: normalizing the normalized text of any single code point
// changes nothing, so a query typed from stored (already normalized) text normalizes to itself.
func TestNormalizeIsIdempotentOnEveryRune(t *testing.T) {
	bad := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		once := NormalizeText(string(r))
		twice := NormalizeText(once)
		if once != twice {
			bad++
			if bad <= 10 {
				t.Errorf("U+%04X: NormalizeText once = %q, twice = %q", r, once, twice)
			}
		}
		checkNormalizedShape(t, string(r), once)
	}
	if bad > 0 {
		t.Errorf("%d code points are not idempotent", bad)
	}
}

// TestNormalizeCaseOrbitsMeet: every code point and every other case variant Unicode lists for it
// (its simple-fold orbit: "k", "K", the Kelvin sign; "s", "S", the long s; the Cherokee capital and
// small letters, which the folding of x/text swaps) normalize to the same text.
func TestNormalizeCaseOrbitsMeet(t *testing.T) {
	bad := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		want := NormalizeText(string(r))
		for o := unicode.SimpleFold(r); o != r; o = unicode.SimpleFold(o) {
			if got := NormalizeText(string(o)); got != want {
				bad++
				if bad <= 10 {
					t.Errorf("U+%04X normalizes to %q but its case variant U+%04X to %q", r, want, o, got)
				}
				break
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d code points do not meet their case variants", bad)
	}
}

// checkNormalizedShape asserts the output contract: valid UTF-8, no NUL, no Cc/Cf rune, and no
// run of two spaces, no leading or trailing space and no variation selector.
func checkNormalizedShape(t testing.TB, in, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("NormalizeText(%q) = %q is not valid UTF-8", in, out)
	}
	if strings.Contains(out, "  ") {
		t.Fatalf("NormalizeText(%q) = %q holds a run of spaces", in, out)
	}
	if strings.HasPrefix(out, " ") || strings.HasSuffix(out, " ") {
		t.Fatalf("NormalizeText(%q) = %q has a leading or trailing space", in, out)
	}
	for _, r := range out {
		if (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef) {
			t.Fatalf("NormalizeText(%q) = %q holds the variation selector U+%04X", in, out, r)
		}
	}
	for _, r := range out {
		if r == 0 || unicode.In(r, unicode.Cc, unicode.Cf) {
			t.Fatalf("NormalizeText(%q) = %q holds the forbidden rune U+%04X", in, out, r)
		}
		if r != ' ' && unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp) {
			t.Fatalf("NormalizeText(%q) = %q holds the space-like rune U+%04X", in, out, r)
		}
	}
}

func TestNormalizeOutputShapeOnHostileStrings(t *testing.T) {
	for _, in := range []string{
		"",
		strings.Repeat("\u0301", 5000),
		"a" + strings.Repeat("\u0323\u0301\u0307", 2000),
		strings.Repeat("\u200b ", 1000),
		strings.Repeat("\xff", 4097),
		strings.Repeat("\x00 \r\n", 1000),
		strings.Repeat("\ufdfa", 1000),
		strings.Repeat("\u1112\u1161\u11ab\u1100", 500),
	} {
		checkNormalizedShape(t, fmt.Sprintf("%.20q...", in), NormalizeText(in))
	}
}

// TestNormalizeLongCombiningRunIsFast: a hostile run of combining marks of descending class (the
// worst case of canonical reordering) must not cost quadratic time.
func TestNormalizeLongCombiningRunIsFast(t *testing.T) {
	var b strings.Builder
	b.WriteString("a")
	for range 200000 {
		b.WriteString("\u0301\u0323") // class 230 then 220: each pair needs reordering
	}
	in := b.String()
	start := time.Now()
	_ = NormalizeText(in)
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("NormalizeText of %d combining marks took %v", 400000, d)
	}
}

func TestFTSNormVersionShape(t *testing.T) {
	v := FTSNormVersion()
	re := regexp.MustCompile(`^fts3/unicode-[0-9]+\.[0-9]+\.[0-9]+/gounicode-[0-9]+\.[0-9]+\.[0-9]+/xtext-v[0-9]+\.[0-9]+\.[0-9]+/sqlite-[0-9]+\.[0-9]+\.[0-9]+$`)
	if !re.MatchString(v) {
		t.Fatalf("FTSNormVersion() = %q does not match %s", v, re)
	}
	if !strings.Contains(v, "/unicode-"+norm.Version+"/") {
		t.Errorf("FTSNormVersion() = %q does not contain norm.Version %q", v, norm.Version)
	}
	if want := fmt.Sprintf("fts%d/", FTSPipelineVersion); !strings.HasPrefix(v, want) {
		t.Errorf("FTSNormVersion() = %q does not start with %q", v, want)
	}
	if !strings.Contains(v, "/xtext-"+xTextVersion+"/") {
		t.Errorf("FTSNormVersion() = %q does not contain the x/text version %q", v, xTextVersion)
	}
	if !strings.Contains(v, "/gounicode-"+unicode.Version+"/") {
		t.Errorf("FTSNormVersion() = %q does not contain the Go unicode version %q", v, unicode.Version)
	}
	if FTSPipelineVersion != 3 {
		t.Errorf("FTSPipelineVersion = %d, want 3 (bump it by hand only with a change of NormalizeText)", FTSPipelineVersion)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var sv string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sv); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(v, "/sqlite-"+sv) {
		t.Errorf("FTSNormVersion() = %q does not end with the SQLite version %q", v, sv)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if got := FTSNormVersion(); got != v {
				t.Errorf("FTSNormVersion() changed between calls: %q then %q", v, got)
			}
		})
	}
	wg.Wait()
}

// TestXTextVersionMatchesGoMod (R31): the index content depends on the x/text tables, so
// FTSNormVersion names the module version. The constant must equal the version go.mod requires,
// otherwise an x/text upgrade would change index content (case folding, NFKC tables) without the
// stored versions noticing.
func TestXTextVersionMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "require" {
			f = f[1:]
		}
		if len(f) >= 2 && f[0] == "golang.org/x/text" {
			found = append(found, f[1])
		}
	}
	if len(found) != 1 {
		t.Fatalf("go.mod requires golang.org/x/text %d times (%v), want exactly once", len(found), found)
	}
	if found[0] != xTextVersion {
		t.Errorf("go.mod requires golang.org/x/text %s but xTextVersion = %q: update xTextVersion, bump FTSPipelineVersion if the index content changes, and re-pin the tests", found[0], xTextVersion)
	}
}

// ---- agreement between NormalizeText and the SQLite tokenizers ----------------------------------
//
// Index text and query terms both go through NormalizeText and then the tokenizer, so any two
// spellings NormalizeText maps to the same text always meet. What can still surprise is the other
// layer: what the tokenizers do with the normalized text. The tests below pin those facts so a
// search that silently misses is a known, documented behaviour, never a surprise.

// tokenizerIndex indexes each doc's NormalizeText (rowid = position + 1) in the word and the
// trigram tables and returns, for a query, the matching positions: the query is normalized and
// quoted, exactly as the query compiler will emit it.
type tokenizerIndex struct {
	t  *testing.T
	db *sql.DB
}

func newTokenizerIndex(t *testing.T, docs ...string) *tokenizerIndex {
	t.Helper()
	db := probeDB(t, probeTables...)
	for i, d := range docs {
		n := NormalizeText(d)
		for _, tb := range probeTables {
			probeExec(t, db, `INSERT INTO `+tb.name+`(rowid, summary, body) VALUES (?, ?, '')`, i+1, n)
		}
	}
	return &tokenizerIndex{t: t, db: db}
}

func (x *tokenizerIndex) word(query string) []int64 {
	x.t.Helper()
	return probeMatch(x.t, x.db, "records_fts", probeQuote(NormalizeText(query)))
}

func (x *tokenizerIndex) sub(query string) []int64 {
	x.t.Helper()
	return probeMatch(x.t, x.db, "records_fts_sub", probeQuote(NormalizeText(query)))
}

func TestNormalizeAndTokenizersAgree(t *testing.T) {
	// One document per spelling of each class; every spelling as a query finds the whole class.
	classes := [][]string{
		{"caf\u00e9", "cafe\u0301", "CAF\u00c9", "cafe", "CAFE"}, // accents: Go keeps them, the tokenizers drop them
		{"Stra\u00dfe", "STRASSE", "strasse", "STRA\u1e9eE"},
		{"\u0130stanbul", "ISTANBUL", "\u0131stanbul", "istanbul", "i\u0307stanbul"},
		{"\uff21\uff22\uff23\uff11\uff12\uff13", "abc123", "ABC123"},
		{"o\ufb04ce", "offlce"},
		{"\u212aelvin", "kelvin", "KELVIN"},
		{"\u03a3\u0391\u03a3", "\u03c3\u03b1\u03c2", "\u03c3\u03b1\u03c3"},
		{"co\u00adoperate", "cooperate", "co\u2060operate"},
		{"co\u200boperate", "co operate"},
	}
	for _, class := range classes {
		x := newTokenizerIndex(t, class...)
		want := make([]int64, len(class))
		for i := range class {
			want[i] = int64(i + 1)
		}
		for _, q := range class {
			if got := x.word(q); !slices.Equal(got, want) {
				t.Errorf("word index: query %q over class %q = %v, want %v", q, class, got, want)
			}
		}
		for _, q := range class {
			if utf8.RuneCountInString(NormalizeText(q)) < 3 {
				continue
			}
			if got := x.sub(q); !slices.Equal(got, want) {
				t.Errorf("trigram index: query %q over class %q = %v, want %v", q, class, got, want)
			}
		}
	}

	// Accent folding is the tokenizers' backstop, not Go's: the Go text differs, the index meets.
	if NormalizeText("caf\u00e9") == NormalizeText("cafe") {
		t.Error("Go normalization folds accents; the design leaves that to the tokenizers")
	}
}

// TestNormalizeKnownSilentMisses pins what the word index (unicode61) and the trigram index do NOT
// find even though the text is there. Each is a documented limit of the tokenizers or of the
// Cf-removal step, not a bug: change them only together with FTSPipelineVersion and the docs.
func TestNormalizeKnownSilentMisses(t *testing.T) {
	x := newTokenizerIndex(t,
		"nice \U0001f600 smile",                            // 1: emoji
		"\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8", // 2: one CJK run
		"foo\u200bbar",                                     // 3: a zero width space between two words
		"\ud55c\uad6d\uc5b4\ub97c",                         // 4: a Hangul word with a particle
		"price: 5\u20ac!",                                  // 5: symbols
		"a.b",                                              // 6: punctuation inside a word
		"i \u2764\ufe0f you",                               // 7: an emoji with a variation selector
		"foo",                                              // 8: the word at the end of a field
		"foo, bar",                                         // 9: the word before punctuation
	)
	type probe struct {
		name  string
		got   []int64
		want  []int64
		why   string
		check string
	}
	probes := []probe{
		{"emoji by word", x.word("\U0001f600"), nil, "symbols (category So) are not token characters in unicode61", "word"},
		{"word beside an emoji", x.word("smile"), []int64{1}, "the words around an emoji are found", "word"},
		{"emoji inside a longer substring", x.sub("e \U0001f600 s"), []int64{1}, "the trigram index holds symbols", "sub"},
		{"CJK sub-run by word misses", x.word("\u65e5\u672c"), nil, "unicode61 makes one token of a CJK run", "word"},
		{"CJK whole run by word", x.word("\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8"), []int64{2}, "the whole run is the token", "word"},
		{"CJK sub-run by prefix", probeMatch(t, x.db, "records_fts", probeQuote(NormalizeText("\u65e5\u672c"))+" *"), []int64{2}, "a prefix finds the start of a run", "word"},
		{"CJK sub-run by substring", x.sub("\u672c\u8a9e\u306e"), []int64{2}, "the trigram index finds any 3-character piece", "sub"},
		{"CJK piece shorter than a trigram", x.sub("\u672c\u8a9e"), nil, "a 2-character substring is below the trigram minimum", "sub"},
		{"ZWSP separates words: the first word", x.word("foo"), []int64{3, 8, 9}, "U+200B maps to a space (R32), so the two words stay two tokens", "word"},
		{"ZWSP separates words: the second word", x.word("bar"), []int64{3, 9}, "likewise", "word"},
		{"ZWSP separates words: the phrase", x.word("foo bar"), []int64{3, 9}, "and the phrase with a space finds them", "word"},
		{"ZWSP separates words: the fused spelling misses", x.word("foobar"), nil, "the word 'foobar' is not in a text that separates them", "word"},
		{"ZWSP separates words: substring across it misses", x.sub("oob"), nil, "the normalized text holds a space between the words", "sub"},
		{"ZWSP separates words: substring with the space", x.sub("oo b"), []int64{3}, "the space is part of the trigram text", "sub"},
		{"emoji spelled without the variation selector finds the text with it", x.sub("i \u2764 you"), []int64{7}, "variation selectors are dropped on both sides (R32 review)", "sub"},
		{"emoji spelled with the variation selector finds the same", x.sub("i \u2764\ufe0f you"), []int64{7}, "likewise", "sub"},
		{"a trailing newline in a substring query does not narrow it", x.sub("foo\n"), []int64{3, 8, 9}, "the query is trimmed, so it finds the word at the end of a field and before punctuation", "sub"},
		{"a trailing space in a substring query does not narrow it", x.sub("foo "), []int64{3, 8, 9}, "likewise", "sub"},
		{"Hangul word with particle by word", x.word("\ud55c\uad6d\uc5b4"), nil, "the particle is part of the token", "word"},
		{"Hangul prefix", probeMatch(t, x.db, "records_fts", probeQuote(NormalizeText("\ud55c\uad6d\uc5b4"))+" *"), []int64{4}, "a prefix finds it", "word"},
		{"currency sign by word", x.word("\u20ac"), nil, "symbols are not tokens", "word"},
		{"digit beside a symbol", x.word("5"), []int64{5}, "the digit is a token", "word"},
		{"punctuation splits tokens: part", x.word("b"), []int64{6}, "a.b is the tokens a and b", "word"},
		{"punctuation splits tokens: phrase with a space", x.word("a b"), []int64{6}, "so the phrase 'a b' meets 'a.b'", "word"},
		{"punctuation splits tokens: phrase with a dot", x.word("a.b"), []int64{6}, "and 'a.b' meets itself", "word"},
	}
	for _, p := range probes {
		if !slices.Equal(p.got, p.want) {
			t.Errorf("%s (%s): got %v, want %v (%s)", p.name, p.check, p.got, p.want, p.why)
		}
	}
}

// TestNormalizeWordInContextMatchesBareWord: a query term (one word) normalizes to text that
// the tokenizer turns into the same tokens as the same word inside a document, for a set of words in
// many scripts: the document holds "xx <word> yy", the query is the bare word.
func TestNormalizeWordInContextMatchesBareWord(t *testing.T) {
	words := []string{
		"caf\u00e9", "Stra\u00dfe", "\u0130stanbul", "\u0131spirta", "\uff21\uff22\uff23", "\u00fcber", "na\u00efve",
		"\u041f\u0440\u0438\u0432\u0435\u0442", "\u03b1\u03bb\u03c6\u03b1", "\u05e9\u05dc\u05d5\u05dd", "\u0645\u0631\u062d\u0628\u0627",
		"\u0928\u092e\u0938\u094d\u0924\u0947", "\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35", "\u65e5\u672c\u8a9e", "\ud55c\uad6d\uc5b4",
		"\ufb01nance", "\u2460\u2461", "x\u00b2",
	}
	docs := make([]string, len(words))
	for i, w := range words {
		docs[i] = "xx " + w + " yy"
	}
	x := newTokenizerIndex(t, docs...)
	for i, w := range words {
		got := x.word(w)
		if !slices.Contains(got, int64(i+1)) {
			t.Errorf("the word %q (normalized %q) does not find its own document %d: got %v", w, NormalizeText(w), i+1, got)
		}
	}
}

// FuzzNormalizeText: never panics; the output is valid UTF-8 without NUL, Cc, Cf or a run of
// spaces; the same input gives the same output; and normalizing the output changes nothing.
func FuzzNormalizeText(f *testing.F) {
	for _, s := range []string{
		"", " ", "plain", "Caf" + string(rune(0xe9)), "cafe" + string(rune(0x301)), "Stra" + string(rune(0xdf)) + "e",
		string(rune(0x130)) + "stanbul", string(rune(0x131)) + string(rune(0x307)), "i" + string(rune(0x307)) + string(rune(0x307)),
		string(rune(0xfdfa)), string(rune(0x13a0)) + string(rune(0xab70)), string(rune(0x202e)) + "a" + string(rune(0x200b)) + "b",
		"a\r\n\t\tb", "a\x00b", "\xff\xfe\xc0\xaf", "\xed\xa0\x80", string(rune(0x1f468)) + string(rune(0x200d)) + string(rune(0x1f469)),
		string(rune(0x1112)) + string(rune(0x1161)) + string(rune(0x11ab)), strings.Repeat(string(rune(0x301)), 40),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := NormalizeText(s)
		checkNormalizedShape(t, s, out)
		if again := NormalizeText(s); again != out {
			t.Fatalf("NormalizeText(%q) is not deterministic: %q then %q", s, out, again)
		}
		if twice := NormalizeText(out); twice != out {
			t.Fatalf("NormalizeText(%q) = %q is not stable: normalizing it again gives %q", s, out, twice)
		}
	})
}
