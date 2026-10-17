package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// An FTS5 shadow row no structure record references makes `case verify` exit 4 and name the table and row.
func TestCaseVerifyUnreferencedFTSRowExits4(t *testing.T) {
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 7, Artifacts: ids})
	recordstest.Ingest(t, c, records.Parser{Name: "fts", Version: "1"}, ids, gen.Block(0, 50))
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
		t.Fatalf("clean case: exit %d:\n%s", code, out)
	}
	recordstest.InsertFTSShadowRow(t, dir, "records_fts_data", int64(900)<<37|1, []byte("x"))
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 4 || !strings.Contains(out, "records_fts: data row") || !strings.Contains(out, "(segment 900) is not referenced by the index structure") {
		t.Fatalf("exit %d, want 4 naming the table and the row:\n%s", code, out)
	}
}
