package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Every string that comes out of the case's database reaches the terminal through printable (or
// escapeMarkers(printable())). Each test here plants a value with an escape sequence through a named
// tamper helper (or hands the printer a synthetic result) and fails when the escape is lost.

const escText = "\x1b[31mred\x1b[0m"

// requireEscaped fails when out holds a raw escape character or lacks the escaped spelling.
func requireEscaped(t *testing.T, out, what string) {
	t.Helper()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("%s: a raw ESC reached the output: %q", what, out)
	}
	if !strings.Contains(out, `\x1b[31m`) {
		t.Errorf("%s: the escaped form \\x1b[31m is missing: %q", what, out)
	}
}

// TestSearchAndListEscapeTheRecordType: the type of a record is database text.
func TestSearchAndListEscapeTheRecordType(t *testing.T) {
	dir := searchDataset(t)
	recordstest.SetRecordColumn(t, dir, 1, "type", "msg"+escText)
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if code != 0 {
		t.Fatalf("search: %d %s", code, out)
	}
	requireEscaped(t, out, "search line")
	code, out = run(t, Deps{}, "records", "list", "--case", dir)
	if code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	requireEscaped(t, out, "list line")
	// the marker characters of a stored type are never taken for a match on a search line
	recordstest.SetRecordColumn(t, dir, 1, "type", "m"+mOpen+"x"+mClose)
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "alpha", "--no-snippets")
	if code != 0 || strings.Contains(out, mOpen) || strings.Contains(out, mClose) || !strings.Contains(out, `m`+`\u`+`27e6x`+`\u`+`27e7`) {
		t.Errorf("search line with marker characters in the type: %d %q", code, out)
	}
}

// TestReindexTextEscapesTheVersions: the version the index state held (records_meta, text an
// attacker can set) and the version the printer is handed.
func TestReindexTextEscapesTheVersions(t *testing.T) {
	dir := searchDataset(t)
	recordstest.SetFTSNormVersion(t, dir, escText)
	code, out := run(t, Deps{}, "records", "reindex", "--case", dir)
	if code != 0 {
		t.Fatalf("reindex: %d %s", code, out)
	}
	requireEscaped(t, out, "reindex from")
	var b bytes.Buffer
	printReindex(&b, evidence.ReindexResult{FromNormVersion: "fts1/x", NormVersion: escText, Records: 1, Indexed: 1})
	requireEscaped(t, b.String(), "reindex to")
	b.Reset()
	printReindex(&b, evidence.ReindexResult{NormVersion: "fts1/x"})
	if !strings.Contains(b.String(), "norm version (none) -> fts1/x") {
		t.Errorf("an index never built: %q", b.String())
	}
}

// TestStatsTextEscapesTheIndexState: the state value is database text; the kind is escaped too.
func TestStatsTextEscapesTheIndexState(t *testing.T) {
	dir := searchDataset(t)
	recordstest.SetFTSNormVersion(t, dir, escText)
	code, out := run(t, Deps{}, "records", "stats", "--case", dir)
	if code != 0 {
		t.Fatalf("stats: %d %s", code, out)
	}
	requireEscaped(t, out, "stats value")
	var b bytes.Buffer
	printStats(&b, records.Overview{}, records.IndexStatus{Kind: escText, Value: "v"}, "type", nil)
	requireEscaped(t, b.String(), "stats kind")
}

// TestStatsTextIndexLine pins the two branches of the index line: a case older than v3 says the index
// is unavailable, and the word and substring document counts are not swapped.
func TestStatsTextIndexLine(t *testing.T) {
	var b bytes.Buffer
	printStats(&b, records.Overview{}, records.IndexStatus{Kind: "unavailable"}, "type", nil)
	if !strings.Contains(b.String(), "index:        unavailable (the case schema is older than v3; run: minutiae case upgrade)") {
		t.Errorf("unavailable: %q", b.String())
	}
	b.Reset()
	printStats(&b, records.Overview{}, records.IndexStatus{Kind: "current", Value: "fts1/x", WordDocs: 3, SubstringDocs: 7}, "type", nil)
	if !strings.Contains(b.String(), "index:        current (fts1/x; 3 word documents, 7 substring documents)") {
		t.Errorf("counts: %q", b.String())
	}
	// on a real v2 case the state is unavailable
	code, out := run(t, Deps{}, "records", "stats", "--case", recordstest.CopyV2Case(t))
	if code != 0 || !strings.Contains(out, "index:        unavailable") {
		t.Errorf("stats on a v2 case: %d %s", code, out)
	}
}

// TestSearchTextEscapesTheCursorLine: the cursor line goes through printable.
func TestSearchTextEscapesTheCursorLine(t *testing.T) {
	var b bytes.Buffer
	printSearchText(&b, records.SearchResult{NextCursor: escText}, nil)
	requireEscaped(t, b.String(), "cursor")
	b.Reset()
	printSearchText(&b, records.SearchResult{NextCursor: "abc"}, nil)
	if want := "# more records: --cursor abc\n"; b.String() != want {
		t.Errorf("cursor line %q, want %q", b.String(), want)
	}
}

// TestSearchMarksHitsOfAnIncompleteArtifact: the marker of records list is on a search hit too.
func TestSearchMarksHitsOfAnIncompleteArtifact(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddIncompleteArtifact(t, c, "cut.db", []byte("partial"))
	recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{
		{Type: "message", ArtifactID: art.ID, Summary: "needle one", Payload: map[string]any{}},
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "needle", "--no-snippets")
	if code != 0 || !strings.Contains(out, "[artifact incomplete]") {
		t.Fatalf("search: %d %q, want the incomplete marker", code, out)
	}
	if code, out := run(t, Deps{}, "records", "list", "--case", dir); code != 0 || !strings.Contains(out, "[artifact incomplete]") {
		t.Fatalf("list: %d %q", code, out)
	}
}

// TestSearchQueryEchoIsEscapedAndCutOnTheEscapedForm: the query an error echoes is escaped (a newline
// is `\n`, never a raw line break) and the CUT applies to the escaped text, on a rune boundary and
// never inside an escape sequence.
func TestSearchQueryEchoIsEscapedAndCutOnTheEscapedForm(t *testing.T) {
	dir := searchDataset(t)
	// a query whose terms are all below the floor: refused with the echo
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "ab\n", "--substring")
	if code == 0 {
		t.Fatalf("a short substring query succeeded: %s", out)
	}
	if strings.Contains(out, "ab\n") || !strings.Contains(out, `ab\n`) {
		t.Errorf("the newline of the query is not escaped in the echo: %q", out)
	}
	// 100 escape characters are 400 bytes escaped: cut to the cap, whole escapes only
	code, out = run(t, Deps{}, "records", "search", "--case", dir, strings.Repeat("\x1b", 100), "--substring")
	if code == 0 {
		t.Fatalf("an escape-only query succeeded: %s", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("a raw ESC in the echo: %q", out)
	}
	got := echoQuery(strings.Repeat("\x1b", 100))
	if len(got) > maxEchoedQueryBytes+len("...") || !strings.HasSuffix(got, `\x1b...`) {
		t.Errorf("echo %q (%d bytes): want at most %d, ending in a whole escape and ...", got, len(got), maxEchoedQueryBytes+3)
	}
	if rest := strings.TrimSuffix(strings.TrimPrefix(got, `"`), "..."); strings.Count(rest, `\x1b`)*len(`\x1b`) != len(rest) {
		t.Errorf("the cut split an escape sequence: %q", got)
	}
	// short queries are not cut
	if got := echoQuery("abc"); got != "abc" {
		t.Errorf("echoQuery(abc) = %q", got)
	}
	// 64 emoji (4 bytes each) are printable and cut on a rune boundary
	if got := echoQuery(strings.Repeat("\U0001F600", 80)); len(got) > maxEchoedQueryBytes+3 || !strings.HasSuffix(got, "...") || strings.ContainsRune(got, 0xFFFD) {
		t.Errorf("emoji echo %q", got)
	}
}

// TestRecordsSearchJSONStillCarriesTheCursorAsGiven documents that --json keeps strings as given.
func TestRecordsSearchJSONStillCarriesTheCursorAsGiven(t *testing.T) {
	dir := searchDataset(t)
	code, out := run(t, Deps{}, "--json", "records", "search", "--case", dir, "common", "--limit", "1", "--no-snippets")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	var v struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.NextCursor == "" {
		t.Fatalf("json %v: %q", err, out)
	}
}
