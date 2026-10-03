package fat

import "github.com/rbenzing/minutiae/internal/filesys"

const (
	fatScanChunk   = 48 << 10 // bytes of FAT read at a time: a multiple of 2, 3 and 4
	maxUnallocRuns = 1 << 24  // runs reported; more are cut off with a warning
)

// Unallocated returns the byte runs of the clusters whose FAT entry is 0 (free),
// sorted and merged. Bad and reserved clusters are not free. Only clusters the
// image holds are considered (the cluster count is clamped to the image and to
// what the FAT describes). Deleted files' clusters are free space, as the OS
// released their chains.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	cs := int64(f.clusterSize())
	total := uint64(f.count) + 2 // FAT entries 0 .. count+1
	var perChunk uint64
	switch f.fatType {
	case 12:
		perChunk = fatScanChunk / 3 * 2
	case 16:
		perChunk = fatScanChunk / 2
	default:
		perChunk = fatScanChunk / 4
	}
	buf := make([]byte, fatScanChunk)
	var runs []filesys.Run
	for start := uint64(0); start < total; start += perChunk {
		n := min(perChunk, total-start) // entries in this chunk; start is even (FAT12 pairs share 3 bytes)
		var byteOff, nbytes uint64
		switch f.fatType {
		case 12:
			byteOff, nbytes = start/2*3, (n*3+1)/2
		case 16:
			byteOff, nbytes = start*2, n*2
		default:
			byteOff, nbytes = start*4, n*4
		}
		if err := readFull(f.data, buf[:nbytes], f.fatOff+int64(byteOff)); err != nil {
			return nil, corrupt("FAT", f.fatOff+int64(byteOff), "read failed: %v", err)
		}
		for i := uint64(0); i < n; i++ {
			c := start + i
			if c < 2 {
				continue
			}
			var v uint32
			switch f.fatType {
			case 12:
				o := i + i/2
				v = uint32(buf[o]) | uint32(buf[o+1])<<8
				if i&1 == 1 {
					v >>= 4
				} else {
					v &= 0xFFF
				}
			case 16:
				v = uint32(buf[2*i]) | uint32(buf[2*i+1])<<8
			default:
				v = (uint32(buf[4*i]) | uint32(buf[4*i+1])<<8 | uint32(buf[4*i+2])<<16 | uint32(buf[4*i+3])<<24) & 0x0FFFFFFF
			}
			if v != 0 {
				continue
			}
			off, ok := f.clusterOffset(uint32(c))
			if !ok {
				continue
			}
			if k := len(runs); k > 0 && runs[k-1].Offset+runs[k-1].Length == off {
				runs[k-1].Length += cs
				continue
			}
			if len(runs) >= maxUnallocRuns {
				f.warn("unallocated space: more than %d free runs, the rest is not reported", maxUnallocRuns)
				return runs, nil
			}
			runs = append(runs, filesys.Run{Offset: off, Length: cs})
		}
	}
	return runs, nil
}
