// Package volumetest builds partition-table disk images for tests and fuzz
// seeds. The images contain only table structures (the rest is zeros).
package volumetest

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode/utf16"
)

// Part describes one partition. Fields marked MBR/GPT only apply to that scheme.
type Part struct {
	StartLBA, Sectors uint64
	MBRType           byte   // MBR only
	TypeGUID, GUID    string // GPT only, canonical text form
	Name              string // GPT only
	Logical           bool   // MBR: place inside the extended partition
}

const sector = 512

func put(b []byte, off int, v any) {
	switch x := v.(type) {
	case uint16:
		binary.LittleEndian.PutUint16(b[off:], x)
	case uint32:
		binary.LittleEndian.PutUint32(b[off:], x)
	case uint64:
		binary.LittleEndian.PutUint64(b[off:], x)
	}
}

func mbrEntry(b []byte, typ byte, start, count uint32) {
	b[4] = typ
	put(b, 8, start)
	put(b, 12, count)
}

// MBR builds a disk image with an MBR. Logical partitions are chained via
// EBRs inside one 0x0F extended partition; each EBR sits in the sector just
// before its logical partition.
func MBR(diskSectors uint64, parts []Part) []byte {
	img := make([]byte, diskSectors*sector)
	mbr := img[:sector]
	mbr[510], mbr[511] = 0x55, 0xAA

	slot := 0
	var logical []Part
	for _, p := range parts {
		if p.Logical {
			logical = append(logical, p)
			continue
		}
		if slot >= 4 {
			panic("volumetest: too many primary MBR partitions")
		}
		mbrEntry(mbr[446+16*slot:], p.MBRType, uint32(p.StartLBA), uint32(p.Sectors))
		slot++
	}
	if len(logical) == 0 {
		return img
	}
	if slot >= 4 {
		panic("volumetest: no free primary slot for the extended partition")
	}
	extStart := logical[0].StartLBA - 1
	last := logical[len(logical)-1]
	extEnd := last.StartLBA + last.Sectors
	mbrEntry(mbr[446+16*slot:], 0x0F, uint32(extStart), uint32(extEnd-extStart))
	for i, p := range logical {
		ebrLBA := p.StartLBA - 1
		ebr := img[ebrLBA*sector : (ebrLBA+1)*sector]
		ebr[510], ebr[511] = 0x55, 0xAA
		mbrEntry(ebr[446:], p.MBRType, 1, uint32(p.Sectors))
		if i+1 < len(logical) {
			n := logical[i+1]
			nextEBR := n.StartLBA - 1
			mbrEntry(ebr[462:], 0x0F, uint32(nextEBR-extStart), uint32(n.StartLBA+n.Sectors-nextEBR))
		}
	}
	return img
}

// GPT builds a disk image with a protective MBR, primary and backup headers
// and 128 entries of 128 bytes each, with valid CRCs.
func GPT(sectorSize int, diskSectors uint64, diskGUID string, parts []Part) []byte {
	const numEntries, entrySize = 128, 128
	ss := uint64(sectorSize)
	arraySectors := uint64(numEntries*entrySize) / ss
	if diskSectors < 2*(2+arraySectors)+1 || len(parts) > numEntries {
		panic("volumetest: disk too small or too many partitions")
	}
	img := make([]byte, diskSectors*ss)

	// Protective MBR.
	protLen := diskSectors - 1
	if protLen > 0xFFFFFFFF {
		protLen = 0xFFFFFFFF
	}
	img[510], img[511] = 0x55, 0xAA
	mbrEntry(img[446:], 0xEE, 1, uint32(protLen))

	array := make([]byte, numEntries*entrySize)
	for i, p := range parts {
		e := array[i*entrySize:]
		copy(e[0:16], guidBytes(p.TypeGUID))
		copy(e[16:32], guidBytes(p.GUID))
		put(e, 32, p.StartLBA)
		put(e, 40, p.StartLBA+p.Sectors-1)
		units := utf16.Encode([]rune(p.Name))
		if len(units) > 36 {
			panic("volumetest: partition name too long")
		}
		for j, u := range units {
			put(e, 56+2*j, u)
		}
	}
	arrayCRC := crc32.ChecksumIEEE(array)

	last := diskSectors - 1
	backupArrayLBA := last - arraySectors
	header := func(my, alt, entLBA uint64) []byte {
		h := make([]byte, 92)
		copy(h, "EFI PART")
		put(h, 8, uint32(0x00010000))
		put(h, 12, uint32(92))
		put(h, 24, my)
		put(h, 32, alt)
		put(h, 40, 2+arraySectors)   // first usable
		put(h, 48, backupArrayLBA-1) // last usable
		copy(h[56:72], guidBytes(diskGUID))
		put(h, 72, entLBA)
		put(h, 80, uint32(numEntries))
		put(h, 84, uint32(entrySize))
		put(h, 88, arrayCRC)
		put(h, 16, crc32.ChecksumIEEE(h))
		return h
	}
	copy(img[ss:], header(1, last, 2))
	copy(img[2*ss:], array)
	copy(img[backupArrayLBA*ss:], array)
	copy(img[last*ss:], header(last, 1, backupArrayLBA))
	return img
}

// guidBytes encodes a canonical GUID string in the mixed-endian on-disk form.
func guidBytes(s string) []byte {
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		panic(fmt.Sprintf("volumetest: bad GUID %q", s))
	}
	out := make([]byte, 16)
	out[0], out[1], out[2], out[3] = raw[3], raw[2], raw[1], raw[0]
	out[4], out[5] = raw[5], raw[4]
	out[6], out[7] = raw[7], raw[6]
	copy(out[8:], raw[8:])
	return out
}
