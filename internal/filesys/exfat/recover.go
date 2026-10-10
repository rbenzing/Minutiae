package exfat

import (
	"fmt"
	"math"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Recovery methods of exFAT. The kernel driver clears InUse in the entry set
// and the bits of the allocation bitmap on delete and keeps the FAT entries
// (probe P3, Task 1), so the stream extension (first cluster, lengths, the
// NoFatChain flag) is what survives.
const (
	// MethodNoFatChain: the file was stored without a FAT chain, which the
	// format defines as contiguous. This is a fact of the format, not a guess.
	MethodNoFatChain = "exfat-nofatchain"
	// MethodContiguous: the file had a FAT chain that is not used here, so
	// the clusters are ASSUMED to have been contiguous.
	MethodContiguous = "exfat-contiguous"
	// MethodFatChain: reserved for the stale-chain method (Task 8).
	MethodFatChain = "exfat-fatchain"
)

// Recoverable implements filesys.Recoverer. The map is metadata only and
// covers ValidDataLength bytes: DataLength beyond it is never presented as
// content. Whether the clusters are free now is the caller's check.
func (f *FS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	kind, a, b, err := parseID(e.ID)
	if err != nil {
		return nil, err
	}
	if kind == idDir {
		st, err := f.resolveDir(a, "")
		if err != nil {
			return nil, err
		}
		if st != nil {
			return nil, fmt.Errorf("%w: %q is a live directory", filesys.ErrNotDeleted, e.ID)
		}
		return nil, fmt.Errorf("%w: no live directory with first cluster %d", filesys.ErrNotFound, a)
	}
	r, err := f.locate(a, b)
	if err != nil {
		return nil, err
	}
	const st = "recovery map"
	switch {
	case !r.deleted:
		return nil, fmt.Errorf("%w: %q", filesys.ErrNotDeleted, e.ID)
	case r.attrs&attrDirectory != 0:
		return nil, fmt.Errorf("%w: a deleted directory is not recovered", filesys.ErrUnsupported)
	case r.damaged || r.checksum:
		return nil, corrupt(st, -1, "the deleted entry set %q is damaged or fails its checksum", e.ID)
	case r.valid > r.dataLen:
		return nil, corrupt(st, -1, "ValidDataLength %d exceeds DataLength %d", r.valid, r.dataLen)
	case r.dataLen > math.MaxInt64:
		return nil, corrupt(st, -1, "DataLength %d is not a valid length", r.dataLen)
	}
	capacity, ok := filesys.MulOK(int64(f.clusterCount), f.cs)
	if !ok || int64(r.dataLen) > capacity {
		return nil, corrupt(st, -1, "DataLength %d is larger than the %d bytes of the cluster heap", r.dataLen, capacity)
	}
	entry := f.makeEntry(a, r)
	valid := int64(r.valid)
	c := filesys.Candidate{Size: valid, Mode: entry.Mode, Times: entry.Times}
	c.Basis = []string{
		direntID(a, r.idx),
		"first_cluster=" + strconv.FormatUint(uint64(r.first), 10),
		"data_length=" + strconv.FormatUint(r.dataLen, 10),
		"valid_data_length=" + strconv.FormatUint(r.valid, 10),
	}
	if r.noFatChain() {
		c.Basis = append(c.Basis, "no_fat_chain")
	} else {
		c.Basis = append(c.Basis, "fat_chain")
	}
	if r.nameNote != "" { // "truncated" never reaches here: such a set is rejected as damaged above (F63 is void)
		c.Basis = append(c.Basis, "name="+r.nameNote)
	}
	if valid == 0 {
		if r.dataLen > 0 {
			c.Assumptions = []string{"data-length=" + strconv.FormatUint(r.dataLen, 10)}
		}
		return []filesys.Candidate{c}, nil
	}
	need := uint64(valid-1)/uint64(f.cs) + 1 // valid <= capacity: cannot overflow
	if !f.validCluster(r.first) {
		return nil, corrupt(st, -1, "first cluster %d of a %d-byte file is outside the %d clusters", r.first, valid, f.clusterCount)
	}
	if uint64(r.first-fatReservedClusters)+need > uint64(f.clusterCount) {
		return nil, corrupt(st, -1, "%d clusters from cluster %d run past the last cluster %d", need, r.first, uint64(f.clusterCount)+fatReservedClusters-1)
	}
	if r.noFatChain() {
		c.Method = MethodNoFatChain
		c.Assumptions = []string{"contiguity-from-no-fat-chain"}
	} else {
		// Until the stale-chain method (Task 8) a chained file is only assumed contiguous.
		c.Method = MethodContiguous
		c.Assumptions = []string{"contiguous-allocation"}
	}
	c.Runs = []filesys.Run{{Offset: f.clusterOff(r.first), Length: valid}}
	if r.valid < r.dataLen {
		c.Assumptions = append(c.Assumptions,
			"data-length="+strconv.FormatUint(r.dataLen, 10),
			"tail-beyond-valid-data-length-not-stored")
	}
	return []filesys.Candidate{c}, nil
}

var _ filesys.Recoverer = (*FS)(nil)
