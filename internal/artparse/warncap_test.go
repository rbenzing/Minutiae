package artparse_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// parse.MaxWarnings is what the host's contract check allows a job; the records writer applies the same
// number per ingest. If they drift, a parser could be in contract yet suppressed (or the reverse).
func TestParseMaxWarningsEqualsWriterCap(t *testing.T) {
	if parse.MaxWarnings != records.MaxWarningsPerIngest {
		t.Fatalf("parse.MaxWarnings = %d, records.MaxWarningsPerIngest = %d", parse.MaxWarnings, records.MaxWarningsPerIngest)
	}
}
