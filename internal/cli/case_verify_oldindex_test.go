package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestCaseVerifyOldPipelineIndexExitsZero (M22): a case indexed by an earlier pipeline is a notice,
// not a problem: `case verify` prints one NOTICE line and OK, and exits 0.
func TestCaseVerifyOldPipelineIndexExitsZero(t *testing.T) {
	old := strings.Replace(evidence.FTSNormVersion(), "fts4/", "fts3/", 1)
	if old == evidence.FTSNormVersion() {
		t.Fatalf("FTSNormVersion() = %q is not a pipeline 4 version", old)
	}
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, 1<<10))
	recs := []records.Record{{Type: "note", ArtifactID: art.ID, Summary: "hello world", Payload: map[string]any{"i": 1}}}
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1", Hash: "h"}, []string{art.ID}, recs)
	details := evidence.ReindexStart{ReindexID: "rx-old", NormVersion: old, Tables: evidence.FTSTables()}.Details()
	if _, err := c.Audit.Append(evidence.ActionReindex, "", details); err != nil {
		t.Fatal(err)
	}
	done := evidence.ReindexDone{ReindexID: "rx-old", NormVersion: old, Docs: map[string]int64{}}.Details()
	if _, err := c.Audit.Append(evidence.ActionReindexDone, "", done); err != nil {
		t.Fatal(err)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.SetFTSNormVersion(t, dir, old)
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 0 {
		t.Fatalf("case verify exit %d, want 0:\n%s", code, out)
	}
	if n := strings.Count(out, "NOTICE:"); n != 1 || !strings.Contains(out, "records reindex") || !strings.Contains(out, "OK:") || strings.Contains(out, "PROBLEM:") {
		t.Errorf("want one NOTICE naming `records reindex`, OK and no PROBLEM:\n%s", out)
	}
}
