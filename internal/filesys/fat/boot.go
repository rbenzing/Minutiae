package fat

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	bootSize = 512 // the BPB lives in the first 512 bytes whatever the sector size

	// Largest cluster counts of each FAT type (the cluster-count rule's upper
	// bounds; cluster numbers are 2 .. count+1).
	maxCountFAT12 = 4084
	maxCountFAT16 = 65524
	maxCountFAT32 = 0x0FFFFFF5
)

// bpb is the validated BIOS parameter block plus the geometry derived from it.
type bpb struct {
	bytsPerSec int
	secPerClus int
	rsvd       int
	numFATs    int
	rootEnt    int    // root directory entries (FAT12/16; 0 for FAT32)
	fat32      bool   // FAT32 field layout (FATSz16 == 0)
	fatSz      uint64 // sectors per FAT copy
	totSec     uint64 // volume sectors, clamped to the image
	rootClus   uint32 // FAT32 root directory cluster
	activeFAT  int    // FAT copy that is read (FAT32 mirroring flags)
	volID      uint32
	label      string // the BPB label, "" when absent or the "NO NAME" placeholder

	rootDirSectors uint64
	dataStart      uint64 // first sector of cluster 2
	count          uint32 // CountOfClusters, clamped to what the image and the FAT hold
	fatType        int    // 12, 16 or 32
}

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

// typeByCount is the Microsoft rule: fewer than 4085 clusters is FAT12, fewer
// than 65525 is FAT16, anything else FAT32.
func typeByCount(n uint64) int {
	switch {
	case n < 4085:
		return 12
	case n < 65525:
		return 16
	}
	return 32
}

func maxCount(typ int) uint64 {
	switch typ {
	case 12:
		return maxCountFAT12
	case 16:
		return maxCountFAT16
	}
	return maxCountFAT32
}

func entryBits(typ int) uint64 {
	switch typ {
	case 12:
		return 12
	case 16:
		return 16
	}
	return 32
}

// oemString renders a space-padded OEM-codepage field. The codepage is not
// stored on the volume, so bytes above 0x7F are shown as their Latin-1 code
// points and control bytes as '?'.
func oemString(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == 0:
			sb.WriteByte(' ')
		case c < 0x20 || c == 0x7F:
			sb.WriteByte('?')
		default:
			sb.WriteRune(rune(c))
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

// parseBoot validates the boot sector b (the first 512 bytes) of a volume in an
// image of size bytes. Problems that do not stop the read (a declared size
// larger than the image, a FAT too small for the declared clusters ...) are
// returned as warnings; anything that makes the geometry unusable is a
// *filesys.CorruptError. No byte of b is trusted before it is range-checked.
func parseBoot(b []byte, size int64) (*bpb, []string, error) {
	const st = "FAT boot sector"
	if len(b) < bootSize {
		return nil, nil, corrupt(st, 0, "%d bytes read, need %d", len(b), bootSize)
	}
	if b[510] != 0x55 || b[511] != 0xAA {
		return nil, nil, corrupt(st, 510, "missing 0x55AA signature (found %02x %02x)", b[510], b[511])
	}
	if (b[0] != 0xEB || b[2] != 0x90) && b[0] != 0xE9 {
		return nil, nil, corrupt(st, 0, "no x86 jump instruction (starts %02x %02x %02x)", b[0], b[1], b[2])
	}
	if oem := string(b[3:11]); oem == "EXFAT   " || oem == "NTFS    " {
		return nil, nil, corrupt(st, 3, "OEM id %q: not a FAT volume", oem)
	}
	le16 := func(off int) int { return int(binary.LittleEndian.Uint16(b[off:])) }
	le32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

	p := &bpb{
		bytsPerSec: le16(11),
		secPerClus: int(b[13]),
		rsvd:       le16(14),
		numFATs:    int(b[16]),
		rootEnt:    le16(17),
	}
	switch p.bytsPerSec {
	case 512, 1024, 2048, 4096:
	default:
		return nil, nil, corrupt(st, 11, "BytsPerSec %d is not 512, 1024, 2048 or 4096", p.bytsPerSec)
	}
	if p.secPerClus == 0 || p.secPerClus&(p.secPerClus-1) != 0 || p.secPerClus > 128 {
		return nil, nil, corrupt(st, 13, "SecPerClus %d is not a power of two from 1 to 128", p.secPerClus)
	}
	if p.bytsPerSec*p.secPerClus > 32<<20 {
		return nil, nil, corrupt(st, 13, "cluster size %d exceeds 32 MiB", p.bytsPerSec*p.secPerClus)
	}
	if p.rsvd == 0 {
		return nil, nil, corrupt(st, 14, "RsvdSecCnt is 0")
	}
	if p.numFATs != 1 && p.numFATs != 2 {
		return nil, nil, corrupt(st, 16, "NumFATs %d is not 1 or 2", p.numFATs)
	}
	if m := b[21]; m != 0xF0 && m < 0xF8 {
		return nil, nil, corrupt(st, 21, "media descriptor %#x is not 0xF0 or 0xF8-0xFF", m)
	}

	claimedTot := uint64(le16(19))
	if claimedTot == 0 {
		claimedTot = uint64(le32(32))
	}
	if claimedTot == 0 {
		return nil, nil, corrupt(st, 19, "TotSec16 and TotSec32 are both 0")
	}
	p.fatSz = uint64(le16(22))
	p.fat32 = p.fatSz == 0
	switch {
	case p.fat32:
		p.fatSz = uint64(le32(36))
		if p.fatSz == 0 {
			return nil, nil, corrupt(st, 36, "FATSz16 and FATSz32 are both 0")
		}
		if p.rootEnt != 0 {
			return nil, nil, corrupt(st, 17, "RootEntCnt %d in a FAT32 boot sector (want 0)", p.rootEnt)
		}
	case p.rootEnt == 0:
		return nil, nil, corrupt(st, 17, "RootEntCnt is 0 in a FAT12/16 boot sector")
	}

	bps := uint64(p.bytsPerSec)
	spc := uint64(p.secPerClus)
	p.rootDirSectors = (uint64(p.rootEnt)*32 + bps - 1) / bps
	meta := uint64(p.rsvd) + uint64(p.numFATs)*p.fatSz + p.rootDirSectors // < 2^34: cannot overflow
	if claimedTot <= meta {
		return nil, nil, corrupt(st, 19, "%d sectors do not cover the %d sectors of reserved area, FATs and root directory", claimedTot, meta)
	}
	p.dataStart = meta
	claimed := (claimedTot - meta) / spc
	if claimed == 0 {
		return nil, nil, corrupt(st, 19, "no data cluster: %d sectors, %d of them metadata, %d per cluster", claimedTot, meta, spc)
	}

	var warns []string
	p.totSec = claimedTot
	if have := uint64(size) / bps; p.totSec > have {
		warns = append(warns, fmt.Sprintf("volume declares %d sectors but the image holds %d: reading is limited to the image", claimedTot, have))
		p.totSec = have
	}
	if p.totSec <= meta {
		return nil, nil, corrupt(st, 19, "the image ends inside the %d sectors of reserved area, FATs and root directory (%d sectors present)", meta, p.totSec)
	}
	count := (p.totSec - meta) / spc
	if count == 0 {
		return nil, nil, corrupt(st, 19, "no complete data cluster fits in the image")
	}

	// The type follows from the declared cluster count; the field layout of the
	// BPB decides when the two disagree.
	byCount := typeByCount(claimed)
	switch {
	case p.fat32:
		p.fatType = 32
		if byCount != 32 {
			warns = append(warns, fmt.Sprintf("FAT32 boot sector with only %d clusters (a FAT%d count)", claimed, byCount))
		}
	case byCount == 32:
		p.fatType = 16
		warns = append(warns, fmt.Sprintf("FAT12/16 boot sector declaring %d clusters (a FAT32 count): read as FAT16", claimed))
	default:
		p.fatType = byCount
	}

	if m := maxCount(p.fatType); count > m {
		warns = append(warns, fmt.Sprintf("%d clusters exceed the FAT%d maximum: limited to %d", count, p.fatType, m))
		count = m
	}
	// The FAT must be able to describe every cluster (entries 0 and 1 are
	// reserved).
	entries := p.fatSz * bps * 8 / entryBits(p.fatType)
	if entries < 3 {
		return nil, nil, corrupt(st, 22, "a FAT of %d sectors holds no cluster entry", p.fatSz)
	}
	if count > entries-2 {
		warns = append(warns, fmt.Sprintf("FAT of %d sectors describes %d clusters but the volume has %d: limited to the FAT", p.fatSz, entries-2, count))
		count = entries - 2
	}
	p.count = uint32(count) // <= maxCountFAT32

	if p.fat32 {
		p.rootClus = le32(44)
		if p.rootClus < 2 || uint64(p.rootClus) > count+1 {
			return nil, nil, corrupt(st, 44, "RootClus %d outside the %d clusters", p.rootClus, count)
		}
		if flags := le16(40); flags&0x80 != 0 {
			p.activeFAT = flags & 0x0F
			if p.activeFAT >= p.numFATs {
				warns = append(warns, fmt.Sprintf("ExtFlags %#x selects FAT %d but there are %d: reading FAT 0", flags, p.activeFAT, p.numFATs))
				p.activeFAT = 0
			}
		}
	}

	// Volume ID and label exist only when the extended boot signature says so.
	sigOff, idOff, labOff := 38, 39, 43
	if p.fat32 {
		sigOff, idOff, labOff = 66, 67, 71
	}
	if sig := b[sigOff]; sig == 0x29 || sig == 0x28 {
		p.volID = le32(idOff)
		if sig == 0x29 {
			if l := oemString(b[labOff : labOff+11]); l != "NO NAME" {
				p.label = l
			}
		}
	}
	return p, warns, nil
}
