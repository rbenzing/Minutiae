package exfattest

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func mustPanicLabelled(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if s, ok := r.(string); !ok || !strings.HasPrefix(s, "exfattest: ") {
			t.Errorf("%s: panic %v is not a labelled exfattest panic", name, r)
		}
	}()
	f()
}

func patchImage(t *testing.T) ([]byte, *Layout, EntryLoc) {
	t.Helper()
	data := bytes.Repeat([]byte("0123456789abcdef"), 600) // several clusters at the default size
	img, l := BuildLayout(Options{}, []File{
		{Path: "/keep.bin", Data: []byte("live")},
		{Path: "/gone.bin", Data: data, Deleted: true, FatChain: true},
	})
	return img, l, l.Find("/gone.bin", true)
}

func TestSetFATEntryLE32(t *testing.T) {
	img, l, _ := patchImage(t)
	before := bytes.Clone(img)
	SetFATEntry(img, l, 9, 0x12345678)
	off := l.FatEntryOffset(9)
	if !bytes.Equal(img[off:off+4], []byte{0x78, 0x56, 0x34, 0x12}) {
		t.Fatalf("entry bytes % x", img[off:off+4])
	}
	img2 := bytes.Clone(img)
	copy(img2[off:], before[off:off+4])
	if !bytes.Equal(img2, before) {
		t.Fatal("SetFATEntry changed bytes outside the entry")
	}
	SetFATEntry(img, l, 9, 0xFFFFFFFF) // a FAT32-style exFAT entry holds all 32 bits
	if binary.LittleEndian.Uint32(img[off:]) != 0xFFFFFFFF {
		t.Fatal("full-width value not stored")
	}
}

func TestSetFATEntryRejectsClustersOutsideTheTable(t *testing.T) {
	img, l, _ := patchImage(t)
	before := bytes.Clone(img)
	for _, c := range []uint32{0, 1, l.ClusterCount + 2, l.ClusterCount + 1000} {
		mustPanicLabelled(t, "cluster outside [2, count+1]", func() { SetFATEntry(img, l, c, 5) })
	}
	if !bytes.Equal(img, before) {
		t.Fatal("a rejected write changed the image")
	}
	SetFATEntry(img, l, 2, 5)
	SetFATEntry(img, l, l.ClusterCount+1, 6)
}

func TestSetBitmapBitRoundTrip(t *testing.T) {
	img, l, _ := patchImage(t)
	const c = 7
	idx := (c - 2) / 8
	at := l.ClusterOffset(l.BitmapClusters[0]) + int64(idx)
	bit := byte(1) << ((c - 2) % 8)
	for _, want := range []bool{true, false, true, false} {
		before := bytes.Clone(img)
		old := img[at]
		SetBitmapBit(img, l, c, want)
		wantByte := old &^ bit
		if want {
			wantByte |= bit
		}
		if img[at] != wantByte {
			t.Fatalf("allocated=%v: byte %08b, want %08b", want, img[at], wantByte)
		}
		img2 := bytes.Clone(img)
		img2[at] = before[at]
		if !bytes.Equal(img2, before) {
			t.Fatalf("allocated=%v: bytes other than the bitmap byte changed", want)
		}
	}
}

func TestSetBitmapBitLastClusterAndBounds(t *testing.T) {
	img, l, _ := patchImage(t)
	last := l.ClusterCount + 1
	i := int(last - 2)
	at := l.ClusterOffset(l.BitmapClusters[(i/8)/l.ClusterSize]) + int64((i/8)%l.ClusterSize)
	SetBitmapBit(img, l, last, true)
	if img[at]&(1<<(i%8)) == 0 {
		t.Fatal("the last cluster's bit is not set where the bitmap puts it")
	}
	before := bytes.Clone(img)
	for _, c := range []uint32{0, 1, l.ClusterCount + 2} {
		mustPanicLabelled(t, "bitmap cluster out of range", func() { SetBitmapBit(img, l, c, true) })
	}
	if !bytes.Equal(img, before) {
		t.Fatal("a rejected write changed the image")
	}
}

func streamOf(img []byte, loc EntryLoc) []byte { return img[loc.Offset+32 : loc.Offset+64] }

func TestPatchStreamKeepsChecksumConsistent(t *testing.T) {
	img, _, loc := patchImage(t)
	n := (int(img[loc.Offset+1]) + 1) * entrySize
	stored := func() uint16 { return binary.LittleEndian.Uint16(img[loc.Offset+2:]) }
	recomputed := func() uint16 { return setChecksum(img[loc.Offset : loc.Offset+int64(n)]) }
	if stored() != recomputed() {
		t.Fatal("precondition: the built deleted set has a stale checksum")
	}
	dl, vl, first, nfc := uint64(12345), uint64(777), uint32(0x0A0B0C0D), true
	before := bytes.Clone(streamOf(img, loc))
	PatchStream(img, loc, StreamPatch{DataLen: &dl})
	s := streamOf(img, loc)
	if binary.LittleEndian.Uint64(s[24:]) != dl {
		t.Fatalf("data length %d", binary.LittleEndian.Uint64(s[24:]))
	}
	if !bytes.Equal(s[:24], before[:24]) || !bytes.Equal(s[32-0:], before[32:]) {
		t.Fatal("patching DataLen changed other stream fields")
	}
	if stored() != recomputed() {
		t.Fatalf("stored checksum %#x != recomputed %#x", stored(), recomputed())
	}
	PatchStream(img, loc, StreamPatch{Valid: &vl, First: &first, NoFatChain: &nfc})
	s = streamOf(img, loc)
	if binary.LittleEndian.Uint64(s[8:]) != vl || binary.LittleEndian.Uint32(s[20:]) != first || s[1]&2 == 0 {
		t.Fatalf("stream after patch: % x", s)
	}
	if stored() != recomputed() {
		t.Fatal("checksum not consistent after the second patch")
	}
	nfc = false
	PatchStream(img, loc, StreamPatch{NoFatChain: &nfc})
	if streamOf(img, loc)[1]&2 != 0 || streamOf(img, loc)[1]&1 == 0 {
		t.Fatalf("flags %#x: NoFatChain not cleared alone", streamOf(img, loc)[1])
	}

	// KeepChecksum leaves the stored value, which is then stale.
	stale := stored()
	dl2 := dl + 1
	PatchStream(img, loc, StreamPatch{DataLen: &dl2, KeepChecksum: true})
	if stored() != stale || stored() == recomputed() {
		t.Fatalf("KeepChecksum: stored %#x, stale %#x, recomputed %#x", stored(), stale, recomputed())
	}
}

func TestPatchStreamRejectsANonStreamEntry(t *testing.T) {
	img, _, loc := patchImage(t)
	dl := uint64(1)
	bad := loc
	bad.Offset += 32 // the stream entry is not a primary File entry; its successor is a name entry
	mustPanicLabelled(t, "not a file set", func() { PatchStream(img, bad, StreamPatch{DataLen: &dl}) })
	past := loc
	past.Offset = int64(len(img)) - 32
	mustPanicLabelled(t, "set past the image", func() { PatchStream(img, past, StreamPatch{DataLen: &dl}) })
	past.Offset = -32
	mustPanicLabelled(t, "negative offset", func() { PatchStream(img, past, StreamPatch{DataLen: &dl}) })
}

func TestCorruptSetChecksum(t *testing.T) {
	img, _, loc := patchImage(t)
	n := (int(img[loc.Offset+1]) + 1) * entrySize
	before := bytes.Clone(img)
	good := binary.LittleEndian.Uint16(img[loc.Offset+2:])
	CorruptSetChecksum(img, loc)
	got := binary.LittleEndian.Uint16(img[loc.Offset+2:])
	if got == good || got == setChecksum(img[loc.Offset:loc.Offset+int64(n)]) {
		t.Fatalf("checksum %#x was %#x", got, good)
	}
	img2 := bytes.Clone(img)
	copy(img2[loc.Offset+2:], before[loc.Offset+2:loc.Offset+4])
	if !bytes.Equal(img2, before) {
		t.Fatal("CorruptSetChecksum changed more than the checksum")
	}
	mustPanicLabelled(t, "past the image", func() { CorruptSetChecksum(img, EntryLoc{Offset: int64(len(img)) - 16}) })
}

func TestSetBitmapBitInASecondBitmapCluster(t *testing.T) {
	img, l := BuildLayout(Options{OneSectorClusters: true, ClusterCount: 5000, FragmentBitmap: true}, nil)
	if len(l.BitmapClusters) < 2 {
		t.Fatalf("want a bitmap of 2+ clusters, got %v", l.BitmapClusters)
	}
	const c = 4500 // bit 4498: byte 562 = byte 50 of the second 512-byte bitmap cluster
	at := l.ClusterOffset(l.BitmapClusters[1]) + 50
	SetBitmapBit(img, l, c, true)
	if img[at]&(1<<(4498%8)) == 0 {
		t.Fatal("bit not in the second bitmap cluster")
	}
	SetBitmapBit(img, l, c, false)
	if img[at]&(1<<(4498%8)) != 0 {
		t.Fatal("bit not cleared in the second bitmap cluster")
	}
}

func TestSetFATEntryRejectsAnEntryPastTheFATRegion(t *testing.T) {
	img, l, _ := patchImage(t)
	before := bytes.Clone(img)
	short := *l
	short.HeapOffset = l.FatOffset + 8 // the FAT claims room for two entries only
	mustPanicLabelled(t, "past the FAT region", func() { SetFATEntry(img, &short, l.ClusterCount+1, 5) })
	if !bytes.Equal(img, before) {
		t.Fatal("a rejected write changed the image")
	}
}

func TestPatchStreamRejectsASetWithoutAStreamEntry(t *testing.T) {
	img, _, loc := patchImage(t)
	img[loc.Offset+entrySize] = 0x41 // a deleted name entry where the stream belongs
	dl := uint64(1)
	mustPanicLabelled(t, "entry 1 is not a stream", func() { PatchStream(img, loc, StreamPatch{DataLen: &dl}) })
}
