package fat

// Test-only accessors, so the external fat_test package can check internals
// without widening the public API.

// ClusterCount returns CountOfClusters after clamping.
func (f *FS) ClusterCount() uint32 { return f.count }

// FATType returns 12, 16 or 32.
func (f *FS) FATType() int { return f.fatType }

// Chain follows the cluster chain starting at first.
func (f *FS) Chain(first uint32) ([]uint32, error) { return f.chain(first) }

// ChainN follows at most limit clusters and reports whether it stopped early.
func (f *FS) ChainN(first uint32, limit int) ([]uint32, bool, error) {
	return f.chainN(first, limit)
}

// Entry returns the raw (masked) FAT entry of cluster c.
func (f *FS) Entry(c uint32) (uint32, error) { return f.entry(c) }

// ClusterOffset returns the byte offset of cluster c.
func (f *FS) ClusterOffset(c uint32) (int64, bool) { return f.clusterOffset(c) }

// ActiveFAT returns the index of the FAT copy that is read.
func (f *FS) ActiveFAT() int { return f.activeFAT }

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// TypeByCount exposes the cluster-count rule that decides the FAT type.
var TypeByCount = typeByCount

// SetDirBudget sets the number of directory entries the instance may still read.
func (f *FS) SetDirBudget(entries int64) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	f.entryBudget, f.budgetWarned = entries, false
}
