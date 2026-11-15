package carvetest

import (
	"encoding/binary"
	"slices"
)

// JXLOpts shapes a synthetic JPEG XL container built by an encoder independent
// of the validator: the signature box, an ftyp, optional boxes, then the
// codestream in one jxlc box or in jxlp parts. The codestream bytes are fill:
// the container validator never reads them.
type JXLOpts struct {
	Codestream []byte   // nil = 64 bytes starting FF 0A
	Parts      int      // 0 = one jxlc; n = n jxlp boxes
	Extra      []string // boxes before the codestream: "Exif", "xml ", "jumb", "jxll", "brob"
	Brand      string   // major brand of the ftyp; "" = "jxl "

	SizeZeroLast bool // the last codestream box carries size 0 (extends to the end)
	BadIndex     bool // jxlp indices 0 and 2 (at least two parts)
	TwoLast      bool // two jxlp boxes both flagged last
	Both         bool // a jxlc and a jxlp
	NoCodestream bool
	Trailer      []byte
}

// jxlExtra returns one optional box by type.
func jxlExtra(typ string) []byte {
	switch typ {
	case "Exif":
		return Box(typ, make([]byte, 4), []byte("Exif\x00\x00"))
	case "xml ":
		return Box(typ, []byte("<x:xmpmeta/>"))
	case "jumb":
		return Box(typ, Box("jumd", make([]byte, 20)))
	case "jxll":
		return Box(typ, []byte{5})
	case "brob":
		return Box(typ, []byte("xml "), make([]byte, 24))
	}
	return Box(typ, make([]byte, 8))
}

// JXLContainer builds the container per o.
func JXLContainer(o JXLOpts) []byte {
	if o.Brand == "" {
		o.Brand = "jxl "
	}
	if o.Codestream == nil {
		o.Codestream = append([]byte{0xFF, 0x0A}, make([]byte, 62)...)
		for i := 2; i < len(o.Codestream); i++ {
			o.Codestream[i] = byte(i*7 + 3)
		}
	}
	b := Box("JXL ", []byte{0x0D, 0x0A, 0x87, 0x0A})
	b = slices.Concat(b, Box("ftyp", []byte(o.Brand), make([]byte, 4), []byte("jxl ")))
	for _, e := range o.Extra {
		b = slices.Concat(b, jxlExtra(e))
	}
	var cs [][]byte // codestream boxes in order
	jxlp := func(index uint32, last bool, data []byte) []byte {
		if last {
			index |= 1 << 31
		}
		return Box("jxlp", binary.BigEndian.AppendUint32(nil, index), data)
	}
	switch {
	case o.NoCodestream:
	case o.Both:
		cs = append(cs, Box("jxlc", o.Codestream[:len(o.Codestream)/2]), jxlp(0, true, o.Codestream[len(o.Codestream)/2:]))
	case o.Parts > 0 || o.BadIndex || o.TwoLast:
		n := max(o.Parts, 1)
		if o.BadIndex || o.TwoLast {
			n = max(n, 2)
		}
		size := (len(o.Codestream) + n - 1) / n
		for i := range n {
			lo := min(i*size, len(o.Codestream))
			hi := min(lo+size, len(o.Codestream))
			index, last := uint32(i), i == n-1
			if o.BadIndex && i > 0 {
				index++
			}
			if o.TwoLast && i == 0 {
				last = true
			}
			cs = append(cs, jxlp(index, last, o.Codestream[lo:hi]))
		}
	default:
		cs = append(cs, Box("jxlc", o.Codestream))
	}
	if o.SizeZeroLast && len(cs) > 0 {
		binary.BigEndian.PutUint32(cs[len(cs)-1], 0)
	}
	return slices.Concat(b, slices.Concat(cs...), o.Trailer)
}
