package volume

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
)

type mbrEntry struct {
	boot, typ byte
	start     uint32
	count     uint32
}

func parseMBREntry(b []byte) mbrEntry {
	return mbrEntry{
		boot:  b[0],
		typ:   b[4],
		start: binary.LittleEndian.Uint32(b[8:]),
		count: binary.LittleEndian.Uint32(b[12:]),
	}
}

func (e mbrEntry) used() bool { return e.typ != 0 && e.count != 0 }

func hasMBRSignature(sector []byte) bool {
	return len(sector) >= 512 && sector[510] == 0x55 && sector[511] == 0xAA
}

// LooksLikeBootSector reports whether sector 0 is a FAT12/16/32, exFAT or NTFS
// boot sector. Such a sector also ends in 0x55AA but is not a partition table.
func LooksLikeBootSector(sector0 []byte) bool {
	if len(sector0) < 64 {
		return false
	}
	jump := (sector0[0] == 0xEB && sector0[2] == 0x90) || sector0[0] == 0xE9
	if !jump {
		return false
	}
	oem := sector0[3:11]
	if bytes.Equal(oem, []byte("EXFAT   ")) || bytes.Equal(oem, []byte("NTFS    ")) {
		return true
	}
	bytesPerSector := binary.LittleEndian.Uint16(sector0[11:])
	switch bytesPerSector {
	case 512, 1024, 2048, 4096:
	default:
		return false
	}
	spc := sector0[13]
	if spc == 0 || bits.OnesCount8(spc) != 1 {
		return false
	}
	if binary.LittleEndian.Uint16(sector0[14:]) == 0 {
		return false
	}
	fats := sector0[16]
	return fats == 1 || fats == 2
}

// readMBR returns the MBR table, or nil when sector 0 is not a partition
// table. gptWarn carries the GPT failure notes: a protective-only MBR whose
// GPT could not be parsed is returned as scheme "mbr" with those warnings.
func readMBR(s source, ss int, gptWarn []string) (*Table, error) {
	sector0, err := s.read(0, minMBRSector)
	if err == errRange {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hasMBRSignature(sector0) || LooksLikeBootSector(sector0) {
		return nil, nil
	}
	var entries [4]mbrEntry
	plausible := false
	for i := range entries {
		entries[i] = parseMBREntry(sector0[446+16*i:])
		e := entries[i]
		if e.used() && e.start > 0 {
			plausible = true // uint32 start+count cannot overflow uint64
		}
	}
	if !plausible {
		return nil, nil
	}

	t := &Table{Scheme: "mbr", SectorSize: ss}
	structs := []Run{{0, int64(ss)}}
	var extended []mbrEntry
	protective := false
	for i, e := range entries {
		if !e.used() {
			continue
		}
		index := i + 1
		if e.start == 0 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("partition %d has start LBA 0; skipped", index))
			continue
		}
		if isExtendedType(e.typ) {
			extended = append(extended, e)
			continue
		}
		if e.typ == 0xEE {
			protective = true
		}
		start, length, ok := clampExtent(index, uint64(e.start), uint64(e.count), ss, s.size, &t.Warnings)
		if !ok {
			continue
		}
		t.Partitions = append(t.Partitions, mbrPartition(index, e, start, length))
	}

	visited := map[uint64]bool{}
	logical := 0
	for _, ext := range extended {
		if err := walkEBRChain(s, ss, ext, visited, &logical, t, &structs); err != nil {
			return nil, err
		}
	}

	if protective {
		t.Warnings = append(t.Warnings, gptWarn...)
		t.Warnings = append(t.Warnings, "protective MBR present but no valid GPT found")
	}
	finish(t, s.size, structs)
	return t, nil
}

func mbrPartition(index int, e mbrEntry, start, length int64) Partition {
	return Partition{
		Index:      index,
		Start:      start,
		Length:     length,
		Type:       mbrTypeString(e.typ),
		TypeName:   mbrTypeNames[e.typ],
		Attributes: uint64(e.boot),
	}
}

// walkEBRChain follows the EBR chain of one extended partition. Visited EBR
// LBAs are shared across chains so loops are caught; at most maxLogical EBRs
// are visited in total.
func walkEBRChain(s source, ss int, ext mbrEntry, visited map[uint64]bool, logical *int, t *Table, structs *[]Run) error {
	extStart := uint64(ext.start)
	lba := extStart
	for {
		if visited[lba] {
			t.Warnings = append(t.Warnings, fmt.Sprintf("extended partition EBR chain loop at LBA %d", lba))
			return nil
		}
		if len(visited) >= maxLogical {
			t.Warnings = append(t.Warnings, fmt.Sprintf("more than %d logical partitions; remaining ignored", maxLogical))
			return nil
		}
		visited[lba] = true
		off, ok := sectorOffset(lba, ss)
		if !ok {
			t.Warnings = append(t.Warnings, fmt.Sprintf("EBR at LBA %d lies outside image", lba))
			return nil
		}
		sector, err := s.read(off, ss)
		if err != nil && err != errRange {
			return err
		}
		if err != nil || !hasMBRSignature(sector) {
			t.Warnings = append(t.Warnings, fmt.Sprintf("EBR at LBA %d missing or without signature", lba))
			return nil
		}
		*structs = append(*structs, Run{off, int64(ss)})

		first := parseMBREntry(sector[446:])
		link := parseMBREntry(sector[462:])
		if first.used() {
			index := 5 + *logical
			*logical++
			startLBA := lba + uint64(first.start)
			if start, length, ok := clampExtent(index, startLBA, uint64(first.count), ss, s.size, &t.Warnings); ok {
				t.Partitions = append(t.Partitions, mbrPartition(index, first, start, length))
			}
		}
		if link.typ == 0 {
			return nil
		}
		lba = extStart + uint64(link.start)
	}
}
