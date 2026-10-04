package f2fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// NAT (node address table) geometry.
//
// Derivation (include/linux/f2fs_fs.h): struct f2fs_nat_entry is packed as
// version u8, ino le32, block_addr le32 = 9 bytes; NAT_ENTRY_PER_BLOCK =
// F2FS_BLKSIZE / sizeof(struct f2fs_nat_entry) = 4096 / 9 = 455.
const (
	natEntrySize       = 9
	natEntriesPerBlock = blockSize / natEntrySize // 455
)

// Summary and journal geometry (struct f2fs_summary_block).
//
// struct f2fs_summary is packed: nid le32, version u8, ofs_in_node le16 = 7
// bytes (SUMMARY_SIZE). A summary block is: entries[ENTRIES_IN_SUM = 512]
// (3584 bytes = SUM_ENTRIES_SIZE), then the journal, then struct
// summary_footer { entry_type u8; check_sum le32 } = 5 bytes (SUM_FOOTER_SIZE).
// So the journal is SUM_JOURNAL_SIZE = 4096 - 5 - 3584 = 507 bytes, at byte
// 3584 of the block. A journal starts with a le16 count (n_nats / n_sits) and
// the packed entries follow: NAT_JOURNAL_ENTRIES = (507 - 2) / (4 + 9) = 38
// entries of {nid le32, struct f2fs_nat_entry}.
const (
	sumEntriesSize      = 512 * 7
	sumFooterSize       = 5
	sumJournalSize      = blockSize - sumFooterSize - sumEntriesSize // 507
	natJournalEntrySize = 4 + natEntrySize
	natJournalEntries   = (sumJournalSize - 2) / natJournalEntrySize // 38

	// Indexes of the persistent data cursegs in the checkpoint's summaries:
	// hot data (whose journal is the NAT journal), warm data, cold data (whose
	// journal is the SIT journal).
	curHotData  = 0
	curColdData = 2

	// cpCompactSumFlag (CP_COMPACT_SUM_FLAG): the data summaries are stored
	// compacted. A compact first block holds the hot-data (NAT) journal at
	// byte 0 and the cold-data (SIT) journal at byte SUM_JOURNAL_SIZE
	// (kernel read_compacted_summaries); otherwise each data type has its own
	// full summary block with the journal at byte 3584.
	cpCompactSumFlag = 0x4
)

// natEntry is one decoded NAT entry.
type natEntry struct {
	version uint8
	ino     uint32
	addr    uint32 // block_addr; 0 (NULL_ADDR) = free nid
}

type natJournalEntry struct {
	nid uint32
	natEntry
}

func parseNATEntry(b []byte) natEntry {
	return natEntry{
		version: b[0],
		ino:     binary.LittleEndian.Uint32(b[1:]),
		addr:    binary.LittleEndian.Uint32(b[5:]),
	}
}

// natState caches the NAT journal, which is read once. A genuine I/O error is
// not cached, so a later lookup retries.
type natState struct {
	mu      sync.Mutex
	loaded  bool
	journal []natJournalEntry
	err     error
}

// natCapacity is the number of nids the NAT area can describe: its segment
// pairs, each of blocks_per_seg NAT blocks, each of 455 entries.
func (sb *superblock) natCapacity() uint64 {
	return uint64(sb.natPairs()) << segShift * natEntriesPerBlock
}

// mainEnd is the first block after the main area.
func (sb *superblock) mainEnd() uint64 {
	return uint64(sb.mainAddr) + uint64(sb.segMain)<<segShift
}

// versionedBlock returns the address of block i of a pair-structured area
// (NAT or SIT) that starts at start and has pairs segment pairs, choosing the
// copy with the version bitmap. ok is false when i lies beyond the area.
//
// Derivation (kernel current_nat_addr / current_sit_addr):
//
//	seg_off  = i >> log_blocks_per_seg
//	blk_addr = start + (seg_off << log_blocks_per_seg << 1) + (i & (blocks_per_seg - 1))
//	if test_bit(i, bitmap) { blk_addr += blocks_per_seg }
//
// i.e. each segment of logical blocks is stored twice, back to back, and the
// bitmap bit says which copy is current. The bitmap bit order is the kernel's
// f2fs_test_bit (most significant first), see testBit.
func versionedBlock(start, pairs uint32, bitmap []byte, i uint64) (uint32, bool) {
	segOff := i >> segShift
	if segOff >= uint64(pairs) {
		return 0, false
	}
	addr := uint64(start) + segOff<<segShift<<1 + i&(blocksPerSeg-1)
	if testBit(bitmap, uint32(i)) { // i < pairs*512 <= 2^31, so it fits
		addr += blocksPerSeg
	}
	// start + pairs*1024 <= block_count <= 2^32 was validated with the
	// superblock, so this stays in range.
	return uint32(addr), true
}

// summaryJournal returns the 507-byte journal of the checkpoint's current
// data summary of the given type (curHotData: NAT journal; curColdData: SIT
// journal), following the layout described at sumJournalSize.
func (f *FS) summaryJournal(typ int) ([]byte, error) {
	const st = "f2fs checkpoint summary"
	cp := f.cp
	var blk, off uint32
	if cp.flags&cpCompactSumFlag != 0 {
		blk = cp.startSum
		if typ == curColdData {
			off = sumJournalSize
		}
	} else {
		blk = cp.startSum + uint32(typ)
		off = sumEntriesSize
	}
	// The summaries lie between the header/payload blocks and the trailing
	// copy of the header (cp_pack_start_sum itself was checked at Open).
	if blk >= cp.packBlocks-1 {
		return nil, corrupt(st, int64(cp.addr)*blockSize, "data summary block %d is outside the %d-block checkpoint pack", blk, cp.packBlocks)
	}
	b, err := readBlock(f.r, cp.addr+blk)
	if err != nil {
		return nil, readError(st, int64(cp.addr+blk)*blockSize, err)
	}
	return b[off : off+sumJournalSize], nil
}

// loadNATJournal decodes the NAT journal (the hot-data summary's journal).
// A count beyond the in-block capacity is capped, with a warning.
func (f *FS) loadNATJournal() ([]natJournalEntry, error) {
	s := &f.nat
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.journal, s.err
	}
	j, err := f.summaryJournal(curHotData)
	if err != nil {
		if errors.Is(err, filesys.ErrCorrupt) {
			s.loaded, s.err = true, err
		}
		return nil, err
	}
	s.loaded = true
	n := int(binary.LittleEndian.Uint16(j))
	if n > natJournalEntries {
		// A count the block cannot hold: the journal is not trustworthy, so it
		// is ignored as a whole and lookups use the NAT blocks, which may be
		// older than the journal.
		f.warn("NAT journal entry count %d exceeds capacity %d; journal ignored — file locations may be stale", n, natJournalEntries)
		s.journal = nil
		return nil, nil
	}
	ents := make([]natJournalEntry, 0, n)
	for i := range n {
		e := j[2+i*natJournalEntrySize:]
		ents = append(ents, natJournalEntry{nid: binary.LittleEndian.Uint32(e), natEntry: parseNATEntry(e[4:])})
	}
	s.journal = ents
	return ents, nil
}

// natLookup returns the block address of node nid (see natGet).
func (f *FS) natLookup(nid uint32) (uint32, error) {
	ent, err := f.natGet(nid)
	return ent.addr, err
}

// natGet returns the NAT entry of node nid.
//
// nid 0 and the node/meta inodes (which have no node block) are refused, as is
// any nid beyond the NAT area. The NAT journal overrides the on-disk block (it
// holds entries newer than the last NAT flush); the first matching journal
// entry wins. A NULL_ADDR entry is a free nid and yields an error wrapping
// filesys.ErrNotFound.
func (f *FS) natGet(nid uint32) (natEntry, error) {
	const st = "f2fs NAT"
	sb := f.sb
	switch {
	case nid == 0:
		return natEntry{}, corrupt(st, -1, "node id 0 is not valid")
	case nid == sb.nodeIno || nid == sb.metaIno:
		return natEntry{}, corrupt(st, -1, "node id %d is a reserved inode without a node block", nid)
	case uint64(nid) >= sb.natCapacity():
		return natEntry{}, corrupt(st, -1, "node id %d is beyond the NAT capacity of %d", nid, sb.natCapacity())
	}

	journal, err := f.loadNATJournal()
	if err != nil {
		return natEntry{}, err
	}
	var ent natEntry
	found := false
	for _, j := range journal {
		if j.nid == nid {
			ent, found = j.natEntry, true
			break
		}
	}
	if !found {
		idx := uint64(nid / natEntriesPerBlock)
		blkAddr, ok := versionedBlock(sb.natAddr, sb.natPairs(), f.cp.natBitmap, idx)
		if !ok {
			return natEntry{}, corrupt(st, -1, "NAT block %d for node id %d is outside the NAT area", idx, nid)
		}
		blk, err := readBlock(f.r, blkAddr)
		if err != nil {
			return natEntry{}, readError(st, int64(blkAddr)*blockSize, err)
		}
		off := int(nid%natEntriesPerBlock) * natEntrySize
		ent = parseNATEntry(blk[off : off+natEntrySize])
	}
	if ent.addr == 0 {
		return natEntry{}, fmt.Errorf("%w: node id %d is free (NAT block address 0)", filesys.ErrNotFound, nid)
	}
	return ent, nil
}
