package ext4

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func TestJournalTagEntryIsCompact(t *testing.T) {
	if n := unsafe.Sizeof(jtag{}); n > 24 {
		t.Errorf("a journal tag index entry is %d bytes; want at most 24 (3G-R9)", n)
	}
}

func TestSeqOrderWraps(t *testing.T) {
	for _, tc := range []struct {
		a, b           uint32
		newer, greater bool // seqNewer(a, b), seqGE(a, b)
	}{
		{1, 0xFFFFFFFF, true, true}, // 1 comes after 0xFFFFFFFF
		{0xFFFFFFFF, 1, false, false},
		{5, 5, false, true},
		{6, 5, true, true},
		{5, 6, false, false},
		{0, 0x7FFFFFFF, false, false}, // the half-range boundary: 0x7FFFFFFF is ahead of 0
		{0x7FFFFFFF, 0, true, true},
		{0x80000000, 0, false, false}, // exactly half a lap apart: int32 difference is negative
	} {
		if got := seqNewer(tc.a, tc.b); got != tc.newer {
			t.Errorf("seqNewer(%#x, %#x) = %v; want %v", tc.a, tc.b, got, tc.newer)
		}
		if got := seqGE(tc.a, tc.b); got != tc.greater {
			t.Errorf("seqGE(%#x, %#x) = %v; want %v", tc.a, tc.b, got, tc.greater)
		}
	}
}

func TestTagSizes(t *testing.T) {
	for _, tc := range []struct {
		name string
		sb   jsuper
		want int
	}{
		{"csum v3", jsuper{version: 2, incompat: jIncompatCsumV3}, 16},
		{"csum v3, 64bit", jsuper{version: 2, incompat: jIncompatCsumV3 | jIncompat64Bit}, 16},
		{"csum v2, 64bit", jsuper{version: 2, incompat: jIncompatCsumV2 | jIncompat64Bit}, 14},
		{"csum v2", jsuper{version: 2, incompat: jIncompatCsumV2}, 10},
		{"64bit", jsuper{version: 2, incompat: jIncompat64Bit}, 12},
		{"nothing", jsuper{version: 2}, 8},
		{"v1", jsuper{version: 1}, 8},
	} {
		if got := tagSize(&tc.sb); got != tc.want {
			t.Errorf("%s: tagSize = %d; want %d", tc.name, got, tc.want)
		}
	}
}

func TestParseTagsBoundedByBlock(t *testing.T) {
	sb := &jsuper{version: 2, incompat: jIncompat64Bit | jIncompatCsumV3, blockSize: 1024}
	const bs = 1024
	// A descriptor of 16-byte tags that all say SAME_UUID and none says LAST_TAG: the parse stops at the block's
	// capacity ((1024 - 12 - 4) / 16 = 63), never reading the checksum tail as a tag.
	blk := make([]byte, bs)
	for off := 12; off+16 <= bs; off += 16 {
		binary.BigEndian.PutUint32(blk[off:], uint32(off))
		binary.BigEndian.PutUint32(blk[off+4:], tagSameUUID)
	}
	tags, err := parseTags(blk, sb)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 63 {
		t.Errorf("%d tags without LAST_TAG; want 63 (the capacity of the block)", len(tags))
	}
	if len(tags) > 0 && tags[len(tags)-1].fsblk != uint64(12+62*16) {
		t.Errorf("last tag block %d; want %d", tags[len(tags)-1].fsblk, 12+62*16)
	}

	// LAST_TAG ends the parse early, and the UUID of a tag without SAME_UUID is skipped (16 + 16 bytes).
	blk = make([]byte, bs)
	binary.BigEndian.PutUint32(blk[12:], 7) // no SAME_UUID: 16 bytes of UUID follow
	binary.BigEndian.PutUint32(blk[12+32:], 8)
	binary.BigEndian.PutUint32(blk[12+32+4:], tagSameUUID|tagLast)
	binary.BigEndian.PutUint32(blk[12+48:], 9) // after LAST_TAG: not a tag
	tags, err = parseTags(blk, sb)
	if err != nil || len(tags) != 2 || tags[0].fsblk != 7 || tags[1].fsblk != 8 || tags[1].flags != tagSameUUID|tagLast {
		t.Errorf("parseTags = %+v, %v; want the tags of blocks 7 and 8", tags, err)
	}

	// Tags that all lack SAME_UUID run out of the block after the UUIDs: bounded, no panic.
	blk = make([]byte, bs)
	tags, err = parseTags(blk, sb)
	if err != nil || len(tags) != (bs-12-4+31)/32 {
		t.Errorf("%d tags of 32 bytes each in a zero block, err %v; want %d", len(tags), err, (bs-12-4+31)/32)
	}

	// A block too short to hold a header is an error, not a panic.
	if _, err := parseTags(make([]byte, 8), sb); err == nil {
		t.Error("an 8-byte descriptor parsed without error")
	}
}
