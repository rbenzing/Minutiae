package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestRecordsSearchCorruptIndexExits4 (R64): a damaged full-text structure is an integrity failure
// (exit 4) that names case verify, never a bare driver error with exit 1.
func TestRecordsSearchCorruptIndexExits4(t *testing.T) {
	dir := searchDataset(t)
	recordstest.WreckFTSStructure(t, dir, evidence.FTSWordTable)
	code, out := run(t, Deps{}, "records", "search", "--case", dir, "alpha")
	if code != ExitIntegrity || !strings.Contains(out, "case verify") {
		t.Errorf("exit %d, want %d naming case verify: %s", code, ExitIntegrity, out)
	}
}
