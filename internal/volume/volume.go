// Package volume detects and parses partition tables (GPT, MBR with EBR
// chains, or none) on an io.ReaderAt. It is a pure parser: it never writes
// and imports no other Minutiae package. Every on-disk number is validated
// before it drives a loop, an allocation or an offset computation.
package volume

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// Run is a byte range of the image.
type Run struct{ Offset, Length int64 }

// Partition is one entry of a partition table. Start and Length are in bytes.
type Partition struct {
	Index      int
	Start      int64
	Length     int64
	Type       string // GPT type GUID (lower-case canonical) or MBR id "0x83"
	TypeName   string
	Name       string // GPT name (UTF-16LE decoded)
	GUID       string // GPT unique partition GUID
	Attributes uint64 // GPT attributes / MBR boot flag (bit 7)
}

// Table is a parsed partition table.
type Table struct {
	Scheme      string // "gpt", "mbr" or "none"
	SectorSize  int
	DiskGUID    string // gpt only
	Partitions  []Partition
	Unallocated []Run // image bytes covered by no partition and no table structure
	Warnings    []string
}

// Limits applied to untrusted on-disk values.
const (
	maxGPTEntries   = 1024
	minGPTEntrySize = 128
	maxGPTEntrySize = 4096
	maxLogical      = 128
	maxSectorSize   = 4096
	minMBRSector    = 512
)

// errRange marks a read that falls (partly) outside the image.
var errRange = errors.New("volume: read outside image")

// source bounds-checks every read against the declared image size.
type source struct {
	r    io.ReaderAt
	size int64
}

// read returns exactly n bytes at off. errRange means the range is not fully
// inside the image (or the reader ended early); any other error is an I/O
// failure and must be propagated.
func (s source) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off > s.size || int64(n) > s.size-off {
		return nil, errRange
	}
	buf := make([]byte, n)
	got, err := s.r.ReadAt(buf, off)
	if got == n {
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errRange
	}
	return nil, err
}

// sectorOffset converts an LBA to a byte offset, reporting overflow.
func sectorOffset(lba uint64, ss int) (int64, bool) {
	if lba > uint64(math.MaxInt64)/uint64(ss) {
		return 0, false
	}
	return int64(lba) * int64(ss), true
}

// Read detects and parses the partition table of the image. sectorSize 0
// probes 512 then 4096 for GPT (MBR then assumes 512); a non-zero value is
// used as is and must be 512, 1024, 2048 or 4096.
func Read(r io.ReaderAt, size int64, sectorSize int) (*Table, error) {
	if r == nil {
		return nil, errors.New("volume: nil reader")
	}
	if size < 0 {
		return nil, fmt.Errorf("volume: negative image size %d", size)
	}
	switch sectorSize {
	case 0, 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("volume: unsupported sector size %d", sectorSize)
	}
	s := source{r: r, size: size}

	probe := []int{sectorSize}
	mbrSS := sectorSize
	if sectorSize == 0 {
		probe = []int{512, 4096}
		mbrSS = 512
	}
	var gptWarn []string
	for _, ss := range probe {
		t, w, err := readGPT(s, ss)
		if err != nil {
			return nil, err
		}
		if t != nil {
			return t, nil
		}
		gptWarn = append(gptWarn, w...)
	}
	t, err := readMBR(s, mbrSS, gptWarn)
	if err != nil {
		return nil, err
	}
	if t != nil {
		return t, nil
	}
	return &Table{
		Scheme:     "none",
		SectorSize: mbrSS,
		Partitions: []Partition{{Index: 0, Start: 0, Length: size, TypeName: "whole image"}},
		Warnings:   gptWarn,
	}, nil
}

// clampExtent converts an LBA extent to byte offsets inside the image. It
// reports ok=false (with a warning) when the extent starts outside the image
// or is empty, and clamps (with a warning) when it runs past the end.
func clampExtent(index int, startLBA, count uint64, ss int, size int64, warns *[]string) (start, length int64, ok bool) {
	if count == 0 {
		return 0, 0, false
	}
	start, fits := sectorOffset(startLBA, ss)
	if !fits || start >= size {
		*warns = append(*warns, fmt.Sprintf("partition %d starts past end of image (truncated image); skipped", index))
		return 0, 0, false
	}
	remaining := size - start
	if count > uint64(remaining)/uint64(ss) {
		*warns = append(*warns, fmt.Sprintf("partition %d extends past end of image (truncated image)", index))
		return start, remaining, true
	}
	return start, int64(count) * int64(ss), true
}

// finish fills Unallocated: [0,size) minus partitions minus structures.
func finish(t *Table, size int64, structures []Run) {
	used := make([]Run, 0, len(t.Partitions)+len(structures))
	for _, p := range t.Partitions {
		used = append(used, Run{p.Start, p.Length})
	}
	used = append(used, structures...)
	t.Unallocated = complement(size, used)
}

// complement returns [0,size) minus the union of used, sorted and merged,
// without zero-length runs. Runs are clamped to the image first.
func complement(size int64, used []Run) []Run {
	clamped := make([]Run, 0, len(used))
	for _, u := range used {
		start, end := u.Offset, u.Offset+u.Length
		if start < 0 {
			start = 0
		}
		if end > size {
			end = size
		}
		if end > start {
			clamped = append(clamped, Run{start, end - start})
		}
	}
	sort.Slice(clamped, func(i, j int) bool { return clamped[i].Offset < clamped[j].Offset })
	var out []Run
	var pos int64
	for _, u := range clamped {
		if u.Offset > pos {
			out = append(out, Run{pos, u.Offset - pos})
		}
		if end := u.Offset + u.Length; end > pos {
			pos = end
		}
	}
	if pos < size {
		out = append(out, Run{pos, size - pos})
	}
	return out
}
