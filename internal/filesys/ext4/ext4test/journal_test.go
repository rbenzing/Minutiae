package ext4test_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// TestExistingImagesUnchangedByJournalFields pins the bytes of two images built
// without JournalBlocks. The digests were taken from the tree before the
// journal fields existed (3G Task 2), so adding the fields cannot move a byte
// of any earlier image.
func TestExistingImagesUnchangedByJournalFields(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a.txt", Data: pattern(3000, 1)},
		{Path: "/d", Dir: true},
		{Path: "/d/gone.bin", Data: pattern(2500, 5), Deleted: true},
	}
	for _, c := range []struct {
		name string
		o    ext4test.Options
		want string
	}{
		{"extents-csum", ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, Journal: true, UUID: lUUID}, "a0e1feef6f51a1d6a1406e71705cc68fe03e0b0f825c39f5f141c18da5a57669"},
		{"blockmap", ext4test.Options{Journal: true, UUID: lUUID, Groups: 2}, "c1dcfe490d3087b7c138f88265bc170d2b3000c3eede11117cc40b55fc208611"},
	} {
		sum := sha256.Sum256(ext4test.Build(c.o, files))
		if got := hex.EncodeToString(sum[:]); got != c.want {
			t.Errorf("%s: sha256 %s, want %s", c.name, got, c.want)
		}
	}
}

// ---- an in-test big-endian jbd2 decoder; it never uses the reader ----

var jUUID = [16]byte{0xA1, 0xB2, 0xC3, 0xD4, 0xE5, 0xF6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

func fsOpt(extents bool, jblocks int) ext4test.Options {
	return ext4test.Options{
		Extents: extents, MetadataCsum: true, Bit64: true, UUID: lUUID,
		JournalBlocks: jblocks, Groups: 2, BlocksPerGroup: 2048,
	}
}

func be16(b []byte, off int) uint16 { return binary.BigEndian.Uint16(b[off:]) }
func be32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }
func be64(b []byte, off int) uint64 { return binary.BigEndian.Uint64(b[off:]) }

func jseed(u [16]byte) uint32 { return rawCRC32C(0xFFFFFFFF, u[:]) }

// jblk returns journal block k of the image.
func jblk(img []byte, l *ext4test.Layout, k int) []byte {
	fs := l.JournalFSBlock(k)
	return img[fs*lBS : (fs+1)*lBS]
}

// zeroed returns a copy of b with n bytes at off cleared.
func zeroed(b []byte, off, n int) []byte {
	c := slices.Clone(b)
	clear(c[off : off+n])
	return c
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func jlayout(t *testing.T, extents bool, jblocks int) ([]byte, *ext4test.Layout) {
	t.Helper()
	return ext4test.BuildLayout(fsOpt(extents, jblocks), []ext4test.File{
		{Path: "/a.txt", Data: pattern(3000, 1)},
	})
}

// jsuperOf checks the common superblock facts and returns the block.
func jsuperOf(t *testing.T, img []byte, l *ext4test.Layout, maxlen int) []byte {
	t.Helper()
	sb := jblk(img, l, 0)
	if be32(sb, 0) != ext4test.JBD2Magic || be32(sb, 4) != 4 || be32(sb, 8) != 0 {
		t.Fatalf("superblock header % x", sb[:12])
	}
	if be32(sb, 0xC) != lBS || int(be32(sb, 0x10)) != maxlen {
		t.Fatalf("blocksize %d maxlen %d", be32(sb, 0xC), be32(sb, 0x10))
	}
	return sb
}

func checkSuperCsum(t *testing.T, sb []byte) {
	t.Helper()
	if got, want := be32(sb, 0xFC), rawCRC32C(0xFFFFFFFF, zeroed(sb, 0xFC, 4)); got != want {
		t.Errorf("s_checksum %08x, want %08x", got, want)
	}
}

func TestJournalInodeMapsContiguousBlocks(t *testing.T) {
	for _, extents := range []bool{true, false} {
		base, _ := jlayout(t, extents, 0)
		baseWarn := warnings(t, base)
		img, l := jlayout(t, extents, 256)
		if l.JournalInode != 8 || l.JournalStart == 0 {
			t.Fatalf("extents=%v: Layout journal inode %d start %d", extents, l.JournalInode, l.JournalStart)
		}
		if got := binary.LittleEndian.Uint32(img[1024+0xE0:]); got != 8 {
			t.Errorf("extents=%v: s_journal_inum %d, want 8", extents, got)
		}
		if binary.LittleEndian.Uint32(img[1024+0x5C:])&0x4 == 0 {
			t.Errorf("extents=%v: has_journal not set", extents)
		}
		fsblk, o := l.InodeTableBlock(8)
		raw := img[fsblk*lBS+o : fsblk*lBS+o+256]
		if mode := binary.LittleEndian.Uint16(raw); mode&0xF000 != 0x8000 {
			t.Errorf("extents=%v: inode 8 mode %#o, want a regular file", extents, mode)
		}
		if sz := binary.LittleEndian.Uint32(raw[4:]); sz != 256*lBS {
			t.Errorf("extents=%v: inode 8 size %d", extents, sz)
		}
		if links := binary.LittleEndian.Uint16(raw[0x1A:]); links != 1 {
			t.Errorf("extents=%v: inode 8 links %d", extents, links)
		}
		var mapped []int
		if extents {
			mapped = extentBlocks(t, raw[0x28:0x28+60])
		} else {
			mapped = mapBlocks(img, raw[0x28:0x28+60])
		}
		if len(mapped) != 256 {
			t.Fatalf("extents=%v: inode 8 maps %d blocks, want 256", extents, len(mapped))
		}
		for k, blk := range mapped {
			if blk != l.JournalFSBlock(k) || blk != l.JournalStart+k {
				t.Fatalf("extents=%v: journal block %d maps to %d, JournalFSBlock %d", extents, k, blk, l.JournalFSBlock(k))
			}
		}
		if got := warnings(t, img); !slices.Equal(got, baseWarn) {
			t.Errorf("extents=%v: warnings %v, want %v", extents, got, baseWarn)
		}
		free := freeSet(t, img)
		for k := range 256 {
			if free[l.JournalFSBlock(k)] {
				t.Fatalf("extents=%v: journal block %d is in Unallocated()", extents, k)
			}
		}
	}
}

// extentBlocks decodes a depth-0 extent root.
func extentBlocks(t *testing.T, root []byte) []int {
	t.Helper()
	if binary.LittleEndian.Uint16(root) != 0xF30A || binary.LittleEndian.Uint16(root[6:]) != 0 {
		t.Fatalf("extent root % x", root[:12])
	}
	var out []int
	for i := range int(binary.LittleEndian.Uint16(root[2:])) {
		e := root[12+12*i:]
		lblk, n := int(binary.LittleEndian.Uint32(e)), int(binary.LittleEndian.Uint16(e[4:]))
		start := int(binary.LittleEndian.Uint32(e[8:]))
		if lblk != len(out) {
			t.Fatalf("extent %d starts at logical %d, want %d", i, lblk, len(out))
		}
		for k := range n {
			out = append(out, start+k)
		}
	}
	return out
}

// mapBlocks decodes a block map (direct, single and double indirect).
func mapBlocks(img []byte, iblock []byte) []int {
	var out []int
	ptr := func(b []byte, i int) int { return int(binary.LittleEndian.Uint32(b[4*i:])) }
	var walk func(blk, level int)
	walk = func(blk, level int) {
		if blk == 0 {
			return
		}
		b := img[blk*lBS : (blk+1)*lBS]
		for i := range lBS / 4 {
			if p := ptr(b, i); p != 0 {
				if level == 1 {
					out = append(out, p)
				} else {
					walk(p, level-1)
				}
			}
		}
	}
	for i := range 12 {
		if p := ptr(iblock, i); p != 0 {
			out = append(out, p)
		}
	}
	walk(ptr(iblock, 12), 1)
	walk(ptr(iblock, 13), 2)
	return out
}

func TestWriteJournalLayoutV3(t *testing.T) {
	img, l := jlayout(t, true, 256)
	d1, d2 := pattern(lBS, 11), pattern(lBS, 77)
	l.WriteJournal(img, ext4test.JBD2{
		CsumV3: true, Bit64: true, UUID: jUUID, Sequence: 5, Start: 1,
		Txns: []ext4test.JTxn{{
			Seq: 5, At: -1, CommitSec: 1700000123, CommitNsec: 456,
			Tags: []ext4test.JTag{{FSBlock: 300, Data: d1}, {FSBlock: 1<<32 | 301, Data: d2}},
		}},
	})
	sb := jsuperOf(t, img, l, 256)
	if be32(sb, 0x14) != 1 || be32(sb, 0x18) != 5 || be32(sb, 0x1C) != 1 {
		t.Errorf("first %d sequence %d start %d", be32(sb, 0x14), be32(sb, 0x18), be32(sb, 0x1C))
	}
	if be32(sb, 0x24) != 0 || be32(sb, 0x28) != 0x2|0x10 || be32(sb, 0x2C) != 0 {
		t.Errorf("features %x %x %x, want 0 0x12 0", be32(sb, 0x24), be32(sb, 0x28), be32(sb, 0x2C))
	}
	if sb[0x50] != 4 || !bytes.Equal(sb[0x30:0x40], jUUID[:]) {
		t.Errorf("checksum type %d, uuid % x", sb[0x50], sb[0x30:0x40])
	}
	checkSuperCsum(t, sb)

	seed := jseed(jUUID)
	desc := jblk(img, l, 1)
	if be32(desc, 0) != ext4test.JBD2Magic || be32(desc, 4) != 1 || be32(desc, 8) != 5 {
		t.Fatalf("descriptor header % x", desc[:12])
	}
	// tag 1 at 12 (16 bytes) + the journal UUID; tag 2 at 44 (SAME_UUID|LAST_TAG).
	if be32(desc, 12) != 300 || be32(desc, 16) != 0 || be32(desc, 20) != 0 {
		t.Errorf("tag 1 % x", desc[12:28])
	}
	if !bytes.Equal(desc[28:44], jUUID[:]) {
		t.Errorf("uuid after tag 1 % x", desc[28:44])
	}
	if be32(desc, 44) != 301 || be32(desc, 48) != ext4test.JTagSameUUID|ext4test.JTagLast || be32(desc, 52) != 1 {
		t.Errorf("tag 2 % x", desc[44:60])
	}
	seq := rawCRC32C(seed, []byte{0, 0, 0, 5})
	if got, want := be32(desc, 24), rawCRC32C(seq, d1); got != want {
		t.Errorf("tag 1 checksum %08x, want %08x", got, want)
	}
	if got, want := be32(desc, 56), rawCRC32C(seq, d2); got != want {
		t.Errorf("tag 2 checksum %08x, want %08x", got, want)
	}
	if got, want := be32(desc, lBS-4), rawCRC32C(seed, zeroed(desc, lBS-4, 4)); got != want {
		t.Errorf("descriptor tail %08x, want %08x", got, want)
	}
	if !bytes.Equal(jblk(img, l, 2), d1) || !bytes.Equal(jblk(img, l, 3), d2) {
		t.Error("data slots differ from the data")
	}
	cm := jblk(img, l, 4)
	if be32(cm, 0) != ext4test.JBD2Magic || be32(cm, 4) != 2 || be32(cm, 8) != 5 {
		t.Fatalf("commit header % x", cm[:12])
	}
	if cm[0xC] != 0 || cm[0xD] != 0 {
		t.Errorf("commit chksum_type %d size %d, want 0 0 with csum v3", cm[0xC], cm[0xD])
	}
	if be64(cm, 0x30) != 1700000123 || be32(cm, 0x38) != 456 {
		t.Errorf("commit time %d.%d", be64(cm, 0x30), be32(cm, 0x38))
	}
	if got, want := be32(cm, 0x10), rawCRC32C(seed, zeroed(cm, 0x10, 4)); got != want {
		t.Errorf("commit checksum %08x, want %08x", got, want)
	}
	if !allZero(jblk(img, l, 5)) {
		t.Error("journal block 5 is not zero")
	}
}

func TestWriteJournalLayoutNoCsum(t *testing.T) {
	img, l := jlayout(t, false, 64)
	d1, d2 := pattern(lBS, 3), pattern(lBS, 9)
	l.WriteJournal(img, ext4test.JBD2{
		UUID: jUUID, Sequence: 9, Start: 1,
		Txns: []ext4test.JTxn{{Seq: 9, At: -1, Tags: []ext4test.JTag{{FSBlock: 40, Data: d1}, {FSBlock: 41, Data: d2}}}},
	})
	sb := jsuperOf(t, img, l, 64)
	if be32(sb, 0x28) != 0 || sb[0x50] != 0 || be32(sb, 0xFC) != 0 {
		t.Errorf("incompat %x, checksum type %d, s_checksum %x, want none", be32(sb, 0x28), sb[0x50], be32(sb, 0xFC))
	}
	desc := jblk(img, l, 1)
	// 8-byte tags: blocknr, checksum be16 (0), flags be16.
	if be32(desc, 12) != 40 || be16(desc, 16) != 0 || be16(desc, 18) != 0 {
		t.Errorf("tag 1 % x", desc[12:20])
	}
	if !bytes.Equal(desc[20:36], jUUID[:]) {
		t.Errorf("uuid after tag 1 % x", desc[20:36])
	}
	if be32(desc, 36) != 41 || be16(desc, 40) != 0 || be16(desc, 42) != ext4test.JTagSameUUID|ext4test.JTagLast {
		t.Errorf("tag 2 % x", desc[36:44])
	}
	if !allZero(desc[44:]) {
		t.Error("descriptor has bytes after the tags (a tail?) without checksum features")
	}
	cm := jblk(img, l, 4)
	if be32(cm, 4) != 2 || be32(cm, 0x10) != 0 {
		t.Errorf("commit % x", cm[:0x14])
	}
}

// csum_v2 is from memory of the kernel header and is never written by the
// kernels the probe ran (3G-R4): 14-byte tags with 64bit, 10-byte without.
func TestWriteJournalLayoutCsumV2(t *testing.T) {
	for _, bit64 := range []bool{true, false} {
		img, l := jlayout(t, true, 64)
		d1, d2 := pattern(lBS, 21), pattern(lBS, 22)
		l.WriteJournal(img, ext4test.JBD2{
			CsumV2: true, Bit64: bit64, UUID: jUUID, Sequence: 2, Start: 1,
			Txns: []ext4test.JTxn{{Seq: 2, At: -1, Tags: []ext4test.JTag{{FSBlock: 50, Data: d1}, {FSBlock: 51, Data: d2}}}},
		})
		sb := jsuperOf(t, img, l, 64)
		wantInc := uint32(0x8)
		size := 10
		if bit64 {
			wantInc |= 0x2
			size = 14
		}
		if be32(sb, 0x28) != wantInc || sb[0x50] != 4 {
			t.Errorf("bit64=%v: incompat %x type %d, want %x 4", bit64, be32(sb, 0x28), sb[0x50], wantInc)
		}
		checkSuperCsum(t, sb)
		seed := jseed(jUUID)
		desc := jblk(img, l, 1)
		t2 := 12 + size + 16 // the second tag follows the first tag and the UUID
		if be32(desc, 12) != 50 || be16(desc, 18) != 0 || !bytes.Equal(desc[12+size:12+size+16], jUUID[:]) {
			t.Errorf("bit64=%v: tag 1 % x", bit64, desc[12:t2])
		}
		if be32(desc, t2) != 51 || be16(desc, t2+6) != ext4test.JTagSameUUID|ext4test.JTagLast {
			t.Errorf("bit64=%v: tag 2 % x", bit64, desc[t2:t2+size])
		}
		seq := rawCRC32C(seed, []byte{0, 0, 0, 2})
		if got, want := be16(desc, 16), uint16(rawCRC32C(seq, d1)); got != want {
			t.Errorf("bit64=%v: tag 1 checksum %04x, want %04x", bit64, got, want)
		}
		if got, want := be16(desc, t2+4), uint16(rawCRC32C(seq, d2)); got != want {
			t.Errorf("bit64=%v: tag 2 checksum %04x, want %04x", bit64, got, want)
		}
		// the two padding bytes at the end of a 14- or 10-byte tag are zero
		if be16(desc, t2+size-2) != 0 {
			t.Errorf("bit64=%v: tag 2 padding % x", bit64, desc[t2+size-2:t2+size])
		}
		if !allZero(desc[t2+size : lBS-4]) {
			t.Errorf("bit64=%v: bytes after tag 2", bit64)
		}
		if got, want := be32(desc, lBS-4), rawCRC32C(seed, zeroed(desc, lBS-4, 4)); got != want {
			t.Errorf("bit64=%v: descriptor tail %08x, want %08x", bit64, got, want)
		}
	}
}

func TestWriteJournalEscapes(t *testing.T) {
	img, l := jlayout(t, true, 64)
	data := pattern(lBS, 5)
	binary.BigEndian.PutUint32(data, ext4test.JBD2Magic)
	l.WriteJournal(img, ext4test.JBD2{
		CsumV3: true, Bit64: true, UUID: jUUID, Sequence: 1, Start: 1,
		Txns: []ext4test.JTxn{{Seq: 1, At: -1, Tags: []ext4test.JTag{{FSBlock: 60, Data: data}}}},
	})
	desc, slot := jblk(img, l, 1), jblk(img, l, 2)
	if be32(desc, 16)&ext4test.JTagEscape == 0 {
		t.Errorf("tag flags %x lack ESCAPE", be32(desc, 16))
	}
	if be32(slot, 0) != 0 || !bytes.Equal(slot[4:], data[4:]) {
		t.Error("escaped slot is not the data with its first four bytes zeroed")
	}
	// the checksum covers the block as logged (escaped)
	seq := rawCRC32C(jseed(jUUID), []byte{0, 0, 0, 1})
	if got, want := be32(desc, 24), rawCRC32C(seq, slot); got != want {
		t.Errorf("tag checksum %08x, want %08x over the logged bytes", got, want)
	}
	if be32(data, 0) != ext4test.JBD2Magic {
		t.Error("the caller's data was modified")
	}
}

func TestWriteJournalWrapsAndOverwrites(t *testing.T) {
	img, l := jlayout(t, true, 16)
	d1, d2, over := pattern(lBS, 31), pattern(lBS, 32), pattern(lBS, 99)
	l.WriteJournal(img, ext4test.JBD2{
		CsumV3: true, Bit64: true, UUID: jUUID, Sequence: 8, Start: 15,
		Txns: []ext4test.JTxn{{
			Seq: 8, At: 15,
			Tags: []ext4test.JTag{{FSBlock: 70, Data: d1}, {FSBlock: 71, Data: d2, Overwrite: over}},
		}},
	})
	sb := jsuperOf(t, img, l, 16)
	if be32(sb, 0x1C) != 15 || be32(sb, 0x14) != 1 {
		t.Errorf("start %d first %d", be32(sb, 0x1C), be32(sb, 0x14))
	}
	if be32(jblk(img, l, 15), 4) != 1 {
		t.Error("descriptor is not at journal block 15")
	}
	if !bytes.Equal(jblk(img, l, 1), d1) {
		t.Error("the first data slot did not wrap to s_first (block 1)")
	}
	if !bytes.Equal(jblk(img, l, 2), over) {
		t.Error("the Overwrite did not replace the second slot")
	}
	cm := jblk(img, l, 3)
	if be32(cm, 4) != 2 || be32(cm, 8) != 8 {
		t.Errorf("commit % x, want it at block 3", cm[:12])
	}
	// the tag checksum was taken over the data, not the overwrite
	desc := jblk(img, l, 15)
	seq := rawCRC32C(jseed(jUUID), []byte{0, 0, 0, 8})
	if got, want := be32(desc, 56), rawCRC32C(seq, d2); got != want {
		t.Errorf("tag 2 checksum %08x, want %08x over the original data", got, want)
	}
}

func TestWriteJournalRevokeBlock(t *testing.T) {
	for _, bit64 := range []bool{true, false} {
		img, l := jlayout(t, true, 64)
		recs := []uint64{7, 9, 11}
		if bit64 {
			recs[1] = 1<<33 | 9
		}
		l.WriteJournal(img, ext4test.JBD2{
			CsumV3: true, Bit64: bit64, UUID: jUUID, Sequence: 3, Start: 1,
			Txns: []ext4test.JTxn{{Seq: 3, At: -1, Revokes: recs}},
		})
		sb := jsuperOf(t, img, l, 64)
		wantInc := uint32(0x1 | 0x10) // revoke | csum v3
		rs := 4
		if bit64 {
			wantInc |= 0x2
			rs = 8
		}
		if be32(sb, 0x28) != wantInc {
			t.Errorf("bit64=%v: incompat %x, want %x", bit64, be32(sb, 0x28), wantInc)
		}
		rv := jblk(img, l, 1)
		if be32(rv, 4) != 5 || be32(rv, 8) != 3 || be32(rv, 0xC) != uint32(16+3*rs) {
			t.Fatalf("bit64=%v: revoke header % x", bit64, rv[:16])
		}
		for i, want := range recs {
			var got uint64
			if bit64 {
				got = be64(rv, 16+i*8)
			} else {
				got = uint64(be32(rv, 16+i*4))
			}
			if got != want {
				t.Errorf("bit64=%v: record %d = %d, want %d", bit64, i, got, want)
			}
		}
		if got, want := be32(rv, lBS-4), rawCRC32C(jseed(jUUID), zeroed(rv, lBS-4, 4)); got != want {
			t.Errorf("bit64=%v: revoke tail %08x, want %08x", bit64, got, want)
		}
		if be32(jblk(img, l, 2), 4) != 2 {
			t.Errorf("bit64=%v: the commit does not follow the revoke block", bit64)
		}
	}

	// More records than one block holds go into further revoke blocks, all
	// written before the commit.
	img, l := jlayout(t, true, 64)
	var many []uint64
	for i := range 300 {
		many = append(many, uint64(1000+i))
	}
	l.WriteJournal(img, ext4test.JBD2{
		CsumV3: true, Bit64: true, UUID: jUUID, Sequence: 3, Start: 1,
		Txns: []ext4test.JTxn{{Seq: 3, At: -1, Revokes: many}},
	})
	var got []uint64
	k := 1
	for ; be32(jblk(img, l, k), 4) == 5; k++ {
		rv := jblk(img, l, k)
		n := int(be32(rv, 0xC))
		if n > lBS-4 {
			t.Fatalf("r_count %d runs into the tail", n)
		}
		for off := 16; off < n; off += 8 {
			got = append(got, be64(rv, off))
		}
	}
	if !slices.Equal(got, many) || k != 4 || be32(jblk(img, l, k), 4) != 2 {
		t.Errorf("300 revokes: %d records in %d blocks, commit at %d", len(got), k-1, k)
	}
}

func TestInodeCopyEditsOneSlot(t *testing.T) {
	img, l := jlayout(t, true, 0)
	loc := l.File("/a.txt")
	orig := slices.Clone(img)
	fsblk, off := l.InodeTableBlock(loc.Inode)
	if want := loc.InodeOffset; int64(fsblk*lBS+off) != want {
		t.Fatalf("InodeTableBlock(%d) = %d,%d, want offset %d", loc.Inode, fsblk, off, want)
	}
	cp := l.InodeCopy(img, loc.Inode, func(raw []byte) {
		binary.LittleEndian.PutUint16(raw[0x1A:], 0)
		binary.LittleEndian.PutUint32(raw[0x14:], 0x55AA)
	})
	if !bytes.Equal(img, orig) {
		t.Fatal("InodeCopy modified the image")
	}
	if len(cp) != lBS {
		t.Fatalf("copy is %d bytes", len(cp))
	}
	src := img[fsblk*lBS : (fsblk+1)*lBS]
	for i := range cp {
		if inSlot := i >= off && i < off+256; !inSlot && cp[i] != src[i] {
			t.Fatalf("byte %d outside the slot differs", i)
		}
	}
	slot := cp[off : off+256]
	if binary.LittleEndian.Uint16(slot[0x1A:]) != 0 || binary.LittleEndian.Uint32(slot[0x14:]) != 0x55AA {
		t.Error("edit not applied")
	}
	// only the edited fields and the checksum halves differ
	diffs := diffBytes(slot, src[off:off+256])
	if len(diffs) == 0 {
		t.Fatal("the slot is unchanged")
	}
	for _, i := range diffs {
		switch {
		case i >= 0x1A && i < 0x1C, i >= 0x14 && i < 0x18, i == 0x7C, i == 0x7D, i == 0x82, i == 0x83:
		default:
			t.Errorf("byte %#x of the slot differs unexpectedly", i)
		}
	}
	// the inode checksum verifies (in-test crc32c)
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[:], loc.Inode)
	copy(hdr[4:], slot[0x64:0x68])
	z := slices.Clone(slot)
	clear(z[0x7C:0x7E])
	clear(z[0x82:0x84])
	c := rawCRC32C(rawCRC32C(rawCRC32C(0xFFFFFFFF, lUUID[:]), hdr[:]), z)
	if got := uint32(binary.LittleEndian.Uint16(slot[0x7C:])) | uint32(binary.LittleEndian.Uint16(slot[0x82:]))<<16; got != c {
		t.Errorf("inode checksum %08x, want %08x", got, c)
	}
	bc := l.BlockCopy(img, fsblk)
	if !bytes.Equal(bc, src) || &bc[0] == &src[0] {
		t.Error("BlockCopy is not an independent copy of the block")
	}
}
