package artparse_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Ruling T54: parsertest keeps a copy of artparse's acquisition rule (it cannot import artparse). The copy must give
// exactly what the host gives, so the Acquisition a parser test sees is the one production sees.
func TestHarnessAcquisitionFollowsTheHostRule(t *testing.T) {
	h := parsertest.NewHarness(t)
	for _, p := range []string{
		"", "x", "artifacts", "artifacts/x", "artifacts/D1/A1", "artifacts/D1/A1/", // short
		"artifacts//A1/f", "artifacts/D1//f", "artifacts/D1/A1//f", // empty segments
		"other/D1/A1/f", "/artifacts/D1/A1/f", // wrong root
		"artifacts/D1/A1/deep/er/f", // deeper
		"artifacts/D1/A1/f",         // valid
	} {
		want, _ := artparse.AcquisitionKey(p)
		got := h.Artifact(evidence.ManifestRecord{Path: p}, nil).Acquisition
		if got != want {
			t.Errorf("Harness.Artifact(%q).Acquisition = %q, host AcquisitionKey = %q", p, got, want)
		}
	}
}
