//go:build scale

package artparse_test

import "testing"

// The peak-RSS assertion has three outcomes and only one of them passes: within the limit passes, over
// the limit fails, and a platform that cannot measure skips (never a silent pass).
func TestRSSVerdict(t *testing.T) {
	const limit = 512 * scaleMiB
	for _, c := range []struct {
		name string
		r    phaseResult
		want rssOutcome
	}{
		{"recorded only", phaseResult{PeakRSSMeasured: true, PeakRSSBytes: 9 * limit}, rssPass},
		{"within", phaseResult{PeakRSSLimit: limit, PeakRSSMeasured: true, PeakRSSBytes: limit}, rssPass},
		{"over", phaseResult{PeakRSSLimit: limit, PeakRSSMeasured: true, PeakRSSBytes: limit + 1}, rssOver},
		{"not measured", phaseResult{PeakRSSLimit: limit}, rssNotMeasured},
		{"not measured, zero bytes", phaseResult{PeakRSSLimit: limit, PeakRSSBytes: 0}, rssNotMeasured},
	} {
		if got := rssVerdict(c.r); got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
	}
}
