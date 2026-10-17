package exfattest

// oracleAssumptions maps each assumption token the fixture oracle (tools/fixtures/exfat_recover_oracle.py) writes
// to the reader token it requires. The oracle is independent of the reader and words things its own way, so the
// pairing is a fixed table: a test that meets an oracle token this table does not map must fail, never skip.
var oracleAssumptions = map[string]string{
	"no-fat-chain":          "contiguity-from-no-fat-chain",
	"fat-chain-kept":        "fat-chain-unverified-against-reuse",
	"contiguous-allocation": "contiguous-allocation",
}

// ReaderAssumptionFor returns the reader token that an oracle assumption token requires, and false for a token
// the table does not map.
func ReaderAssumptionFor(oracleToken string) (string, bool) {
	r, ok := oracleAssumptions[oracleToken]
	return r, ok
}
