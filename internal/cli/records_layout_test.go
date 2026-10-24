package cli

import (
	"encoding/json"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestNewerRecordsLayoutExitsIntegrity: a records.* entry a later build wrote (layout 2) makes `case verify`
// and `records show` exit 4, never 0 or 1 (K10, K30).
func TestNewerRecordsLayoutExitsIntegrity(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{art.ID}, recordstest.Records(art.ID, 2, 1))
	d := evidence.BatchCommit{IngestID: "ing-future", BatchNo: 1, FirstID: 1000, Count: 1, Artifacts: map[string]string{}, ArtifactIncomplete: []string{}, Types: map[string]int64{}}.Details()
	d["layout"] = json.Number("2")
	if _, err := c.Audit.Append(evidence.ActionBatch, "", d); err != nil {
		t.Fatal(err)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"case", "verify", "--case", dir},
		{"records", "show", "--case", dir, "1"},
	} {
		if code, out := run(t, Deps{}, args...); code != ExitIntegrity {
			t.Errorf("%v exits %d, want %d:\n%s", args, code, ExitIntegrity, out)
		}
	}
}
