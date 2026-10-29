package ext4

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The leaf index (3C-R12). A freed extent leaf keeps its tail checksum, which binds it to the inode number and
// generation it was written for. The raw CRC register update is linear, so for a block d with checksum
// stored == rawCRC32C(seed, d):
//
//	stored ^ rawCRC32C(0, d) == rawCRC32C(seed, zeros(len d))
//
// The left side needs only the block; the right side needs only the seed and the length. One pass over the free
// blocks therefore indexes every plausible leaf by its key, and each deleted inode finds its leaves with one map
// lookup and no per-pair CRC. A match is a 32-bit coincidence at worst (probability 2^-32 per leaf and inode); the
// consumer re-verifies it with the full CRC and says the binding is not cryptographic.
//
// The index holds SUPERSEDED leaves too: a leaf a later tree change replaced is freed like any other and still
// verifies under the same (inode, generation). Keeping them is correct; deciding which one is current is the
// consumer's job. The bounds are counted, never silent: past maxStaleLeaves the index is full, and a key that
// reached maxLeavesPerKey records that it dropped leaves.

const (
	maxStaleLeaves  = 1 << 20 // index entries
	maxLeavesPerKey = 64      // leaves kept for one (inode, generation) key
	leafScanChunk   = 1 << 20 // bytes read from a free run at a time
)

// leafRef is one plausible leaf: its block and the logical range [first, end) its entries cover.
type leafRef struct {
	blk        uint64
	first, end uint32
	entries    uint16
}

// leafIndex is the free-space leaf index, built once per filesystem by PrepareRecovery.
type leafIndex struct {
	byKey map[uint32][]leafRef
	tail  int // offset of the tail checksum, 12 + 12*eh_max: one constant per filesystem
	n     int // leaves held
	// full is set when maxStaleLeaves was reached: leaves were dropped, so a miss proves nothing.
	full bool
	// keyFull holds the keys that reached maxLeavesPerKey and dropped leaves.
	keyFull map[uint32]struct{}
	dropped int // leaves dropped by either cap
}

// leafCaps are the effective index caps.
func (f *FS) leafCaps() (total, perKey int) {
	total, perKey = maxStaleLeaves, maxLeavesPerKey
	if f.leafTotalCap > 0 {
		total = f.leafTotalCap
	}
	if f.leafKeyCap > 0 {
		perKey = f.leafKeyCap
	}
	return total, perKey
}

// leafKey is the key a leaf block is indexed by: its stored tail checksum with the data part removed.
func leafKey(block []byte, tail int) uint32 {
	return binary.LittleEndian.Uint32(block[tail:]) ^ rawCRC32C(0, block[:tail])
}

// seedKey is the key a leaf written for the checksum seed seed would have: the CRC of tail zero bytes.
func seedKey(seed uint32, tail int) uint32 {
	return rawCRC32C(seed, make([]byte, tail))
}

// leafTail is the tail offset of a full-capacity leaf of block size bs.
func leafTail(bs int) int {
	return extentHeaderLen + (bs-extentHeaderLen)/extentEntryLen*extentEntryLen
}

// plausibleLeaf reports whether block (bs bytes) looks like a depth-0 extent leaf of a filesystem of `blocks`
// blocks, and returns its logical range. The kernel always makes a leaf with eh_max for the full block, so any
// other eh_max is not one. Entries are strictly ascending, non-overlapping, 1..65535 raw length, and end inside the
// filesystem.
func plausibleLeaf(block []byte, bs int, blocks uint64) (leafRef, bool) {
	le := binary.LittleEndian
	if len(block) < bs || bs < extentHeaderLen+extentEntryLen+4 || le.Uint16(block) != extentMagic {
		return leafRef{}, false
	}
	maxEnt := (bs - extentHeaderLen) / extentEntryLen
	entries, eMax, depth := int(le.Uint16(block[2:])), int(le.Uint16(block[4:])), le.Uint16(block[6:])
	if depth != 0 || eMax != maxEnt || entries < 1 || entries > eMax {
		return leafRef{}, false
	}
	ref := leafRef{entries: uint16(entries)}
	var prevEnd uint64
	for i := range entries {
		e := block[extentHeaderLen+i*extentEntryLen:][:extentEntryLen]
		lblk, raw := uint64(le.Uint32(e)), uint64(le.Uint16(e[4:]))
		n := raw
		if raw > extentMaxInit {
			n = raw - extentMaxInit
		}
		if raw == 0 || n == 0 {
			return leafRef{}, false
		}
		phys := uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))
		if phys+n > blocks || (i > 0 && lblk < prevEnd) || lblk+n > 1<<32 {
			return leafRef{}, false
		}
		if i == 0 {
			ref.first = uint32(lblk)
		}
		prevEnd = lblk + n
	}
	ref.end = uint32(prevEnd)
	return ref, true
}

// PrepareRecovery builds the leaf index from the free blocks (filesys.RecoverPreparer). It is idempotent, a no-op
// without metadata_csum and the CRC32C checksum type (no leaf can then be bound to an inode), and reads each free
// run once, sequentially, through the uncached view in leafScanChunk chunks (3C-R25). ctx is checked between chunks;
// a cancel returns ctx.Err() and discards the partial index, so the next call builds it afresh. An I/O error is
// returned as it is.
func (f *FS) PrepareRecovery(ctx context.Context) error {
	if !f.sb.metadataCsum() || f.sb.checksumType != csumTypeCRC32C {
		return nil
	}
	f.leafMu.Lock()
	defer f.leafMu.Unlock()
	if f.leafIdx != nil {
		return nil
	}
	fs, err := f.freeSet()
	if err != nil {
		return err
	}
	bs := f.sb.blockSize
	chunk := f.leafChunk
	if chunk <= 0 {
		chunk = leafScanChunk
	}
	chunk = max(chunk/bs, 1) * bs
	totalCap, keyCap := f.leafCaps()
	idx := &leafIndex{byKey: map[uint32][]leafRef{}, tail: leafTail(bs)}
	buf := make([]byte, chunk)
	for _, run := range fs.runs {
		for off := run.Offset; off < run.Offset+run.Length; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := int(min(int64(chunk), run.Offset+run.Length-off))
			if err := readFull(f.data, buf[:n], off); err != nil {
				return err
			}
			f.leafBytes.Add(int64(n))
			for b := 0; b+bs <= n; b += bs {
				block := buf[b : b+bs]
				ref, ok := plausibleLeaf(block, bs, uint64(f.sb.blocksCount))
				if !ok {
					continue
				}
				ref.blk = uint64(off+int64(b)) / uint64(bs)
				idx.add(leafKey(block, idx.tail), ref, totalCap, keyCap)
			}
			off += int64(n)
		}
	}
	f.leafIdx = idx
	return nil
}

// add inserts ref under key, or counts it as dropped when a cap is reached.
func (x *leafIndex) add(key uint32, ref leafRef, totalCap, keyCap int) {
	switch {
	case x.n >= totalCap:
		x.full = true
		x.dropped++
	case len(x.byKey[key]) >= keyCap:
		if x.keyFull == nil {
			x.keyFull = map[uint32]struct{}{}
		}
		x.keyFull[key] = struct{}{}
		x.dropped++
	default:
		x.byKey[key] = append(x.byKey[key], ref)
		x.n++
	}
}

// leafState is the guarded state PrepareRecovery adds to the FS.
type leafState struct {
	leafMu       sync.Mutex
	leafIdx      *leafIndex
	leafTotalCap int
	leafKeyCap   int
	leafChunk    int
	leafBytes    atomic.Int64 // bytes the free-space pass read; tests only
}

var _ filesys.RecoverPreparer = (*FS)(nil)
