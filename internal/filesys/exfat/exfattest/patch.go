package exfattest

import (
	"encoding/binary"
	"fmt"
)

// The helpers below damage a built image for recovery tests. They are
// test-only, edit the in-memory image they are given and never touch a case.
// Every write is bounds-checked first and panics with an "exfattest:" label
// rather than writing a corrupt fixture silently.

// SetFATEntry writes v as the FAT entry of cluster c. A cluster outside
// [2, ClusterCount+1] or an entry that leaves the FAT region panics.
func SetFATEntry(img []byte, l *Layout, c, v uint32) {
	if c < 2 || c > l.ClusterCount+1 {
		panic(fmt.Sprintf("exfattest: SetFATEntry: cluster %d outside [2, %d]", c, l.ClusterCount+1))
	}
	off := l.FatEntryOffset(c)
	if off < l.FatOffset || off+4 > l.FatOffset+l.FatLength || off+4 > int64(len(img)) {
		panic(fmt.Sprintf("exfattest: SetFATEntry: entry at %d leaves the FAT region [%d, %d)", off, l.FatOffset, l.FatOffset+l.FatLength))
	}
	le32(img, int(off), v)
}

// SetBitmapBit sets or clears the allocation-bitmap bit of cluster c in the
// heap. A cluster outside [2, ClusterCount+1] or a bit outside the bitmap
// clusters panics.
func SetBitmapBit(img []byte, l *Layout, c uint32, allocated bool) {
	if c < 2 || c > l.ClusterCount+1 {
		panic(fmt.Sprintf("exfattest: SetBitmapBit: cluster %d outside [2, %d]", c, l.ClusterCount+1))
	}
	i := int64(c - 2)
	byteIdx := i / 8
	which := byteIdx / int64(l.ClusterSize)
	if which >= int64(len(l.BitmapClusters)) {
		panic(fmt.Sprintf("exfattest: SetBitmapBit: bit of cluster %d is past the %d bitmap clusters", c, len(l.BitmapClusters)))
	}
	off := l.ClusterOffset(l.BitmapClusters[which]) + byteIdx%int64(l.ClusterSize)
	if off < 0 || off >= int64(len(img)) {
		panic(fmt.Sprintf("exfattest: SetBitmapBit: byte %d is outside the %d-byte image", off, len(img)))
	}
	bit := byte(1) << (i % 8)
	if allocated {
		img[off] |= bit
	} else {
		img[off] &^= bit
	}
}

// StreamPatch edits the Stream Extension of one entry set. Nil fields are
// left alone.
type StreamPatch struct {
	First        *uint32
	DataLen      *uint64
	Valid        *uint64
	NoFatChain   *bool
	KeepChecksum bool // default: the set checksum is recomputed so only the patched field is wrong
}

// setBounds checks that the whole entry set at loc is inside img, that its
// primary entry is a File entry (live 0x85 or deleted 0x05) and that entry 1
// is a Stream Extension (0xC0 or 0x40), and returns the set's byte length.
func setBounds(img []byte, loc EntryLoc, who string) int {
	if loc.Offset < 0 || loc.Offset+2*entrySize > int64(len(img)) {
		panic(fmt.Sprintf("exfattest: %s: entry set at %d does not fit the %d-byte image", who, loc.Offset, len(img)))
	}
	if t := img[loc.Offset]; t&0x7F != 0x05 {
		panic(fmt.Sprintf("exfattest: %s: entry at %d has type %#x, not a File entry", who, loc.Offset, t))
	}
	n := (int(img[loc.Offset+1]) + 1) * entrySize
	if n < 2*entrySize {
		panic(fmt.Sprintf("exfattest: %s: entry set at %d is %d bytes, a set holds at least a primary and a stream", who, loc.Offset, n))
	}
	if loc.Offset+int64(n) > int64(len(img)) {
		panic(fmt.Sprintf("exfattest: %s: entry set at %d (%d bytes) does not fit the %d-byte image", who, loc.Offset, n, len(img)))
	}
	if t := img[loc.Offset+entrySize]; t&0x7F != 0x40 {
		panic(fmt.Sprintf("exfattest: %s: entry 1 of the set at %d has type %#x, not a Stream Extension", who, loc.Offset, t))
	}
	return n
}

// PatchStream edits the Stream Extension (entry 1) of the set at loc. Deleted
// sets share the layout with InUse cleared. Unless KeepChecksum is set the set
// checksum is recomputed, so the patched field is the only thing wrong.
func PatchStream(img []byte, loc EntryLoc, p StreamPatch) {
	setBounds(img, loc, "PatchStream")
	s := img[loc.Offset+entrySize : loc.Offset+2*entrySize]
	if p.NoFatChain != nil {
		if *p.NoFatChain {
			s[1] |= 0x02
		} else {
			s[1] &^= 0x02
		}
	}
	if p.Valid != nil {
		le64(s, 8, *p.Valid)
	}
	if p.First != nil {
		le32(s, 20, *p.First)
	}
	if p.DataLen != nil {
		le64(s, 24, *p.DataLen)
	}
	if !p.KeepChecksum {
		FixSetChecksum(img, loc.Offset)
	}
}

// CorruptSetChecksum flips every bit of the stored SetChecksum of the set at
// loc, so it can never equal the recomputed value.
func CorruptSetChecksum(img []byte, loc EntryLoc) {
	if loc.Offset < 0 || loc.Offset+entrySize > int64(len(img)) {
		panic(fmt.Sprintf("exfattest: CorruptSetChecksum: entry at %d does not fit the %d-byte image", loc.Offset, len(img)))
	}
	if t := img[loc.Offset]; t&0x7F != 0x05 {
		panic(fmt.Sprintf("exfattest: CorruptSetChecksum: entry at %d has type %#x, not a File entry", loc.Offset, t))
	}
	o := int(loc.Offset) + 2
	le16(img, o, ^binary.LittleEndian.Uint16(img[o:]))
}
