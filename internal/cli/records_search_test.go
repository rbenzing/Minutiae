package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const (
	mOpen  = "\u27e6"
	mClose = "\u27e7"
)

// searchDataset is a closed case of ten records (ids 1..10, one ingest of sms-parser 1.0, in id
// order, no times): a three-record "alpha" family of type message, three of type call that hold
// "common" in the body, and four filler records.
func searchDataset(t *testing.T) string {
	t.Helper()
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	mk := func(typ, summary, body string) records.Record {
		return records.Record{Type: typ, ArtifactID: art.ID, Summary: summary, Body: body, Payload: map[string]any{}}
	}
	recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{
		mk("message", "alpha report", "the quick brown fox"),
		mk("message", "beta report", "nothing to see"),
		mk("message", "gamma", "alpha fox jumps"),
		mk("call", "call one", "a common thing"),
		mk("call", "call two", "another common thing"),
		mk("call", "call three", "common ground"),
		mk("note", "delta", ""),
		mk("note", "epsilon", "filler"),
		mk("note", "zeta", "filler too"),
		mk("note", "eta", "last"),
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

type jsonSpan struct {
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

type jsonHit struct {
	jsonRow
	Snippets *struct {
		Summary []jsonSpan `json:"summary"`
		Body    []jsonSpan `json:"body"`
	} `json:"snippets"`
}

type jsonSearch struct {
	Records    []jsonHit `json:"records"`
	NextCursor string    `json:"next_cursor"`
}

func hitIDsOf(l jsonSearch) []int64 {
	var ids []int64
	for _, r := range l.Records {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestRecordsSearchTextAndJSON(t *testing.T) {
	dir := searchDataset(t)
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha")
	if code != 0 {
		t.Fatalf("search: %d %s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{
		"1  -  message  alpha report",
		"  summary: " + mOpen + "alpha" + mClose + " report",
		"  body: the quick brown fox",
		"3  -  message  gamma",
		"  summary: gamma",
		"  body: " + mOpen + "alpha" + mClose + " fox jumps",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("text output:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "alpha")
	if ids := hitIDsOf(l); !slices.Equal(ids, []int64{1, 3}) || l.NextCursor != "" {
		t.Fatalf("json hits %v cursor %q", ids, l.NextCursor)
	}
	s := l.Records[0].Snippets
	if s == nil || len(s.Body) != 1 || s.Body[0].Match || len(s.Summary) != 2 || s.Summary[0] != (jsonSpan{"alpha", true}) || s.Summary[1] != (jsonSpan{" report", false}) {
		t.Fatalf("json snippets of 1: %+v", s)
	}
	if l.Records[0].Summary != "alpha report" || l.Records[0].Type != "message" {
		t.Fatalf("json row: %+v", l.Records[0].jsonRow)
	}
	// --no-snippets: the lines of the list, no snippet lines and no snippets key
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if code != 0 || strings.Contains(out, "summary:") || strings.Contains(out, mOpen) || strings.Count(out, "\n") != 2 {
		t.Fatalf("--no-snippets: %d %q", code, out)
	}
	var ns jsonSearch
	cliJSON(t, &ns, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if len(ns.Records) != 2 || ns.Records[0].Snippets != nil {
		t.Fatalf("json --no-snippets: %+v", ns)
	}
	// a substring search
	var sub jsonSearch
	cliJSON(t, &sub, "records", "search", "--case", dir, "--substring", "ommo", "--no-snippets")
	if ids := hitIDsOf(sub); !slices.Equal(ids, []int64{4, 5, 6}) {
		t.Fatalf("substring hits %v", ids)
	}
	// a query that starts with a minus needs --
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "gamma -alpha")
	if code != 0 || strings.Contains(out, "alpha") {
		t.Fatalf("negation: %d %s", code, out)
	}
	if code, out = run(t, Deps{}, "records", "search", "--case", dir, "--", "-alpha"); code != ExitUsage || !strings.Contains(out, "negat") {
		t.Fatalf("a lone negation: %d %s", code, out)
	}
}

func TestRecordsSearchFilterFlags(t *testing.T) {
	dir := searchDataset(t)
	ids := func(args ...string) []int64 {
		var l jsonSearch
		cliJSON(t, &l, append([]string{"records", "search", "--case", dir, "--no-snippets"}, args...)...)
		return hitIDsOf(l)
	}
	for _, tc := range []struct {
		args []string
		want []int64
	}{
		{[]string{"common", "--type", "call"}, []int64{4, 5, 6}},
		{[]string{"common", "--type", "message"}, nil},
		{[]string{"alpha", "--in", "summary"}, []int64{1}},
		{[]string{"alpha", "--in", "body"}, []int64{3}},
		{[]string{"alpha", "--artifact", "nope"}, nil},
		{[]string{"alpha", "--parser", "sms-parser@1.0"}, []int64{1, 3}},
		{[]string{"alpha", "--parser", "other"}, nil},
		{[]string{"alpha", "--deleted", "only"}, nil},
		{[]string{"alpha", "--deleted", "none"}, []int64{1, 3}},
		{[]string{"alpha OR filler"}, []int64{1, 3, 8, 9}},
		{[]string{"common", "--limit", "2"}, []int64{4, 5}},
	} {
		if got := ids(tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestRecordsSearchPagination(t *testing.T) {
	dir := searchDataset(t)
	var all []int64
	cursor := ""
	pages := 0
	for {
		args := []string{"records", "search", "--case", dir, "--limit", "2", "report OR common OR filler OR alpha OR delta OR eta"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		var l jsonSearch
		cliJSON(t, &l, args...)
		all = append(all, hitIDsOf(l)...)
		pages++
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
		if pages > 20 {
			t.Fatal("pagination does not end")
		}
	}
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if !slices.Equal(all, want) || pages != 5 {
		t.Fatalf("pages %d, ids %v; want 5 pages and %v", pages, all, want)
	}
	// the text form ends a page with the cursor
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "--limit", "1", "report", "--no-snippets")
	if code != 0 || !strings.Contains(out, "# more records: --cursor ") {
		t.Fatalf("text page: %d %q", code, out)
	}
	// the union equals what stats counts for the same filter
	var st struct {
		Overview struct {
			Records int64 `json:"records"`
		} `json:"overview"`
	}
	cliJSON(t, &st, "records", "stats", "--case", dir)
	if int64(len(all)) != st.Overview.Records {
		t.Fatalf("search union %d, stats %d", len(all), st.Overview.Records)
	}
}

func TestRecordsSearchRank(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	mk := func(summary, body string) records.Record {
		return records.Record{Type: "note", ArtifactID: art.ID, Summary: summary, Body: body, Payload: map[string]any{}}
	}
	recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{
		mk("one", "needle among many many other words that dilute the match here"),
		mk("needle", "needle needle"),
		mk("two", "needle among words"),
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "--rank", "needle")
	if ids := hitIDsOf(l); len(ids) != 3 || ids[0] != 2 || l.NextCursor != "" {
		t.Fatalf("rank order %v cursor %q; want record 2 first", ids, l.NextCursor)
	}
	for _, args := range [][]string{
		{"--rank", "--cursor", "v1.abc"},
		{"--rank", "--substring"},
		{"--rank", "--limit", "1001"},
	} {
		code, out := run(t, Deps{}, append([]string{"records", "search", "--case", dir, "needle"}, args...)...)
		if code != ExitUsage {
			t.Errorf("%q: exit %d: %s", args, code, out)
		}
	}
}

func TestRecordsSearchBadQueryIsUsage(t *testing.T) {
	dir := searchDataset(t)
	emoji := strings.Repeat("\U0001F600", 64)
	for _, tc := range []struct {
		name string
		args []string
		want string // a word the message must carry
	}{
		{"empty", []string{""}, "empty"},
		{"blank", []string{"   "}, "empty"},
		{"unbalanced quote", []string{`"foo bar`}, "quote"},
		{"trailing OR", []string{"a OR"}, "OR"},
		{"prefix under 2", []string{"a*"}, "prefix"},
		{"short prefix after a hyphen", []string{"ab-c*"}, "prefix"},
		{"substring under 3", []string{"--substring", "ab"}, "shorter"},
		{"only a negation", []string{"--", "-foo"}, "negat"},
		{"4097 bytes", []string{strings.Repeat("a", 4097)}, "4096"},
		{"term over 256", []string{strings.Repeat("a", 257)}, "256"},
		{"emoji only", []string{emoji}, "letter"},
		{"escape in the query", []string{"\x1b[31m\"unterminated"}, "quote"},
		{"bad column", []string{"foo", "--in", "title"}, "--in"},
		{"limit 0", []string{"foo", "--limit", "0"}, "--limit"},
		{"limit 10001", []string{"foo", "--limit", "10001", "--no-snippets"}, "--limit"},
		{"limit 1001 with snippets", []string{"foo", "--limit", "1001"}, "--no-snippets"},
		{"negative timeout", []string{"foo", "--timeout", "-1s"}, "--timeout"},
		{"rank with cursor", []string{"foo", "--rank", "--cursor", "x"}, "--cursor"},
		{"rank with substring", []string{"foo", "--rank", "--substring"}, "--substring"},
		{"bad cursor", []string{"foo", "--cursor", "garbage"}, "cursor"},
		{"no query", nil, "argument"},
		{"two queries", []string{"foo", "bar"}, "argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, Deps{}, append([]string{"records", "search", "--case", dir}, tc.args...)...)
			if code != ExitUsage {
				t.Fatalf("exit %d, want %d: %s", code, ExitUsage, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("message lacks %q: %s", tc.want, out)
			}
			noRawNonPrintable(t, "error", out)
			if !utf8.ValidString(out) || len(out) > 1200 {
				t.Errorf("message is %d bytes (valid %v)", len(out), utf8.ValidString(out))
			}
		})
	}
	// the echoed query is escaped and cut: a long query of controls
	_, out := run(t, Deps{}, "records", "search", "--case", dir, strings.Repeat("\x1b", 250)+`"x`)
	if !strings.Contains(out, `\x1b`) || strings.Contains(out, "\x1b") {
		t.Errorf("the echo is not escaped: %q", out)
	}
	if len(out) > 1500 {
		t.Errorf("the echo is %d bytes", len(out))
	}
}

// TestRecordsSearchCLIEscapesSnippets: hostile text reaches the terminal only escaped, a marker
// character of the stored text never passes for a match, and --json keeps the strings as given.
func TestRecordsSearchCLIEscapesSnippets(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	evil := strings.ReplaceAll(recEvil, "\x00", "")
	forged := mOpen + " forged " + mClose
	res := recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{
		{
			Type: "message", ArtifactID: art.ID, Summary: "zebra " + evil + " " + forged,
			Body:    "zebra " + evil + " " + forged + " zebra\nnew line\u202e \x1b]0;title\x07 \u200b",
			Payload: map[string]any{},
		},
		{Type: "message", ArtifactID: art.ID, Summary: "zebra plain", Payload: map[string]any{}},
	})
	_ = res
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// invalid UTF-8 cannot be ingested; it can be planted in the row of the second record
	recordstest.SetRecordSummary(t, dir, 2, "zebra \xff\xfe bad")

	code, out := run(t, Deps{}, "records", "search", "--case", dir, "zebra")
	if code != 0 {
		t.Fatalf("search: %d %s", code, out)
	}
	noRawNonPrintable(t, "search output", out)
	if !utf8.ValidString(out) {
		t.Errorf("the output is not valid UTF-8: %q", out)
	}
	// matches: summary of 1 (1), body of 1 (2), summary of 2 (1) = 4, each wrapped once; the forged
	// pair of the text is escaped (twice per text: summary line, summary snippet, body snippet)
	if n, m := strings.Count(out, mOpen), strings.Count(out, mClose); n != 4 || m != 4 {
		t.Errorf("markers: %d open, %d close; want 4 and 4 (real matches only):\n%s", n, m, out)
	}
	if !strings.Contains(out, `\u27e6 forged \u27e7`) {
		t.Errorf("the forged markers are not shown escaped:\n%s", out)
	}
	for _, want := range []string{`\x1b`, `\u202e`, `\u200b`, `\xff`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks the escape %s:\n%s", want, out)
		}
	}
	// the list line's summary is escaped too, and one line per row plus snippet lines
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "1  ") && strings.Contains(line, mOpen) {
			t.Errorf("an unescaped marker on the list line: %q", line)
		}
	}
	// --json keeps the text as given
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "zebra")
	if len(l.Records) != 2 {
		t.Fatalf("json hits: %+v", l)
	}
	var body strings.Builder
	for _, sp := range l.Records[0].Snippets.Body {
		body.WriteString(sp.Text)
	}
	if !strings.Contains(body.String(), evil) || !strings.Contains(body.String(), forged) {
		t.Errorf("json body spans changed the text: %q", body.String())
	}
	if l.Records[0].Summary != "zebra "+evil+" "+forged {
		t.Errorf("json summary = %q", l.Records[0].Summary)
	}
}

func TestEscapeSnippet(t *testing.T) {
	sp := func(text string, match bool) records.Span { return records.Span{Text: text, Match: match} }
	for _, tc := range []struct {
		name string
		in   *records.Snippet
		want string
	}{
		{"plain", &records.Snippet{Spans: []records.Span{sp("a ", false), sp("b", true), sp(" c", false)}}, "a " + mOpen + "b" + mClose + " c"},
		{"cuts", &records.Snippet{Spans: []records.Span{sp("mid", true)}, LeadingCut: true, TrailingCut: true}, "..." + mOpen + "mid" + mClose + "..."},
		{"leading cut only", &records.Snippet{Spans: []records.Span{sp("x", false)}, LeadingCut: true}, "...x"},
		{"controls", &records.Snippet{Spans: []records.Span{sp("a\x1b[31m\u202e\u200b\u0085", false)}}, `a\x1b[31m\u202e\u200b\x85`},
		{"newline stays one line", &records.Snippet{Spans: []records.Span{sp("a\nb\r\nc\td", false)}}, `a\x0ab\x0d\x0ac\x09d`},
		{
			"stored markers are escaped", &records.Snippet{Spans: []records.Span{sp(mOpen+"x"+mClose, false), sp(mOpen+"y"+mClose, true)}},
			`\u27e6x\u27e7` + mOpen + `\u27e6y\u27e7` + mClose,
		},
		{"invalid UTF-8", &records.Snippet{Spans: []records.Span{sp("a\xffb", true)}}, mOpen + `a\xffb` + mClose},
		{"empty", &records.Snippet{}, ""},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeSnippet(tc.in); got != tc.want {
				t.Fatalf("escapeSnippet = %q, want %q", got, tc.want)
			}
		})
	}
	if got := escapeMarkers("a" + mOpen + mClose); got != `a\u27e6\u27e7` {
		t.Errorf("escapeMarkers = %q", got)
	}
}

func TestRecordsSearchRefusesStaleIndex(t *testing.T) {
	for _, value := range []string{"", "building", "fts0/some/other/build"} {
		dir := searchDataset(t)
		recordstest.SetFTSNormVersion(t, dir, value)
		code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha")
		if code != ExitUsage || !strings.Contains(out, "records reindex") {
			t.Errorf("index %q: exit %d, want %d naming records reindex: %s", value, code, ExitUsage, out)
		}
	}
}

func TestRecordsSearchOnV2CaseNeedsUpgrade(t *testing.T) {
	dir := recordstest.CopyV2Case(t)
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha")
	if code != ExitUsage || !strings.Contains(out, "case upgrade") {
		t.Fatalf("search on v2: %d %s", code, out)
	}
}

func TestRecordsSearchTimeoutExit1(t *testing.T) {
	dir := searchDataset(t)
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha", "--timeout", "1ns")
	if code != ExitError || !strings.Contains(out, "timed out") {
		t.Fatalf("--timeout 1ns: %d %s", code, out)
	}
	if code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha", "--timeout", "0"); code != 0 {
		t.Fatalf("--timeout 0: %d %s", code, out)
	}
}

func auditActions(t *testing.T, dir string) []string {
	t.Helper()
	es, err := evidence.ReadAuditEntries(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var a []string
	for _, e := range es {
		a = append(a, e.Action)
	}
	return a
}

func TestRecordsReindexCLI(t *testing.T) {
	dir := searchDataset(t)
	recordstest.SetFTSNormVersion(t, dir, "fts0/some/other/build")
	before := auditActions(t, dir)
	code, out := run(t, Deps{}, "records", "reindex", "--case", dir)
	if code != 0 {
		t.Fatalf("reindex: %d %s", code, out)
	}
	if !strings.HasPrefix(out, "reindexed 10 records (10 indexed): norm version fts0/some/other/build -> ") || strings.Count(out, "\n") != 1 {
		t.Fatalf("reindex text: %q", out)
	}
	got := auditActions(t, dir)[len(before):]
	if fmt.Sprint(got) != fmt.Sprint([]string{"case.open", evidence.ActionReindex, evidence.ActionReindexDone}) {
		t.Fatalf("audit grew by %v", got)
	}
	// search works now
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if ids := hitIDsOf(l); !slices.Equal(ids, []int64{1, 3}) {
		t.Fatalf("search after reindex: %v", ids)
	}
	var res struct {
		ReindexID string           `json:"reindex_id"`
		From      string           `json:"from_norm_version"`
		To        string           `json:"norm_version"`
		Records   int64            `json:"records"`
		Indexed   int64            `json:"indexed"`
		Docs      map[string]int64 `json:"docs"`
	}
	cliJSON(t, &res, "records", "reindex", "--case", dir)
	if res.ReindexID == "" || res.Records != 10 || res.Indexed != 10 || res.To == "" || res.From != res.To || len(res.Docs) != 2 {
		t.Fatalf("reindex json: %+v", res)
	}
	// an unbuilt index has no previous version: shown as such
	recordstest.SetFTSNormVersion(t, dir, "")
	if code, out := run(t, Deps{}, "records", "reindex", "--case", dir); code != 0 || !strings.Contains(out, "norm version (none) -> ") {
		t.Fatalf("reindex of an unbuilt index: %d %s", code, out)
	}
	// a v2 case needs the upgrade first
	v2 := recordstest.CopyV2Case(t)
	if code, out := run(t, Deps{}, "records", "reindex", "--case", v2); code != ExitUsage || !strings.Contains(out, "case upgrade") {
		t.Fatalf("reindex of a v2 case: %d %s", code, out)
	}
}

func TestRecordsReindexRefusedWhenCaseInUse(t *testing.T) {
	dir := searchDataset(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	before := auditActions(t, dir)
	code, out := run(t, Deps{}, "records", "reindex", "--case", dir)
	if code != ExitError || !strings.Contains(out, "in use") || strings.Contains(out, "reindexed") {
		t.Fatalf("reindex while open: %d %s", code, out)
	}
	if got := auditActions(t, dir); len(got) != len(before) {
		t.Fatalf("a refused reindex appended %v", got[len(before):])
	}
}

func TestRecordsStatsShowsIndexStatus(t *testing.T) {
	dir := searchDataset(t)
	code, out := run(t, Deps{}, "records", "stats", "--case", dir)
	if code != 0 || !strings.Contains(out, "index:        current (") || !strings.Contains(out, "10 word documents, 10 substring documents)") {
		t.Fatalf("stats text: %d %s", code, out)
	}
	var st struct {
		Index struct {
			State         string `json:"state"`
			Value         string `json:"value"`
			Current       string `json:"current"`
			WordDocs      int64  `json:"word_docs"`
			SubstringDocs int64  `json:"substring_docs"`
		} `json:"index"`
	}
	cliJSON(t, &st, "records", "stats", "--case", dir)
	if st.Index.State != "current" || st.Index.Value == "" || st.Index.Value != st.Index.Current || st.Index.WordDocs != 10 || st.Index.SubstringDocs != 10 {
		t.Fatalf("stats json index: %+v", st.Index)
	}
	recordstest.SetFTSNormVersion(t, dir, "fts0/some/other/build")
	if code, out := run(t, Deps{}, "records", "stats", "--case", dir); code != 0 || !strings.Contains(out, "index:        stale (fts0/some/other/build;") {
		t.Fatalf("stale stats: %d %s", code, out)
	}
	recordstest.SetFTSNormVersion(t, dir, "building")
	cliJSON(t, &st, "records", "stats", "--case", dir)
	if st.Index.State != "building" {
		t.Fatalf("building: %+v", st.Index)
	}
}

func TestCaseVerifyHiddenHitExits4(t *testing.T) {
	dir := searchDataset(t)
	recordstest.HideFromIndex(t, dir, evidence.FTSWordTable, 1)
	// a search no longer finds record 1
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if ids := hitIDsOf(l); !slices.Equal(ids, []int64{3}) {
		t.Fatalf("hidden record found: %v", ids)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "records_fts") {
		t.Fatalf("verify: %d %s", code, out)
	}
	if code, out := run(t, Deps{}, "records", "reindex", "--case", dir); code != 0 {
		t.Fatalf("reindex: %d %s", code, out)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
		t.Fatalf("verify after reindex: %d %s", code, out)
	}
	cliJSON(t, &l, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if ids := hitIDsOf(l); !slices.Equal(ids, []int64{1, 3}) {
		t.Fatalf("after reindex: %v", ids)
	}
}

func TestSearchErrorsMapToExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{records.ErrInvalidQuery, ExitUsage},
		{records.ErrIndexNotCurrent, ExitUsage},
		{records.ErrInvalidPage, ExitUsage},
		{records.ErrInvalidFilter, ExitUsage},
		{records.ErrSearchTimeout, ExitError},
		{records.ErrIngestActive, ExitError},
		{evidence.ErrIntegrity, ExitIntegrity},
		{fmt.Errorf("wrapped: %w", records.ErrInvalidQuery), ExitUsage},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
