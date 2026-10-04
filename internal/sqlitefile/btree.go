package sqlitefile

import (
	"encoding/binary"
	"fmt"
)

// PageType is the flag byte of a b-tree page.
type PageType uint8

// The four b-tree page types.
const (
	PageIndexInterior PageType = 0x02
	PageTableInterior PageType = 0x05
	PageIndexLeaf     PageType = 0x0a
	PageTableLeaf     PageType = 0x0d
)

func (t PageType) interior() bool { return t == PageIndexInterior || t == PageTableInterior }

func (t PageType) valid() bool {
	return t == PageIndexInterior || t == PageTableInterior || t == PageIndexLeaf || t == PageTableLeaf
}

// PageHeader is the b-tree header of a page (at offset 100 on page 1).
type PageHeader struct {
	Type           PageType
	FirstFreeblock int
	CellCount      int
	ContentStart   int // 65536 when stored as 0
	Fragmented     int
	RightChild     uint32 // interior pages only
	Base           int    // offset of the header: 100 on page 1, else 0
	HeaderSize     int    // 8 (leaf) or 12 (interior)
}

// pointerArrayEnd is the offset just past the cell pointer array.
func (h PageHeader) pointerArrayEnd() int { return h.Base + h.HeaderSize + 2*h.CellCount }

// ParsePageHeader decodes the b-tree header of page, which holds the bytes of
// page number pgno that are present (a trailing partial page of a truncated
// file is shorter than the page size). A flag that is not one of the four
// types, a header that does not fit in the bytes present and a cell count
// whose pointer array does not fit are errors wrapping ErrCorrupt.
func ParsePageHeader(page []byte, pgno uint32) (PageHeader, error) {
	var h PageHeader
	if pgno == 1 {
		h.Base = 100
	}
	bad := func(format string, a ...any) (PageHeader, error) {
		return PageHeader{}, &CorruptError{File: FileDB, Page: pgno, Reason: fmt.Sprintf(format, a...)}
	}
	if len(page) < h.Base+8 {
		return bad("the b-tree header does not fit in the %d bytes of the page", len(page))
	}
	h.Type = PageType(page[h.Base])
	if !h.Type.valid() {
		return bad("page flag %#02x is not a b-tree page type", uint8(h.Type))
	}
	h.HeaderSize = 8
	if h.Type.interior() {
		h.HeaderSize = 12
		if len(page) < h.Base+12 {
			return bad("the interior b-tree header does not fit in the %d bytes of the page", len(page))
		}
		h.RightChild = binary.BigEndian.Uint32(page[h.Base+8:])
	}
	be := binary.BigEndian
	h.FirstFreeblock = int(be.Uint16(page[h.Base+1:]))
	h.CellCount = int(be.Uint16(page[h.Base+3:]))
	h.ContentStart = int(be.Uint16(page[h.Base+5:]))
	if h.ContentStart == 0 {
		h.ContentStart = 65536
	}
	h.Fragmented = int(page[h.Base+7])
	if end := h.pointerArrayEnd(); end > len(page) {
		return bad("%d cells need a pointer array to offset %d, past the %d bytes of the page", h.CellCount, end, len(page))
	}
	return h, nil
}

// CellPointers returns the cell pointer array of page, validated: every
// pointer must lie in [max(ContentStart, end of the pointer array), usable)
// and inside the bytes present. A pointer into the reserved-bytes region
// [usable, pagesize), the header, the pointer array or below the content
// start is invalid. The result always has CellCount entries; an invalid
// pointer is returned as -1 and the error (wrapping ErrCorrupt, naming the
// first one and how many) is non-nil, so a caller may keep the cells that
// are good or skip the page. A pointer array that itself reaches past usable
// leaves no valid pointer: the result is nil with the error.
func CellPointers(page []byte, h PageHeader, usable int) ([]int, error) {
	end := h.pointerArrayEnd()
	if end > usable || end > len(page) {
		return nil, &CorruptError{File: FileDB, Reason: fmt.Sprintf("%d cell pointers reach offset %d, past the usable size %d", h.CellCount, end, usable)}
	}
	lo := max(h.ContentStart, end)
	hi := min(usable, len(page))
	out := make([]int, h.CellCount)
	bad, first := 0, -1
	for i := range out {
		p := int(binary.BigEndian.Uint16(page[h.Base+h.HeaderSize+2*i:]))
		if p < lo || p >= hi {
			out[i] = -1
			if bad == 0 {
				first = i
			}
			bad++
			continue
		}
		out[i] = p
	}
	if bad > 0 {
		p := int(binary.BigEndian.Uint16(page[h.Base+h.HeaderSize+2*first:]))
		return out, &CorruptError{File: FileDB, Reason: fmt.Sprintf("%d of %d cell pointers outside [%d, %d); first: cell %d points at %d", bad, h.CellCount, lo, hi, first, p)}
	}
	return out, nil
}
