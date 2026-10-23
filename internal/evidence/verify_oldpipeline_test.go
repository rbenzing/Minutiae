package evidence_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestVerifyRealIngestUnderPipeline3IsOnlyANotice (M22, M28): a case created and ingested by a real
// writer under pipeline version 3 (the audit log, records_meta and the index all say fts3) is, for
// the current build, exactly one IndexStale notice and no problem.
func TestVerifyRealIngestUnderPipeline3IsOnlyANotice(t *testing.T) {
	old := strings.Replace(evidence.FTSNormVersion(), "fts4/", "fts3/", 1)
	if old == evidence.FTSNormVersion() {
		t.Fatalf("FTSNormVersion() = %q is not a pipeline 4 version", old)
	}
	restore := evidence.SetFTSNormVersionOverride(old)
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 1)
	recs := []records.Record{
		{Type: "note", ArtifactID: ids[0], Summary: "hello world", Payload: map[string]any{"i": 1}},
		{Type: "note", ArtifactID: ids[0], Summary: "second note", Payload: map[string]any{"i": 2}},
	}
	recordstest.Ingest(t, c, records.Parser{Name: "old", Version: "1"}, ids, recs)
	restore()
	if evidence.FTSNormVersion() == old {
		t.Fatal("the override was not restored")
	}
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
	if n != 1 || len(rep.Notices) != 1 {
		t.Errorf("notices %q, want exactly one naming the remedy", rep.Notices)
	}
}
