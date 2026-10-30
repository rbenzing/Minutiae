//go:build scale && !windows && !linux

package artparse_test

// peakRSS is not read on this platform: the harness records peak memory as not measured.
func peakRSS() (int64, bool) { return 0, false }
