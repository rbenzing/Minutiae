package carvetest

import "encoding/binary"

// ICOOpts shapes a synthetic ICO or CUR built by an encoder independent of the
// validator. The directory is written for Images in order; each image is
// written after the directory, in Order, and its entry points at where it lies.
type ICOOpts struct {
	Cursor        bool
	Images        [][]byte // PNG or DIB bytes, written after the directory in this order
	Order         []int    // optional placement order (offsets not ascending), nil = in order
	Count         int      // overrides the count field, 0 = len(Images)
	ReservedByte  byte     // entry 0 reserved byte (entry offset 3)
	OffsetIntoDir bool     // entry 0 offset points inside the directory
	ZeroSize      bool     // entry 0 size is 0
	SizeDelta     int      // added to the size of entry 0
}

// ICO builds the file per o. An ICO entry carries planes=1 and bpp=32; a CUR
// entry carries hotspot values (3, 5) in those two fields, which a reader must
// not judge.
func ICO(o ICOOpts) []byte {
	n := len(o.Images)
	count := n
	if o.Count != 0 {
		count = o.Count
	}
	le := binary.LittleEndian
	typ := uint16(1)
	if o.Cursor {
		typ = 2
	}
	dir := make([]byte, 6+16*n)
	le.PutUint16(dir[2:], typ)
	le.PutUint16(dir[4:], uint16(count))
	order := o.Order
	if order == nil {
		order = make([]int, n)
		for i := range order {
			order[i] = i
		}
	}
	offsets := make([]int, n)
	pos := len(dir)
	var body []byte
	for _, i := range order {
		offsets[i] = pos
		body = append(body, o.Images[i]...)
		pos += len(o.Images[i])
	}
	for i, img := range o.Images {
		e := dir[6+16*i:]
		e[0], e[1] = 16, 16
		planes, bpp := uint16(1), uint16(32)
		if o.Cursor {
			planes, bpp = 3, 5
		}
		le.PutUint16(e[4:], planes)
		le.PutUint16(e[6:], bpp)
		size, off := len(img), offsets[i]
		if i == 0 {
			e[3] = o.ReservedByte
			if o.ZeroSize {
				size = 0
			}
			size += o.SizeDelta
			if o.OffsetIntoDir {
				off = 6
			}
		}
		le.PutUint32(e[8:], uint32(size))
		le.PutUint32(e[12:], uint32(off))
	}
	return append(dir, body...)
}
