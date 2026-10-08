package records_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// cp builds a string from code points: the tests spell every non-ASCII character this way so the
// source stays ASCII and nothing is lost to an editor or a tool that rewrites escapes.
func cp(rs ...rune) string { return string(rs) }

func mustCompile(tb testing.TB, in string, o records.TextOptions) *records.TextQuery {
	tb.Helper()
	q, err := records.CompileQuery(in, o)
	if err != nil {
		tb.Fatalf("CompileQuery(%q, %+v): %v", in, o, err)
	}
	return q
}

// textFix is a case holding the recordstest.TextRecords corpus, indexed by the writer.
type textFix struct {
	c    *evidence.Case
	r    *records.Reader
	id   map[string]int64
	name map[int64]string
}

func newTextFix(t *testing.T) textFix {
	t.Helper()
	c, art := setup(t)
	recs, byName := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	fx := textFix{c: c, r: newReader(t, c), id: map[string]int64{}, name: map[int64]string{}}
	for n, i := range byName {
		fx.id[n], fx.name[int64(i+1)] = int64(i+1), n
	}
	return fx
}

func (fx textFix) names(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fx.name[id])
	}
	slices.Sort(out)
	return out
}

// search returns the sorted record names Search finds for q (everything on one page).
func (fx textFix) search(t *testing.T, q *records.TextQuery) []string {
	t.Helper()
	res, err := fx.r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
	if err != nil {
		t.Fatalf("Search(%s): %v", q.Match(), err)
	}
	var ids []int64
	for _, h := range res.Hits {
		ids = append(ids, h.Row.ID)
	}
	return fx.names(ids)
}

func sorted(ss ...string) []string {
	out := append([]string{}, ss...)
	slices.Sort(out)
	return out
}

func hitIDs(hits []records.Hit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.Row.ID
	}
	return out
}

// TestSearchFindsFoldedAndDiacriticForms: every spelling of a word that normalizes to the indexed
// form finds the record, in the word index and in the substring index, in the summary and the body.
func TestSearchFindsFoldedAndDiacriticForms(t *testing.T) {
	fx := newTextFix(t)
	word, sub := records.TextOptions{}, records.TextOptions{Substring: true}
	inSummary, inBody := records.TextOptions{Column: records.ColSummary}, records.TextOptions{Column: records.ColBody}
	subSummary, subBody := records.TextOptions{Substring: true, Column: records.ColSummary}, records.TextOptions{Substring: true, Column: records.ColBody}
	cafe := sorted("cafe_nfc", "cafe_nfd")
	cjkHello := cp(0x4f60, 0x597d, 0x4e16, 0x754c)
	tokyo := cp(0x6771, 0x4eac, 0x30bf, 0x30ef, 0x30fc)
	for _, tc := range []struct {
		name  string
		query string
		opt   records.TextOptions
		want  []string
	}{
		{"cafe", "cafe", word, cafe},
		{"CAFE with acute capital", "CAF" + cp(0xc9), word, cafe},
		{"cafe with precomposed acute", "caf" + cp(0xe9), word, cafe},
		{"cafe with combining acute (NFD)", "cafe" + cp(0x301), word, cafe},
		{"strasse", "strasse", word, sorted("strasse")},
		{"STRASSE", "STRASSE", word, sorted("strasse")},
		{"sharp s", "Stra" + cp(0xdf) + "e", word, sorted("strasse")},
		{"fullwidth query finds ascii form", cp(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13), word, sorted("fullwidth")},
		{"abc123 finds the fullwidth text", "abc123", word, sorted("fullwidth")},
		{"istanbul", "istanbul", word, sorted("turkish")},
		{"dotted capital I", cp(0x130) + "stanbul", word, sorted("turkish")},
		{"ispirta", "ispirta", word, sorted("turkish")},
		{"dotless i spelling", cp(0x131) + "sp" + cp(0x131) + "rta", word, sorted("turkish")},
		{"password finds the bidi record", "password", word, sorted("bidi")},
		{"password typed with a bidi override", "pass" + cp(0x202e) + "word", word, sorted("bidi")},
		// U+200B is a word separator (Thai and Khmer), so the zero-width record holds two words.
		{"zero width space is a separator", "pass" + cp(0x200b) + "word", word, sorted("zerowidth")},
		{"two words pass and word", "pass word", word, sorted("zerowidth")},
		{"chinese run is one token", cjkHello, word, sorted("cjk")},
		{"japanese run is one token", tokyo, word, sorted("cjk")},
		{"a part of a CJK run is a known silent miss", cp(0x4f60, 0x597d), word, nil},
		{"both columns", "only", word, sorted("bodyonly", "summaryonly")},
		{"summary column finds the summary", "summaryneedle", inSummary, sorted("summaryonly")},
		{"summary column does not find the body", "bodyneedle", inSummary, nil},
		{"body column finds the body", "bodyneedle", inBody, sorted("bodyonly")},
		{"body column does not find the summary", "summaryneedle", inBody, nil},
		{"the very end of a 1 MiB body", recordstest.LongNeedle, word, sorted("long")},
		{"the end of the body is not in the summary", recordstest.LongNeedle, inSummary, nil},

		{"substring cafe", "cafe", sub, cafe},
		{"substring CAFE accented", "CAF" + cp(0xc9), sub, cafe},
		{"substring strasse", "strasse", sub, sorted("strasse")},
		{"substring STRASSE", "STRASSE", sub, sorted("strasse")},
		{"substring sharp s", "Stra" + cp(0xdf) + "e", sub, sorted("strasse")},
		{"substring abc", "abc", sub, sorted("fullwidth")},
		{"substring fullwidth", cp(0xff21, 0xff22, 0xff23, 0xff11), sub, sorted("fullwidth")},
		{"substring istanbul", "istanbul", sub, sorted("turkish")},
		{"substring dotted capital I", cp(0x130) + "stanbul", sub, sorted("turkish")},
		{"substring ispirta", "ispirta", sub, sorted("turkish")},
		{"substring password", "password", sub, sorted("bidi")},
		{"substring pass word", "pass word", sub, sorted("zerowidth")},
		{"substring inside a CJK run", cp(0x597d, 0x4e16, 0x754c), sub, sorted("cjk")},
		{"substring across the japanese run", cp(0x4eac, 0x30bf, 0x30ef), sub, sorted("cjk")},
		{"substring in the summary", "summaryneedle", subSummary, sorted("summaryonly")},
		{"substring body needle in the summary column", "bodyneedle", subSummary, nil},
		{"substring in the body", "bodyneedle", subBody, sorted("bodyonly")},
		{"substring summary needle in the body column", "summaryneedle", subBody, nil},
		{"substring in a 1 MiB body", recordstest.LongNeedle, subBody, sorted("long")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := mustCompile(t, tc.query, tc.opt)
			got := fx.search(t, q)
			if !sameSet(got, tc.want) {
				t.Fatalf("Search(%q, %+v) = %v, want %v", tc.query, tc.opt, got, tc.want)
			}
			if n, capped, err := fx.r.Count(ctx, records.Filter{Text: q}, 0); err != nil || capped || int(n) != len(tc.want) {
				t.Fatalf("Count = %d, %v, %v, want %d", n, capped, err, len(tc.want))
			}
		})
	}
}

// summaryFix is a case whose records hold the given summaries (no body), in id order.
func summaryFix(t *testing.T, summaries []string) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	recs := make([]records.Record, len(summaries))
	for i, s := range summaries {
		recs[i] = records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Payload: map[string]any{"i": i}}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, newReader(t, c)
}

// TestSearchGrammarSemantics: AND, OR, negation, phrase adjacency and order, prefix, "or" as a word,
// and text that looks like search syntax is found only by its literal text.
func TestSearchGrammarSemantics(t *testing.T) {
	_, r := summaryFix(t, []string{
		"red apple pie",    // 1
		"green apple tart", // 2
		"red pear",         // 3
		"apple red",        // 4
		"this or that",     // 5
		"cafe au lait",     // 6
		"caffeine",         // 7
		"a-b c",            // 8
		"and",              // 9
		"NEAR(a b) OR x",   // 10
	})
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"red apple", []int64{1, 4}},
		{"red AND apple", []int64{1, 4}},
		{"pie OR tart", []int64{1, 2}},
		{"red OR green", []int64{1, 2, 3, 4}},
		{"apple -red", []int64{2}},
		{"-red apple", []int64{2}},
		{"apple -pie -tart", []int64{4}},
		{"apple pie OR red pear", []int64{1, 3}},
		{`"red apple"`, []int64{1}},
		{`"apple red"`, []int64{4}},
		{`"red pie"`, nil},
		{"app*", []int64{1, 2, 4}},
		{"caf*", []int64{6, 7}},
		{"cafe", []int64{6}},
		{"or", []int64{5, 10}},
		{"and", []int64{9}},
		{"a-b", []int64{8, 10}},
		{"a*b", []int64{8, 10}},
		// syntax typed as text is found only by its literal text
		{"NEAR(a b)", []int64{10}},
		{`"x"`, []int64{10}},
		{"summary:apple", nil},
		{"col:y", nil},
	} {
		q, err := records.CompileQuery(tc.query, records.TextOptions{})
		if err != nil {
			t.Errorf("%q: %v", tc.query, err)
			continue
		}
		res, err := r.Search(ctx, records.Filter{Text: q}, records.Page{}, records.SearchOptions{})
		if err != nil {
			t.Errorf("%q: %v", tc.query, err)
			continue
		}
		got := hitIDs(res.Hits)
		slices.Sort(got)
		if !sameIDs(got, tc.want) {
			t.Errorf("Search(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func sameIDs(got, want []int64) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return slices.Equal(got, want)
}

// TestSearchGrammarOperatorsInTheCorpus: the operators record of the corpus is found by its literal
// text and by nothing an operator reading of the query would hit.
func TestSearchGrammarOperatorsInTheCorpus(t *testing.T) {
	fx := newTextFix(t)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"NEAR(a b)", sorted("operators")},
		{"col:y", sorted("operators")},
		{`"x" col:y`, sorted("operators")},
		{"a*b", sorted("operators")},
		{"-z a", nil},                           // a negation of z: the record holds z
		{"summary:foo", nil},                    // not a column filter: the phrase "summary foo"
		{"x OR y", sorted("operators", "url")},  // OR as an operator
		{"x AND y", sorted("operators", "url")}, // AND as an operator
		{"x NOT y", nil},                        // NOT is a word here: needs the three words
		{"near", sorted("operators")},           // a word of the record
		{"nea*", sorted("operators")},           // a prefix
		{"cafe -au", nil},                       // negation excludes both cafe records
		{"cafe -nothere", cafeNames()},          // negation of an absent word
		{`"au lait"`, cafeNames()},              // phrase
		{`"lait au"`, nil},                      // order matters
		{"or", sorted("operators")},             // or as a word: only the operators record holds it
		{"OR or", nil},                          // an operator with nothing before it is refused below
		{"lait OR zzzz", cafeNames()},           // OR with an absent word
		{"zzzz OR zzzzz", nil},                  // nothing
		{"au lait cafe", cafeNames()},           // implicit AND, any order
		{"au AND cafe AND lait", cafeNames()},
	} {
		q, err := records.CompileQuery(tc.query, records.TextOptions{})
		if tc.query == "OR or" {
			if !errors.Is(err, records.ErrInvalidQuery) {
				t.Errorf("%q: %v, want ErrInvalidQuery", tc.query, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.query, err)
			continue
		}
		if got := fx.search(t, q); !sameSet(got, tc.want) {
			t.Errorf("Search(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func cafeNames() []string { return sorted("cafe_nfc", "cafe_nfd") }

// TestSearchSubstring: substring search matches inside tokens and across punctuation, folds like the
// word search, and needs three characters.
func TestSearchSubstring(t *testing.T) {
	fx := newTextFix(t)
	sub := records.TextOptions{Substring: true}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"555) 123", sorted("phone")},
		{"+1 (5", sorted("phone")},
		{"23-45", sorted("phone")},
		{"ample.com/pa", sorted("url")},
		{"https://", sorted("url")},
		{"x=1&y", sorted("url")},
		{cp(0x597d, 0x4e16, 0x754c), sorted("cjk")},
		{cp(0x4f60, 0x597d, 0x4e16), sorted("cjk")},
		{"abc", sorted("fullwidth")},
		{"  caf  ", cafeNames()},
		{"zzz", nil},
	} {
		q := mustCompile(t, tc.query, sub)
		if got := fx.search(t, q); !sameSet(got, tc.want) {
			t.Errorf("Search(substring %q) = %v, want %v", tc.query, got, tc.want)
		}
	}
	for _, short := range []string{"ab", "a", "", "  ab  ", cp(0x597d, 0x4e16)} {
		if _, err := records.CompileQuery(short, sub); !errors.Is(err, records.ErrInvalidQuery) {
			t.Errorf("substring %q: %v, want ErrInvalidQuery (three characters at least)", short, err)
		}
	}
}

// textDataset is 150 records with tied and missing timestamps whose summaries start with alpha, beta
// or gamma (by id), for the pagination tests.
func textDataset(t *testing.T) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	recs := recordstest.Records(art.ID, 150, 7)
	for i := range recs {
		recs[i].Summary = fmt.Sprintf("%s item %d", []string{"alpha", "beta", "gamma"}[i%3], i)
		recs[i].Body = ""
		if i%10 == 0 {
			recs[i].Body = "body mentions alpha too"
		}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, newReader(t, c)
}

func searchPages(t *testing.T, r *records.Reader, f records.Filter, size int, desc bool, maxRows int) []records.Hit {
	t.Helper()
	var all []records.Hit
	cur := ""
	for i := 0; ; i++ {
		if i > maxRows+2 {
			t.Fatalf("size %d desc=%v: more pages than rows", size, desc)
		}
		res, err := r.Search(ctx, f, records.Page{Limit: size, Cursor: cur, Desc: desc}, records.SearchOptions{})
		if err != nil {
			t.Fatalf("size %d desc=%v page %d: %v", size, desc, i, err)
		}
		if len(res.Hits) == 0 || len(res.Hits) > size {
			t.Fatalf("size %d desc=%v page %d: %d hits", size, desc, i, len(res.Hits))
		}
		all = append(all, res.Hits...)
		if res.NextCursor == "" {
			return all
		}
		if len(res.Hits) != size {
			t.Fatalf("size %d desc=%v page %d: a short page (%d) with a next cursor", size, desc, i, len(res.Hits))
		}
		cur = res.NextCursor
	}
}

// TestSearchKeysetPaginationWithText: pages of 1..7 and 100 over text queries with tied and NULL
// timestamps, ascending and descending, concatenate to the unpaged result with no gap or duplicate;
// and the hits are the rows List returns for the same filter.
func TestSearchKeysetPaginationWithText(t *testing.T) {
	_, r := textDataset(t)
	for _, tc := range []struct {
		name string
		q    *records.TextQuery
	}{
		{"one word", mustCompile(t, "alpha", records.TextOptions{})},
		{"or", mustCompile(t, "alpha OR beta", records.TextOptions{})},
		{"summary only", mustCompile(t, "alpha OR beta", records.TextOptions{Column: records.ColSummary})},
		{"prefix", mustCompile(t, "ite*", records.TextOptions{})},
		{"substring", mustCompile(t, "pha item", records.TextOptions{Substring: true})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := records.Filter{Text: tc.q}
			whole, err := r.Search(ctx, f, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
			if err != nil || whole.NextCursor != "" || len(whole.Hits) < 20 {
				t.Fatalf("unpaged: %d hits, cursor %q, %v", len(whole.Hits), whole.NextCursor, err)
			}
			rows := make([]records.Row, len(whole.Hits))
			for i, h := range whole.Hits {
				rows[i] = h.Row
			}
			if want := wantOrder(rows); !reflect.DeepEqual(ids(want), ids(rows)) {
				t.Fatalf("hits are not in (untimed last, ts, id) order:\n got %v\nwant %v", ids(rows), ids(want))
			}
			seenNull := false
			for _, row := range rows {
				seenNull = seenNull || row.TS == nil
			}
			if !seenNull {
				t.Fatal("the dataset holds no untimed hit")
			}
			list, err := r.List(ctx, f, records.Page{Limit: records.MaxLimit})
			if err != nil || !reflect.DeepEqual(list.Rows, rows) {
				t.Fatalf("List with the same text differs from Search: %v", err)
			}
			rev, err := r.Search(ctx, f, records.Page{Limit: records.MaxLimit, Desc: true}, records.SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantRev := slices.Clone(whole.Hits)
			slices.Reverse(wantRev)
			if !reflect.DeepEqual(rev.Hits, wantRev) {
				t.Fatalf("descending is not the exact reverse")
			}
			for _, size := range []int{1, 2, 3, 4, 5, 6, 7, 100} {
				if got := searchPages(t, r, f, size, false, len(whole.Hits)); !reflect.DeepEqual(got, whole.Hits) {
					t.Fatalf("size %d ascending: pages differ from the unpaged result: %v vs %v", size, hitIDs(got), hitIDs(whole.Hits))
				}
				if got := searchPages(t, r, f, size, true, len(whole.Hits)); !reflect.DeepEqual(got, wantRev) {
					t.Fatalf("size %d descending: pages differ from the unpaged result", size)
				}
			}
		})
	}
}

// TestSearchCursorBoundToQuery: a cursor continues only the query that produced it.
func TestSearchCursorBoundToQuery(t *testing.T) {
	_, r := textDataset(t)
	a := mustCompile(t, "alpha OR beta", records.TextOptions{})
	cur := func(f records.Filter, desc bool) string {
		res, err := r.Search(ctx, f, records.Page{Limit: 3, Desc: desc}, records.SearchOptions{})
		if err != nil || res.NextCursor == "" {
			t.Fatalf("first page: %q, %v", res.NextCursor, err)
		}
		return res.NextCursor
	}
	curA := cur(records.Filter{Text: a}, false)
	for _, tc := range []struct {
		name string
		f    records.Filter
	}{
		{"another query", records.Filter{Text: mustCompile(t, "alpha OR gamma", records.TextOptions{})}},
		{"another column", records.Filter{Text: mustCompile(t, "alpha OR beta", records.TextOptions{Column: records.ColSummary})}},
		{"another table", records.Filter{Text: mustCompile(t, "alpha", records.TextOptions{Substring: true})}},
		{"with a filter", records.Filter{Text: a, Types: []string{"message"}}},
	} {
		_, err := r.Search(ctx, tc.f, records.Page{Limit: 3, Cursor: curA}, records.SearchOptions{})
		if !errors.Is(err, records.ErrBadCursor) {
			t.Errorf("%s: %v, want ErrBadCursor", tc.name, err)
		}
	}
	// rank is a different listing: List has no rank order and refuses it (R52)
	if _, err := r.List(ctx, records.Filter{Text: mustCompile(t, "alpha OR beta", records.TextOptions{Rank: true})}, records.Page{Limit: 3, Cursor: curA}); !errors.Is(err, records.ErrInvalidPage) {
		t.Errorf("rank query: %v, want ErrInvalidPage", err)
	}
	// no text: the cursor of a text search is refused by a listing without text, and the converse
	if _, err := r.List(ctx, records.Filter{}, records.Page{Limit: 3, Cursor: curA}); !errors.Is(err, records.ErrBadCursor) {
		t.Errorf("no text: %v, want ErrBadCursor", err)
	}
	plain, err := r.List(ctx, records.Filter{}, records.Page{Limit: 3})
	if err != nil || plain.NextCursor == "" {
		t.Fatal(plain.NextCursor, err)
	}
	if _, err := r.Search(ctx, records.Filter{Text: a}, records.Page{Limit: 3, Cursor: plain.NextCursor}, records.SearchOptions{}); !errors.Is(err, records.ErrBadCursor) {
		t.Errorf("a cursor without text used with a text: %v, want ErrBadCursor", err)
	}
	// the other direction
	if _, err := r.Search(ctx, records.Filter{Text: a}, records.Page{Limit: 3, Cursor: curA, Desc: true}, records.SearchOptions{}); !errors.Is(err, records.ErrBadCursor) {
		t.Errorf("direction: %v, want ErrBadCursor", err)
	}
	// the same query compiled again continues, and so does List (the same listing)
	again := mustCompile(t, "alpha OR beta", records.TextOptions{})
	s, err := r.Search(ctx, records.Filter{Text: again}, records.Page{Limit: 3, Cursor: curA}, records.SearchOptions{})
	if err != nil || len(s.Hits) != 3 {
		t.Fatalf("same query compiled again: %d hits, %v", len(s.Hits), err)
	}
	l, err := r.List(ctx, records.Filter{Text: a}, records.Page{Limit: 3, Cursor: curA})
	if err != nil || !reflect.DeepEqual(ids(l.Rows), hitIDs(s.Hits)) {
		t.Fatalf("List continues the Search cursor: %v, %v", ids(l.Rows), err)
	}
}

// TestFingerprintWithoutTextUnchanged: a filter without Text keeps the fingerprint 5A produced
// (the values were taken from the 5A code), so a cursor made before the text search existed stays
// valid; the text query is part of the fingerprint.
func TestFingerprintWithoutTextUnchanged(t *testing.T) {
	const caseID = "CASE\x002026-01-01T00:00:00Z"
	from := time.Unix(1_600_000_000, 0)
	three := 3
	full := records.Filter{
		Types: []string{"message", "call"}, From: &from, IncludeUntimed: true, ArtifactIDs: []string{"a1"}, PathPrefix: "/data/",
		Deleted: records.None, Recovered: records.Only, Parsers: []records.ParserRef{{Name: "sms", Version: "1"}}, MinConfidence: &three, IngestID: "ing",
	}
	for _, tc := range []struct {
		name         string
		f            records.Filter
		asc, descend string
	}{
		{"zero filter", records.Filter{}, "9aa4519f023786299ebc7ad3d8eb461a", "fbb2d2fe92fe350c39678f5e42d74f49"},
		{"every field", full, "b62b1e82f5c239a4e7bf37e016847341", "4e9597c667afd17eaac1aa9250c483fb"},
	} {
		if got := records.Fingerprint(tc.f, caseID, false); got != tc.asc {
			t.Errorf("%s ascending: %s, want the 5A value %s", tc.name, got, tc.asc)
		}
		if got := records.Fingerprint(tc.f, caseID, true); got != tc.descend {
			t.Errorf("%s descending: %s, want the 5A value %s", tc.name, got, tc.descend)
		}
	}
	a := records.Filter{Text: mustCompile(t, "alpha", records.TextOptions{})}
	b := records.Filter{Text: mustCompile(t, "beta", records.TextOptions{})}
	col := records.Filter{Text: mustCompile(t, "alpha", records.TextOptions{Column: records.ColBody})}
	fps := map[string]string{
		"none": records.Fingerprint(records.Filter{}, caseID, false), "a": records.Fingerprint(a, caseID, false),
		"b": records.Fingerprint(b, caseID, false), "column": records.Fingerprint(col, caseID, false),
	}
	seen := map[string]string{}
	for name, fp := range fps {
		if other, dup := seen[fp]; dup {
			t.Errorf("filters %s and %s have the same fingerprint %s", name, other, fp)
		}
		seen[fp] = name
	}
	if again := records.Fingerprint(records.Filter{Text: mustCompile(t, "alpha", records.TextOptions{})}, caseID, false); again != fps["a"] {
		t.Errorf("the same query compiled twice has two fingerprints: %s and %s", again, fps["a"])
	}
}

// TestSearchRespectsFiltersAndSupersession: the filters combine with the text, and the hits of a
// superseded run are hidden unless the filter asks for them.
func TestSearchRespectsFiltersAndSupersession(t *testing.T) {
	f := newFixture(t)
	h := func(n int) *time.Time { return ptr(hourTime(n)) }
	all := mustCompile(t, "r0* OR r1*", records.TextOptions{})
	some := mustCompile(t, "r0*", records.TextOptions{})
	labels := func(rows []records.Row) []string {
		out := summaries(rows)
		if out == nil {
			out = []string{}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		f    records.Filter
	}{
		{"none", records.Filter{}},
		{"type", records.Filter{Types: []string{"message"}}},
		{"two types", records.Filter{Types: []string{"call", "location"}}},
		{"from", records.Filter{From: h(3)}},
		{"from with untimed", records.Filter{From: h(3), IncludeUntimed: true}},
		{"range", records.Filter{From: h(3), To: h(6)}},
		{"artifact", records.Filter{ArtifactIDs: []string{f.arts[1].ID}}},
		{"deleted", records.Filter{Deleted: records.Only}},
		{"not deleted", records.Filter{Deleted: records.None}},
		{"recovered", records.Filter{Recovered: records.Only}},
		{"parser name", records.Filter{Parsers: []records.ParserRef{{Name: "alpha"}}}},
		{"parser version", records.Filter{Parsers: []records.ParserRef{{Name: "alpha", Version: "2"}}}},
		{"confidence", records.Filter{MinConfidence: ptr(60)}},
		{"ingest", records.Filter{IngestID: f.ingest[1]}},
		{"path", records.Filter{PathPrefix: "/data/50%"}},
		{"combined", records.Filter{Types: []string{"message", "contact"}, Deleted: records.None, From: h(1), MinConfidence: ptr(45)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, err := f.r.List(ctx, tc.f, records.Page{Limit: records.MaxLimit})
			if err != nil {
				t.Fatal(err)
			}
			withAll := tc.f
			withAll.Text = all
			res, err := f.r.Search(ctx, withAll, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rows := make([]records.Row, len(res.Hits))
			for i, h := range res.Hits {
				rows[i] = h.Row
			}
			if !reflect.DeepEqual(rows, plain.Rows) {
				t.Fatalf("a text that matches everything changes the selection:\n got %v\nwant %v", labels(rows), labels(plain.Rows))
			}
			var want []records.Row
			for _, row := range plain.Rows {
				if strings.HasPrefix(row.Summary, "r0") {
					want = append(want, row)
				}
			}
			withSome := tc.f
			withSome.Text = some
			res, err = f.r.Search(ctx, withSome, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rows = rows[:0]
			for _, h := range res.Hits {
				rows = append(rows, h.Row)
			}
			if !sameSet(labels(rows), labels(want)) {
				t.Fatalf("text r0* with the filter: %v, want %v", labels(rows), labels(want))
			}
			n, _, err := f.r.Count(ctx, withSome, 0)
			if err != nil || int(n) != len(want) {
				t.Fatalf("Count = %d, %v, want %d", n, err, len(want))
			}
		})
	}

	tr := newTwoRuns(t, true)
	old := mustCompile(t, "old*", records.TextOptions{})
	newer := mustCompile(t, "new*", records.TextOptions{})
	count := func(q *records.TextQuery, superseded bool) int {
		res, err := tr.r.Search(ctx, records.Filter{Text: q, IncludeSuperseded: superseded}, records.Page{Limit: 100}, records.SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Hits)
	}
	if got := count(old, false); got != 0 {
		t.Errorf("the hits of a superseded run: %d, want 0 (hidden)", got)
	}
	if got := count(old, true); got != len(tr.run1Labs) {
		t.Errorf("IncludeSuperseded: %d hits, want %d", got, len(tr.run1Labs))
	}
	if got := count(newer, false); got != len(tr.run2Labs) {
		t.Errorf("the hits of the current run: %d, want %d", got, len(tr.run2Labs))
	}
	res, err := tr.r.Search(ctx, records.Filter{Text: old, IncludeSuperseded: true}, records.Page{Limit: 100}, records.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if !h.Row.Superseded {
			t.Errorf("hit %d of a superseded run is not marked Superseded", h.Row.ID)
		}
	}
	if n, _, err := tr.r.Count(ctx, records.Filter{Text: old}, 0); err != nil || n != 0 {
		t.Errorf("Count hides superseded hits: %d, %v", n, err)
	}
	ov, err := tr.r.Overview(ctx, records.Filter{Text: old, IncludeSuperseded: true})
	if err != nil || int(ov.Records) != len(tr.run1Labs) {
		t.Errorf("Overview with text: %+v, %v", ov, err)
	}
	st, err := tr.r.Stats(ctx, records.Filter{Text: newer}, "type")
	if err != nil || len(st) != 1 || int(st[0].Count) != len(tr.run2Labs) {
		t.Errorf("Stats with text: %+v, %v", st, err)
	}
}

// rankFix holds three records: 1 and 3 match in the summary, 2 matches in the body (same lengths).
func rankFix(t *testing.T) (*evidence.Case, *records.Reader) {
	t.Helper()
	c, art := setup(t)
	mk := func(summary, body string) records.Record {
		return records.Record{Type: "note", ArtifactID: art.ID, Summary: summary, Body: body, Payload: map[string]any{}}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{
		mk("alpha gamma", "delta epsilon"),
		mk("delta epsilon", "alpha gamma"),
		mk("alpha gamma", "delta epsilon"),
	})
	return c, newReader(t, c)
}

// TestSearchRankReturnsBestFirstWithoutCursor: relevance order weighs the summary twice the body,
// ties are broken by id, there is no cursor and the limits are enforced.
func TestSearchRankReturnsBestFirstWithoutCursor(t *testing.T) {
	_, r := rankFix(t)
	q := mustCompile(t, "alpha", records.TextOptions{Rank: true})
	f := records.Filter{Text: q}
	res, err := r.Search(ctx, f, records.Page{Limit: 10}, records.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hitIDs(res.Hits), []int64{1, 3, 2}; !slices.Equal(got, want) || res.NextCursor != "" {
		t.Fatalf("rank order %v (cursor %q), want the summary hits first, ties by id: %v", got, res.NextCursor, want)
	}
	res, err = r.Search(ctx, f, records.Page{Limit: 2}, records.SearchOptions{})
	if err != nil || !slices.Equal(hitIDs(res.Hits), []int64{1, 3}) || res.NextCursor != "" {
		t.Fatalf("best 2: %v, cursor %q, %v", hitIDs(res.Hits), res.NextCursor, err)
	}
	res, err = r.Search(ctx, f, records.Page{}, records.SearchOptions{})
	if err != nil || len(res.Hits) != 3 {
		t.Fatalf("default limit: %d hits, %v", len(res.Hits), err)
	}
	// a filter still applies
	res, err = r.Search(ctx, records.Filter{Text: q, Types: []string{"nope"}}, records.Page{}, records.SearchOptions{})
	if err != nil || len(res.Hits) != 0 {
		t.Fatalf("rank with a filter that selects nothing: %d hits, %v", len(res.Hits), err)
	}
	// snippets work in rank order
	res, err = r.Search(ctx, f, records.Page{Limit: 3}, records.SearchOptions{Snippets: true})
	if err != nil || len(res.Hits) != 3 || res.Hits[0].Summary == nil {
		t.Fatalf("rank with snippets: %+v, %v", res, err)
	}

	for name, p := range map[string]records.Page{
		"cursor":         {Limit: 2, Cursor: "anything"},
		"limit 1001":     {Limit: records.MaxRankLimit + 1},
		"negative limit": {Limit: -1},
		"descending":     {Limit: 2, Desc: true}, // R43: an option is never ignored silently
	} {
		if _, err := r.Search(ctx, f, p, records.SearchOptions{}); !errors.Is(err, records.ErrInvalidPage) {
			t.Errorf("%s: %v, want ErrInvalidPage", name, err)
		}
	}
	if res, err := r.Search(ctx, f, records.Page{Limit: records.MaxRankLimit}, records.SearchOptions{}); err != nil || len(res.Hits) != 3 {
		t.Errorf("limit 1000: %d hits, %v", len(res.Hits), err)
	}
	// ranking is refused with a substring query at compile time
	if _, err := records.CompileQuery("alpha", records.TextOptions{Substring: true, Rank: true}); !errors.Is(err, records.ErrInvalidQuery) {
		t.Errorf("rank with substring: %v", err)
	}
	// R17: the rank statement holds one MATCH (the full-text table drives it), not a second one in a subquery
	sqlText := records.RankSQL(f)
	if n := strings.Count(sqlText, "MATCH"); n != 1 {
		t.Errorf("the rank statement holds %d MATCH operators, want 1:\n%s", n, sqlText)
	}
	if !strings.Contains(sqlText, "bm25(records_fts, 2.0, 1.0)") || strings.Contains(sqlText, " IN (SELECT rowid") {
		t.Errorf("unexpected rank statement:\n%s", sqlText)
	}
}

// TestSearchRequiresText: Search without a text query, or with one that was not compiled, is refused.
func TestSearchRequiresText(t *testing.T) {
	fx := newTextFix(t)
	for name, f := range map[string]records.Filter{"no text": {}, "zero query": {Text: &records.TextQuery{}}} {
		if _, err := fx.r.Search(ctx, f, records.Page{}, records.SearchOptions{}); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("Search with %s: %v, want ErrInvalidFilter", name, err)
		}
	}
	for name, f := range map[string]records.Filter{"zero query": {Text: &records.TextQuery{}}} {
		if _, _, err := fx.r.Count(ctx, f, 0); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("Count with %s: %v, want ErrInvalidFilter", name, err)
		}
		if _, err := fx.r.List(ctx, f, records.Page{}); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("List with %s: %v, want ErrInvalidFilter", name, err)
		}
	}
	q := mustCompile(t, "cafe", records.TextOptions{})
	for name, tc := range map[string]struct {
		p  records.Page
		so records.SearchOptions
	}{
		"snippets over their limit": {records.Page{Limit: records.MaxSnippetLimit + 1}, records.SearchOptions{Snippets: true}},
		"width below 20":            {records.Page{}, records.SearchOptions{Snippets: true, SnippetWidth: 19}},
		"width above 400":           {records.Page{}, records.SearchOptions{Snippets: true, SnippetWidth: 401}},
		"negative width":            {records.Page{}, records.SearchOptions{Snippets: true, SnippetWidth: -1}},
	} {
		if _, err := fx.r.Search(ctx, records.Filter{Text: q}, tc.p, tc.so); !errors.Is(err, records.ErrInvalidPage) {
			t.Errorf("%s: %v, want ErrInvalidPage", name, err)
		}
	}
	if _, err := fx.r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: records.MaxSnippetLimit + 1}, records.SearchOptions{}); err != nil {
		t.Errorf("without snippets the page limit is the listing's: %v", err)
	}
	if _, err := fx.r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: records.MaxSnippetLimit}, records.SearchOptions{Snippets: true, SnippetWidth: 20}); err != nil {
		t.Errorf("1000 snippets of width 20: %v", err)
	}
}

// TestSearchSnippets: the hits carry snippets built from the original text, with the match
// highlighted in its original spelling; a field that is empty has none.
func TestSearchSnippets(t *testing.T) {
	fx := newTextFix(t)
	hit := func(query string, o records.TextOptions, name string) records.Hit {
		t.Helper()
		res, err := fx.r.Search(ctx, records.Filter{Text: mustCompile(t, query, o)}, records.Page{Limit: 100}, records.SearchOptions{Snippets: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range res.Hits {
			if h.Row.ID == fx.id[name] {
				return h
			}
		}
		t.Fatalf("Search(%q) did not return %s", query, name)
		return records.Hit{}
	}
	matches := func(s *records.Snippet) []string {
		var out []string
		if s == nil {
			return nil
		}
		for _, sp := range s.Spans {
			if sp.Match {
				out = append(out, sp.Text)
			}
		}
		return out
	}
	text := func(s *records.Snippet) string {
		var b strings.Builder
		for _, sp := range s.Spans {
			b.WriteString(sp.Text)
		}
		return b.String()
	}

	h := hit("strasse", records.TextOptions{}, "strasse")
	if got := matches(h.Summary); !slices.Equal(got, []string{"Stra" + cp(0xdf) + "e"}) {
		t.Errorf("strasse matches %q, want the original spelling", got)
	}
	if h.Body != nil {
		t.Errorf("a record without a body has a body snippet %+v", h.Body)
	}
	h = hit("cafe", records.TextOptions{}, "cafe_nfd")
	if got := matches(h.Summary); !slices.Equal(got, []string{"cafe" + cp(0x301)}) {
		t.Errorf("an NFD e acute matches %q, want it whole", got)
	}
	h = hit("bodyneedle", records.TextOptions{}, "bodyonly")
	if h.Summary != nil {
		t.Errorf("an empty summary has a snippet %+v", h.Summary)
	}
	if got := matches(h.Body); !slices.Equal(got, []string{"bodyneedle"}) {
		t.Errorf("body matches %q", got)
	}
	// the match of a 1 MiB body lies past the scan limit: the head is shown, not highlighted
	h = hit(recordstest.LongNeedle, records.TextOptions{}, "long")
	if h.Body == nil || len(matches(h.Body)) != 0 || !h.Body.TrailingCut || h.Body.LeadingCut || !strings.HasPrefix(text(h.Body), "lorem ipsum") {
		t.Errorf("the body snippet of the long record: %+v", h.Body)
	}
	if h.Summary == nil || text(h.Summary) != "long record" {
		t.Errorf("the summary snippet of the long record: %+v", h.Summary)
	}
	// substring mode highlights too
	h = hit("555) 123", records.TextOptions{Substring: true}, "phone")
	if got := matches(h.Summary); !slices.Equal(got, []string{"555) 123"}) {
		t.Errorf("substring matches %q", got)
	}
	// without the option there are no snippets
	res, err := fx.r.Search(ctx, records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{})}, records.Page{}, records.SearchOptions{})
	if err != nil || len(res.Hits) != 2 || res.Hits[0].Summary != nil || res.Hits[0].Body != nil {
		t.Errorf("no snippets requested: %+v, %v", res, err)
	}
	// the width is honoured
	res, err = fx.r.Search(ctx, records.Filter{Text: mustCompile(t, recordstest.LongNeedle, records.TextOptions{})}, records.Page{}, records.SearchOptions{Snippets: true, SnippetWidth: 30})
	if err != nil || len(res.Hits) != 1 || len([]rune(text(res.Hits[0].Body))) != 30 {
		t.Errorf("width 30: %+v, %v", res, err)
	}
}

func newIndexState(t *testing.T, value string) textFix {
	t.Helper()
	fx := newTextFix(t)
	recordstest.SetFTSNormVersion(t, fx.c.Dir, value)
	return fx
}

// TestSearchRefusesIndexThatIsNotCurrent: with the index unbuilt, building, stale or invalid every
// call that carries a text refuses, naming the remedy; a filter without text still works.
func TestSearchRefusesIndexThatIsNotCurrent(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"unbuilt", ""},
		{"building", "building"},
		{"stale", "fts0/some/other/build"},
		{"invalid", "garbage value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newIndexState(t, tc.value)
			q := mustCompile(t, "cafe", records.TextOptions{})
			f := records.Filter{Text: q}
			calls := map[string]func() error{
				"Search": func() error { _, err := fx.r.Search(ctx, f, records.Page{}, records.SearchOptions{}); return err },
				"Search rank": func() error {
					_, err := fx.r.Search(ctx, records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{Rank: true})}, records.Page{}, records.SearchOptions{})
					return err
				},
				"List":     func() error { _, err := fx.r.List(ctx, f, records.Page{}); return err },
				"Count":    func() error { _, _, err := fx.r.Count(ctx, f, 0); return err },
				"Stats":    func() error { _, err := fx.r.Stats(ctx, f, "type"); return err },
				"Overview": func() error { _, err := fx.r.Overview(ctx, f); return err },
				"TermHits": func() error {
					_, err := fx.r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "cafe"}}, 5)
					return err
				},
			}
			for name, call := range calls {
				err := call()
				if !errors.Is(err, records.ErrIndexNotCurrent) || !errors.Is(err, evidence.ErrIndexNotCurrent) {
					t.Errorf("%s: %v, want ErrIndexNotCurrent", name, err)
					continue
				}
				if !strings.Contains(err.Error(), "records reindex") {
					t.Errorf("%s: %q does not name the remedy `records reindex`", name, err)
				}
			}
			// without a text the same case still answers
			if res, err := fx.r.List(ctx, records.Filter{}, records.Page{}); err != nil || len(res.Rows) != 16 {
				t.Errorf("List without text: %d rows, %v", len(res.Rows), err)
			}
			if n, _, err := fx.r.Count(ctx, records.Filter{}, 0); err != nil || n != 16 {
				t.Errorf("Count without text: %d, %v", n, err)
			}
			if _, err := fx.r.Stats(ctx, records.Filter{}, "type"); err != nil {
				t.Errorf("Stats without text: %v", err)
			}
			if _, err := fx.r.Overview(ctx, records.Filter{}); err != nil {
				t.Errorf("Overview without text: %v", err)
			}
			// and once the index is current again, so does the search
			recordstest.SetFTSNormVersion(t, fx.c.Dir, evidence.FTSNormVersion())
			if got := fx.search(t, q); !sameSet(got, cafeNames()) {
				t.Errorf("after the state is current again: %v", got)
			}
		})
	}
}

// TestSearchNeedsSchemaV3: a case that was never upgraded to v3 has no index: every call with a text is
// ErrNeedsUpgrade, while the records stay readable without one.
func TestSearchNeedsSchemaV3(t *testing.T) {
	c := openCase(t, recordstest.CopyV2Case(t))
	r := newReader(t, c)
	q := mustCompile(t, "alpha", records.TextOptions{})
	f := records.Filter{Text: q}
	for name, call := range map[string]func() error{
		"Search":   func() error { _, err := r.Search(ctx, f, records.Page{}, records.SearchOptions{}); return err },
		"List":     func() error { _, err := r.List(ctx, f, records.Page{}); return err },
		"Count":    func() error { _, _, err := r.Count(ctx, f, 0); return err },
		"Stats":    func() error { _, err := r.Stats(ctx, f, "type"); return err },
		"Overview": func() error { _, err := r.Overview(ctx, f); return err },
		"TermHits": func() error {
			_, err := r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "alpha"}}, 5)
			return err
		},
	} {
		if err := call(); !errors.Is(err, evidence.ErrNeedsUpgrade) || errors.Is(err, records.ErrIndexNotCurrent) {
			t.Errorf("%s on a v2 case: %v, want ErrNeedsUpgrade", name, err)
		} else if !strings.Contains(err.Error(), "case upgrade") {
			t.Errorf("%s: %q does not name `case upgrade`", name, err)
		}
	}
	if res, err := r.List(ctx, records.Filter{}, records.Page{Limit: 100}); err != nil || len(res.Rows) == 0 {
		t.Errorf("List without text on a v2 case: %d rows, %v", len(res.Rows), err)
	}
}

// TestSearchCannotWriteIndex: inside the reader's read transaction an INSERT or the FTS5 'delete'
// command is refused by ReadHandle, so a search can never change the index.
func TestSearchCannotWriteIndex(t *testing.T) {
	fx := newTextFix(t)
	before := recordstest.IndexedIDs(t, fx.c, evidence.FTSWordTable)
	hits := recordstest.FTSMatch(t, fx.c, evidence.FTSWordTable, `"cafe"`)
	for _, stmt := range []string{
		`INSERT INTO records_fts(rowid, summary, body) VALUES (99, 'x', 'y')`,
		`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 1, 'cafe au lait', '')`,
		`INSERT INTO records_fts(records_fts) VALUES ('delete-all')`,
		`INSERT INTO records_fts_sub(records_fts_sub) VALUES ('rebuild')`,
		`DELETE FROM records_fts_data`,
		`SELECT 1; INSERT INTO records_fts(records_fts) VALUES ('delete-all')`,
		`WITH x AS (SELECT 1) INSERT INTO records_fts(rowid, summary, body) SELECT 98, 'x', 'y' FROM x`,
	} {
		err := fx.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
			rows, err := h.QueryContext(ctx, stmt)
			if err == nil {
				_ = rows.Close()
			}
			return err
		})
		if !errors.Is(err, evidence.ErrReadSQLRefused) {
			t.Errorf("%q: %v, want ErrReadSQLRefused", stmt, err)
		}
	}
	if after := recordstest.IndexedIDs(t, fx.c, evidence.FTSWordTable); !slices.Equal(before, after) {
		t.Errorf("the index changed: %v -> %v", before, after)
	}
	if after := recordstest.FTSMatch(t, fx.c, evidence.FTSWordTable, `"cafe"`); !slices.Equal(hits, after) {
		t.Errorf("a MATCH changed: %v -> %v", hits, after)
	}
}

// TestSearchTimeoutMapsToErrSearchTimeout: when the deadline of the context passes while the MATCH
// statement runs, the error is ErrSearchTimeout wrapping context.DeadlineExceeded; a cancellation is
// not a timeout; and the case answers the next search.
func TestSearchTimeoutMapsToErrSearchTimeout(t *testing.T) {
	fx := newTextFix(t)
	q := mustCompile(t, "cafe", records.TextOptions{})
	f := records.Filter{Text: q}
	calls := map[string]func(ctx context.Context) error{
		"Search": func(ctx context.Context) error {
			_, err := fx.r.Search(ctx, f, records.Page{}, records.SearchOptions{})
			return err
		},
		"Search rank": func(ctx context.Context) error {
			_, err := fx.r.Search(ctx, records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{Rank: true})}, records.Page{}, records.SearchOptions{})
			return err
		},
		"Count":    func(ctx context.Context) error { _, _, err := fx.r.Count(ctx, f, 0); return err },
		"Stats":    func(ctx context.Context) error { _, err := fx.r.Stats(ctx, f, "type"); return err },
		"Overview": func(ctx context.Context) error { _, err := fx.r.Overview(ctx, f); return err },
		"List":     func(ctx context.Context) error { _, err := fx.r.List(ctx, f, records.Page{}); return err },
		"TermHits": func(ctx context.Context) error {
			_, err := fx.r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "cafe"}}, 5)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			// the deadline passes right before the statement runs
			dctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			ran := 0
			fx.r.SetBeforeQuery(func() { ran++; <-dctx.Done() })
			err := call(dctx)
			fx.r.SetBeforeQuery(nil)
			if ran == 0 {
				t.Fatal("the seam never ran: the MATCH statement did not start")
			}
			if !errors.Is(err, records.ErrSearchTimeout) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%v, want ErrSearchTimeout wrapping context.DeadlineExceeded", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("a timeout is not a cancellation: %v", err)
			}
			// a cancellation is a cancellation, not a timeout
			cctx, ccancel := context.WithCancel(ctx)
			fx.r.SetBeforeQuery(func() { ccancel() })
			err = call(cctx)
			fx.r.SetBeforeQuery(nil)
			if !errors.Is(err, context.Canceled) || errors.Is(err, records.ErrSearchTimeout) {
				t.Fatalf("cancelled: %v, want context.Canceled and not ErrSearchTimeout", err)
			}
			// the case answers the next search
			if got := fx.search(t, q); !sameSet(got, cafeNames()) {
				t.Fatalf("the search after a timeout: %v", got)
			}
		})
	}
	// an already expired deadline is a timeout too
	dctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if _, err := fx.r.Search(dctx, f, records.Page{}, records.SearchOptions{}); !errors.Is(err, records.ErrSearchTimeout) {
		t.Errorf("expired deadline: %v, want ErrSearchTimeout", err)
	}
	if got := fx.search(t, q); !sameSet(got, cafeNames()) {
		t.Fatalf("the search after an expired deadline: %v", got)
	}
}

// TestSearchPlanUsesTheIndexForMatch: the plan of the default-order text query drives the MATCH
// through the full-text virtual table (a MATCH-driven scan of it, never a full scan of its content)
// and reads the records through their primary key or an ordered index of r.
//
// Plan recorded 2026-10 (SQLite as pinned in go.mod), 150 records, word and substring, both
// directions: FTS first, then the records by primary key, the order made by a temp b-tree:
//
//	SEARCH r USING INTEGER PRIMARY KEY (rowid=?)
//	LIST SUBQUERY 1
//	SCAN records_fts VIRTUAL TABLE INDEX 0:M2
//	SEARCH p ... / SEARCH b ... (LEFT-JOIN by primary key)
//	USE TEMP B-TREE FOR ORDER BY
//
// The alternative (a scan of the ts index probing the MATCH per row) is the planner's choice on other
// data; tuning it is 5D.
func TestSearchPlanUsesTheIndexForMatch(t *testing.T) {
	c, _ := textDataset(t)
	for _, tc := range []struct {
		name, query string
		opt         records.TextOptions
		table       string
	}{
		{"word", "alpha", records.TextOptions{}, "records_fts"},
		{"substring", "pha item", records.TextOptions{Substring: true}, "records_fts_sub"},
	} {
		for _, desc := range []bool{false, true} {
			f := records.Filter{Text: mustCompile(t, tc.query, tc.opt)}
			qs, err := records.ListQueries(f, desc, nil, 10, false)
			if err != nil || len(qs) != 1 {
				t.Fatalf("%s: %d queries, %v", tc.name, len(qs), err)
			}
			if !strings.HasSuffix(strings.TrimSpace(qs[0].SQL), "LIMIT ?") { // R67: the sorter is bounded
				t.Errorf("%s: the list statement has no LIMIT: %s", tc.name, qs[0].SQL)
			}
			plan := planOf(t, c.Dir, qs[0].SQL, qs[0].Args...)
			t.Logf("%s desc=%v:\n  %s", tc.name, desc, strings.Join(plan, "\n  "))
			var fts, outer string
			for _, d := range plan {
				if strings.Contains(d, tc.table) && !strings.Contains(d, tc.table+"_") {
					fts = d
				}
				if d == "SCAN r" || strings.HasPrefix(d, "SCAN r ") || strings.HasPrefix(d, "SEARCH r ") {
					outer = d
				}
			}
			if !strings.Contains(fts, "VIRTUAL TABLE INDEX 0:M") {
				t.Errorf("%s: the plan has no MATCH-driven use of %s: %q\n%s", tc.name, tc.table, fts, strings.Join(plan, "\n"))
			}
			if strings.Contains(fts, "VIRTUAL TABLE INDEX 0:") && !strings.Contains(fts, "VIRTUAL TABLE INDEX 0:M") {
				t.Errorf("%s: a full scan of %s: %q", tc.name, tc.table, fts)
			}
			// R23: the outer access to r is part of the assertion: FTS first, then the records by their
			// primary key (never a scan of records), the order made by a temporary b-tree.
			if outer != "SEARCH r USING INTEGER PRIMARY KEY (rowid=?)" {
				t.Errorf("%s: the records are not reached by primary key from the MATCH: %q (plan: %s)", tc.name, outer, strings.Join(plan, "; "))
			}
			if !slices.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("%s: the order is no longer made by a temporary b-tree (the plan changed; record the new one): %s", tc.name, strings.Join(plan, "; "))
			}
		}
	}
}

// planOf returns the detail column of EXPLAIN QUERY PLAN for a statement, run on a second connection
// to the case database (it writes nothing).
func planOf(t *testing.T, dir, query string, args ...any) []string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
