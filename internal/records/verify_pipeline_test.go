package records_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestVerifyOldPipelineIndexIsOnlyANotice (M22, D8): a case whose audit log (a reindex.done) and
// index state both name a pipeline 3 build is not a problem: verify reports exactly one notice that
// names the remedy, and no problem.
func TestVerifyOldPipelineIndexIsOnlyANotice(t *testing.T) {
	old := strings.Replace(evidence.FTSNormVersion(), "fts4/", "fts3/", 1)
	if old == evidence.FTSNormVersion() {
		t.Fatalf("FTSNormVersion() = %q is not a pipeline 4 version", old)
	}
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs[:4])
	forgeReindexDone(t, c, "rx-old", old)
	recordstest.SetFTSNormVersion(t, c.Dir, old)
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || len(rep.Problems) != 0 {
		t.Fatalf("verify is not OK: %q", rep.Problems)
	}
	n := 0
	for _, s := range rep.Notices {
		if strings.Contains(s, "was built by") && strings.Contains(s, "records reindex") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d matching notices in %q, want exactly 1", n, rep.Notices)
	}
}
