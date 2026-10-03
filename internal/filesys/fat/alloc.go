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
//
// Free space is read from the active FAT copy only. With two or more copies
// the scan also compares the others, chunk by chunk, with the active one and
// warns once if they differ (a stale or tampered copy; the active one is
// trusted).
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
	var other []byte // the same bytes of another FAT copy, when there is one to compare
	if f.b.numFATs > 1 {
		other = make([]byte, fatScanChunk)
	}
	copiesDiffer := false
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
		if !copiesDiffer {
			copiesDiffer = f.compareFATCopies(buf[:nbytes], other[:min(len(other), int(nbytes))], int64(byteOff))
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

// compareFATCopies compares the bytes active, read from the active FAT copy at
// byte offset off of the FAT, with the same bytes of every other copy (scratch
// is the buffer to read them into; nil when there is a single copy). It warns
// about the first copy that differs, or cannot be read, and reports whether it
// did.
func (f *FS) compareFATCopies(active, scratch []byte, off int64) bool {
	if len(scratch) < len(active) {
		return false
	}
	bps := int64(f.b.bytsPerSec)
	for n := range f.b.numFATs {
		if n == f.activeFAT {
			continue
		}
		base := (int64(f.b.rsvd) + int64(n)*int64(f.b.fatSz)) * bps // inside the volume: checked by parseBoot
		got := scratch[:len(active)]
		if err := readFull(f.data, got, base+off); err != nil {
			f.warn("FAT copy %d cannot be compared with the active FAT copy %d: %v", n, f.activeFAT, err)
			return true
		}
		for i := range active {
			if got[i] != active[i] {
				f.warn("FAT copy %d differs from the active FAT copy %d (first difference at byte %d of the FAT); free space is taken from copy %d", n, f.activeFAT, off+int64(i), f.activeFAT)
				return true
			}
		}
	}
	return false
}
