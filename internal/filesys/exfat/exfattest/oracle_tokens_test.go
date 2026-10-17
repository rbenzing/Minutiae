package exfattest_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// F110: every oracle token maps to the reader token it requires, and a token outside the table is reported.
func TestReaderAssumptionForOracleTokens(t *testing.T) {
	for oracle, reader := range map[string]string{
		"no-fat-chain":          "contiguity-from-no-fat-chain",
		"fat-chain-kept":        "fat-chain-unverified-against-reuse",
		"contiguous-allocation": "contiguous-allocation",
	} {
		if got, ok := exfattest.ReaderAssumptionFor(oracle); !ok || got != reader {
			t.Errorf("%s -> %q, %v; want %q", oracle, got, ok, reader)
		}
	}
	if got, ok := exfattest.ReaderAssumptionFor("a-new-oracle-token"); ok {
		t.Errorf("an unmapped oracle token mapped to %q", got)
	}
}
