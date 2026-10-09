package fat

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// MethodContiguous names the only recovery method FAT has: the vfat driver
// zeroes the whole FAT chain of a deleted file (probe, Task 1), so the first
// cluster and size of the directory entry are all that survive and the content
// is ASSUMED to have been contiguous.
const MethodContiguous = "fat-contiguous"

// Recoverable implements filesys.Recoverer. The map is metadata only: the first
// cluster and the size of the deleted short entry, never the FAT. Whether the
// clusters are free now is the caller's check, not the reader's.
func (f *FS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	if n, ok := parseDirID(e.ID); ok {
		if err := f.resolveDir(n, ""); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %q is a live directory", filesys.ErrNotDeleted, e.ID)
	}
	dir, idx, ok := parseDirentID(e.ID)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a FAT entry id", filesys.ErrNotFound, e.ID)
	}
	raw, err := f.deletedShortEntry(dir, idx)
	if err != nil {
		return nil, err
	}
	size := int64(binary.LittleEndian.Uint32(raw[28:]))
	first := f.firstCluster(raw)
	it, ok := f.makeItem(raw, idx, dir, true, nil, false)
	if !ok { // a volume label is refused by deletedShortEntry already
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d is not a file", filesys.ErrUnsupported, idx, dir)
	}
	c := filesys.Candidate{
		Method:      MethodContiguous,
		Size:        size,
		Assumptions: []string{"contiguous-allocation", "size-from-directory-entry"},
		Basis:       append([]string{direntID(dir, idx), "first_cluster=" + strconv.FormatUint(uint64(first), 10)}, f.deletedNameBasis(dir, idx, raw)...),
		Mode:        it.e.Mode,
		Times:       it.e.Times,
	}
	if size == 0 {
		return []filesys.Candidate{c}, nil
	}
	if !f.validCluster(first) {
		return nil, corrupt("recovery map", -1, "first cluster %d of a %d-byte file is outside the %d clusters", first, size, f.count)
	}
	cs := int64(f.clusterSize())
	need := uint64((size + cs - 1) / cs)
	if uint64(first)+need-1 > uint64(f.count)+1 {
		return nil, corrupt("recovery map", -1, "%d clusters from cluster %d run past the last cluster %d", need, first, uint64(f.count)+1)
	}
	// Defence only: validCluster and the run-past-the-end check above make this
	// unreachable on valid geometry; kept so that an overflow in the offset
	// arithmetic of a hostile geometry is a typed error, never a bad run.
	off, ok := f.clusterOffset(first)
	if !ok {
		return nil, corrupt("recovery map", -1, "cluster %d has no valid offset", first)
	}
	c.Runs = []filesys.Run{{Offset: off, Length: size}}
	return []filesys.Candidate{c}, nil
}

// deletedShortEntry reads entry idx of the directory at dir and returns it when
// it is a deleted short entry of a file. Errors: ErrNotFound for a directory
// that is not a live directory of this volume, a free slot or a long-name
// entry; ErrNotDeleted for a live entry; ErrUnsupported for a deleted volume
// label or directory (a deleted directory is not recovered).
func (f *FS) deletedShortEntry(dir uint32, idx int) (raw []byte, err error) {
	if (dir != 0 || f.fatType == 32) && !f.validCluster(dir) {
		return nil, fmt.Errorf("%w: cluster %d cannot hold a directory", filesys.ErrNotFound, dir)
	}
	if err := f.resolveDir(dir, ""); err != nil {
		return nil, err
	}
	raw, err = f.rawEntry(dir, idx)
	if err != nil {
		return nil, err
	}
	switch {
	case raw[0] == 0x00:
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d is a free slot", filesys.ErrNotFound, idx, dir)
	case isLFN(raw):
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d is a long-name entry", filesys.ErrNotFound, idx, dir)
	case raw[0] != 0xE5:
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d", filesys.ErrNotDeleted, idx, dir)
	}
	switch raw[11] & 0x18 {
	case 0:
		return raw, nil
	case attrDir:
		return nil, fmt.Errorf("%w: a deleted directory is not recovered", filesys.ErrUnsupported)
	default:
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d is not a file", filesys.ErrUnsupported, idx, dir)
	}
}

var _ filesys.Recoverer = (*FS)(nil)

// deletedNameBasis states how far the name of the deleted short entry raw
// (entry idx of the directory at dir) is verified, as Basis markers only: the
// name text is never part of a basis (the listing owns display, and a basis
// string must stay short and clean whatever the name holds). It reads at most
// maxLFNEntries entries before idx, in one block per directory cluster, and
// stops at an entry that is not a deleted long-name entry, at entry 0 or at a
// read error (the entries nearer the short entry are then all it has).
//
//	lfn=recovered, lfn_tail=unterminated: a verified long name whose farthest
//	  entry has no terminator, so the name may be cut short
//	lfn=recovered: a verified, terminated long name
//	name=short-only: no long-name set verifies against the short name
func (f *FS) deletedNameBasis(dir uint32, idx int, raw []byte) []string {
	lo := idx - min(idx, maxLFNEntries)
	var near [][32]byte // deleted long-name entries, the nearest first
walk:
	for end := idx; end > lo; {
		off, first, ok := f.dirRegion(dir, end-1)
		if !ok {
			break
		}
		start := max(lo, first)
		buf := make([]byte, (end-start)*32)
		if readFull(f.r, buf, off-int64(end-1-start)*32) != nil {
			break
		}
		for i := end - 1; i >= start; i-- {
			e := buf[(i-start)*32 : (i-start+1)*32]
			if !isLFN(e) || e[0] != 0xE5 {
				break walk
			}
			near = append(near, [32]byte(e))
		}
		end = start
	}
	slices.Reverse(near) // disk order
	if _, unterminated, ok := deletedLFN(near, raw[:11]); ok {
		if unterminated {
			return []string{"lfn=recovered", "lfn_tail=unterminated"}
		}
		return []string{"lfn=recovered"}
	}
	return []string{"name=short-only"}
}

// dirRegion returns the byte offset of entry idx of the directory at dir and
// the index of the first entry of the contiguous run (the fixed FAT12/16 root,
// or the cluster) that holds it, so that the entries from first to idx are one
// read.
func (f *FS) dirRegion(dir uint32, idx int) (off int64, first int, ok bool) {
	if dir == 0 && f.fatType != 32 {
		if idx < 0 || idx >= f.b.rootEnt {
			return 0, 0, false
		}
		return (int64(f.b.rsvd)+int64(f.b.numFATs)*int64(f.b.fatSz))*int64(f.b.bytsPerSec) + int64(idx)*32, 0, true
	}
	clusters, _, _ := f.dirChain(dir)
	cs := int64(f.clusterSize())
	k := int64(idx) * 32 / cs
	if idx < 0 || k >= int64(len(clusters)) {
		return 0, 0, false
	}
	base, ok := f.clusterOffset(clusters[k])
	if !ok {
		return 0, 0, false
	}
	return base + int64(idx)*32%cs, int(k * cs / 32), true
}
