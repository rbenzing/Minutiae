package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// gapCase is a case whose only ingest never concluded and whose batch 2 was audited but has no rows
// (what a power loss leaves after a relaxed-sync ingest).
func gapCase(t *testing.T) (dir, ingestID string) {
	t.Helper()
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	w, err := records.NewWriter(c, recParser, records.WriterOptions{BatchRows: 2, RelaxedSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(t.Context(), records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recordstest.Records(art.ID, 4, 1) {
		if err := w.Add(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	id := w.IngestID()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.DeleteBatch(t, c.Dir, id, 2)
	return c.Dir, id
}

// TestRecordsStatsAndShowNameTheAuditedButAbsentBatches (D90 c): the run is never shown as complete.
func TestRecordsStatsAndShowNameTheAuditedButAbsentBatches(t *testing.T) {
	dir, id := gapCase(t)
	code, out := run(t, Deps{}, "records", "stats", "--case", dir)
	if code != 0 {
		t.Fatalf("stats: %d %s", code, out)
	}
	for _, want := range []string{"INCOMPLETE", id, "never concluded", "batches audited but absent from the database: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}
	var st struct {
		Overview struct {
			Unfinished []struct {
				IngestID      string `json:"ingest_id"`
				Recovered     bool   `json:"recovered"`
				AbsentBatches []int  `json:"absent_batches"`
			} `json:"unfinished"`
		} `json:"overview"`
	}
	cliJSON(t, &st, "records", "stats", "--case", dir)
	u := st.Overview.Unfinished
	if len(u) != 1 || u[0].IngestID != id || u[0].Recovered || len(u[0].AbsentBatches) != 1 || u[0].AbsentBatches[0] != 2 {
		t.Fatalf("stats json unfinished = %+v", u)
	}
	code, out = run(t, Deps{}, "records", "show", "--case", dir, "1")
	if code != 0 {
		t.Fatalf("show: %d %s", code, out)
	}
	for _, want := range []string{"run", "INCOMPLETE", "absent from the database: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	var sh struct {
		Gap *struct {
			AbsentBatches []int `json:"absent_batches"`
		} `json:"incomplete_ingest"`
	}
	cliJSON(t, &sh, "records", "show", "--case", dir, "1")
	if sh.Gap == nil || len(sh.Gap.AbsentBatches) != 1 || sh.Gap.AbsentBatches[0] != 2 {
		t.Fatalf("show json incomplete_ingest = %+v", sh.Gap)
	}
}

// TestCaseVerifyOfALostRelaxedIngestFailsAndNamesTheBatch: verify never passes the case (a case with a problem exits 4, as for any
// problem) and its report names the interrupted ingest and the audited-but-absent batch. The tampered rows here also leave index
// leftovers, which verify reports too: the interrupted ingest is among the problems, never hidden by them.
func TestCaseVerifyOfALostRelaxedIngestFailsAndNamesTheBatch(t *testing.T) {
	dir, id := gapCase(t)
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code == 0 {
		t.Fatalf("verify passed:\n%s", out)
	}
	for _, want := range []string{id, "interrupted", "batch 2 of ingest"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output lacks %q:\n%s", want, out)
		}
	}
}
