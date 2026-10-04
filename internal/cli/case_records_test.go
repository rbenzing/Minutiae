package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestCaseVerifySummaryNamesRecordsOnlyWhenThereAreSome(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// no records yet: the line is what it always was
	code, out := run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 0 || !strings.Contains(out, "OK: 1 artifacts, ") || strings.Contains(out, "records") {
		t.Fatalf("verify: %d %s", code, out)
	}

	c = reopen(t, dir)
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 12, 3))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out = run(t, Deps{}, "case", "verify", "--case", dir)
	if code != 0 || !strings.Contains(out, "OK: 1 artifacts, 12 records, ") {
		t.Fatalf("verify: %d %s", code, out)
	}
	code, out = run(t, Deps{}, "case", "verify", "--case", dir, "--json")
	if code != 0 || !strings.Contains(out, `"records_checked": 12`) || !strings.Contains(out, `"record_batches_checked": 1`) {
		t.Fatalf("verify --json: %d %s", code, out)
	}

	recordstest.SetRecordSummary(t, dir, 5, "tampered")
	code, out = run(t, Deps{}, "case", "verify", "--case", dir)
	if code != ExitIntegrity || !strings.Contains(out, "digest mismatch") || !strings.Contains(out, "FAILED: 1 artifacts, 12 records, ") {
		t.Fatalf("verify after tamper: %d %s", code, out)
	}
}

func reopen(t *testing.T, dir string) *evidence.Case {
	t.Helper()
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
