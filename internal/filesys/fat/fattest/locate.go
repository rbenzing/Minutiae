package fattest

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// EntryLoc says where Build put one file or directory entry.
type EntryLoc struct {
	Path         string // as given to Build, without a leading slash
	Deleted, Dir bool
	DirCluster   uint32   // first cluster of the directory holding it (0: the FAT12/16 root)
	Index        int      // index of the short entry in that directory
	Offset       int64    // image offset of the 32-byte short entry
	LFNOffsets   []int64  // image offsets of its long-name entries, in on-disk order
	FirstCluster uint32   // 0: none
	Clusters     []uint32 // every cluster of the content, in file order (fragments included)
}

// Built is the geometry of an image and the location of every entry in it.
type Built struct {
	Geometry Geometry
	Entries  []EntryLoc
}

// Find returns the entry at path whose Deleted flag equals deleted. It panics
// when there is none.
func (b *Built) Find(path string, deleted bool) EntryLoc {
	p := strings.Trim(path, "/")
	for _, e := range b.Entries {
		if e.Path == p && e.Deleted == deleted {
			return e
		}
	}
	panic(fmt.Sprintf("fattest: no entry %q (deleted %v)", path, deleted))
}

// SetFATEntry writes v as entry c of every FAT copy. FAT12 keeps the nibble
// that belongs to the neighbouring entry; FAT32 keeps the top four bits. It
// panics, before writing anything, when c lies outside [2, Clusters+1], when a
// copy's write would leave that copy's FATSectors region, or when v is wider
// than the entry (0xFFF, 0xFFFF, 0x0FFFFFFF): a test helper must never
// silently write a corrupt fixture.
func SetFATEntry(img []byte, g Geometry, c, v uint32) {
	if c < 2 || c > g.Clusters+1 {
		panic(fmt.Sprintf("fattest: SetFATEntry: cluster %d outside [2, %d]", c, g.Clusters+1))
	}
	var max uint32
	var lo, width int // byte offset of the entry in a copy, and the bytes touched
	switch g.Type {
	case 12:
		max, lo, width = 0xFFF, int(c+c/2), 2
	case 16:
		max, lo, width = 0xFFFF, int(c)*2, 2
	default:
		max, lo, width = 0x0FFFFFFF, int(c)*4, 4
	}
	if v > max {
		panic(fmt.Sprintf("fattest: SetFATEntry: value %#x does not fit a FAT%d entry", v, g.Type))
	}
	region := int(g.FATSectors) * g.SectorSize
	if lo+width > region {
		panic(fmt.Sprintf("fattest: SetFATEntry: entry %d ends at byte %d, past the %d-byte FAT copy", c, lo+width, region))
	}
	for n := 0; n < g.NumFATs; n++ {
		start := int(g.FATStart(n)) * g.SectorSize
		if start+region > len(img) {
			panic(fmt.Sprintf("fattest: SetFATEntry: FAT copy %d ends at %d, past the %d-byte image", n, start+region, len(img)))
		}
	}
	for n := 0; n < g.NumFATs; n++ {
		t := img[int(g.FATStart(n))*g.SectorSize:]
		switch g.Type {
		case 12:
			o := lo
			if c&1 == 0 {
				t[o] = byte(v)
				t[o+1] = t[o+1]&0xF0 | byte(v>>8)&0x0F
			} else {
				t[o] = t[o]&0x0F | byte(v<<4)
				t[o+1] = byte(v >> 4)
			}
		case 16:
			binary.LittleEndian.PutUint16(t[lo:], uint16(v))
		default:
			old := binary.LittleEndian.Uint32(t[lo:])
			binary.LittleEndian.PutUint32(t[lo:], old&0xF0000000|v)
		}
	}
}

// PatchEntry rewrites the first-cluster halves and/or the size field of the
// short entry at loc. A nil pointer leaves that field alone. The high half
// (bytes 20-21) is written on FAT32 only; a first cluster above 0xFFFF on
// FAT12/16 panics.
func PatchEntry(img []byte, g Geometry, loc EntryLoc, first *uint32, size *uint32) {
	if loc.Offset < 0 || loc.Offset+32 > int64(len(img)) {
		panic(fmt.Sprintf("fattest: PatchEntry: entry at %d does not fit the %d-byte image", loc.Offset, len(img)))
	}
	e := img[loc.Offset : loc.Offset+32]
	if first != nil {
		if g.Type != 32 && *first > 0xFFFF {
			panic(fmt.Sprintf("fattest: first cluster %#x does not fit FAT%d", *first, g.Type))
		}
		if g.Type == 32 {
			binary.LittleEndian.PutUint16(e[20:], uint16(*first>>16))
		}
		binary.LittleEndian.PutUint16(e[26:], uint16(*first))
	}
	if size != nil {
		binary.LittleEndian.PutUint32(e[28:], *size)
	}
}
