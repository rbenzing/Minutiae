package ext4

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Tag flags (journal_block_tag_t.t_flags), from memory of include/linux/jbd2.h (3G-R4).
const (
	tagEscape   = 0x1
	tagSameUUID = 0x2
	tagDeleted  = 0x4
	tagLast     = 0x8
)

const (
	jHeaderLen    = 12   // magic, block type, sequence
	jRevokeHdr    = 16   // header and r_count
	jCommitSumOff = 0x10 // h_chksum[0]
	jCommitSecOff = 0x30 // h_commit_sec
	jCommitNsecOf = 0x38 // h_commit_nsec
	jUUIDLen      = 16
	jTailLen      = 4 // jbd2_journal_block_tail with csum v2/v3

	maxCommitSec = 253402300799 // 9999-12-31T23:59:59Z: a later time cannot be rendered, so it is not a time
)

// copyState says what the checksum of a logged copy proves (3G-R5).
type copyState uint8

const (
	copyVerified   copyState = iota // the tag checksum feature is on and the checksum matches
	copyUnverified                  // the journal records no tag checksum
	copyMismatch                    // the feature is on and the checksum fails
	copyMissing                     // the data slot is not available
	copyOutside                     // the tag names a block beyond the filesystem
)

func (c copyState) String() string {
	switch c {
	case copyVerified:
		return "verified"
	case copyUnverified:
		return "unverified"
	case copyMismatch:
		return "mismatch"
	case copyMissing:
		return "missing"
	case copyOutside:
		return "outside"
	}
	return fmt.Sprintf("copy(%d)", uint8(c))
}

// jtag is one block a transaction logs. At most 24 bytes (TestJournalTagEntryIsCompact).
type jtag struct {
	fsblk   uint64
	jblk    uint32
	txn     uint32 // index into jindex.txns
	flags   uint32
	copy    copyState
	revoked bool
}

// jtxn is one transaction (live) or one stale fragment.
type jtxn struct {
	seq              uint32
	stale, committed bool
	start, commit    int64 // journal blocks; commit -1 = none
	commitTime       filesys.Timestamp
	csum             string    // ok | bad | not-recorded | not-verified
	tags, revokes    [2]uint32 // [from, to) into jindex.tags / jindex.revokes
	warnings         []string
}

// rawTag is a tag as stored in a descriptor block.
type rawTag struct {
	fsblk uint64
	flags uint32
	csum  uint32 // as stored: 32 bits with csum_v3, 16 bits otherwise
}

// tagSize is the kernel's journal_tag_bytes (3G-R4): 16 with csum v3; otherwise 8, plus 2 with csum v2, plus 4 with
// 64bit.
func tagSize(sb *jsuper) int {
	if sb.incompat&jIncompatCsumV3 != 0 {
		return 16
	}
	n := 8
	if sb.incompat&jIncompatCsumV2 != 0 {
		n += 2
	}
	if sb.incompat&jIncompat64Bit != 0 {
		n += 4
	}
	return n
}

// tailLen is the size of the checksum tail of a descriptor or revoke block.
func (s *jsuper) tailLen() int {
	if s.csum() {
		return jTailLen
	}
	return 0
}

// parseTags decodes the tags of a descriptor block. The parse is bounded by the block: it stops at LAST_TAG or when
// the next tag no longer fits before the checksum tail, never reading past the block (3G-R4, R9).
func parseTags(block []byte, sb *jsuper) ([]rawTag, error) {
	be := binary.BigEndian
	tail, tsz := sb.tailLen(), tagSize(sb)
	if len(block) < jHeaderLen+tail {
		return nil, corrupt("ext4 journal descriptor", 0, "a descriptor block of %d bytes cannot hold a header", len(block))
	}
	limit := len(block) - tail
	var tags []rawTag
	for off := jHeaderLen; off+tsz <= limit; {
		rt := rawTag{fsblk: uint64(be.Uint32(block[off:]))}
		if sb.incompat&jIncompatCsumV3 != 0 {
			rt.flags, rt.csum = be.Uint32(block[off+4:]), be.Uint32(block[off+12:])
		} else {
			rt.csum, rt.flags = uint32(be.Uint16(block[off+4:])), uint32(be.Uint16(block[off+6:]))
		}
		if sb.incompat&jIncompat64Bit != 0 {
			rt.fsblk |= uint64(be.Uint32(block[off+8:])) << 32
		}
		tags = append(tags, rt)
		off += tsz
		if rt.flags&tagSameUUID == 0 {
			off += jUUIDLen
		}
		if rt.flags&tagLast != 0 {
			break
		}
	}
	return tags, nil
}

// seqNewer and seqGE are the kernel's tid_gt and tid_geq (3G-R31): sequence numbers wrap.
func seqNewer(a, b uint32) bool { return int32(a-b) > 0 }

func seqGE(a, b uint32) bool { return int32(a-b) >= 0 }

// warn records a finding on the result (JournalInfo.Warnings), never on the filesystem (3C-R5).
func (x *jindex) warn(format string, a ...any) {
	x.info.Warnings = append(x.info.Warnings, fmt.Sprintf(format, a...))
}

// capHit records that a scan cap was reached: the result is incomplete, never complete (3G-R9).
func (x *jindex) capHit(stop string) {
	x.info.Incomplete = true
	if x.info.Stop == "" {
		x.info.Stop = stop
	}
	x.warn("the journal scan reached a cap (%s); the index is incomplete", stop)
}

// adv is the next log block after b: the log wraps from the end of the log area to s_first.
func (x *jindex) adv(b uint32) uint32 {
	if b++; b >= x.last {
		b = x.sb.first
	}
	return b
}

// readBlock reads journal block j through the uncached view. It is the only reader of journal blocks; each is read
// at most once per build by the scans, because a consumed block is never read again.
func (x *jindex) readBlock(f *FS, j uint32) ([]byte, error) {
	bs := int64(x.sb.blockSize)
	fsb, ok := x.jm.fsBlock(j, bs)
	if !ok {
		return nil, corrupt("ext4 journal", -1, "journal block %d is outside the mapped journal", j)
	}
	off := int64(fsb) * bs // the block lies in the image, so this cannot overflow
	buf := make([]byte, bs)
	f.jrnBlocks.Add(1)
	if err := readFull(f.data, buf, off); err != nil {
		return nil, ioOrCorrupt("ext4 journal", off, err)
	}
	return buf, nil
}

// scanLive walks the live region (3G-R3): from s_start with s_sequence, transaction by transaction. The region is empty
// when s_start is 0.
func (x *jindex) scanLive(f *FS) error {
	if x.sb.start == 0 {
		return nil
	}
	b, expected := x.sb.start, x.sb.sequence
	for {
		next, stop, err := x.scanTxn(f, b, expected, false)
		if err != nil {
			return err
		}
		if stop != "" {
			x.liveStop = stop
			if len(x.txns) == 0 && !strings.HasPrefix(stop, "cap-") {
				x.warn("s_start %d does not hold a transaction of sequence %d (%s); the live region is empty", x.sb.start, x.sb.sequence, stop)
			}
			return nil
		}
		b, expected = next, expected+1
	}
}

// scanTxn scans one transaction beginning at journal block start, whose blocks carry sequence expected (the kernel's
// PASS_SCAN rule, 3G-R3; the stale fragments of 3G-R5 use the same rule with stale set). It appends the transaction
// (when its first block is a header of that sequence) to the index. A committed transaction returns the block after its
// commit and an empty stop; any other end returns the stop reason (end-of-log@b, descriptor-checksum@b,
// commit-checksum@b, revoke-checksum@b, revoke-damaged@b, unexpected-block@b, cycle@b, cap-txns@b, cap-tags@b,
// cap-revokes@b). Only an I/O or image-cut error is an error.
func (x *jindex) scanTxn(f *FS, start uint32, expected uint32, stale bool) (next uint32, stop string, err error) {
	sb := x.sb
	be := binary.BigEndian
	bs := int(sb.blockSize)
	tail := sb.tailLen()
	t := jtxn{seq: expected, stale: stale, start: int64(start), commit: -1}
	t.tags = [2]uint32{uint32(len(x.tags)), uint32(len(x.tags))}
	t.revokes = [2]uint32{uint32(len(x.revokes)), uint32(len(x.revokes))}
	switch {
	case sb.csum():
		t.csum = "ok"
	case sb.compat&jCompatChecksum != 0:
		t.csum = "not-verified"
	default:
		t.csum = "not-recorded"
	}
	opened := false
	b := start
	// The region-level note for a stop that is damage, not the ordinary end of the log.
	damage := func(stop, why string) {
		t.warnings = append(t.warnings, fmt.Sprintf("%s; the kernel's recovery would stop here", why))
		if !stale {
			x.warn("the live region ends at %s: %s; the kernel's recovery would stop here", stop, why)
		}
	}
	finish := func(stop string) (uint32, string, error) {
		if opened {
			t.tags[1], t.revokes[1] = uint32(len(x.tags)), uint32(len(x.revokes))
			x.txns = append(x.txns, t)
		}
		return b, stop, nil
	}

	for {
		if b >= x.last {
			return finish(fmt.Sprintf("end-of-log@%d", b))
		}
		if x.isConsumed(b) {
			s := fmt.Sprintf("cycle@%d", b)
			if opened {
				t.warnings = append(t.warnings, fmt.Sprintf("the transaction reaches journal block %d again", b))
			}
			x.warn("the %s region revisits journal block %d (%s)", regionName(stale), b, s)
			return finish(s)
		}
		blk, err := x.readBlock(f, b)
		if err != nil {
			return 0, "", err
		}
		if be.Uint32(blk) != jbd2Magic || be.Uint32(blk[8:]) != expected {
			return finish(fmt.Sprintf("end-of-log@%d", b))
		}
		typ := be.Uint32(blk[4:])
		if typ != jbd2Descriptor && typ != jbd2Commit && typ != jbd2Revoke {
			return finish(fmt.Sprintf("unexpected-block@%d", b))
		}
		if !opened {
			if len(x.txns) >= f.jrnTxnCap {
				s := fmt.Sprintf("cap-txns@%d", b)
				x.capHit(s)
				return 0, s, nil
			}
			opened = true
			if t.csum == "not-verified" {
				t.warnings = append(t.warnings, "the commit block's CRC32 (compat checksum) is not verified")
			}
		}
		x.consume(b)

		switch typ {
		case jbd2Descriptor:
			if sb.csum() && !x.tailOK(blk) {
				t.csum = "bad"
				s := fmt.Sprintf("descriptor-checksum@%d", b)
				damage(s, fmt.Sprintf("the descriptor block at journal block %d fails its checksum", b))
				return finish(s)
			}
			rts, err := parseTags(blk, sb)
			if err != nil {
				return 0, "", err
			}
			slot := x.adv(b)
			for _, rt := range rts {
				if len(x.tags) >= f.jrnTagCap {
					s := fmt.Sprintf("cap-tags@%d", slot)
					x.capHit(s)
					return finish(s)
				}
				if x.isConsumed(slot) || slot >= x.last {
					s := fmt.Sprintf("cycle@%d", slot)
					t.warnings = append(t.warnings, fmt.Sprintf("a data slot of the transaction is journal block %d, which the scan already consumed", slot))
					x.warn("the %s region revisits journal block %d (%s)", regionName(stale), slot, s)
					return finish(s)
				}
				data, err := x.readBlock(f, slot)
				if err != nil {
					return 0, "", err
				}
				x.consume(slot)
				g := jtag{fsblk: rt.fsblk, jblk: slot, txn: uint32(len(x.txns)), flags: rt.flags}
				switch {
				case rt.fsblk >= uint64(f.sb.declaredBlocks):
					g.copy = copyOutside
					t.warnings = append(t.warnings, fmt.Sprintf("a tag names block %d, outside the %d-block filesystem; it is never a source", rt.fsblk, f.sb.declaredBlocks))
				case sb.csum():
					var sq [4]byte
					be.PutUint32(sq[:], expected)
					sum := rawCRC32C(rawCRC32C(sb.seed, sq[:]), data)
					if sb.incompat&jIncompatCsumV3 == 0 {
						sum &= 0xFFFF
					}
					if sum == rt.csum {
						g.copy = copyVerified
					} else {
						g.copy = copyMismatch
						if !stale {
							t.csum = "bad"
							t.warnings = append(t.warnings, fmt.Sprintf("the checksum of the copy of block %d at journal block %d does not match; the kernel's recovery would fail here", rt.fsblk, slot))
						}
					}
				default:
					g.copy = copyUnverified
				}
				x.tags = append(x.tags, g)
				slot = x.adv(slot)
			}
			b = slot

		case jbd2Revoke:
			if sb.csum() && !x.tailOK(blk) {
				t.csum = "bad"
				s := fmt.Sprintf("revoke-checksum@%d", b)
				damage(s, fmt.Sprintf("the revoke block at journal block %d fails its checksum", b))
				return finish(s)
			}
			rc := be.Uint32(blk[12:])
			if rc < jRevokeHdr || int64(rc) > int64(bs-tail) {
				s := fmt.Sprintf("revoke-damaged@%d", b)
				damage(s, fmt.Sprintf("the revoke block at journal block %d has r_count %d outside %d..%d", b, rc, jRevokeHdr, bs-tail))
				return finish(s)
			}
			rs := 4
			if sb.incompat&jIncompat64Bit != 0 {
				rs = 8
			}
			if rem := (int(rc) - jRevokeHdr) % rs; rem != 0 {
				t.warnings = append(t.warnings, fmt.Sprintf("the revoke block at journal block %d ends in %d bytes that hold no whole record", b, rem))
			}
			for off := jRevokeHdr; off+rs <= int(rc); off += rs {
				if len(x.revokes) >= f.jrnRevokeCap {
					s := fmt.Sprintf("cap-revokes@%d", b)
					x.capHit(s)
					return finish(s)
				}
				if rs == 8 {
					x.revokes = append(x.revokes, be.Uint64(blk[off:]))
				} else {
					x.revokes = append(x.revokes, uint64(be.Uint32(blk[off:])))
				}
			}
			b = x.adv(b)

		case jbd2Commit:
			if sb.csum() {
				c := append([]byte(nil), blk...)
				clear(c[jCommitSumOff : jCommitSumOff+4])
				if rawCRC32C(sb.seed, c) != be.Uint32(blk[jCommitSumOff:]) {
					t.csum = "bad"
					s := fmt.Sprintf("commit-checksum@%d", b)
					damage(s, fmt.Sprintf("the commit block at journal block %d fails its checksum", b))
					return finish(s)
				}
			}
			t.committed, t.commit = true, int64(b)
			sec, nsec := be.Uint64(blk[jCommitSecOff:]), be.Uint32(blk[jCommitNsecOf:])
			switch {
			case sec == 0 && nsec == 0:
				// no time stamp: unknown
			case sec > maxCommitSec || nsec >= 1_000_000_000:
				t.warnings = append(t.warnings, fmt.Sprintf("the commit time (%d s, %d ns) is not a time; it is reported as unknown", sec, nsec))
			default:
				t.commitTime = filesys.Timestamp{T: time.Unix(int64(sec), int64(nsec)).UTC(), ZoneKnown: true}
			}
			b = x.adv(b)
			next, _, _ = finish("")
			return next, "", nil
		}
	}
}

// tailOK verifies the checksum tail of a descriptor or revoke block: crc32c over the block with the tail zeroed, seeded
// with the journal UUID's checksum (3G-R3).
func (x *jindex) tailOK(blk []byte) bool {
	n := len(blk) - jTailLen
	c := append([]byte(nil), blk...)
	clear(c[n:])
	return rawCRC32C(x.sb.seed, c) == binary.BigEndian.Uint32(blk[n:])
}

func regionName(stale bool) string {
	if stale {
		return "stale"
	}
	return "live"
}
