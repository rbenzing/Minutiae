package artparse_test

import (
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
)

// A re-hash that runs out of time proves nothing about the evidence: the job is an error with the
// reason recheck-timeout (exit 1), never complete and never an integrity verdict.
func TestRecheckTimeoutIsAnErrorNotComplete(t *testing.T) {
	defer artparse.SetRecheckTimeout(-time.Second)()
	f := newRx(t, "slow")
	sum := f.run(f.host(nil, wbp("slow")), artparse.Selection{})
	j := jobOf(t, sum, "slow")
	if j.Outcome != artparse.OutcomeIncomplete || j.Reason != "recheck-timeout" || j.Integrity {
		t.Fatalf("job %+v", j)
	}
	if sum.Class() == artparse.ClassIntegrity {
		t.Error("a timeout was classed as integrity")
	}
}
