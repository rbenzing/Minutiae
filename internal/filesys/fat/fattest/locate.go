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
// that belongs to the neighbouring entry; FAT32 keeps the top four bits.
func SetFATEntry(img []byte, g Geometry, c, v uint32) {
	for n := 0; n < g.NumFATs; n++ {
		t := img[int(g.FATStart(n))*g.SectorSize:]
		switch g.Type {
		case 12:
			o := int(c + c/2)
			if c&1 == 0 {
				t[o] = byte(v)
				t[o+1] = t[o+1]&0xF0 | byte(v>>8)&0x0F
			} else {
				t[o] = t[o]&0x0F | byte(v<<4)
				t[o+1] = byte(v >> 4)
			}
		case 16:
			binary.LittleEndian.PutUint16(t[c*2:], uint16(v))
		default:
			old := binary.LittleEndian.Uint32(t[c*4:])
			binary.LittleEndian.PutUint32(t[c*4:], old&0xF0000000|v&0x0FFFFFFF)
		}
	}
}

// PatchEntry rewrites the first-cluster halves and/or the size field of the
// short entry at loc. A nil pointer leaves that field alone. The high half
// (bytes 20-21) is written on FAT32 only; a first cluster above 0xFFFF on
// FAT12/16 panics.
func PatchEntry(img []byte, g Geometry, loc EntryLoc, first *uint32, size *uint32) {
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
