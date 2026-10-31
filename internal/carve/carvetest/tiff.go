package carvetest

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
)

// TIFFEntry is one directory entry the caller supplies. Value is the entry's
// value bytes already in the file's byte order (use TIFFValue); it is written
// inline when it fits the entry's value field and out of line otherwise. A
// Count of 0 with a non-empty Value is taken as len(Value) / the type size.
type TIFFEntry struct {
	Tag, Type uint16
	Count     uint64
	Value     []byte
}

// TIFFIFD is one image directory. The builder writes Strips, Tiles and
// JPEGInterchange data itself and adds the offset and count tags.
type TIFFIFD struct {
	Entries         []TIFFEntry
	Strips, Tiles   [][]byte
	ShortArrays     bool   // the offset and count arrays are typed SHORT
	JPEGInterchange []byte // tags 513 and 514
	SubIFDs         []TIFFIFD
	// Exif, GPS and Interop hang under a page: Interop is written as the
	// Interoperability IFD of the page's Exif IFD (an empty Exif IFD is made if
	// Exif is nil).
	Exif, GPS, Interop *TIFFIFD
	MakerNote          []byte // tag 37500 in the page's Exif IFD
	DNGVersion         bool   // tag 50706
}

// TIFFOpts shapes a TIFF built by an encoder independent of the validator.
type TIFFOpts struct {
	BigEndian, Big bool
	Magic          string   // "" = by form; "IIRO", "IIRS", "MMOR", "IIU\x00" replace bytes 0-3
	CR2            bool     // "CR\x02\x00" at 8 and IFD0 at 16 (the IFDs are then written first)
	CR2Raw         *TIFFIFD // with CR2: an IFD on no next-IFD chain, its offset in header bytes 12..15
	Pages          []TIFFIFD
	IFDsFirst      bool // IFDs before the data (else after, the libtiff layout)
	// LateValueOf writes the first out-of-line value of the Entries of page N
	// last in the file; -1 switches it off (the zero value is page 0).
	LateValueOf int
	Hostile     TIFFHostile
}

// TIFFHostile switches on one malformation, each applied to the first page
// unless noted.
type TIFFHostile struct {
	NextLoop      bool // the last page's next-IFD points back at the first page
	SelfSubIFD    bool // page 0 lists its own offset in tag 330
	ExifToIFD0    bool // the last page's Exif pointer targets IFD0
	ZeroByteCount bool // the first strip (tile, JPEG) byte count is 0
	CountMismatch bool // one more byte count than offsets
	BadEntryType  uint16
	EntryCount    uint64 // overrides IFD0's count field, 0 = real
}

// TIFFValue returns vals encoded as the type: one value per element, two per
// RATIONAL or SRATIONAL.
func TIFFValue(bigEndian bool, typ uint16, vals ...uint64) []byte {
	sz := tiffSize(typ)
	if sz == 0 {
		panic(fmt.Sprintf("carvetest: TIFF type %d", typ))
	}
	if typ == 5 || typ == 10 {
		sz /= 2
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		bo = binary.BigEndian
	}
	var out []byte
	for _, v := range vals {
		out = append(out, tiffUint(bo, v, sz)...)
	}
	return out
}

func tiffSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11, 13:
		return 4
	case 5, 10, 12, 16, 17, 18:
		return 8
	}
	return 0
}

func tiffUint(bo binary.ByteOrder, v uint64, width int) []byte {
	b := make([]byte, width)
	switch width {
	case 1:
		b[0] = byte(v)
	case 2:
		if v > 0xFFFF {
			panic("carvetest: TIFF value does not fit a SHORT")
		}
		bo.PutUint16(b, uint16(v))
	case 4:
		if v > 0xFFFFFFFF {
			panic("carvetest: TIFF value does not fit a LONG")
		}
		bo.PutUint32(b, uint32(v))
	case 8:
		bo.PutUint64(b, v)
	}
	return b
}

// tiffRef is a position in one of the builder's segments, known once placed.
type tiffRef struct {
	seg, off int
	set      bool
}

type tiffPatch struct {
	hdr   bool // pos is in the header, else in the structure segment
	pos   int
	width int
	ref   *tiffRef
}

type tiffEnt struct {
	tag, typ uint16
	count    uint64
	val      []byte
	patches  []tiffPatch // pos relative to val
	late     bool
}

const (
	segStruct = 0 // IFDs and out-of-line values
	segData   = 1 // strips, tiles, JPEG interchange data
	segLate   = 2 // values forced to the end of the file
)

type tiffEnc struct {
	o        TIFFOpts
	bo       binary.ByteOrder
	hdrLen   int
	seg      [3][]byte
	patches  []tiffPatch
	pageRef0 *tiffRef
}

func (e *tiffEnc) inline() int {
	if e.o.Big {
		return 8
	}
	return 4
}

func (e *tiffEnc) emit(seg int, b []byte) *tiffRef {
	if len(e.seg[seg])%2 == 1 {
		e.seg[seg] = append(e.seg[seg], 0)
	}
	r := &tiffRef{seg: seg, off: len(e.seg[seg]), set: true}
	e.seg[seg] = append(e.seg[seg], b...)
	return r
}

func (e *tiffEnc) newRef() *tiffRef { return &tiffRef{} }

// offsetWidth is the width of an offset-valued element for this form.
func (e *tiffEnc) offsetWidth(short bool) (typ uint16, width int) {
	switch {
	case short:
		return 3, 2
	case e.o.Big:
		return 16, 8
	}
	return 4, 4
}

func (e *tiffEnc) refArray(tag uint16, short bool, refs []*tiffRef) tiffEnt {
	typ, w := e.offsetWidth(short)
	ent := tiffEnt{tag: tag, typ: typ, count: uint64(len(refs)), val: make([]byte, w*len(refs))}
	for i, r := range refs {
		ent.patches = append(ent.patches, tiffPatch{pos: i * w, width: w, ref: r})
	}
	return ent
}

func (e *tiffEnc) litArray(tag uint16, short bool, vals []uint64) tiffEnt {
	typ, w := e.offsetWidth(short)
	ent := tiffEnt{tag: tag, typ: typ, count: uint64(len(vals))}
	for _, v := range vals {
		ent.val = append(ent.val, tiffUint(e.bo, v, w)...)
	}
	return ent
}

// dataEnts places the data blobs and returns the offset and count entries.
func (e *tiffEnc) dataEnts(offTag, cntTag uint16, blobs [][]byte, short bool, hostile TIFFHostile, first bool) []tiffEnt {
	if len(blobs) == 0 {
		return nil
	}
	refs := make([]*tiffRef, len(blobs))
	lens := make([]uint64, len(blobs))
	for i, b := range blobs {
		refs[i] = e.emit(segData, b)
		lens[i] = uint64(len(b))
	}
	if first && hostile.ZeroByteCount {
		lens[0] = 0
	}
	if first && hostile.CountMismatch {
		lens = append(lens, 1)
	}
	return []tiffEnt{e.refArray(offTag, short, refs), e.litArray(cntTag, short, lens)}
}

type ifdKind int

const (
	kindMain ifdKind = iota
	kindExif
	kindOther
)

// ifd writes d into the structure segment, with self set to its offset and
// next (nil = none) as its next-IFD, then its sub-IFDs.
func (e *tiffEnc) ifd(d TIFFIFD, kind ifdKind, self, next *tiffRef, page int, lastPage bool) {
	h := e.o.Hostile
	first := page == 0
	var ents []tiffEnt
	late := page >= 0 && page == e.o.LateValueOf
	for _, u := range d.Entries {
		sz := tiffSize(u.Type)
		cnt := u.Count
		if cnt == 0 && len(u.Value) > 0 && sz > 0 {
			cnt = uint64(len(u.Value) / sz)
		}
		ent := tiffEnt{tag: u.Tag, typ: u.Type, count: cnt, val: slices.Clone(u.Value)}
		if late && len(ent.val) > e.inline() {
			ent.late, late = true, false
		}
		ents = append(ents, ent)
	}
	if kind == kindMain {
		ents = append(ents, e.dataEnts(273, 279, d.Strips, d.ShortArrays, h, first)...)
		ents = append(ents, e.dataEnts(324, 325, d.Tiles, d.ShortArrays, h, first && len(d.Strips) == 0)...)
		if d.JPEGInterchange != nil {
			r := e.emit(segData, d.JPEGInterchange)
			n := uint64(len(d.JPEGInterchange))
			if first && h.ZeroByteCount && len(d.Strips) == 0 && len(d.Tiles) == 0 {
				n = 0
			}
			typ, w := e.offsetWidth(false)
			jo := tiffEnt{tag: 513, typ: typ, count: 1, val: make([]byte, w), patches: []tiffPatch{{width: w, ref: r}}}
			jl := tiffEnt{tag: 514, typ: typ, count: 1, val: tiffUint(e.bo, n, w)}
			ents = append(ents, jo, jl)
		}
		if first && h.BadEntryType != 0 {
			ents = append(ents, tiffEnt{tag: 65000, typ: h.BadEntryType, count: 1, val: make([]byte, e.inline())})
		}
	}
	if d.DNGVersion {
		ents = append(ents, tiffEnt{tag: 50706, typ: 1, count: 4, val: []byte{1, 4, 0, 0}})
	}
	if kind == kindExif && d.MakerNote != nil {
		ents = append(ents, tiffEnt{tag: 37500, typ: 7, count: uint64(len(d.MakerNote)), val: slices.Clone(d.MakerNote)})
	}

	// child IFDs, written after this one
	type child struct {
		d    TIFFIFD
		kind ifdKind
		ref  *tiffRef
	}
	var kids []child
	addPtr := func(tag uint16, refs ...*tiffRef) {
		ent := e.refArray(tag, false, refs)
		ents = append(ents, ent)
	}
	if kind == kindMain {
		var subs []*tiffRef
		for _, s := range d.SubIFDs {
			c := child{s, kindMain, e.newRef()}
			kids, subs = append(kids, c), append(subs, c.ref)
		}
		if first && h.SelfSubIFD {
			subs = append(subs, self)
		}
		if len(subs) > 0 {
			addPtr(330, subs...)
		}
		exif := d.Exif
		if d.MakerNote != nil || d.Interop != nil {
			x := TIFFIFD{}
			if exif != nil {
				x = *exif
			}
			if x.MakerNote == nil {
				x.MakerNote = d.MakerNote
			}
			if x.Interop == nil {
				x.Interop = d.Interop
			}
			exif = &x
		}
		switch {
		case lastPage && h.ExifToIFD0:
			addPtr(34665, e.pageRef0)
		case exif != nil:
			c := child{*exif, kindExif, e.newRef()}
			kids = append(kids, c)
			addPtr(34665, c.ref)
		}
		if d.GPS != nil {
			c := child{*d.GPS, kindOther, e.newRef()}
			kids = append(kids, c)
			addPtr(34853, c.ref)
		}
	}
	if kind == kindExif && d.Interop != nil {
		c := child{*d.Interop, kindOther, e.newRef()}
		kids = append(kids, c)
		addPtr(40965, c.ref)
	}

	sort.SliceStable(ents, func(i, j int) bool { return ents[i].tag < ents[j].tag })
	if len(ents) == 0 {
		panic("carvetest: an empty TIFF directory")
	}

	// the directory itself: reserve its bytes, then fill them in place
	entSize, cntSize, nextSize := 12, 2, 4
	if e.o.Big {
		entSize, cntSize, nextSize = 20, 8, 8
	}
	count := uint64(len(ents))
	if page == 0 && h.EntryCount != 0 {
		count = h.EntryCount
	}
	at := e.emit(segStruct, make([]byte, cntSize+entSize*len(ents)+nextSize))
	*self = *at
	blob := e.seg[segStruct][at.off : at.off+cntSize+entSize*len(ents)+nextSize]
	copy(blob, tiffUint(e.bo, count, cntSize))
	for i, ent := range ents {
		p := cntSize + i*entSize
		e.bo.PutUint16(blob[p:], ent.tag)
		e.bo.PutUint16(blob[p+2:], ent.typ)
		cw := e.inline() // the count is as wide as the inline value field
		copy(blob[p+4:], tiffUint(e.bo, ent.count, cw))
		field := p + 4 + cw
		if len(ent.val) <= e.inline() {
			copy(blob[field:], ent.val)
			for _, pt := range ent.patches {
				e.patches = append(e.patches, tiffPatch{pos: at.off + field + pt.pos, width: pt.width, ref: pt.ref})
			}
			continue
		}
		seg := segStruct
		if ent.late {
			seg = segLate
		}
		vr := e.emit(seg, ent.val)
		blob = e.seg[segStruct][at.off : at.off+cntSize+entSize*len(ents)+nextSize] // emit may have moved the segment
		e.patches = append(e.patches, tiffPatch{pos: at.off + field, width: e.inline(), ref: vr})
		if seg == segLate && len(ent.patches) > 0 {
			panic("carvetest: a late value cannot carry offsets")
		}
		for _, pt := range ent.patches {
			e.patches = append(e.patches, tiffPatch{pos: vr.off + pt.pos, width: pt.width, ref: pt.ref})
		}
	}
	if next != nil {
		e.patches = append(e.patches, tiffPatch{pos: at.off + cntSize + entSize*len(ents), width: nextSize, ref: next})
	}
	for _, k := range kids {
		e.ifd(k.d, k.kind, k.ref, nil, -1, false)
	}
}

// TIFF builds a TIFF or BigTIFF per o.
func TIFF(o TIFFOpts) []byte {
	if len(o.Pages) == 0 {
		panic("carvetest: a TIFF needs a page")
	}
	e := &tiffEnc{o: o, bo: binary.LittleEndian}
	if o.BigEndian {
		e.bo = binary.BigEndian
	}
	if o.CR2 {
		o.IFDsFirst, e.o.IFDsFirst = true, true
	}
	e.hdrLen = 8
	if o.Big || o.CR2 {
		e.hdrLen = 16
	}
	refs := make([]*tiffRef, len(o.Pages))
	for i := range refs {
		refs[i] = e.newRef()
	}
	e.pageRef0 = refs[0]
	for i, p := range o.Pages {
		var next *tiffRef
		switch {
		case i+1 < len(refs):
			next = refs[i+1]
		case o.Hostile.NextLoop:
			next = refs[0]
		}
		e.ifd(p, kindMain, refs[i], next, i, i == len(refs)-1)
	}
	var rawRef *tiffRef
	if o.CR2 && o.CR2Raw != nil {
		rawRef = e.newRef()
		e.ifd(*o.CR2Raw, kindMain, rawRef, nil, -1, false)
	}

	// assemble
	hdr := make([]byte, e.hdrLen)
	order := "II"
	if o.BigEndian {
		order = "MM"
	}
	copy(hdr, order)
	switch {
	case o.Big:
		e.bo.PutUint16(hdr[2:], 43)
		e.bo.PutUint16(hdr[4:], 8)
	default:
		e.bo.PutUint16(hdr[2:], 42)
	}
	if o.Magic != "" {
		if len(o.Magic) != 4 {
			panic("carvetest: a TIFF magic is four bytes")
		}
		copy(hdr, o.Magic)
	}
	if o.CR2 {
		copy(hdr[8:], "CR\x02\x00")
	}
	ifd0Pos, ifd0W := 4, 4
	if o.Big {
		ifd0Pos, ifd0W = 8, 8
	}
	e.patches = append(e.patches, tiffPatch{hdr: true, pos: ifd0Pos, width: ifd0W, ref: refs[0]})
	if rawRef != nil {
		e.patches = append(e.patches, tiffPatch{hdr: true, pos: 12, width: 4, ref: rawRef})
	}

	var base [3]int
	if o.IFDsFirst {
		base[segStruct], base[segData] = e.hdrLen, e.hdrLen+len(e.seg[segStruct])
	} else {
		base[segData], base[segStruct] = e.hdrLen, e.hdrLen+len(e.seg[segData])
	}
	base[segLate] = e.hdrLen + len(e.seg[segStruct]) + len(e.seg[segData])
	var out []byte
	out = append(out, hdr...)
	if o.IFDsFirst {
		out = append(out, e.seg[segStruct]...)
		out = append(out, e.seg[segData]...)
	} else {
		out = append(out, e.seg[segData]...)
		out = append(out, e.seg[segStruct]...)
	}
	out = append(out, e.seg[segLate]...)
	for _, p := range e.patches {
		if !p.ref.set {
			panic("carvetest: an offset was never placed")
		}
		at := p.pos
		if !p.hdr {
			at += base[segStruct]
		}
		copy(out[at:], tiffUint(e.bo, uint64(base[p.ref.seg]+p.ref.off), p.width))
	}
	return out
}
