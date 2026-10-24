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

// imageUpper returns the up-case function of img: the table its root directory
// names (read from the contiguous clusters the builder allocates for it), or
// the identity for an image without one. It panics on an image it cannot read.
func imageUpper(img []byte) func(uint16) uint16 {
	id := func(u uint16) uint16 { return u }
	if len(img) < 120 {
		panic("exfattest: FixNameHash: image shorter than a boot sector")
	}
	ss, cs := 1<<img[108], 1<<(int(img[108])+int(img[109]))
	heap := int64(binary.LittleEndian.Uint32(img[88:])) * int64(ss)
	fat := int64(binary.LittleEndian.Uint32(img[80:])) * int64(ss)
	clusterOff := func(c uint32) int64 { return heap + int64(c-2)*int64(cs) }
	c := binary.LittleEndian.Uint32(img[96:])
	for n := 0; n < 1<<16; n++ {
		off := clusterOff(c)
		if c < 2 || off < 0 || off+int64(cs) > int64(len(img)) {
			panic(fmt.Sprintf("exfattest: FixNameHash: root cluster %d is outside the image", c))
		}
		for e := off; e < off+int64(cs); e += entrySize {
			switch img[e] {
			case 0x00:
				return id
			case 0x82:
				first, length := binary.LittleEndian.Uint32(img[e+20:]), binary.LittleEndian.Uint64(img[e+24:])
				if length > 1<<17 || clusterOff(first)+int64(length) > int64(len(img)) {
					panic("exfattest: FixNameHash: up-case table outside the image")
				}
				data := img[clusterOff(first) : clusterOff(first)+int64(length)]
				t := make([]uint16, 1<<16)
				for i := range t {
					t[i] = uint16(i)
				}
				idx := 0
				for i := 0; i+2 <= len(data) && idx < len(t); i += 2 {
					v := binary.LittleEndian.Uint16(data[i:])
					if v == 0xFFFF && i+4 <= len(data) {
						idx += int(binary.LittleEndian.Uint16(data[i+2:]))
						i += 2
						continue
					}
					t[idx] = v
					idx++
				}
				return func(u uint16) uint16 { return t[u] }
			}
		}
		next := fat + int64(c)*4
		if next+4 > int64(len(img)) {
			return id
		}
		c = binary.LittleEndian.Uint32(img[next:])
		if c < 2 || c >= 0xFFFFFFF7 {
			return id
		}
	}
	return id
}

// FixNameHash recomputes bytes 4-5 (NameHash) of the Stream Extension of the
// entry set at image offset setOffset from its Name entries and the image's
// own up-case table, then the set checksum. Tests that rewrite a name call it
// so the patched name is still a consistent set (the NameHash is verified for
// listing).
func FixNameHash(img []byte, setOffset int64) {
	n := setBounds(img, EntryLoc{Offset: setOffset}, "FixNameHash")
	st := img[setOffset+entrySize:]
	nameLen := int(st[3])
	var units []uint16
	for e := 2 * entrySize; e+entrySize <= n && len(units) < nameLen; e += entrySize {
		ent := img[setOffset+int64(e) : setOffset+int64(e)+entrySize]
		if ent[0]&0x7F != 0x41 {
			continue
		}
		for u := 0; u < 15 && len(units) < nameLen; u++ {
			units = append(units, binary.LittleEndian.Uint16(ent[2+2*u:]))
		}
	}
	up := imageUpper(img)
	var h uint16
	for _, u := range units {
		u = up(u)
		for _, c := range [2]byte{byte(u), byte(u >> 8)} {
			h = (h&1)<<15 | h>>1
			h += uint16(c)
		}
	}
	le16(img, int(setOffset)+entrySize+4, h)
	FixSetChecksum(img, setOffset)
}
