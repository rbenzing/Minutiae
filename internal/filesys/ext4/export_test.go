package ext4

// Test-only accessors, so the external ext4_test package can check internals
// without widening the public API.

// GroupCount returns the number of block groups read from the descriptor table.
func (f *FS) GroupCount() int { return len(f.groups) }

// Group returns the locations, flags and validity of one group descriptor.
func (f *FS) Group(i int) (blockBitmap, inodeBitmap, inodeTable uint64, flags uint16, bad bool) {
	g := f.groups[i]
	return g.blockBitmap, g.inodeBitmap, g.inodeTable, g.flags, g.bad
}

// CRC16 exposes the group-descriptor crc16.
var CRC16 = crc16

// RawCRC32C exposes the kernel-style crc32c (no final inversion).
var RawCRC32C = rawCRC32C
