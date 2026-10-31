package ext4test

import (
	"encoding/binary"
	"fmt"
)

// This file writes a jbd2 journal into the blocks of inode 8
// (Options.JournalBlocks). It is an independent encoder: it shares nothing
// with the reader. The layout follows the kernel header include/linux/jbd2.h
// from memory; the 3G probe (tools/fixtures/README.md, "ext4 journal probe")
// confirmed it for what the kernels write: a v2 superblock, csum v3 with
// 16-byte tags and crc32c, the descriptor, commit and revoke checksums, the
// restart of logging at s_first and the first tag of a descriptor carrying the
// journal UUID. What the probe never saw is labelled "from memory" where it is
// written: the escape flag, csum v2 tags, a second descriptor block in one
// transaction, the wrap of the log, compat checksum and async commit.

// Journal block format constants, from memory of include/linux/jbd2.h.
const (
	JBD2Magic = 0xC03B3998

	// Tag flags (journal_block_tag_t.t_flags).
	JTagEscape   = 0x1 // the logged block had the magic as its first word; it is zeroed in the log
	JTagSameUUID = 0x2 // no 16-byte UUID follows this tag
	JTagDeleted  = 0x4
	JTagLast     = 0x8 // the last tag of its descriptor block
)

const (
	journalInode = 8 // EXT4_JOURNAL_INO

	jbd2Descriptor = 1
	jbd2Commit     = 2
	jbd2SuperV1    = 3
	jbd2SuperV2    = 4
	jbd2Revoke     = 5

	jIncompatRevoke = 0x1
	jIncompat64Bit  = 0x2
	jIncompatAsync  = 0x4
	jIncompatCsumV2 = 0x8
	jIncompatCsumV3 = 0x10
	jIncompatFast   = 0x20
	jCompatChecksum = 0x1

	jCsumCRC32C  = 4 // s_checksum_type
	jHeaderLen   = 12
	jRevokeHdr   = 16 // header + r_count
	jCommitSec   = 0x30
	jCommitNsec  = 0x38
	jCommitSum   = 0x10 // h_chksum[0]
	jCRC32Chksum = 1    // h_chksum_type of the compat v1 checksum
	jCRC32Size   = 4
)

// JBD2 describes the journal WriteJournal lays down.
type JBD2 struct {
	Version                                                        int // 1 or 2 (default 2)
	CsumV2, CsumV3, Bit64, AsyncCommit, FastCommit, CompatChecksum bool
	Sequence, Start, First                                         uint32 // s_sequence; s_start (0 = clean); s_first (default 1)
	NumFC                                                          uint32
	UUID                                                           [16]byte
	UnknownIncompat                                                uint32 // ORed into the incompat word (hostile tests)
	BadSuperCsum                                                   bool
	Txns                                                           []JTxn
}

// JTxn is one transaction: its revoke blocks, then its descriptors with their
// data slots, then the commit block.
type JTxn struct {
	Seq               uint32
	At                int      // journal block of its first block; -1 = right after the previous txn
	Revokes           []uint64 // one revoke block (or more when they do not fit), written first
	Tags              []JTag
	TagsPerDescriptor int // 0 = as many as fit
	// NoCommit leaves the commit block out. The Bad flags write a wrong tail or
	// commit checksum (they have no effect when the journal has no checksum feature).
	NoCommit, BadDescCsum, BadCommitCsum, BadRevokeCsum bool
	CommitSec                                           uint64
	CommitNsec                                          uint32
}

// JTag is one logged block. The first tag of every descriptor carries the
// journal UUID after it, as the kernel writes it; OwnUUID does the same for a
// later tag.
type JTag struct {
	FSBlock   uint64
	Data      []byte // one block (shorter data is zero padded); a block starting with the magic is escaped
	BadCsum   bool
	OwnUUID   bool   // clear SAME_UUID and write the 16-byte UUID after the tag
	Overwrite []byte // when set, written into the data slot AFTER the txn (a later lap's overwrite)
}

// placeJournal creates inode 8 over Options.JournalBlocks contiguous blocks.
func (b *builder) placeJournal() {
	n := b.o.JournalBlocks
	if n <= 0 {
		return
	}
	f := File{Mode: 0o600, Pieces: []Piece{{Data: make([]byte, n*b.bs)}}}
	b.writeInode(journalInode, b.renderInode(journalInode, &f))
}

// putJournalSuper sets s_journal_inum and the backup of the journal inode's
// map that mke2fs keeps in the superblock (s_jnl_blocks, s_jnl_backup_type).
func (b *builder) putJournalSuper(sb []byte) {
	if b.o.JournalBlocks <= 0 {
		return
	}
	le32(sb, 0xE0, journalInode)
	raw := b.img[b.groups[0].itable*b.bs+(journalInode-1)*b.isz:][:b.isz]
	copy(sb[0x10C:], raw[0x28:0x28+60]) // i_block
	copy(sb[0x10C+60:], raw[0x6C:0x70]) // i_size_high
	copy(sb[0x10C+64:], raw[0x4:0x8])   // i_size
	sb[0xFD] = 1                        // s_jnl_backup_type: EXT3_JNL_BACKUP_BLOCKS
}

// JournalFSBlock returns the filesystem block of journal block jblk.
func (l *Layout) JournalFSBlock(jblk int) int {
	p := l.b.rec[journalInode]
	if p == nil || jblk < 0 || jblk >= len(p.data) {
		panic(fmt.Sprintf("ext4test: journal block %d is outside the journal", jblk))
	}
	return p.data[jblk]
}

// InodeTableBlock returns the filesystem block that holds inode ino and the
// byte offset of its slot in that block.
func (l *Layout) InodeTableBlock(ino uint32) (fsblk int, off int) {
	if ino == 0 || int(ino-1)/l.b.ipg >= len(l.b.groups) {
		panic(fmt.Sprintf("ext4test: inode %d is outside the image", ino))
	}
	g := int(ino-1) / l.b.ipg
	pos := (int(ino-1) % l.b.ipg) * l.b.isz
	return l.b.groups[g].itable + pos/l.b.bs, pos % l.b.bs
}

// InodeCopy returns a copy of the inode-table block that holds ino, with the
// inode's slot edited and its checksum recomputed (with metadata_csum). The
// image is not changed.
func (l *Layout) InodeCopy(img []byte, ino uint32, edit func(raw []byte)) []byte {
	fsblk, off := l.InodeTableBlock(ino)
	cp := l.BlockCopy(img, fsblk)
	raw := cp[off : off+l.b.isz]
	edit(raw)
	if l.b.o.MetadataCsum {
		extra := 0
		if l.b.isz > inodeHeader {
			extra = int(binary.LittleEndian.Uint16(raw[0x80:]))
		}
		l.b.putInodeChecksum(raw, ino, extra)
	}
	return cp
}

// BlockCopy returns a copy of filesystem block fsblk.
func (l *Layout) BlockCopy(img []byte, fsblk int) []byte {
	if fsblk < 0 || (fsblk+1)*l.b.bs > len(img) {
		panic(fmt.Sprintf("ext4test: block %d is outside the image", fsblk))
	}
	return append([]byte(nil), img[fsblk*l.b.bs:(fsblk+1)*l.b.bs]...)
}

func be32put(b []byte, off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }
func be16put(b []byte, off int, v uint16) { binary.BigEndian.PutUint16(b[off:], v) }

// tagSize is journal_tag_bytes (from memory): 16 with csum v3; otherwise 8,
// plus 2 with csum v2 (a padding word), plus 4 with 64bit.
func (j *JBD2) tagSize() int {
	if j.CsumV3 {
		return 16
	}
	s := 8
	if j.CsumV2 {
		s += 2
	}
	if j.Bit64 {
		s += 4
	}
	return s
}

// WriteJournal writes the whole journal into inode 8's blocks (Options.
// JournalBlocks must be set): the blocks no transaction writes are zero. A
// transaction is its revoke blocks, then per descriptor its tags and data
// slots, then the commit block; a position past the last journal block wraps
// to s_first. Checksums are the builder's own unless a Bad flag says otherwise.
func (l *Layout) WriteJournal(img []byte, j JBD2) {
	b := l.b
	bs := b.bs
	p := b.rec[journalInode]
	if p == nil {
		panic("ext4test: WriteJournal needs Options.JournalBlocks")
	}
	n := len(p.data)
	for _, fs := range p.data {
		clear(img[fs*bs : (fs+1)*bs])
	}
	version := orDefault(j.Version, 2)
	first := orDefault(int(j.First), 1)
	if version == 1 && (j.CsumV2 || j.CsumV3 || j.Bit64 || j.AsyncCommit || j.FastCommit || j.CompatChecksum || j.UnknownIncompat != 0) {
		panic("ext4test: a version 1 journal superblock has no feature words")
	}
	csum := j.CsumV2 || j.CsumV3
	tail := 0
	if csum {
		tail = 4
	}
	seed := rawCRC32C(0xFFFFFFFF, j.UUID[:])
	tagSize := j.tagSize()
	hasRevoke := false

	put := func(pos int, blk []byte) { copy(img[p.data[pos]*bs:(p.data[pos]+1)*bs], blk) }
	adv := func(pos int) int {
		if pos++; pos >= n {
			pos = first
		}
		return pos
	}
	header := func(typ, seq uint32) []byte {
		blk := make([]byte, bs)
		be32put(blk, 0, JBD2Magic)
		be32put(blk, 4, typ)
		be32put(blk, 8, seq)
		return blk
	}
	// putTail stores the t_checksum of a descriptor or revoke block.
	putTail := func(blk []byte, bad bool) {
		if !csum {
			return
		}
		c := rawCRC32C(seed, blk) // the tail word is still zero
		if bad {
			c ^= 1
		}
		be32put(blk, bs-4, c)
	}

	pos := first
	if j.Start != 0 {
		pos = int(j.Start)
	}
	for _, t := range j.Txns {
		if t.At >= 0 {
			pos = t.At
		}
		if pos < 0 || pos >= n {
			panic(fmt.Sprintf("ext4test: transaction %d starts at journal block %d of %d", t.Seq, pos, n))
		}
		// Revoke blocks first.
		rs := 4
		if j.Bit64 {
			rs = 8
		}
		perRevoke := (bs - jRevokeHdr - tail) / rs
		for i := 0; i < len(t.Revokes); i += perRevoke {
			hasRevoke = true
			chunk := t.Revokes[i:min(i+perRevoke, len(t.Revokes))]
			blk := header(jbd2Revoke, t.Seq)
			be32put(blk, 0xC, uint32(jRevokeHdr+len(chunk)*rs))
			for k, r := range chunk {
				if j.Bit64 {
					binary.BigEndian.PutUint64(blk[jRevokeHdr+k*8:], r)
				} else {
					if r>>32 != 0 {
						panic("ext4test: a revoke record needs 64bit")
					}
					be32put(blk, jRevokeHdr+k*4, uint32(r))
				}
			}
			putTail(blk, t.BadRevokeCsum)
			put(pos, blk)
			pos = adv(pos)
		}
		// Descriptors with their data slots.
		type overwrite struct {
			pos  int
			data []byte
		}
		var overwrites []overwrite
		tags := t.Tags
		for len(tags) > 0 {
			space := bs - jHeaderLen - tail
			count, used := 0, 0
			for count < len(tags) {
				cost := tagSize
				if count == 0 || tags[count].OwnUUID {
					cost += 16
				}
				if used+cost > space || (t.TagsPerDescriptor > 0 && count == t.TagsPerDescriptor) {
					break
				}
				used += cost
				count++
			}
			if count == 0 || (t.TagsPerDescriptor > 0 && count < t.TagsPerDescriptor && count < len(tags)) {
				panic(fmt.Sprintf("ext4test: %d tags per descriptor do not fit in a block (or one tag is too large)", t.TagsPerDescriptor))
			}
			group := tags[:count]
			tags = tags[count:]

			desc := header(jbd2Descriptor, t.Seq)
			off := jHeaderLen
			var slots [][]byte
			for i, tag := range group {
				if len(tag.Data) > bs {
					panic("ext4test: tag data is longer than a block")
				}
				slot := make([]byte, bs)
				copy(slot, tag.Data)
				flags := uint32(0)
				if binary.BigEndian.Uint32(slot) == JBD2Magic {
					flags |= JTagEscape
					clear(slot[:4])
				}
				ownUUID := i == 0 || tag.OwnUUID
				if !ownUUID {
					flags |= JTagSameUUID
				}
				if i == len(group)-1 {
					flags |= JTagLast
				}
				var sum uint32
				if csum {
					var sq [4]byte
					binary.BigEndian.PutUint32(sq[:], t.Seq)
					sum = rawCRC32C(rawCRC32C(seed, sq[:]), slot)
					if tag.BadCsum {
						sum ^= 1
					}
				}
				if tag.FSBlock>>32 != 0 && !j.Bit64 {
					panic("ext4test: a block number above 32 bits needs 64bit")
				}
				be32put(desc, off, uint32(tag.FSBlock))
				switch {
				case j.CsumV3:
					be32put(desc, off+4, flags)
					if j.Bit64 {
						be32put(desc, off+8, uint32(tag.FSBlock>>32))
					}
					be32put(desc, off+12, sum)
				default:
					be16put(desc, off+4, uint16(sum))
					be16put(desc, off+6, uint16(flags))
					if j.Bit64 {
						be32put(desc, off+8, uint32(tag.FSBlock>>32))
					}
					// csum v2 pads the tag by two zero bytes (from memory)
				}
				off += tagSize
				if ownUUID {
					copy(desc[off:], j.UUID[:])
					off += 16
				}
				slots = append(slots, slot)
			}
			putTail(desc, t.BadDescCsum)
			put(pos, desc)
			pos = adv(pos)
			for i, slot := range slots {
				put(pos, slot)
				if group[i].Overwrite != nil {
					if len(group[i].Overwrite) > bs {
						panic("ext4test: overwrite data is longer than a block")
					}
					ow := make([]byte, bs)
					copy(ow, group[i].Overwrite)
					overwrites = append(overwrites, overwrite{pos, ow})
				}
				pos = adv(pos)
			}
		}
		if !t.NoCommit {
			cm := header(jbd2Commit, t.Seq)
			if j.CompatChecksum && !csum {
				// The v1 CRC32 over the transaction is not computed (the reader does not verify it, 3G-R10).
				cm[0xC], cm[0xD] = jCRC32Chksum, jCRC32Size
			}
			binary.BigEndian.PutUint64(cm[jCommitSec:], t.CommitSec)
			be32put(cm, jCommitNsec, t.CommitNsec)
			if csum {
				c := rawCRC32C(seed, cm)
				if t.BadCommitCsum {
					c ^= 1
				}
				be32put(cm, jCommitSum, c)
			}
			put(pos, cm)
			pos = adv(pos)
		}
		for _, o := range overwrites {
			put(o.pos, o.data)
		}
	}

	// The superblock last: s_feature_incompat depends on what was written.
	sb := make([]byte, bs)
	be32put(sb, 0, JBD2Magic)
	be32put(sb, 4, jbd2SuperV2)
	if version == 1 {
		be32put(sb, 4, jbd2SuperV1)
	}
	be32put(sb, 0xC, uint32(bs))
	be32put(sb, 0x10, uint32(n))
	be32put(sb, 0x14, uint32(first))
	be32put(sb, 0x18, j.Sequence)
	be32put(sb, 0x1C, j.Start)
	if version == 2 {
		if j.CompatChecksum {
			be32put(sb, 0x24, jCompatChecksum)
		}
		inc := j.UnknownIncompat
		for _, f := range []struct {
			on  bool
			bit uint32
		}{
			{hasRevoke, jIncompatRevoke},
			{j.Bit64, jIncompat64Bit},
			{j.AsyncCommit, jIncompatAsync},
			{j.CsumV2, jIncompatCsumV2},
			{j.CsumV3, jIncompatCsumV3},
			{j.FastCommit, jIncompatFast},
		} {
			if f.on {
				inc |= f.bit
			}
		}
		be32put(sb, 0x28, inc)
		copy(sb[0x30:0x40], j.UUID[:])
		if csum {
			sb[0x50] = jCsumCRC32C
		}
		be32put(sb, 0x54, j.NumFC)
		if csum {
			c := rawCRC32C(0xFFFFFFFF, sb[:1024])
			if j.BadSuperCsum {
				c ^= 1
			}
			be32put(sb, 0xFC, c)
		}
	}
	put(0, sb)
}
