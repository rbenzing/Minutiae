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

	maxLocation = 1<<47 - 1 // chunkRef offset field; also the "no location" sentinel
)

// chunkRef is one chunk's table entry, resolved to a segment file location:
// bit 63 compressed, bits 47..62 segment index (0-based), bits 0..46 file
// offset. An entry that points outside its segment (or whose base+offset
// overflows) keeps its flag and segment but carries the offset maxLocation, so
// the read of that chunk fails with a ChunkError while its neighbours still
// read. Memory is 8 bytes per chunk, bounded by the table bytes present.
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
// inconsistent); refs and outside are only meaningful for a trusted table.
type tbl struct {
	off     int64 // section descriptor offset, for messages
	label   string
	refs    []chunkRef
	reason  string
	outside int // entries that point outside the segment
	first   int // index of the first such entry
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
	t.refs = make([]chunkRef, 0, min(n, tableReadBlock/tableEntryLen))
	buf := make([]byte, min(n, tableReadBlock/tableEntryLen)*tableEntryLen)
	pos := sec.payloadOff() + tableHeaderLen
	for done := int64(0); done < n; {
		k := min(n-done, int64(len(buf)/tableEntryLen))
		b := buf[:k*tableEntryLen]
		if err := readFull(s.R, b, pos); err != nil {
			return t, ioError(seg+1, t.label+" section", pos, err)
		}
		_, _ = sum.Write(b)
		t.appendRefs(seg, s.Size, base, b, int(done))
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
	}
	return t, nil
}

// appendRefs resolves a block of 4-byte entries; first is the table index of
// the block's first entry.
func (t *tbl) appendRefs(seg int, segSize int64, base uint64, b []byte, first int) {
	for i := 0; i+tableEntryLen <= len(b); i += tableEntryLen {
		e := binary.LittleEndian.Uint32(b[i:])
		comp := e&0x80000000 != 0
		loc, ok := addOK(base, uint64(e&0x7fffffff))
		if !ok || loc >= uint64(segSize) || loc >= maxLocation {
			if t.outside == 0 {
				t.first = first + i/tableEntryLen
			}
			t.outside++
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

// tables parses every table group of segment i. A group is a table section
// optionally followed immediately by its table2 copy; a lone table or table2 is
// accepted with a warning. Per group the table is used when trusted, else the
// table2 when trusted (warning), else the image is corrupt. The chosen
// entries cover the next consecutive chunk indexes.
func (o *opener) tables(i int, secs []section) error {
	n := i + 1
	s := o.r.segs[i]
	for j := 0; j < len(secs); j++ {
		var tab, tab2 *section
		switch secs[j].kind {
		case kTable:
			tab = &secs[j]
			if j+1 < len(secs) && secs[j+1].kind == kTable2 {
				tab2 = &secs[j+1]
				j++
			}
		case kTable2:
			tab2 = &secs[j]
		default:
			continue
		}
		if err := o.tableGroup(i, n, s, tab, tab2); err != nil {
			return err
		}
	}
	return nil
}

func (o *opener) tableGroup(i, n int, s Segment, tab, tab2 *section) error {
	limit := int64(o.r.geo.chunks) - int64(len(o.r.refs))
	var a, b tbl
	var err error
	if tab != nil {
		if a, err = parseTable(i, s, *tab, limit); err != nil {
			return err
		}
	}
	if tab2 != nil {
		if b, err = parseTable(i, s, *tab2, limit); err != nil {
			return err
		}
	}
	var use *tbl
	switch {
	case tab != nil && tab2 != nil:
		switch {
		case a.trusted() && b.trusted():
			use = &a
			if d := firstDiff(a.refs, b.refs); d >= 0 {
				o.r.warn.add("segment %d: table at offset %d and its table2 differ (first at entry %d); using table", n, a.off, d)
			}
		case a.trusted():
			use = &a
			o.r.warn.add("segment %d: table2 at offset %d unreadable (%s); using table", n, b.off, b.reason)
		case b.trusted():
			use = &b
			o.r.warn.add("segment %d: table at offset %d unreadable (%s); using table2", n, a.off, a.reason)
		default:
			return corrupt(n, "table", "table at offset %d unreadable (%s) and table2 at offset %d unreadable (%s)", a.off, a.reason, b.off, b.reason)
		}
	case tab != nil:
		if !a.trusted() {
			return corrupt(n, "table", "table at offset %d unreadable (%s) and has no table2", a.off, a.reason)
		}
		use = &a
		o.r.warn.add("segment %d: table at offset %d has no table2", n, a.off)
	default:
		if !b.trusted() {
			return corrupt(n, "table2", "table2 at offset %d unreadable (%s) and has no table", b.off, b.reason)
		}
		use = &b
		o.r.warn.add("segment %d: table2 at offset %d has no table", n, b.off)
	}
	if use.outside > 0 {
		o.r.warn.add("segment %d: %s at offset %d: %d of %d entries point outside the segment (first: chunk %d); those chunks cannot be read",
			n, use.label, use.off, use.outside, len(use.refs), int64(len(o.r.refs))+int64(use.first))
	}
	o.r.refs = append(o.r.refs, use.refs...)
	return nil
}
