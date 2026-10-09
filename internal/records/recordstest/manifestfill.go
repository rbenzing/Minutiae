package recordstest

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// AppendFillerManifest appends n artifact records (ids "filler-NNNNNN", paths that do not exist) to the
// manifest in one write, so a benchmark can measure a manifest of any length. The case no longer
// verifies; use it only for measurements.
func AppendFillerManifest(t testing.TB, caseDir string, n int) {
	t.Helper()
	recs := readManifestLines(t, caseDir)
	for i := 0; i < n; i++ {
		recs = append(recs, evidence.ManifestRecord{
			ID: fmt.Sprintf("filler-%06d", i), Path: fmt.Sprintf("artifacts/filler/%06d.bin", i), Size: 1,
			SHA256: "00", MD5: "00", Source: evidence.Source{Kind: "file", DeviceID: "dev1"},
		})
	}
	writeManifestLines(t, caseDir, recs)
}
