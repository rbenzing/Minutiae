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
	// An empty file assumes no clusters at all: the name is kept for it (it is
	// not renamed) and it claims no run and no assumption.
	MethodContiguous = "exfat-contiguous"
	// MethodFatChain: the clusters follow the FAT chain the driver left behind
	// (stale, unverified against reuse; examine proves freeness).
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
		// An empty map has no runs and walks no chain: it still names a method (a candidate
		// without one is malformed). The NoFatChain flag is read, nothing is assumed about clusters.
		c.Method = MethodContiguous
		if r.noFatChain() {
			c.Method = MethodNoFatChain
		}
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
	switch {
	case r.noFatChain():
		c.Method = MethodNoFatChain
		c.Assumptions = []string{"contiguity-from-no-fat-chain"}
		c.Runs = []filesys.Run{{Offset: f.clusterOff(r.first), Length: valid}}
	default:
		exts, stop, at, err := f.walkStaleChain(r.first, need)
		if err != nil {
			return nil, err
		}
		if stop == stopFirstFree {
			// no chain survived: the only case where contiguity is assumed
			c.Method = MethodContiguous
			c.Assumptions = []string{"contiguous-allocation"}
			c.Runs = []filesys.Run{{Offset: f.clusterOff(r.first), Length: valid}}
			break
		}
		c.Method = MethodFatChain
		c.Runs = trimRuns(f.extentRuns(exts), valid)
		if stop == stopComplete {
			c.Basis = append(c.Basis, "fat-chain=stale")
			c.Assumptions = []string{"fat-chain-unverified-against-reuse"}
			break
		}
		// the prefix: the clusters up to the stop, never a contiguous fallback and never a cluster after the stop
		var got uint64
		for _, x := range exts {
			got += x.n
		}
		c.Assumptions = []string{
			"stale-chain-stop=" + stop.String() + "@" + strconv.FormatUint(uint64(at), 10),
			"map-covers-clusters=" + strconv.FormatUint(got, 10) + "-of-" + strconv.FormatUint(need, 10),
		}
	}
	if r.valid < r.dataLen {
		c.Assumptions = append(c.Assumptions,
			"data-length="+strconv.FormatUint(r.dataLen, 10),
			"tail-beyond-valid-data-length-not-stored")
	}
	return []filesys.Candidate{c}, nil
}

// trimRuns cuts runs to limit bytes in total (the last cluster of a file is only partly valid).
func trimRuns(runs []filesys.Run, limit int64) []filesys.Run {
	for i := range runs {
		if limit <= runs[i].Length {
			runs[i].Length = limit
			return runs[: i+1 : i+1]
		}
		limit -= runs[i].Length
	}
	return runs
}

var _ filesys.Recoverer = (*FS)(nil)
