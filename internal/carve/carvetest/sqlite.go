package carvetest

import "encoding/binary"

// SQLiteOpts shapes a synthetic SQLite 3 file built independently of the
// validator. Zero PageSize is 4096 and zero Pages is 1; PageSize 65536 is
// written as the value 1. ValidFor makes the in-header size trustworthy (the
// change counter equals the version-valid-for field). Page1Type is the b-tree
// type byte of page 1 (zero: 0x0D, a table leaf). BadFractions writes a
// payload fraction of 31 instead of 32.
type SQLiteOpts struct {
	PageSize, Pages uint32
	ValidFor        bool
	Page1Type       byte
	BadFractions    bool
}

// SQLiteFill is the byte that pads the pages after the headers.
const SQLiteFill = 0xA5

// SQLiteHeader returns the 100-byte database header and page 1's b-tree
// header, padded with SQLiteFill to PageSize x Pages bytes.
func SQLiteHeader(o SQLiteOpts) []byte {
	if o.PageSize == 0 {
		o.PageSize = 4096
	}
	if o.Pages == 0 {
		o.Pages = 1
	}
	if o.Page1Type == 0 {
		o.Page1Type = 0x0D
	}
	b := make([]byte, int(o.PageSize)*int(o.Pages))
	for i := range b {
		b[i] = SQLiteFill
	}
	h := b[:112]
	clear(h)
	copy(h, "SQLite format 3\x00")
	ps := o.PageSize
	if ps == 65536 {
		ps = 1
	}
	binary.BigEndian.PutUint16(h[16:], uint16(ps))
	h[18], h[19] = 1, 1
	h[21], h[22], h[23] = 64, 32, 32
	if o.BadFractions {
		h[23] = 31
	}
	binary.BigEndian.PutUint32(h[24:], 7) // change counter
	binary.BigEndian.PutUint32(h[28:], o.Pages)
	binary.BigEndian.PutUint32(h[44:], 4) // schema format
	binary.BigEndian.PutUint32(h[56:], 1) // UTF-8
	if o.ValidFor {
		binary.BigEndian.PutUint32(h[92:], 7)
	} else {
		binary.BigEndian.PutUint32(h[92:], 3)
	}
	binary.BigEndian.PutUint32(h[96:], 3040001)
	h[100] = o.Page1Type // 0 free blocks, 0 cells, content area at the page end
	return b
}
