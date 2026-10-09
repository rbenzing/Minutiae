package fat

import (
	"encoding/binary"
	"fmt"
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
		Basis:       []string{direntID(dir, idx), "first_cluster=" + strconv.FormatUint(uint64(first), 10)},
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
