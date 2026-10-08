package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// FuzzCompileQuery: CompileQuery never panics; every refusal is ErrInvalidQuery with an error that echoes
// no raw control character; every accepted expression has the closed shape (quoted strings with doubled
// quotes, parentheses, AND/OR/NOT, one blank and a star after a quoted string, and the one column filter
// that was asked for), uses a star only where the input had one, and EXECUTES on a real FTS table of its
// kind without any error. A substring query is exactly the one quoted normalized input.
func FuzzCompileQuery(f *testing.F) {
	for _, s := range []string{
		"foo bar", "foo OR bar", "foo -bar", `"foo bar"`, "foo*", "a b OR c -d", "Stra\u00dfe", "caf\u00e9", "+1 (555) 123", "or",
		"NEAR(a b)", "col:x", "summary:foo", "a*b", "^x", "a AND", ")", "(", `"`, `x"y`, `""`, "{summary}:", "* ", `\`,
		"\u201cx\u201d", "\u2018foo\u2019", "NEAR(a b", `"x" *`, `"x"*`, "-x", "a OR -b", "x" + strings.Repeat(" y", 70), "\x00", "\u200b\u200d",
		"foo\x1b[31m", "\u202eabc", "a AND OR b", "-*", "ab*", "NOT x", "x NOT y", "AND", "OR", "--foo", "-\"foo bar\"", "foo\n-bar",
	} {
		f.Add(s, false, 0, false)
		f.Add(s, true, 0, false)
		f.Add(s, false, 1, true)
		f.Add(s, true, 2, false)
	}
	fx := fuzzFixture(f)
	f.Fuzz(func(t *testing.T, input string, substring bool, column int, rank bool) {
		o := records.TextOptions{Substring: substring, Column: records.Column((column%3 + 3) % 3), Rank: rank}
		q, err := records.CompileQuery(input, o)
		if err != nil {
			if !errors.Is(err, records.ErrInvalidQuery) || q != nil {
				t.Fatalf("CompileQuery(%q, %+v) = %v, %v; want nil and ErrInvalidQuery", input, o, q, err)
			}
			if bad := rawControl(err.Error()); bad != 0 {
				t.Fatalf("CompileQuery(%q): the error echoes the raw character %U: %q", input, bad, err.Error())
			}
			return
		}
		if substring && rank {
			t.Fatalf("--rank with --substring was accepted: %q", input)
		}
		want := evidence.FTSWordTable
		if substring {
			want = evidence.FTSSubTable
		}
		if q.Table() != want || q.Rank() != rank {
			t.Fatalf("CompileQuery(%q, %+v): table %s rank %v", input, o, q.Table(), q.Rank())
		}
		col := ""
		switch o.Column {
		case records.ColSummary:
			col = "summary"
		case records.ColBody:
			col = "body"
		}
		if err := exprShapeError(q.Match(), col); err != nil {
			t.Fatalf("CompileQuery(%q, %+v) = %s: %v", input, o, q.Match(), err)
		}
		if stars := strings.Count(q.Match(), " *"); !substring && stars > strings.Count(input, "*") {
			t.Fatalf("CompileQuery(%q) = %s emits %d prefix stars from %d input stars", input, q.Match(), stars, strings.Count(input, "*"))
		}
		if substring {
			quoted := `"` + strings.ReplaceAll(evidence.NormalizeText(input), `"`, `""`) + `"`
			if q.Match() != quoted && q.Match() != col+" : ("+quoted+")" {
				t.Fatalf("substring CompileQuery(%q) = %s, want the one quoted normalized input %s", input, q.Match(), quoted)
			}
		}
		if len(q.Needles()) == 0 {
			t.Fatalf("CompileQuery(%q) = %s has no positive needle", input, q.Match())
		}
		for _, n := range q.Needles() {
			if n.Text == "" || evidence.NormalizeText(n.Text) != n.Text {
				t.Fatalf("CompileQuery(%q): needle %+v is not normalized text", input, n)
			}
		}
		if q.Canonical() == "" {
			t.Fatal("empty canonical form")
		}
		if _, err := fx.match(q.Table(), q.Match()); err != nil {
			t.Fatalf("CompileQuery(%q, %+v) = %s fails on %s: %v", input, o, q.Match(), q.Table(), err)
		}
	})
}
