package ewf

import (
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"sort"
)

const (
	tableHeaderLen = 24 // entry_count u32, pad [4], base_offset u64, pad [4], adler32 u32
	tableEntryLen  = 4
	tableReadBlock = 64 << 10 // bytes of entries read per ReadAt while parsing

	maxLocation      = 1<<47 - 1 // chunkRef offset field; also the "outside the segment" sentinel
	disputedLocation = maxLocation - 1
)

// chunkRef is one chunk's table entry, resolved to a segment file location:
// bit 63 compressed, bits 47..62 segment index (0-based), bits 0..46 file
// offset. An entry that points outside its segment (or whose base+offset
// overflows) keeps its flag and segment but carries the offset maxLocation, so
// the read of that chunk fails with a ChunkError while its neighbours still
// read. The offset disputedLocation marks an entry on which table and table2
// disagree about its location; neither is believed.
// Memory is 8 bytes per chunk, bounded by the table bytes present.
type chunkRef uint64

const (
	refCompressed = chunkRef(1) << 63
	refSegShift   = 47
	refSegMask    = 1<<16 - 1
)

func makeRef(seg int, off uint64, compressed bool) chunkRef {
	r := chunkRef(seg)<<refSegShift | chunkRef(off)
	if compressed {
		r |= refCompressed
	}
	return r
}

func (c chunkRef) compressed() bool { return c&refCompressed != 0 }
func (c chunkRef) seg() int         { return int(c >> refSegShift & refSegMask) }
func (c chunkRef) off() int64       { return int64(c & maxLocation) }
func (c chunkRef) outside() bool    { return c&maxLocation == maxLocation }
func (c chunkRef) disputed() bool   { return c&maxLocation == disputedLocation }

// unlocated reports a ref that names no readable location.
func (c chunkRef) unlocated() bool { return c.outside() || c.disputed() }

// span is a [start, end) byte range of a segment file.
type span struct{ start, end int64 }

// spanOf returns the span of spans (sorted, non-overlapping) containing off.
func spanOf(spans []span, off int64) (span, bool) {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].end > off })
	if i < len(spans) && spans[i].start <= off {
		return spans[i], true
	}
	return span{}, false
}

// tbl is the outcome of parsing one table or table2 section. reason is
// non-empty when the table is NOT trusted (a checksum failed or the counts are
// inconsistent); refs is only meaningful for a trusted table. rule is set (by
// the caller) to the first structural rule the entries break, see rules.
type tbl struct {
	off    int64 // section descriptor offset, for messages
	label  string
	refs   []chunkRef
	reason string
	rule   string
	footer bool // the entries' Adler-32 footer is present and verified
}

func (t *tbl) trusted() bool { return t.reason == "" }

// parseTable validates one table/table2 section of segment seg and resolves
// its entries. A table is trusted when the header checksum passes, the
// entry count is consistent with the section size (no footer, or a footer
// whose Adler-32 passes) and it does not exceed limit, the number of chunks
// the volume still expects. Entries are streamed in blocks, so memory is
// bounded by the entries actually present. The error is only ever an I/O
// error from the segment reader.
func parseTable(seg int, s Segment, sec section, limit int64) (tbl, error) {
	t := tbl{off: sec.off, label: sec.label()}
	plen := sec.payloadLen()
	if plen < tableHeaderLen {
		t.reason = fmt.Sprintf("payload is %d bytes, smaller than the %d-byte header", plen, tableHeaderLen)
		return t, nil
	}
	var h [tableHeaderLen]byte
	if err := readFull(s.R, h[:], sec.payloadOff()); err != nil {
		return t, ioError(seg+1, t.label+" section", sec.payloadOff(), err)
	}
	if got, want := binary.LittleEndian.Uint32(h[20:]), adler32.Checksum(h[:20]); got != want {
		t.reason = fmt.Sprintf("header checksum mismatch (stored %#08x, computed %#08x)", got, want)
		return t, nil
	}
	n := int64(binary.LittleEndian.Uint32(h[0:]))
	base := binary.LittleEndian.Uint64(h[8:])
	fit := (plen - tableHeaderLen) / tableEntryLen
	if n > fit {
		t.reason = fmt.Sprintf("entry_count %d, but only %d entries fit the %d-byte payload", n, fit, plen)
		return t, nil
	}
	// Older writers omit the trailing Adler-32; size arithmetic decides.
	rem := plen - tableHeaderLen - n*tableEntryLen
	if rem != 0 && rem != 4 {
		t.reason = fmt.Sprintf("%d entries leave %d unexplained bytes in the %d-byte payload", n, rem, plen)
		return t, nil
	}
	if n > limit {
		t.reason = fmt.Sprintf("%d entries, but the volume has only %d chunks left to cover", n, limit)
		return t, nil
	}
	sum := adler32.New()
	t.refs = make([]chunkRef, 0, n) // n entries fit the payload: bounded by the bytes present
	buf := make([]byte, min(n, tableReadBlock/tableEntryLen)*tableEntryLen)
	pos := sec.payloadOff() + tableHeaderLen
	for done := int64(0); done < n; {
		k := min(n-done, int64(len(buf)/tableEntryLen))
		b := buf[:k*tableEntryLen]
		if err := readFull(s.R, b, pos); err != nil {
			return t, ioError(seg+1, t.label+" section", pos, err)
		}
		_, _ = sum.Write(b)
		t.appendRefs(seg, s.Size, base, b)
		pos += int64(len(b))
		done += k
	}
	if rem == 4 {
		var f [4]byte
		if err := readFull(s.R, f[:], pos); err != nil {
			return t, ioError(seg+1, t.label+" section", pos, err)
		}
		if got, want := binary.LittleEndian.Uint32(f[:]), sum.Sum32(); got != want {
			t.reason = fmt.Sprintf("entries checksum mismatch (stored %#08x, computed %#08x)", got, want)
			t.refs = nil
			return t, nil
		}
		t.footer = true
	}
	return t, nil
}

// appendRefs resolves a block of 4-byte entries. An entry outside the segment
// is kept as such; rule (d) then rejects the table.
func (t *tbl) appendRefs(seg int, segSize int64, base uint64, b []byte) {
	for i := 0; i+tableEntryLen <= len(b); i += tableEntryLen {
		e := binary.LittleEndian.Uint32(b[i:])
		comp := e&0x80000000 != 0
		loc, ok := addOK(base, uint64(e&0x7fffffff))
		if !ok || loc >= uint64(segSize) || loc >= disputedLocation {
			t.refs = append(t.refs, makeRef(seg, maxLocation, comp))
			continue
		}
		t.refs = append(t.refs, makeRef(seg, loc, comp))
	}
}

// firstDiff returns the index of the first entry where a and b differ, or -1.
func firstDiff(a, b []chunkRef) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}
