//go:build sqlitematrix

package sqlitefile_test

import "testing"

// TestLiveMatchesEngineFullMatrix: page size {512, 1024, 4096, 16384, 65536} x
// three encodings x three auto_vacuum modes = 45 scenarios; run manually with
// -tags sqlitematrix.
func TestLiveMatchesEngineFullMatrix(t *testing.T) {
	for _, s := range fullLiveMatrix() {
		t.Run(s.String(), func(t *testing.T) { runLiveScenario(t, s) })
	}
}

// fullLiveMatrix: page size {512, 1024, 4096, 16384, 65536} x three encodings x
// three auto_vacuum modes = 45 scenarios.
func fullLiveMatrix() []liveScenario {
	var out []liveScenario
	for _, ps := range []int{512, 1024, 4096, 16384, 65536} {
		for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
			for _, av := range []int{0, 1, 2} {
				out = append(out, liveScenario{ps, enc, av})
			}
		}
	}
	return out
}
