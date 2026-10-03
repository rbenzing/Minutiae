package fat

import (
	"errors"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// visited is a sparse bitmap of cluster numbers: memory grows with the chain
// being walked, never with the (possibly hostile) declared cluster count.
type visited map[uint32]uint64

// add marks c and reports false when it was already marked.
func (v visited) add(c uint32) bool {
	w, bit := c>>6, uint64(1)<<(c&63)
	if v[w]&bit != 0 {
		return false
	}
	v[w] |= bit
	return true
}

// validCluster reports whether c can hold data: 2 .. CountOfClusters+1.
func (f *FS) validCluster(c uint32) bool {
	return c >= 2 && uint64(c) <= uint64(f.count)+1
}

func (f *FS) isEOC(v uint32) bool {
	switch f.fatType {
	case 12:
		return v >= 0xFF8
	case 16:
		return v >= 0xFFF8
	}
	return v >= 0x0FFFFFF8
}

func (f *FS) isBad(v uint32) bool {
	switch f.fatType {
	case 12:
		return v == 0xFF7
	case 16:
		return v == 0xFFF7
	}
	return v == 0x0FFFFFF7
}

// clusterOffset returns the byte offset of cluster c, false when c is not a
// data cluster.
func (f *FS) clusterOffset(c uint32) (int64, bool) {
	if !f.validCluster(c) {
		return 0, false
	}
	sec, ok := filesys.MulOK(int64(c-2), int64(f.b.secPerClus))
	if !ok {
		return 0, false
	}
	if sec, ok = filesys.AddOK(sec, int64(f.b.dataStart)); !ok {
		return 0, false
	}
	return filesys.MulOK(sec, int64(f.b.bytsPerSec))
}

// entry returns the FAT entry of cluster c (0 .. CountOfClusters+1) from the
// active FAT copy. FAT32 entries are masked to their low 28 bits.
func (f *FS) entry(c uint32) (uint32, error) {
	const st = "FAT"
	if uint64(c) > uint64(f.count)+1 {
		return 0, corrupt(st, f.fatOff, "cluster %d is outside the %d clusters", c, f.count)
	}
	var off int64
	var n int
	switch f.fatType {
	case 12:
		off, n = f.fatOff+int64(c)+int64(c)/2, 2
	case 16:
		off, n = f.fatOff+int64(c)*2, 2
	default:
		off, n = f.fatOff+int64(c)*4, 4
	}
	var buf [4]byte
	if err := readFull(f.r, buf[:n], off); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return 0, corrupt(st, off, "entry %d lies beyond the end of the image", c)
		}
		return 0, err
	}
	switch f.fatType {
	case 12:
		v := uint32(buf[0]) | uint32(buf[1])<<8
		if c&1 == 1 {
			return v >> 4, nil
		}
		return v & 0xFFF, nil
	case 16:
		return uint32(buf[0]) | uint32(buf[1])<<8, nil
	}
	return (uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24) & 0x0FFFFFFF, nil
}

// chain returns the clusters of the chain that starts at first, in order. A
// first cluster of 0 is an empty file and gives an empty chain. A chain that
// leaves the volume, runs into a free or bad cluster, loops, or ends without an
// end-of-chain mark within the cluster count is a *filesys.CorruptError; the
// clusters collected before the problem are returned with it.
func (f *FS) chain(first uint32) ([]uint32, error) {
	clusters, truncated, err := f.chainN(first, int(f.count))
	if err == nil && truncated {
		err = corrupt("cluster chain", -1, "chain at cluster %d is longer than the %d clusters", first, f.count)
	}
	return clusters, err
}

// chainN follows a chain like chain but stops after limit clusters; truncated
// reports that it stopped there with the chain still going. A cluster joins the
// result only once its own FAT entry has been validated: a cluster whose entry
// is free or marked bad is not part of any chain, so the clusters returned with
// an error are exactly the ones that can be trusted. A cluster whose entry
// points outside the volume is returned (its data is valid; the pointer is not).
func (f *FS) chainN(first uint32, limit int) (clusters []uint32, truncated bool, err error) {
	const st = "cluster chain"
	if first == 0 {
		return nil, false, nil
	}
	if !f.validCluster(first) {
		return nil, false, corrupt(st, -1, "first cluster %d is outside the %d clusters", first, f.count)
	}
	seen := visited{}
	for c := first; ; {
		if limit <= len(clusters) {
			return clusters, true, nil
		}
		if !seen.add(c) {
			return clusters, false, corrupt(st, -1, "chain from cluster %d loops back to cluster %d", first, c)
		}
		next, err := f.entry(c)
		if err != nil {
			return clusters, false, err
		}
		switch {
		case next == 0:
			return clusters, false, corrupt(st, -1, "chain from cluster %d reaches cluster %d, which is free", first, c)
		case f.isBad(next):
			return clusters, false, corrupt(st, -1, "chain from cluster %d reaches cluster %d, which is marked bad", first, c)
		}
		clusters = append(clusters, c)
		switch {
		case f.isEOC(next):
			return clusters, false, nil
		case !f.validCluster(next):
			return clusters, false, corrupt(st, -1, "chain from cluster %d points from cluster %d to %d, outside the %d clusters", first, c, next, f.count)
		}
		c = next
	}
}
