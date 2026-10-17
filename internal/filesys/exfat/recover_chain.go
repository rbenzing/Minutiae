package exfat

// staleStop says why walkStaleChain stopped.
type staleStop int

const (
	stopComplete  staleStop = iota // need clusters collected
	stopFirstFree                  // FAT[first] == 0: no chain survived
	stopEnd                        // end-of-chain mark before need clusters
	stopFreeEntry                  // FAT[c] == 0 for a cluster after the first: c is not part of the chain
	stopBad                        // FAT[c] is the bad-cluster mark: c is not part of the chain
	stopRange                      // FAT[c] points outside the heap (or to a reserved value); c itself is data
	stopLoop                       // FAT[c] points to an already walked cluster; c itself is data
)

// String is the token used in the stale-chain-stop assumption.
func (s staleStop) String() string {
	switch s {
	case stopEnd:
		return "end"
	case stopFreeEntry:
		return "free-entry"
	case stopBad:
		return "bad"
	case stopRange:
		return "range"
	case stopLoop:
		return "loop"
	case stopFirstFree:
		return "first-free"
	}
	return "complete"
}

// walkStaleChain follows the FAT from first for at most need links (need =
// ceil(ValidDataLength / cluster size), computed by the caller after the bounds
// checks; never from DataLength and never allocated from a declared size). A
// cluster belongs to the chain when its own FAT entry is a plausible link; the
// entry of the last needed cluster is never read. exts are the extents of the
// clusters that belong to the chain, in order; at is the cluster the stop
// concerns. err is an I/O or structure error of the FAT read only.
func (f *FS) walkStaleChain(first uint32, need uint64) (exts []extent, stop staleStop, at uint32, err error) {
	if !f.validCluster(first) {
		return nil, stopRange, first, nil
	}
	seen := seenSet{space: f.clusterCount + fatReservedClusters}
	seen.add(first)
	add := func(c uint32) {
		if n := len(exts); n > 0 && uint64(exts[n-1].first)+exts[n-1].n == uint64(c) {
			exts[n-1].n++
			return
		}
		exts = append(exts, extent{first: c, n: 1})
	}
	c := first
	for k := uint64(1); ; k++ {
		if k >= need { // the last needed cluster: its entry is not read
			add(c)
			return exts, stopComplete, c, nil
		}
		next, err := f.fatEntry(c)
		if err != nil {
			return exts, stopComplete, c, err
		}
		switch {
		case next == 0 && k == 1:
			return nil, stopFirstFree, c, nil
		case next == 0:
			return exts, stopFreeEntry, c, nil
		case next == fatBadCluster:
			return exts, stopBad, c, nil
		}
		add(c)
		switch {
		case next >= fatEOCMin:
			return exts, stopEnd, c, nil
		case !f.validCluster(next):
			return exts, stopRange, c, nil
		case !seen.add(next):
			return exts, stopLoop, c, nil
		}
		c = next
	}
}
