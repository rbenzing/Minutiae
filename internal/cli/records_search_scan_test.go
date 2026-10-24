package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// scanDataset is a closed case of three records (ids 1..3) with the given summaries.
func scanDataset(t *testing.T, summaries ...string) string {
	t.Helper()
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	var recs []records.Record
	for _, s := range summaries {
		recs = append(recs, records.Record{Type: "note", ArtifactID: art.ID, Summary: s, Payload: recordstest.ValidPayload("note")})
	}
	recordstest.Ingest(t, c, recParser, []string{art.ID}, recs)
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runScanSplit(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var errBuf bytes.Buffer
	code, stdout = run(t, Deps{Err: &errBuf}, args...)
	return code, stdout, errBuf.String()
}

func TestRecordsSearchScanFindsAnEmoji(t *testing.T) {
	dir := scanDataset(t, "love ❤ you", "cab driver", "plain")
	code, out, errOut := runScanSplit(t, "records", "search", "--case", dir, "--scan", "❤")
	if code != 0 {
		t.Fatalf("scan: %d %s%s", code, out, errOut)
	}
	if !strings.HasPrefix(out, "1  -  note  love ❤ you\n") {
		t.Fatalf("stdout hit line: %q", out)
	}
	if !strings.Contains(errOut, "note: --scan read 3 records (stored text, no index)\n") {
		t.Fatalf("stderr note: %q", errOut)
	}
	if strings.Contains(out, "note:") {
		t.Fatalf("stdout carries the note: %q", out)
	}
}

func TestRecordsSearchScanJSON(t *testing.T) {
	dir := scanDataset(t, "love ❤ you", "cab driver", "plain")
	var l map[string]any
	cliJSON(t, &l, "records", "search", "--case", dir, "--scan", "ab")
	recs, _ := l["records"].([]any)
	if len(recs) != 1 || l["next_cursor"] != "" || l["scanned"] != float64(3) {
		t.Fatalf("scan json: %v", l)
	}
	var sub map[string]any
	cliJSON(t, &sub, "records", "search", "--case", dir, "--substring", "abc")
	if _, has := sub["scanned"]; has {
		t.Fatalf("a non-scan search has a scanned key: %v", sub)
	}
}

func TestRecordsSearchShortSubstringNamesScan(t *testing.T) {
	dir := scanDataset(t, "love ❤ you", "cab driver", "plain")
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "--substring", "ab")
	if code != ExitUsage || !strings.Contains(out, "--substring --scan") {
		t.Fatalf("short substring: %d %s", code, out)
	}
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "a*")
	if code != ExitUsage || strings.Contains(out, "--scan") {
		t.Fatalf("short prefix: %d %s", code, out)
	}
	// the other M12 refusal: a term with no letter or digit
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "!!!")
	if code != ExitUsage || !strings.Contains(out, "--substring --scan") {
		t.Fatalf("symbol-only term: %d %s", code, out)
	}
	// refusals the scan cannot serve either carry no hint
	code, out = run(t, Deps{}, "records", "search", "--case", dir, strings.Repeat("a", records.MaxTermBytes+1))
	if code != ExitUsage || strings.Contains(out, "--scan") {
		t.Fatalf("long term: %d %s", code, out)
	}
	code, out = run(t, Deps{}, "records", "search", "--case", dir, strings.TrimSpace(strings.Repeat("ab ", records.MaxQueryBytes/3+1)))
	if code != ExitUsage || strings.Contains(out, "--scan") {
		t.Fatalf("long query: %d %s", code, out)
	}
}

func TestRecordsSearchScanWithRankIsUsageError(t *testing.T) {
	dir := scanDataset(t, "a", "b", "c")
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "--scan", "--rank", "a")
	if code != ExitUsage || !strings.Contains(out, "--rank cannot be combined with --scan") {
		t.Fatalf("--scan --rank: %d %s", code, out)
	}
	code, out = run(t, Deps{}, "records", "search", "--case", dir, "--substring", "--rank", "abc")
	if code != ExitUsage || !strings.Contains(out, "--rank cannot be combined with --substring") || strings.Contains(out, "--scan") {
		t.Fatalf("--substring --rank: %d %s", code, out)
	}
}

// TestRecordsSearchScanOverCapBodyExitsFour: a stored body above the ingest cap is an integrity error.
func TestRecordsSearchScanOverCapBodyExitsFour(t *testing.T) {
	dir := scanDataset(t, "one", "two", "three")
	recordstest.SetRecordColumn(t, dir, 2, "body", strings.Repeat("a", 4<<20+1))
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "--scan", "zzz")
	if code != ExitIntegrity || !strings.Contains(out, "record 2") {
		t.Fatalf("over-cap body: %d %s", code, out)
	}
}

func TestRecordsSearchScanImpliesSubstring(t *testing.T) {
	dir := scanDataset(t, "a", "b", `say "a OR b" now`)
	var l jsonSearch
	cliJSON(t, &l, "records", "search", "--case", dir, "--scan", `"a OR b"`)
	if ids := hitIDsOf(l); !slices.Equal(ids, []int64{3}) {
		t.Fatalf("hits %v", ids)
	}
}

func TestRecordsSearchScanTimeoutExitsOne(t *testing.T) {
	dir := scanDataset(t, "x", "y", "z")
	old := searchContext
	searchContext = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithDeadline(ctx, time.Now().Add(-time.Hour))
	}
	defer func() { searchContext = old }()
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "--scan", "x")
	if code != ExitError || !strings.Contains(out, "search timed out") {
		t.Fatalf("expired scan: %d %s", code, out)
	}
}
