package f2fstest

import (
	"encoding/binary"
	"math/bits"
)

// SIT encoding for the builder: segment information table blocks and the SIT
// journal. Like the rest of the package it shares no code with the reader.
//
// struct f2fs_sit_entry is packed: vblocks le16 (the low 10 bits are the
// valid-block count, the top 6 the segment type), valid_map[64] (one bit per
// block of the segment, most significant bit first within each byte) and
// mtime le64 = 74 bytes; a block holds 4096/74 = 55 of them.

const (
	// SITEntrySize is the size of struct f2fs_sit_entry.
	SITEntrySize = 74
	// SITPerBlock is the number of SIT entries in a 4 KiB block.
	SITPerBlock = BlockSize / SITEntrySize
	// SITJournalEntries is the number of {segno, sit_entry} pairs the SIT
	// journal holds: (507 - 2) / (4 + 74).
	SITJournalEntries = (507 - 2) / (4 + SITEntrySize)
)

// SITEntry is one segment's SIT entry. Segno counts main-area segments from 0.
type SITEntry struct {
	Segno uint32
	// Type is the segment type kept in the top 6 bits of vblocks.
	Type uint16
	// Valid lists the block offsets (0..511) of the segment whose valid bit is
	// set. Map, when non-nil, is used instead (at most 64 bytes).
	Valid []int
	Map   []byte
	// VBlocks, when non-nil, replaces the valid-block count (default: the
	// number of set bits), so a test can write an inconsistent entry.
	VBlocks *uint16
}

// bytes encodes the 74-byte entry.
func (e SITEntry) bytes() []byte {
	b := make([]byte, SITEntrySize)
	m := b[2:66]
	if e.Map != nil {
		copy(m, e.Map)
	}
	for _, v := range e.Valid {
		if v < 0 || v >= BlocksPerSeg {
			panic("f2fstest: SIT valid block offset outside the segment")
		}
		m[v/8] |= 0x80 >> (v % 8)
	}
	n := 0
	for _, c := range m {
		n += bits.OnesCount8(c)
	}
	vb := uint16(n)
	if e.VBlocks != nil {
		vb = *e.VBlocks
	}
	binary.LittleEndian.PutUint16(b[0:], vb&0x3ff|e.Type<<10)
	return b
}

// SITBlock returns the absolute address of SIT block i (counting the logical
// blocks of the area): the first copy, or the second one when second is set.
// The layout is the NAT's: each segment of 512 logical blocks is stored twice
// back to back.
func (l Layout) SITBlock(i int, second bool) uint32 {
	a := l.SIT + uint32(i/BlocksPerSeg)*2*BlocksPerSeg + uint32(i%BlocksPerSeg)
	if second {
		a += BlocksPerSeg
	}
	return a
}

// writeSIT writes the SIT blocks into the copy the SIT version bitmap selects.
// Unless o.NoSIT, a segment's entry marks as valid every main-area block that
// o.Nodes and o.Data place (a consistent image); o.SIT entries replace those.
// With o.SITDecoy the other copy of every block describes every segment as
// fully valid, so a reader that picks the wrong copy finds no free space.
func writeSIT(img []byte, o Options, l Layout) {
	entries := map[uint32]SITEntry{}
	if !o.NoSIT {
		valid := map[uint32][]int{}
		mark := func(addr uint32) {
			if addr >= l.Main && addr < l.BlockCount {
				seg := (addr - l.Main) / BlocksPerSeg
				valid[seg] = append(valid[seg], int((addr-l.Main)%BlocksPerSeg))
			}
		}
		for idx, n := range o.Nodes {
			addr := n.Addr
			if addr == 0 {
				addr = l.Main + uint32(idx)
			}
			if n.Block != nil {
				mark(addr)
			}
		}
		for _, d := range o.Data {
			mark(d.Addr)
		}
		for seg, v := range valid {
			entries[seg] = SITEntry{Segno: seg, Valid: v}
		}
	}
	for _, e := range o.SIT {
		entries[e.Segno] = e
	}
	put := func(e SITEntry, second bool) {
		i := int(e.Segno / SITPerBlock)
		blk := l.SITBlock(i, second != bitSet(o.SITBitmap, i))
		copy(img[int(blk)*BlockSize+int(e.Segno%SITPerBlock)*SITEntrySize:], e.bytes())
	}
	for _, e := range entries {
		put(e, false)
	}
	if o.SITDecoy {
		full := make([]byte, 64)
		for i := range full {
			full[i] = 0xff
		}
		for seg := range l.MainSegs {
			put(SITEntry{Segno: seg, Map: full}, true)
		}
	}
}

// writeSITJournal writes o.SITJournal into the cold-data summary's journal of
// the pack at base: n_sits le16, then {segno le32, sit_entry} pairs.
func writeSITJournal(img []byte, o Options, l Layout, base uint32) {
	if len(o.SITJournal) > SITJournalEntries {
		panic("f2fstest: the SIT journal holds at most 6 entries")
	}
	j := int(base+l.StartSum+2)*BlockSize + 3584
	if o.CompactSum {
		j = int(base+l.StartSum)*BlockSize + 507
	}
	binary.LittleEndian.PutUint16(img[j:], uint16(len(o.SITJournal)))
	for i, e := range o.SITJournal {
		p := j + 2 + i*(4+SITEntrySize)
		binary.LittleEndian.PutUint32(img[p:], e.Segno)
		copy(img[p+4:], e.bytes())
	}
}
