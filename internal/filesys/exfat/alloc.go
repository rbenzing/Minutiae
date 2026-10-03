package exfat

import (
	"encoding/binary"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// FAT entry values.
const (
	fatBadCluster = 0xFFFFFFF7
	fatEOCMin     = 0xFFFFFFF8 // 0xFFFFFFF8-0xFFFFFFFF end a chain
)

// extent is a run of n physically consecutive clusters starting at first.
type extent struct {
	first uint32
	n     uint64
}

// validCluster reports whether c is a cluster of the heap (2 .. count+1).
func (f *FS) validCluster(c uint32) bool {
	return c >= fatReservedClusters && c-fatReservedClusters < f.clusterCount
}

// clusterOff returns the byte offset of cluster c, which must be valid. It
// cannot overflow: clusterCount x cluster size fits the volume.
func (f *FS) clusterOff(c uint32) int64 {
	return f.heapOff + int64(c-fatReservedClusters)*f.cs
}

// fatEntry reads the FAT entry of cluster c.
func (f *FS) fatEntry(c uint32) (uint32, error) {
	if c >= f.fatEntries {
		return 0, corrupt("exFAT FAT", -1, "cluster %d has no FAT entry (the FAT holds %d)", c, f.fatEntries)
	}
	var b [4]byte
	off := f.fatOff + 4*int64(c)
	if err := readFull(f.r, b[:], off); err != nil {
		return 0, corrupt("exFAT FAT", off, "entry of cluster %d is unreadable: %v", c, err)
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

// seenSet remembers visited clusters: a short list first, a bitmap of the
// cluster space once a chain grows past it.
type seenSet struct {
	small [64]uint32
	n     int
	bits  []uint64
	space uint32 // number of cluster numbers (count + 2)
}

// add records c and reports whether it was new.
func (s *seenSet) add(c uint32) bool {
	if s.bits == nil {
		for _, v := range s.small[:s.n] {
			if v == c {
				return false
			}
		}
		if s.n < len(s.small) {
			s.small[s.n] = c
			s.n++
			return true
		}
		s.bits = make([]uint64, (uint64(s.space)+63)/64)
		for _, v := range s.small[:s.n] {
			s.bits[v/64] |= 1 << (v % 64)
		}
	}
	w, m := &s.bits[c/64], uint64(1)<<(c%64)
	if *w&m != 0 {
		return false
	}
	*w |= m
	return true
}

// chain returns the clusters of the chain that starts at first as extents.
//
// With noFat the clusters are consecutive (the NoFatChain flag) and want of
// them are taken; the FAT is not consulted. Otherwise the FAT is followed: a
// free, reserved, bad, out-of-range or looping entry is an error, and so is
// an end of chain before want clusters when exact is set. Without exact the
// walk ends at the end of the chain or after want clusters, whichever comes
// first. Walking stops as soon as want clusters are collected, without
// looking at the entry after the last one.
//
// On an error the extents collected before the damage are returned too, and
// the clusters walked never exceed the cluster count: a visited set catches a
// loop at the first revisit.
func (f *FS) chain(first uint32, want uint64, exact, noFat bool) ([]extent, error) {
	if want == 0 {
		return nil, nil
	}
	if !f.validCluster(first) {
		return nil, corrupt("exFAT cluster chain", -1, "first cluster %d is not a cluster of the volume (2-%d)", first, uint64(f.clusterCount)+1)
	}
	if noFat {
		avail := uint64(f.clusterCount) + fatReservedClusters - uint64(first)
		if want > avail {
			return []extent{{first, avail}}, corrupt("exFAT cluster chain", -1, "%d contiguous clusters from cluster %d run past the end of the volume (%d available)", want, first, avail)
		}
		return []extent{{first, want}}, nil
	}
	var (
		exts []extent
		got  uint64
		seen = seenSet{space: f.clusterCount + fatReservedClusters}
	)
	for c := first; ; {
		if !seen.add(c) {
			return exts, corrupt("exFAT cluster chain", -1, "the chain from cluster %d loops back to cluster %d (cycle)", first, c)
		}
		if n := len(exts); n > 0 && uint64(exts[n-1].first)+exts[n-1].n == uint64(c) {
			exts[n-1].n++
		} else {
			exts = append(exts, extent{c, 1})
		}
		got++
		if got == want {
			return exts, nil
		}
		next, err := f.fatEntry(c)
		if err != nil {
			return exts, err
		}
		switch {
		case next >= fatEOCMin:
			if exact {
				return exts, corrupt("exFAT cluster chain", -1, "the chain from cluster %d ends after %d clusters, %d are needed", first, got, want)
			}
			return exts, nil
		case next == fatBadCluster:
			return exts, corrupt("exFAT cluster chain", -1, "the chain from cluster %d reaches cluster %d, which is marked bad", first, c)
		case !f.validCluster(next):
			return exts, corrupt("exFAT cluster chain", -1, "the chain from cluster %d has entry %#x after cluster %d (free, reserved or out of range)", first, next, c)
		}
		c = next
	}
}

// metaExtents returns the clusters of a system file (bitmap, up-case table)
// of n clusters starting at first. They are normally chained in the FAT; some
// writers leave the FAT entries of system files empty, so a contiguous run is
// the fallback.
func (f *FS) metaExtents(first uint32, n uint64) ([]extent, error) {
	exts, err := f.chain(first, n, true, false)
	if err == nil {
		return exts, nil
	}
	return f.chain(first, n, true, true)
}

// readExtents fills dst from the clusters in exts, which hold at least
// len(dst) bytes.
func (f *FS) readExtents(exts []extent, dst []byte) error {
	var done int
	for _, e := range exts {
		for c := uint64(0); c < e.n && done < len(dst); c++ {
			n := min(int(f.cs), len(dst)-done)
			if err := readFull(f.r, dst[done:done+n], f.clusterOff(e.first+uint32(c))); err != nil {
				return err
			}
			done += n
		}
	}
	if done < len(dst) {
		return fmt.Errorf("only %d of %d bytes are mapped", done, len(dst))
	}
	return nil
}

// bitmapChunk is how much of the allocation bitmap is read at a time.
const bitmapChunk = 1 << 20

// Unallocated returns the byte runs of the clusters the allocation bitmap
// marks free (a clear bit), sorted and merged. The bitmap is read from the
// root directory's Allocation Bitmap entry; if that is missing, unreadable or
// shorter than the cluster count needs, the problem is reported through
// Info().Warnings and the clusters it does not cover are never reported as
// free. Bits past the cluster count are ignored.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	if !f.bitmap.present {
		f.warn("allocation bitmap: the root directory has no Allocation Bitmap entry; no free space is reported")
		return nil, nil
	}
	need := (uint64(f.clusterCount) + 7) / 8
	have := min(f.bitmap.length, need)
	if f.bitmap.length < need {
		f.warn("allocation bitmap: %d bytes cover only %d of %d clusters; the clusters beyond are not reported as free", f.bitmap.length, f.bitmap.length*8, f.clusterCount)
	}
	if have == 0 {
		f.warn("allocation bitmap: the Allocation Bitmap entry has length 0; no free space is reported")
		return nil, nil
	}
	nclusters := (have + uint64(f.cs) - 1) / uint64(f.cs) // <= bitmap size / cluster size + 1
	exts, err := f.metaExtents(f.bitmap.first, nclusters)
	if err != nil {
		f.warn("allocation bitmap: cannot be read (first cluster %d): %v; no free space is reported", f.bitmap.first, err)
		return nil, nil
	}

	var (
		runs      []filesys.Run
		runStart  = int64(-1) // cluster index of the open free run
		buf       = make([]byte, min(have, bitmapChunk))
		index     int64 // cluster index of the next bit
		remaining = have
	)
	flush := func(end int64) {
		if runStart >= 0 {
			runs = append(runs, filesys.Run{Offset: f.heapOff + runStart*f.cs, Length: (end - runStart) * f.cs})
			runStart = -1
		}
	}
read:
	for _, e := range exts {
		base, total := f.clusterOff(e.first), e.n*uint64(f.cs) // the clusters of an extent are consecutive
		for off := uint64(0); off < total && remaining > 0; {
			n := min(remaining, total-off, uint64(len(buf)))
			if err := readFull(f.r, buf[:n], base+int64(off)); err != nil {
				f.warn("allocation bitmap: unreadable at byte %d: %v; the clusters from there on are not reported as free", have-remaining, err)
				break read
			}
			for _, b := range buf[:n] {
				for bit := 0; bit < 8 && index < int64(f.clusterCount); bit++ {
					if b&(1<<bit) != 0 {
						flush(index)
					} else if runStart < 0 {
						runStart = index
					}
					index++
				}
			}
			remaining -= n
			off += n
		}
	}
	flush(index)
	return filesys.MergeRuns(runs), nil
}
