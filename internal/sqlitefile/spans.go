package sqlitefile

import (
	"encoding/binary"
	"fmt"
)

// spanResult is what freeSpans found in a b-tree page image.
type spanResult struct {
	spans      []Span
	fragmented int
	rejected   int    // freeblock-chain violations (the chain stops at the first)
	problem    string // the first violation, for the warning
}

// freeSpans lists the free areas of a b-tree page image: the gap between the
// cell pointer array and the cell content area, and each freeblock of the
// chain. A chain is followed with ascending offsets only; each block must lie
// inside the usable area, below no other structure (after the pointer array and
// the content start), be at least 4 bytes, end before the next block and fit in
// the page; the walk is capped at usable/4 blocks. The first violation ends the
// chain (the blocks before it are kept). fileOff is where the page starts in the
// file the image comes from.
func freeSpans(page []byte, h PageHeader, usable int, fileOff int64) spanResult {
	res := spanResult{fragmented: h.Fragmented}
	hi := min(usable, len(page))
	lo := h.pointerArrayEnd()
	gapEnd := min(h.ContentStart, hi)
	if gapEnd > lo {
		res.spans = append(res.spans, Span{Kind: SpanGap, Offset: lo, Length: gapEnd - lo, FileOffset: fileOff + int64(lo)})
	}
	// a freeblock lies in the content area, never in the gap
	floor := max(lo, h.ContentStart)
	limit := hi / 4
	off, prevEnd := h.FirstFreeblock, 0
	for n := 0; off != 0; n++ {
		bad := func(format string, a ...any) spanResult {
			res.rejected++
			res.problem = fmt.Sprintf(format, a...)
			return res
		}
		switch {
		case n >= limit:
			return bad("the freeblock chain is longer than %d blocks", limit)
		case off < floor || off+4 > hi:
			return bad("freeblock at offset %d lies outside the content area [%d, %d)", off, floor, hi)
		case off < prevEnd:
			return bad("freeblock at offset %d does not lie after the end (%d) of the one before it (descending, repeated or overlapping offsets)", off, prevEnd)
		}
		next := int(binary.BigEndian.Uint16(page[off:]))
		size := int(binary.BigEndian.Uint16(page[off+2:]))
		switch {
		case size < 4:
			return bad("freeblock at offset %d has size %d, below 4", off, size)
		case off+size > hi:
			return bad("freeblock at offset %d of size %d ends past the usable area (%d)", off, size, hi)
		}
		res.spans = append(res.spans, Span{Kind: SpanFreeblock, Offset: off, Length: size, FileOffset: fileOff + int64(off)})
		prevEnd, off = off+size, next
	}
	return res
}

// trunkTail is the span of the bytes of a freelist trunk page after its leaf
// list (8 + 4L), up to the end of the usable area; nil when the leaf count does
// not leave any.
func trunkTail(page []byte, usable int, fileOff int64) []Span {
	if len(page) < 8 {
		return nil
	}
	hi := int64(min(usable, len(page)))
	start := 8 + 4*int64(binary.BigEndian.Uint32(page[4:]))
	if start >= hi {
		return nil
	}
	return []Span{{Kind: SpanTrunkTail, Offset: int(start), Length: int(hi - start), FileOffset: fileOff + start}}
}

// Parse reads the image as a b-tree page: the header, the cells that can be
// parsed, the free spans and the fragmented byte count. A pointer or cell that
// cannot be used, and a freeblock chain that breaks, are counted in Rejected
// (the chain also warns freeblock-chain). An image that is not a b-tree page is
// an error wrapping ErrCorrupt.
func (p PageImage) Parse() (pp PageParse, err error) {
	defer guard(&err)
	data, err := p.Bytes()
	if err != nil {
		return PageParse{}, err
	}
	usable := p.h.d.info.UsableSize
	h, err := ParsePageHeader(data, p.Number)
	if err != nil {
		return PageParse{}, err
	}
	cp, err := CellPointers(data, h, usable)
	if err != nil {
		return PageParse{}, err
	}
	pp.Header = h
	pp.Rejected = len(cp.Bad)
	for _, g := range cp.Good {
		c, err := ParseCell(data, usable, h, g.Offset)
		if err != nil {
			pp.Rejected++
			continue
		}
		c.Index = g.Index
		pp.Cells = append(pp.Cells, c)
	}
	sr := freeSpans(data, h, usable, p.Loc.Offset)
	pp.Spans, pp.FragmentedBytes = sr.spans, sr.fragmented
	pp.Rejected += sr.rejected
	if sr.rejected > 0 {
		p.h.warnChain(p, sr.problem)
	}
	return pp, nil
}

// warnChain reports a broken freeblock chain of image p.
func (h *Hist) warnChain(p PageImage, problem string) {
	h.warns.add(Warning{
		Code: WarnFreeblockChain, File: p.Loc.File, Page: p.Number, Offset: p.Loc.Offset,
		Msg: fmt.Sprintf("page %d (%s): %s; the free spans after it are not listed", p.Number, p.Origin, problem),
	})
}
