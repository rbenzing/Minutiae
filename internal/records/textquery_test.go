package records_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

func TestCompileQueryTable(t *testing.T) {
	word := records.TextOptions{}
	sub := records.TextOptions{Substring: true}
	summary := records.TextOptions{Column: records.ColSummary}
	body := records.TextOptions{Column: records.ColBody}
	rank := records.TextOptions{Rank: true}
	tests := []struct {
		name  string
		input string
		opts  records.TextOptions
		table string
		want  string
	}{
		{"two words", `foo bar`, word, evidence.FTSWordTable, `("foo" AND "bar")`},
		{"explicit AND", `foo AND bar`, word, evidence.FTSWordTable, `("foo" AND "bar")`},
		{"lower-case and is a word", `foo and bar`, word, evidence.FTSWordTable, `("foo" AND "and" AND "bar")`},
		{"OR", `foo OR bar`, word, evidence.FTSWordTable, `("foo") OR ("bar")`},
		{"lower-case or is a word", `or`, word, evidence.FTSWordTable, `("or")`},
		{"negation", `foo -bar`, word, evidence.FTSWordTable, `("foo") NOT ("bar")`},
		{"phrase", `"foo bar"`, word, evidence.FTSWordTable, `("foo bar")`},
		{"negated phrase", `-"foo bar" baz`, word, evidence.FTSWordTable, `("baz") NOT ("foo bar")`},
		{"prefix", `foo*`, word, evidence.FTSWordTable, `("foo" *)`},
		{"negated prefix and word", `foo -bar* -baz`, word, evidence.FTSWordTable, `("foo") NOT ("bar" * OR "baz")`},
		{"groups and negation", `a b OR c -d`, word, evidence.FTSWordTable, `("a" AND "b") OR ("c") NOT ("d")`},
		{"hyphen inside a word", `foo-bar`, word, evidence.FTSWordTable, `("foo-bar")`},
		{"sharp s", "Stra\u00dfe", word, evidence.FTSWordTable, `("strasse")`},
		{"accent", "caf\u00e9", word, evidence.FTSWordTable, "(\"caf\u00e9\")"},
		{"decomposed accent meets the composed one", "cafe\u0301", word, evidence.FTSWordTable, "(\"caf\u00e9\")"},
		{"phone number", `+1 (555) 123`, word, evidence.FTSWordTable, `("+1" AND "(555)" AND "123")`},
		{"fullwidth", "\uff21\uff22\uff23123", word, evidence.FTSWordTable, `("abc123")`},
		{"tabs and newlines separate", "foo\tbar\nbaz\r\n", word, evidence.FTSWordTable, `("foo" AND "bar" AND "baz")`},
		{"summary column", `foo -bar OR baz`, summary, evidence.FTSWordTable, `summary : (("foo") NOT ("bar") OR ("baz"))`},
		{"body column", `foo`, body, evidence.FTSWordTable, `body : (("foo"))`},
		{"rank", `foo bar`, rank, evidence.FTSWordTable, `("foo" AND "bar")`},
		{"substring is the whole input", `foo bar`, sub, evidence.FTSSubTable, `"foo bar"`},
		{"substring keeps grammar as text", `foo OR -bar*`, sub, evidence.FTSSubTable, `"foo or -bar*"`},
		{"substring doubles a quote", `abc"d`, sub, evidence.FTSSubTable, `"abc""d"`},
		{"substring keeps literal quotes", `"foo"`, sub, evidence.FTSSubTable, `"""foo"""`},
		{"substring folds", "Stra\u00dfe", sub, evidence.FTSSubTable, `"strasse"`},
		{"substring trims", "  FOO\n", sub, evidence.FTSSubTable, `"foo"`},
		{"substring column", `foo`, records.TextOptions{Substring: true, Column: records.ColSummary}, evidence.FTSSubTable, `summary : ("foo")`},
		{"64 terms", strings.TrimSpace(strings.Repeat("a ", 64)), word, evidence.FTSWordTable, "(" + strings.TrimSuffix(strings.Repeat(`"a" AND `, 64), " AND ") + ")"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := records.CompileQuery(tc.input, tc.opts)
			if err != nil {
				t.Fatalf("CompileQuery(%q) = %v", tc.input, err)
			}
			if q.Match() != tc.want || q.Table() != tc.table || q.Rank() != tc.opts.Rank {
				t.Errorf("CompileQuery(%q) = %s on %s rank=%v, want %s on %s rank=%v", tc.input, q.Match(), q.Table(), q.Rank(), tc.want, tc.table, tc.opts.Rank)
			}
		})
	}
}

// TestCompileQueryNeutralizesOperators (ruling R16): for each input the exact outcome is pinned, for
// the closed grammar and for --substring. An accepted expression is executed on real FTS tables holding
// crafted documents (queryDocs) and must match exactly the documents that hold the LITERAL text, never
// the ones an operator reading would find (a prefix, a column filter, an anchor, NEAR, a boolean
// operator), and never fail.
func TestCompileQueryNeutralizesOperators(t *testing.T) {
	fx := newQueryFixture(t)
	type outcome struct {
		err   bool   // ErrInvalidQuery
		match string // the expression
		ids   []int64
	}
	bad := outcome{err: true}
	tests := []struct {
		input string
		word  outcome
		sub   outcome
	}{
		// NEAR( ) is text: the words "near(a" and "b)" (the phrase "near a", and "b"); a trigram needle is the whole text
		{"NEAR(a b)", outcome{match: `("near(a" AND "b)")`, ids: []int64{4, 13}}, outcome{match: `"near(a b)"`, ids: []int64{13}}},
		// "col:" is not a column filter: the phrase "col x"
		{"col:x", outcome{match: `("col:x")`, ids: []int64{5}}, outcome{match: `"col:x"`, ids: []int64{}}},
		{"summary:foo", outcome{match: `("summary:foo")`, ids: []int64{6}}, outcome{match: `"summary:foo"`, ids: []int64{}}},
		{"{summary}:", outcome{match: `("{summary}:")`, ids: []int64{6, 11}}, outcome{match: `"{summary}:"`, ids: []int64{11}}},
		// a star inside a word is punctuation, not a prefix: the phrase "a b"
		{"a*b", outcome{match: `("a*b")`, ids: []int64{3, 4, 9, 13}}, outcome{match: `"a*b"`, ids: []int64{13}}},
		// "^" does not anchor: "x" is found where it is not at the start of the column too
		{"^x", outcome{match: `("^x")`, ids: []int64{2, 3, 5, 10, 13}}, bad},
		{"a AND", bad, outcome{match: `"a and"`, ids: []int64{}}},
		{"NOT x", outcome{match: `("not" AND "x")`, ids: []int64{}}, outcome{match: `"not x"`, ids: []int64{}}},
		{"a NEAR b", outcome{match: `("a" AND "near" AND "b")`, ids: []int64{4, 13}}, outcome{match: `"a near b"`, ids: []int64{}}},
		{")", bad, bad},
		{"(", bad, bad},
		{`"`, bad, bad},
		{`x"y`, bad, outcome{match: `"x""y"`, ids: []int64{}}},
		{`""`, bad, bad},
		{"* ", bad, bad},
		{"*", bad, bad},
		{`\`, bad, bad},
		{"summary : x", bad, outcome{match: `"summary : x"`, ids: []int64{}}},
		{`"x" *`, bad, outcome{match: `"""x"" *"`, ids: []int64{}}},
		{`"x"*`, bad, outcome{match: `"""x""*"`, ids: []int64{}}},
		// unicode quotes are ordinary punctuation, not quotes
		{"\u201cx\u201d", outcome{match: "(\"\u201cx\u201d\")", ids: []int64{2, 3, 5, 10, 13}}, outcome{match: "\"\u201cx\u201d\"", ids: []int64{}}},
		{"\u201cx y\u201d", outcome{match: "(\"\u201cx\" AND \"y\u201d\")", ids: []int64{3, 10, 13}}, outcome{match: "\"\u201cx y\u201d\"", ids: []int64{}}},
		{"\u2018foo\u2019", outcome{match: "(\"\u2018foo\u2019\")", ids: []int64{6, 7, 16}}, outcome{match: "\"\u2018foo\u2019\"", ids: []int64{}}},
	}
	run := func(t *testing.T, input string, o records.TextOptions, want outcome) {
		t.Helper()
		q, err := records.CompileQuery(input, o)
		if want.err {
			if !errors.Is(err, records.ErrInvalidQuery) {
				t.Fatalf("CompileQuery(%q, %+v) = %v, want ErrInvalidQuery", input, o, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("CompileQuery(%q, %+v) = %v, want %s", input, o, err, want.match)
		}
		if q.Match() != want.match {
			t.Fatalf("CompileQuery(%q, %+v).Match() = %s, want %s", input, o, q.Match(), want.match)
		}
		got, err := fx.match(q.Table(), q.Match())
		if err != nil {
			t.Fatalf("MATCH %s on %s failed: %v", q.Match(), q.Table(), err)
		}
		if !slices.Equal(got, want.ids) {
			t.Errorf("MATCH %s on %s = %v, want %v", q.Match(), q.Table(), got, want.ids)
		}
	}
	for _, tc := range tests {
		t.Run("word/"+tc.input, func(t *testing.T) { run(t, tc.input, records.TextOptions{}, tc.word) })
		t.Run("substring/"+tc.input, func(t *testing.T) { run(t, tc.input, records.TextOptions{Substring: true}, tc.sub) })
	}
}

// TestCompileQuerySemantics executes compiled queries on the fixture: boolean precedence, negation,
// phrases, prefixes and the column filter mean what the grammar says.
func TestCompileQuerySemantics(t *testing.T) {
	fx := newQueryFixture(t)
	tests := []struct {
		name  string
		input string
		opts  records.TextOptions
		want  []int64
	}{
		{"AND", `foo bar`, records.TextOptions{}, []int64{7, 16}},
		{"negation", `foo -bar`, records.TextOptions{}, []int64{6}},
		{"OR", `foo OR apple`, records.TextOptions{}, []int64{1, 6, 7, 16}},
		{"negation binds inside its group, not across OR", `foo -bar OR apple`, records.TextOptions{}, []int64{1, 6}},
		{"phrase is adjacent in one column", `"foo bar"`, records.TextOptions{}, []int64{7}},
		{"prefix", `fo*`, records.TextOptions{}, []int64{6, 7, 16}},
		{"negated prefix", `foo -ba*`, records.TextOptions{}, []int64{6}},
		{"hyphenated word is a phrase", `back-slash`, records.TextOptions{}, []int64{12}},
		{"accent and case", `CAFE`, records.TextOptions{}, []int64{14}},
		{"sharp s", `strasse`, records.TextOptions{}, []int64{15}},
		{"column summary", `foo -bar`, records.TextOptions{Column: records.ColSummary}, []int64{6, 16}},
		{"column body", `foo`, records.TextOptions{Column: records.ColBody}, []int64{6}},
		{"column with OR group", `apple OR zzz`, records.TextOptions{Column: records.ColBody}, []int64{1}},
		{"substring", `oo ba`, records.TextOptions{Substring: true}, []int64{7}},
		{"substring in a column", `foo`, records.TextOptions{Substring: true, Column: records.ColBody}, []int64{6}},
		{"substring with the diacritic", "af\u00e9", records.TextOptions{Substring: true}, []int64{14}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := records.CompileQuery(tc.input, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fx.match(q.Table(), q.Match())
			if err != nil {
				t.Fatalf("MATCH %s: %v", q.Match(), err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s -> %s = %v, want %v", tc.input, q.Match(), got, tc.want)
			}
		})
	}
}

func TestCompileQueryErrors(t *testing.T) {
	word := records.TextOptions{}
	sub := records.TextOptions{Substring: true}
	tests := []struct {
		name  string
		input string
		opts  records.TextOptions
	}{
		{"unbalanced quote", `"foo bar`, word},
		{"unbalanced quote after a word", `foo "bar`, word},
		{"text after a closing quote", `"foo"bar`, word},
		{"star after a closing quote", `"foo"*`, word},
		{"a quote inside a word", `foo"bar baz"`, word},
		{"empty", ``, word},
		{"whitespace only", " \t\n ", word},
		{"empty phrase", `""`, word},
		{"phrase of blanks", `"   "`, word},
		{"only a negation", `-foo`, word},
		{"only negations", `-foo -bar`, word},
		{"an OR group with no positive", `a OR -b`, word},
		{"a leading negation group", `-a OR b`, word},
		{"a lone minus", `-`, word},
		{"a minus before a blank", `foo - bar`, word},
		{"leading OR", `OR a`, word},
		{"trailing OR", `a OR`, word},
		{"doubled OR", `a OR OR b`, word},
		{"leading AND", `AND a`, word},
		{"trailing AND", `a AND`, word},
		{"AND then OR", `a AND OR b`, word},
		{"OR then AND", `a OR AND b`, word},
		{"65 terms", strings.TrimSpace(strings.Repeat("a ", 65)), word},
		{"65 terms with negations", "a " + strings.TrimSpace(strings.Repeat("-a ", 64)), word},
		{"4097 bytes", strings.Repeat("a", 4097), word},
		{"4097 bytes in blanks and words", strings.Repeat("ab ", 1366), word},
		{"a 257-byte term", strings.Repeat("a", 257), word},
		{"a 257-byte phrase", `"` + strings.Repeat("a", 257) + `"`, word},
		{"a 257-byte substring", strings.Repeat("a", 257), sub},
		{"prefix under 2", `a*`, word},
		{"prefix of punctuation", `-*`, word},
		{"a lone star", `*`, word},
		{"a prefix whose letters are under 2", `a-*`, word},
		{"substring under 3", `ab`, sub},
		{"substring of two letters and blanks", ` ab `, sub},
		{"punctuation only", `!!!`, word},
		{"punctuation only substring", `...`, sub},
		{"punctuation only phrase", `"?!"`, word},
		{"a closing parenthesis", `)`, word},
		{"emoji only (not a word token)", "\u2764\ufe0f", word},
		{"zero-width characters only", "\u200b\u200d\u2060\ufeff", word},
		{"zero-width characters only in a phrase", "\"\u200d\u2060\"", word},
		{"zero-width characters only substring", "\u200b\u200d\u2060\ufeff\u200b", sub},
		{"NUL", "foo\x00bar", word},
		{"NUL substring", "foo\x00bar", sub},
		{"invalid UTF-8", "foo\xffbar", word},
		{"a control character in an error", "\x1b[31m\"unterminated", word},
		{"a bidi override in an error", "\u202e\"unterminated", word},
		{"a control character in a long term", "\x1b[31m" + strings.Repeat("a", 300), word},
		{"rank with substring", `foo`, records.TextOptions{Substring: true, Rank: true}},
		{"bad column", `foo`, records.TextOptions{Column: records.Column(9)}},
		{"negative column", `foo`, records.TextOptions{Column: records.Column(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := records.CompileQuery(tc.input, tc.opts)
			if !errors.Is(err, records.ErrInvalidQuery) || q != nil {
				t.Fatalf("CompileQuery = %v, %v; want nil and ErrInvalidQuery", q, err)
			}
			if bad := rawControl(err.Error()); bad != 0 {
				t.Errorf("the error echoes the raw character %U: %q", bad, err.Error())
			}
			if len(err.Error()) > 300 {
				t.Errorf("the error is %d bytes long; it must stay short", len(err.Error()))
			}
		})
	}
}

// rawControl returns the first control, format, separator or invalid character of s (0 if none).
func rawControl(s string) rune {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar || (unicode.IsSpace(r) && r != ' ') {
			return r
		}
	}
	return 0
}

// TestCompileQueryLimitsBoundary: the caps are inclusive.
func TestCompileQueryLimitsBoundary(t *testing.T) {
	for name, input := range map[string]string{
		"64 terms":                 strings.TrimSpace(strings.Repeat("a ", 64)),
		"4096 bytes":               strings.TrimSpace(strings.Repeat(strings.Repeat("a", 255)+" ", 16)),
		"a 256-byte term":          strings.Repeat("a", 256),
		"a 256-byte quoted phrase": `"` + strings.Repeat("a", 256) + `"`,
		"prefix of 2":              `ab*`,
	} {
		if len(input) > records.MaxQueryBytes {
			t.Fatalf("%s: the test input is %d bytes", name, len(input))
		}
		if _, err := records.CompileQuery(input, records.TextOptions{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := records.CompileQuery("abc", records.TextOptions{Substring: true}); err != nil {
		t.Errorf("a substring of 3: %v", err)
	}
	if _, err := records.CompileQuery(strings.Repeat("a", 256), records.TextOptions{Substring: true}); err != nil {
		t.Errorf("a substring of 256 bytes: %v", err)
	}
	if records.MaxQueryBytes != 4096 || records.MaxQueryTerms != 64 || records.MaxTermBytes != 256 || records.MinPrefixChars != 2 || records.MinSubstringChars != 3 || records.MaxTerms != 1000 {
		t.Error("a query limit constant changed")
	}
}

// TestCompileQueryFoldsLikeTheIndex: every term the compiler emits is evidence.NormalizeText of the
// input word, the one function the index text goes through.
func TestCompileQueryFoldsLikeTheIndex(t *testing.T) {
	words := []string{
		"Stra\u00dfe", "caf\u00e9", "cafe\u0301", "\uff21\uff22\uff23123", "\u0130stanbul", "\u0131sp\u0131rta", "pass\u202eword",
		"pass\u200bword", "pass\u200dword", "NEAR(a", "B)", "+1", "(555)", "123-4567", "\u1e9e", "\ufb01nd",
		"\u4f60\u597d", "\u13a0\u13a1",
	}
	for _, w := range words {
		want := evidence.NormalizeText(w)
		q, err := records.CompileQuery(w, records.TextOptions{})
		if err != nil {
			t.Errorf("CompileQuery(%+q): %v", w, err)
			continue
		}
		if wantExpr := `("` + want + `")`; q.Match() != wantExpr {
			t.Errorf("CompileQuery(%+q).Match() = %s, want %s (NormalizeText of the word)", w, q.Match(), wantExpr)
		}
		if n := q.Needles(); len(n) != 1 || n[0].Text != want || n[0].Kind != records.NeedleWord {
			t.Errorf("CompileQuery(%+q).Needles() = %+v, want one word %q", w, n, want)
		}
		if s, err := records.CompileQuery(w, records.TextOptions{Substring: true}); err == nil {
			if s.Match() != `"`+strings.ReplaceAll(want, `"`, `""`)+`"` {
				t.Errorf("substring CompileQuery(%+q).Match() = %s", w, s.Match())
			}
		}
	}
	// a phrase and a prefix go through the same function
	q, err := records.CompileQuery("\"Stra\u00dfe  \u0130STANBUL\" caf\u00e9* -\u4f60\u597d", records.TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `("strasse istanbul" AND "` + evidence.NormalizeText("caf\u00e9") + `" *) NOT ("` + evidence.NormalizeText("\u4f60\u597d") + `")`; q.Match() != want {
		t.Errorf("Match() = %s, want %s", q.Match(), want)
	}
}

func TestCompileQueryNeedles(t *testing.T) {
	q, err := records.CompileQuery(`foo "Bar Baz" qu* -nope OR other`, records.TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []records.Needle{
		{Kind: records.NeedleWord, Text: "foo"},
		{Kind: records.NeedlePhrase, Text: "bar baz"},
		{Kind: records.NeedlePrefix, Text: "qu"},
		{Kind: records.NeedleWord, Text: "other"},
	}
	if got := q.Needles(); !slices.Equal(got, want) {
		t.Errorf("Needles() = %+v, want the positive ones %+v", got, want)
	}
	got := q.Needles()
	got[0].Text = "mutated"
	if q.Needles()[0].Text != "foo" {
		t.Error("Needles() returns the query's own slice")
	}
	s, err := records.CompileQuery(`Foo Bar`, records.TextOptions{Substring: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Needles(); !slices.Equal(got, []records.Needle{{Kind: records.NeedleSubstring, Text: "foo bar"}}) {
		t.Errorf("substring Needles() = %+v", got)
	}
}

func TestCompileQueryDeterministicAndCanonicalDiffers(t *testing.T) {
	base, err := records.CompileQuery(`foo -bar`, records.TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := records.CompileQuery(`foo -bar`, records.TextOptions{})
	if base.Match() != again.Match() || base.Table() != again.Table() || base.Canonical() != again.Canonical() {
		t.Error("the same input compiled differently")
	}
	seen := map[string]string{base.Canonical(): "base"}
	for name, c := range map[string]struct {
		in string
		o  records.TextOptions
	}{
		"summary":   {`foo -bar`, records.TextOptions{Column: records.ColSummary}},
		"body":      {`foo -bar`, records.TextOptions{Column: records.ColBody}},
		"rank":      {`foo -bar`, records.TextOptions{Rank: true}},
		"substring": {`foo -bar`, records.TextOptions{Substring: true}},
		"other":     {`foo -baz`, records.TextOptions{}},
		"sub+col":   {`foo -bar`, records.TextOptions{Substring: true, Column: records.ColSummary}},
	} {
		q, err := records.CompileQuery(c.in, c.o)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[q.Canonical()]; dup {
			t.Errorf("%s has the same canonical form as %s: %q", name, prev, q.Canonical())
		}
		seen[q.Canonical()] = name
	}
	// the rank flag is in the form even where the expression is the same
	a, _ := records.CompileQuery(`foo`, records.TextOptions{})
	b, _ := records.CompileQuery(`foo`, records.TextOptions{Rank: true})
	if a.Match() != b.Match() || a.Canonical() == b.Canonical() {
		t.Errorf("rank must change the canonical form only: %q vs %q", a.Canonical(), b.Canonical())
	}
}

func TestCompileTermsLiteral(t *testing.T) {
	qs, err := records.CompileTerms([]records.Term{
		{Text: `a OR -b "c"`},
		{Text: `foo*`},
		{Text: "Stra\u00dfe"},
		{Text: `x"y`, Substring: true},
		{Text: `NEAR(a b)`, Substring: true},
		{Text: `pass-word`},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ table, match string }{
		{evidence.FTSWordTable, `"a or -b ""c"""`},
		{evidence.FTSWordTable, `"foo*"`},
		{evidence.FTSWordTable, `"strasse"`},
		{evidence.FTSSubTable, `"x""y"`},
		{evidence.FTSSubTable, `"near(a b)"`},
		{evidence.FTSWordTable, `"pass-word"`},
	}
	if len(qs) != len(want) {
		t.Fatalf("%d queries for %d terms", len(qs), len(want))
	}
	fx := newQueryFixture(t)
	for i, w := range want {
		if qs[i].Table() != w.table || qs[i].Match() != w.match || qs[i].Rank() {
			t.Errorf("term %d: %s on %s rank=%v, want %s on %s", i, qs[i].Match(), qs[i].Table(), qs[i].Rank(), w.match, w.table)
		}
		if _, err := fx.match(qs[i].Table(), qs[i].Match()); err != nil {
			t.Errorf("term %d: MATCH %s failed: %v", i, qs[i].Match(), err)
		}
	}
	// a term is one phrase, not grammar: OR is not an operator in it
	or, err := records.CompileTerms([]records.Term{{Text: "foo OR apple"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fx.match(or[0].Table(), or[0].Match()); err != nil || len(got) != 0 {
		t.Errorf("the term `foo OR apple` matched %v, %v; as a phrase it matches nothing", got, err)
	}
	if n := qs[0].Needles(); len(n) != 1 || n[0].Kind != records.NeedlePhrase || n[0].Text != `a or -b "c"` {
		t.Errorf("Needles() = %+v, want the one phrase", n)
	}
	if n := qs[3].Needles(); len(n) != 1 || n[0].Kind != records.NeedleSubstring {
		t.Errorf("a substring term's Needles() = %+v", n)
	}
}

func TestCompileTermsRefusals(t *testing.T) {
	many := func(n int) []records.Term {
		ts := make([]records.Term, n)
		for i := range ts {
			ts[i] = records.Term{Text: "word"}
		}
		return ts
	}
	if qs, err := records.CompileTerms(many(1000)); err != nil || len(qs) != 1000 {
		t.Errorf("1000 terms = %d queries, %v; want them all", len(qs), err)
	}
	for name, terms := range map[string][]records.Term{
		"1001 terms":             many(1001),
		"no terms":               nil,
		"a term of nothing":      {{Text: "ok"}, {Text: "\u200d\u2060"}},
		"an empty term":          {{Text: ""}},
		"blank term":             {{Text: " \t"}},
		"punctuation term":       {{Text: "?!"}},
		"short substring":        {{Text: "ab", Substring: true}},
		"long term":              {{Text: strings.Repeat("a", 257)}},
		"NUL term":               {{Text: "a\x00b"}},
		"invalid UTF-8 term":     {{Text: "a\xffb"}},
		"one bad among good":     {{Text: "good"}, {Text: "also good"}, {Text: "!"}},
		"short substring second": {{Text: "good"}, {Text: "no", Substring: true}},
		"control char in a long": {{Text: "\x1b[31m" + strings.Repeat("a", 300)}},
	} {
		qs, err := records.CompileTerms(terms)
		if !errors.Is(err, records.ErrInvalidQuery) || qs != nil {
			t.Errorf("%s: CompileTerms = %v, %v; want nil and ErrInvalidQuery", name, qs, err)
		} else if bad := rawControl(err.Error()); bad != 0 {
			t.Errorf("%s: the error echoes the raw character %U: %q", name, bad, err.Error())
		}
	}
}

// TestCompileQueryFloorsCountWhatTheTokenizerIndexes (R39): the substring floor counts the
// characters the trigram tokenizer keeps (combining marks it strips do not count) and the prefix
// floor counts the token the star attaches to, not the whole word.
func TestCompileQueryFloorsCountWhatTheTokenizerIndexes(t *testing.T) {
	sub := records.TextOptions{Substring: true}
	word := records.TextOptions{}
	for _, tc := range []struct {
		name, input string
		opts        records.TextOptions
	}{
		{"substring ab + combining acute", "ab́", sub},
		{"substring ab + two combining marks", "ab́̂", sub},
		{"substring a + b + grave in blanks", " ab̀ ", sub},
		{"prefix ab-c*", "ab-c*", word},
		{"prefix with a combining mark only", "á*", word},
		{"prefix of a short last token", "abc.d*", word},
		{"phrase-less prefix a-*", "a-*", word},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := records.CompileQuery(tc.input, tc.opts)
			if !errors.Is(err, records.ErrInvalidQuery) || q != nil {
				t.Fatalf("CompileQuery(%q) = %v, %v; want ErrInvalidQuery", tc.input, q, err)
			}
		})
	}
	for name, input := range map[string]string{
		"prefix a-bc* (last token has 2)": "a-bc*",
		"prefix ab-*":                     "ab-*",
	} {
		if _, err := records.CompileQuery(input, word); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := records.CompileQuery("ab́c", sub); err != nil {
		t.Errorf("a substring of 3 kept characters: %v", err)
	}
}

// TestCompileSizeCapComesBeforeTheTextScan (R40): a substring needle or a term over 256 bytes is
// refused for its size before the text is scanned for UTF-8 or NUL, and an echo stays short.
func TestCompileSizeCapComesBeforeTheTextScan(t *testing.T) {
	long := strings.Repeat("a", 300) + "\x00\xff"
	_, err := records.CompileQuery(long, records.TextOptions{Substring: true})
	if !errors.Is(err, records.ErrInvalidQuery) || !strings.Contains(err.Error(), "256") {
		t.Errorf("substring: %v; want the size refusal", err)
	}
	_, err = records.CompileTerms([]records.Term{{Text: long}})
	if !errors.Is(err, records.ErrInvalidQuery) || !strings.Contains(err.Error(), "256") {
		t.Errorf("terms: %v; want the size refusal", err)
	}
	// 64 emoji: 256 bytes, no letter or digit; the error that echoes it stays under 300 bytes
	emoji := strings.Repeat("\U0001F600", 64)
	for _, call := range []func() error{
		func() error { _, err := records.CompileQuery(emoji, records.TextOptions{}); return err },
		func() error { _, err := records.CompileQuery(emoji, records.TextOptions{Substring: true}); return err },
		func() error { _, err := records.CompileTerms([]records.Term{{Text: emoji}}); return err },
	} {
		err := call()
		if !errors.Is(err, records.ErrInvalidQuery) {
			t.Fatalf("64 emoji: %v; want ErrInvalidQuery", err)
		}
		if len(err.Error()) > 300 || rawControl(err.Error()) != 0 {
			t.Errorf("the echo is %d bytes (%q); want at most 300 and escaped", len(err.Error()), err.Error())
		}
	}
}
