package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// markIncomplete records that part of the journal was not examined: the result is incomplete (the first reason stays
// the Stop, as with the fast-commit area).
func (x *jindex) markIncomplete(stop string) {
	x.info.Incomplete = true
	if x.info.Stop == "" {
		x.info.Stop = stop
	}
}

// capHit records that a scan cap was reached: the result is incomplete, never complete (3G-R9).
func (x *jindex) capHit(stop string) {
	x.capped = true
	x.markIncomplete(stop)
	x.warn("the journal scan reached a cap (%s); the index is incomplete", stop)
}

// imageEnd records that journal block b lies beyond the end of the image (a cut image, ruling C9): a stop, not a
// corrupt journal. The result is incomplete; the first occurrence is warned, later ones are counted.
func (x *jindex) imageEnd(b uint32) string {
	s := fmt.Sprintf("image-end@%d", b)
	if x.imageEnds++; x.imageEnds == 1 {
		x.markIncomplete(s)
		x.warn("journal block %d lies beyond the end of the image (%s); the journal behind it is not examined", b, s)
	}
	return s
}

// adv is the next log block after b: the log wraps from the end of the log area to s_first.
func (x *jindex) adv(b uint32) uint32 {
	if b++; b >= x.last {
		b = x.sb.first
	}
	return b
}

// errImageEnd is a journal block read that ran past the end of the image.
var errImageEnd = errors.New("ext4 journal: the image ends inside a journal block")

// readRaw reads journal block j through the uncached view into a new buffer. A read past the end of the image is
// errImageEnd; any other failure is returned as it is (an I/O error says nothing about the image and is never cached).
func (x *jindex) readRaw(f *FS, j uint32) ([]byte, error) {
	bs := int64(x.sb.blockSize)
	fsb, ok := x.jm.fsBlock(j, bs)
	if !ok {
		return nil, corrupt("ext4 journal", -1, "journal block %d is outside the mapped journal", j)
	}
	off := int64(fsb) * bs // the block lies in the image, so this cannot overflow
	buf := make([]byte, bs)
	f.jrnBlocks.Add(1)
	if err := readFull(f.data, buf, off); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, errImageEnd
		}
		return nil, err
	}
	return buf, nil
}

// readBlock is the reader of the scans. It remembers the last block it read, because the block that ends one scan
// step is the one the next step starts from: each journal block is read at most once per build (plus the re-reads of
// a consumed block that a clean wrap tests, and one per JournalBlock call). The returned slice is shared and must not
// be modified.
func (x *jindex) readBlock(f *FS, j uint32) ([]byte, error) {
	if x.peekOK && x.peekJ == j {
		return x.peek, nil
	}
	if x.keepOK && x.keepJ == j {
		return x.keep, nil
	}
	if x.endOff >= 0 {
		// A read at or beyond the first offset that failed to read ends in the same way: do not try.
		if off, ok := x.jm.fsBlock(j, int64(x.sb.blockSize)); ok && int64(off)*int64(x.sb.blockSize) >= x.endOff {
			return nil, errImageEnd
		}
	}
	buf, err := x.readRaw(f, j)
	if err != nil {
		if errors.Is(err, errImageEnd) {
			if off, ok := x.jm.fsBlock(j, int64(x.sb.blockSize)); ok && (x.endOff < 0 || int64(off)*int64(x.sb.blockSize) < x.endOff) {
				x.endOff = int64(off) * int64(x.sb.blockSize)
			}
		}
		return nil, err
	}
	x.peekJ, x.peek, x.peekOK = j, buf, true
	return buf, nil
}

// isHeader reports whether blk is a journal header block: the magic with a descriptor, commit or revoke block type
// (3G-R5). Superblock types and unknown types are not headers.
func isHeader(blk []byte) bool {
	be := binary.BigEndian
	if be.Uint32(blk) != jbd2Magic {
		return false
	}
	t := be.Uint32(blk[4:])
	return t == jbd2Descriptor || t == jbd2Commit || t == jbd2Revoke
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
			if len(x.txns) == 0 && !strings.HasPrefix(stop, "cap-") && !strings.HasPrefix(stop, "image-end@") {
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
// commit and an empty stop; any other end returns the stop reason: end-of-log@b (fragment-end@b when stale),
// descriptor-checksum@b, commit-checksum@b, revoke-checksum@b, revoke-damaged@b, unexpected-block@b, cycle@b,
// image-end@b, cap-txns@b, cap-tags@b, cap-revokes@b. Only an I/O error is an error.
//
// A live stop that is damage (a checksum or revoke-damaged stop) also marks the result incomplete: the blocks behind
// it went unexamined (ruling C8). The reader stops there as the oracle does; whether the kernel's recovery does is
// stated only where it is known.
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
	var firstType uint32
	b := start
	endTok := "end-of-log"
	if stale {
		endTok = "fragment-end"
	}
	// The note for a stop that is damage, not the ordinary end of the log. firm is true where the kernel's recovery
	// certainly stops; the checksum cases and r_count below the header are flagged by the kernel from memory.
	damage := func(stop, why string, firm bool) {
		verdict := "the kernel's recovery may stop or fail here (from memory, not probed)"
		if firm {
			verdict = "the kernel's recovery would stop here"
		}
		t.warnings = append(t.warnings, why+"; "+verdict)
		if !stale {
			x.markIncomplete(stop)
			x.warn("the live region ends at %s: %s; %s", stop, why, verdict)
		}
	}
	finish := func(stop string) (uint32, string, error) {
		if opened {
			if stale && !t.committed && stop != "" {
				t.warnings = append(t.warnings, fmt.Sprintf("the fragment has no commit block (%s)", stop))
			}
			t.tags[1], t.revokes[1] = uint32(len(x.tags)), uint32(len(x.revokes))
			x.txns = append(x.txns, t)
		}
		return b, stop, nil
	}

	for {
		if b >= x.last {
			return finish(fmt.Sprintf("%s@%d", endTok, b))
		}
		if stale && x.isConsumed(b) {
			return finish(fmt.Sprintf("%s@%d", endTok, b)) // a wall: the live region or an earlier fragment
		}
		blk, err := x.readBlock(f, b)
		if errors.Is(err, errImageEnd) {
			return finish(x.imageEnd(b))
		}
		if err != nil {
			return 0, "", err
		}
		// The magic and sequence test comes before the consumed test: a full lap of the log that ends cleanly meets
		// a consumed block of an older sequence, which is the end of the log. Only a matching header on a block the
		// scan already consumed is a cycle.
		if be.Uint32(blk) != jbd2Magic || be.Uint32(blk[8:]) != expected {
			return finish(fmt.Sprintf("%s@%d", endTok, b))
		}
		if x.isConsumed(b) {
			s := fmt.Sprintf("cycle@%d", b)
			if opened {
				t.warnings = append(t.warnings, fmt.Sprintf("the transaction reaches journal block %d again", b))
			}
			x.warn("the %s region revisits journal block %d (%s)", regionName(stale), b, s)
			return finish(s)
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
			opened, firstType = true, typ
			if t.csum == "not-verified" {
				t.warnings = append(t.warnings, "the commit block's CRC32 (compat checksum) is not verified")
			}
		}
		x.consume(b)

		switch typ {
		case jbd2Descriptor:
			untrusted := false // stale only: the descriptor fails its checksum, so its tags are read but none is trusted
			if sb.csum() && !x.tailOK(blk) {
				t.csum = "bad"
				s := fmt.Sprintf("descriptor-checksum@%d", b)
				if !stale {
					damage(s, fmt.Sprintf("the descriptor block at journal block %d fails its checksum", b), false)
					return finish(s)
				}
				// A fragment with zero tags would read as "logged nothing"; the tags are unknown, not absent.
				untrusted = true
				t.warnings = append(t.warnings, fmt.Sprintf("the descriptor block at journal block %d fails its checksum", b))
			}
			rts, err := parseTags(blk, sb)
			if err != nil {
				if !untrusted {
					return 0, "", err
				}
				t.warnings = append(t.warnings, "the descriptor is damaged; its tags were not read")
				b = x.adv(b)
				return finish(fmt.Sprintf("fragment-end@%d", b))
			}
			if untrusted {
				t.warnings = append(t.warnings, "the descriptor is damaged; its tags were not trusted")
			}
			slot := x.adv(b)
			missingAt := -1 // stale only: the first data slot without a usable copy; it and the later ones are missing
			for ri, rt := range rts {
				if len(x.tags) >= f.jrnTagCap {
					s := fmt.Sprintf("cap-tags@%d", slot)
					x.capHit(s)
					return finish(s)
				}
				g := jtag{fsblk: rt.fsblk, jblk: slot, txn: uint32(len(x.txns)), flags: rt.flags}
				var data []byte
				if missingAt < 0 {
					var why string
					if x.isConsumed(slot) {
						if !stale {
							s := fmt.Sprintf("cycle@%d", slot)
							t.warnings = append(t.warnings, fmt.Sprintf("a data slot of the transaction is journal block %d, which the scan already consumed", slot))
							x.warn("the live region revisits journal block %d (%s)", slot, s)
							return finish(s)
						}
						why = "lies in the live region or was already consumed"
					} else {
						d, err := x.readBlock(f, slot)
						switch {
						case errors.Is(err, errImageEnd):
							s := x.imageEnd(slot)
							if !stale {
								t.warnings = append(t.warnings, fmt.Sprintf("the tags from journal block %d on (%d of the descriptor) lie beyond the end of the image and were dropped", slot, len(rts)-ri))
								return finish(s)
							}
							why = "lies beyond the end of the image"
						case err != nil:
							return 0, "", err
						case stale && isHeader(d):
							why = "holds a header block"
						default:
							data = d
							x.consume(slot)
						}
					}
					if why != "" {
						missingAt = int(slot)
						t.warnings = append(t.warnings, fmt.Sprintf("the data slot at journal block %d %s; this tag and the later ones of the descriptor have no copy", slot, why))
					}
				}
				switch {
				case missingAt >= 0:
					g.copy = copyMissing
				case rt.fsblk >= uint64(f.sb.declaredBlocks):
					// The copy state records one fact per tag, and "outside" wins: a tag that names no block of this
					// filesystem is never a source whatever its checksum says, so the checksum is not evaluated and
					// does not make the transaction's checksum "bad".
					g.copy = copyOutside
					t.warnings = append(t.warnings, fmt.Sprintf("a tag names block %d, outside the %d-block filesystem; it is never a source", rt.fsblk, f.sb.declaredBlocks))
				case untrusted:
					g.copy = copyUnverified // never verified: the descriptor that vouches for the tag failed its own checksum
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
						// In the stale region a mismatch says a later lap overwrote the slot; it is never "verified".
						g.copy = copyMismatch
						if !stale {
							t.csum = "bad"
							t.warnings = append(t.warnings, fmt.Sprintf("the checksum of the copy of block %d at journal block %d does not match; the kernel's recovery may fail here (from memory, not probed)", rt.fsblk, slot))
						}
					}
				default:
					g.copy = copyUnverified
				}
				x.tags = append(x.tags, g)
				slot = x.adv(slot)
			}
			if missingAt >= 0 {
				b = uint32(missingAt)
				return finish(fmt.Sprintf("fragment-end@%d", b))
			}
			b = slot

		case jbd2Revoke:
			if sb.csum() && !x.tailOK(blk) {
				t.csum = "bad"
				s := fmt.Sprintf("revoke-checksum@%d", b)
				damage(s, fmt.Sprintf("the revoke block at journal block %d fails its checksum", b), false)
				return finish(s)
			}
			rc := be.Uint32(blk[12:])
			if rc < jRevokeHdr || int64(rc) > int64(bs-tail) {
				s := fmt.Sprintf("revoke-damaged@%d", b)
				// Above the block the kernel's recovery stops; below the header it is from memory only.
				damage(s, fmt.Sprintf("the revoke block at journal block %d has r_count %d outside %d..%d", b, rc, jRevokeHdr, bs-tail), rc >= jRevokeHdr)
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
					damage(s, fmt.Sprintf("the commit block at journal block %d fails its checksum", b), false)
					return finish(s)
				}
			}
			t.committed, t.commit = true, int64(b)
			if stale && firstType == jbd2Commit {
				// Expected, not an anomaly: every mount restarts logging at s_first, so a transaction that straddles
				// the restart point of a later boot loses its descriptor and keeps its commit block.
				t.warnings = append(t.warnings, "the descriptor blocks of this transaction are overwritten")
			}
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
