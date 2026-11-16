package f2fs

// Test-only access to the roll-forward chain (3H Task 4).

// SetChainCap lowers the number of chain nodes the walk may take.
func (f *FS) SetChainCap(n int) { f.chainCap = n }

// ChainStats counts what the chain walk did.
type ChainStats struct {
	Builds, Nodes int
	Stop          string
	Complete      bool
	BlocksRead    int
}

// ChainStats builds the chain (once) and reports its counters.
func (f *FS) ChainStats() (ChainStats, error) {
	c, err := f.chain()
	if err != nil {
		return ChainStats{}, err
	}
	f.rf.mu.Lock()
	defer f.rf.mu.Unlock()
	return ChainStats{Builds: f.rf.builds, Nodes: len(c.entries), Stop: c.sum.Stop, Complete: c.sum.Complete, BlocksRead: c.blocksRead}, nil
}

// ChainWouldReplay returns wouldReplay of every chain node, in order.
func (f *FS) ChainWouldReplay() ([]bool, error) {
	c, err := f.chain()
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(c.entries))
	for i := range out {
		out[i] = c.wouldReplay(i)
	}
	return out, nil
}

// ChainSummary returns the chain summary (without Files).
func (f *FS) ChainSummary() (RFChainSummary, error) {
	c, err := f.chain()
	if err != nil {
		return RFChainSummary{}, err
	}
	return RFChainSummary{c.sum.FooterVersion, c.sum.Start, c.sum.Warnings, c.sum.CRCRecovery, c.sum.NoCRCRecovery, c.sum.Unmount}, nil
}

// RFChainSummary is the part of the summary the chain walk fills.
type RFChainSummary struct {
	FooterVersion       uint64
	Start               uint64
	Warnings            []string
	CRC, NoCRC, Unmount bool
}

// MaxChainNodes is the bound on the chain (3H-R14).
const MaxChainNodes = maxChainNodes
