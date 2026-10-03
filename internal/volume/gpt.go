package volume

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unicode/utf16"
)

const (
	gptSignature  = "EFI PART"
	gptMinHeader  = 92
	gptCRCOffset  = 16
	gptNameOffset = 56
	gptNameLen    = 72
)

// gptHeader is a GPT header read at one location. seen means the signature
// matched; the header is usable only when seen and reason is empty.
type gptHeader struct {
	lba      uint64
	seen     bool
	reason   string // why the header is invalid; empty when valid
	alt      uint64
	diskGUID string
	entLBA   uint64
	count    int
	esz      int
	arrOff   int64
	arrLen   int64
	arr      []byte
	arrCRC   uint32
}

func (h *gptHeader) valid() bool { return h.seen && h.reason == "" }

// parseGPTHeader reads and validates the header at lba. The entry-array size
// is bounded and range-checked before anything is allocated.
func parseGPTHeader(s source, ss int, lba uint64) (*gptHeader, error) {
	h := &gptHeader{lba: lba}
	off, ok := sectorOffset(lba, ss)
	if !ok {
		return h, nil
	}
	sector, err := s.read(off, ss)
	if err == errRange {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if string(sector[:8]) != gptSignature {
		return h, nil
	}
	h.seen = true
	h.alt = binary.LittleEndian.Uint64(sector[32:])

	hs := int(binary.LittleEndian.Uint32(sector[12:]))
	if hs < gptMinHeader || hs > ss {
		h.reason = fmt.Sprintf("header size %d out of range", hs)
		return h, nil
	}
	hdr := bytes.Clone(sector[:hs])
	want := binary.LittleEndian.Uint32(hdr[gptCRCOffset:])
	clear(hdr[gptCRCOffset : gptCRCOffset+4])
	if crc32.ChecksumIEEE(hdr) != want {
		h.reason = "header CRC mismatch"
		return h, nil
	}
	if my := binary.LittleEndian.Uint64(sector[24:]); my != lba {
		h.reason = fmt.Sprintf("MyLBA %d does not match header location %d", my, lba)
		return h, nil
	}
	h.diskGUID = guidString(sector[56:72])
	h.entLBA = binary.LittleEndian.Uint64(sector[72:])
	num := binary.LittleEndian.Uint32(sector[80:])
	esz := binary.LittleEndian.Uint32(sector[84:])
	if num > maxGPTEntries || esz < minGPTEntrySize || esz > maxGPTEntrySize || esz%8 != 0 {
		h.reason = fmt.Sprintf("invalid entry count %d or entry size %d", num, esz)
		return h, nil
	}
	h.count, h.esz = int(num), int(esz)
	h.arrLen = int64(h.count) * int64(h.esz) // <= 4 MiB
	var inRange bool
	h.arrOff, inRange = sectorOffset(h.entLBA, ss)
	if h.entLBA < 2 || !inRange || h.arrOff > s.size || h.arrLen > s.size-h.arrOff {
		h.reason = "partition entry array outside image"
		return h, nil
	}
	h.arr, err = s.read(h.arrOff, int(h.arrLen))
	if err == errRange {
		h.reason = "partition entry array outside image"
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	h.arrCRC = binary.LittleEndian.Uint32(sector[88:])
	if crc32.ChecksumIEEE(h.arr) != h.arrCRC {
		h.reason = "partition entry array CRC mismatch"
	}
	return h, nil
}

// readGPT returns the GPT table at sector size ss, or nil plus warnings
// describing any header that was present but unusable.
func readGPT(s source, ss int) (*Table, []string, error) {
	prefix := fmt.Sprintf("GPT (sector size %d): ", ss)
	lastLBA := uint64(s.size / int64(ss))
	if lastLBA == 0 {
		return nil, nil, nil
	}
	lastLBA--

	primary, err := parseGPTHeader(s, ss, 1)
	if err != nil {
		return nil, nil, err
	}
	var warns []string
	var warnings []string
	structs := []Run{{0, 2 * int64(ss)}}

	// Candidate backup locations: the primary's AlternateLBA, then the last LBA.
	cands := make([]uint64, 0, 2)
	if primary.seen && primary.alt >= 2 {
		cands = append(cands, primary.alt)
	}
	if lastLBA >= 2 && (len(cands) == 0 || cands[0] != lastLBA) {
		cands = append(cands, lastLBA)
	}
	var backup *gptHeader // first valid backup
	var firstBackup *gptHeader
	for _, lba := range cands {
		b, err := parseGPTHeader(s, ss, lba)
		if err != nil {
			return nil, nil, err
		}
		if firstBackup == nil {
			firstBackup = b
		}
		if b.valid() {
			backup = b
			break
		}
		if b.seen {
			warns = append(warns, prefix+"backup header invalid: "+b.reason)
		}
	}

	chosen := primary
	if !primary.valid() {
		if primary.seen {
			warns = append(warns, prefix+"primary header invalid: "+primary.reason)
		}
		if backup == nil {
			return nil, warns, nil
		}
		chosen = backup
		warnings = append(warnings, "primary GPT header invalid; using backup")
	}
	warnings = append(warnings, warns...)
	if primary.valid() && backup != nil && (primary.diskGUID != backup.diskGUID || primary.arrCRC != backup.arrCRC ||
		primary.count != backup.count || primary.esz != backup.esz) {
		warnings = append(warnings, "primary and backup GPT differ")
	}
	if primary.valid() && backup == nil && firstBackup != nil && !firstBackup.seen {
		warnings = append(warnings, prefix+"backup header not found")
	}

	t := &Table{Scheme: "gpt", SectorSize: ss, DiskGUID: chosen.diskGUID, Warnings: warnings}
	structs = append(structs, Run{chosen.arrOff, chosen.arrLen})
	if chosen == primary {
		if backup != nil {
			structs = append(structs, Run{backup.arrOff, backup.arrLen})
			structs = append(structs, Run{int64(backup.lba) * int64(ss), int64(ss)})
		} else if firstBackup != nil && firstBackup.seen {
			structs = append(structs, Run{int64(firstBackup.lba) * int64(ss), int64(ss)})
		}
	} else {
		structs = append(structs, Run{int64(chosen.lba) * int64(ss), int64(ss)})
	}

	for i := range chosen.count {
		e := chosen.arr[i*chosen.esz : (i+1)*chosen.esz]
		if allZero(e[:16]) {
			continue
		}
		index := i + 1
		first := binary.LittleEndian.Uint64(e[32:])
		last := binary.LittleEndian.Uint64(e[40:])
		if last < first {
			t.Warnings = append(t.Warnings, fmt.Sprintf("partition %d has last LBA before first LBA; skipped", index))
			continue
		}
		n := last - first
		if n < ^uint64(0) {
			n++
		}
		start, length, ok := clampExtent(index, first, n, ss, s.size, &t.Warnings)
		if !ok {
			continue
		}
		typ := guidString(e[0:16])
		t.Partitions = append(t.Partitions, Partition{
			Index:      index,
			Start:      start,
			Length:     length,
			Type:       typ,
			TypeName:   gptTypeNames[typ],
			Name:       utf16Name(e[gptNameOffset:min(gptNameOffset+gptNameLen, len(e))]),
			GUID:       guidString(e[16:32]),
			Attributes: binary.LittleEndian.Uint64(e[48:]),
		})
	}
	finish(t, s.size, structs)
	return t, nil, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// guidString renders a mixed-endian on-disk GUID in lower-case canonical form.
func guidString(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:4]),
		binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]),
		b[8:10], b[10:16])
}

// utf16Name decodes UTF-16LE up to the first NUL code unit.
func utf16Name(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}
