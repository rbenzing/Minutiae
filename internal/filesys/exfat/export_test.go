package exfat

// Test-only accessors, so the external exfat_test package can check internals
// without widening the public API.

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// SetDirRecordCap lowers the per-directory entry-set cap (before any read).
func (f *FS) SetDirRecordCap(n int) { f.dirRecordCap = n }

// ClusterCount returns the cluster count the reader uses (after clamping).
func (f *FS) ClusterCount() uint32 { return f.clusterCount }

// UpcaseLoaded reports whether an up-case table is in use for Lookup.
func (f *FS) UpcaseLoaded() bool { return f.upcase != nil }

// Upper returns the up-case table's image of u (u itself without a table).
func (f *FS) Upper(u uint16) uint16 { return f.upper(u) }

// TableChecksum exposes the up-case table checksum.
var TableChecksum = tableChecksum

// SetChecksum exposes the entry-set checksum over a whole set.
var SetChecksum = setChecksum

// BootChecksum exposes the boot region checksum.
var BootChecksum = bootChecksum

// SetBitmapChunk sets how many bitmap bytes Unallocated reads at a time.
func (f *FS) SetBitmapChunk(n int) { f.bitmapChunk = max(n, 1) }

// SetDirBudget sets the bytes of directories the instance may still read.
func (f *FS) SetDirBudget(n int64) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	f.dirBudget, f.dirBudgetTotal = n, n
}

// MaxDirRecords is the default per-directory entry-set cap.
const MaxDirRecords = maxDirRecords

// DirRecordCap returns the per-directory entry-set cap in force.
func (f *FS) DirRecordCap() int { return f.dirRecordCap }

// SetUnallocatedRunCap lowers the number of runs Unallocated reports.
func (f *FS) SetUnallocatedRunCap(n int) { f.unallocCap = n }
