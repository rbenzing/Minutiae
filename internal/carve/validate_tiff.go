package carve

import (
	"encoding/binary"
	"math"
	"strconv"
)

// TIFF walk bounds (spec 5). The first three are counted: a walk that reaches
// one is carve-bounded with a reason, never validated.
const (
	tiffMaxIFDs    = 64       // IFDs per object (a visited set of at most this many offsets)
	tiffMaxEntries = 1024     // entries read per IFD
	tiffMaxValues  = 1 << 20  // values per array tag
	tiffArrayBuf   = 4 << 10  // one read buffer of an array walk
	tiffMakerNote  = 37500    // MakerNote: private offsets are not followed (spec D6)
	tiffDNGVersion = 50706    // DNGVersion in IFD0 gives the type dng
	tiffMagicORF   = 0x4F52   // "OR" in the file's byte order (IIRO, MMOR)
	tiffMagicORFS  = 0x5352   // "RS", little-endian only (IIRS)
	tiffMagicRW2   = 0x55     // little-endian only
	tiffEntryBytes = 4 << 10  // read buffer of the entries of one IFD
	tiffKindMain   = uint8(0) // IFD0, its chain and SubIFDs: image data is read here
	tiffKindExif   = uint8(1) // Exif: may point at Interoperability
	tiffKindOther  = uint8(2) // GPS, Interoperability: nothing is followed from them but the chain
)

// tiffTypeSize is the size in bytes of one value of a TIFF entry type, 0 when
// the type is not legal for the form: 1..13 in both, 16..18 (LONG8, SLONG8,
// IFD8) only in BigTIFF.
func tiffTypeSize(typ uint16, big bool) int64 {
	switch typ {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11, 13:
		return 4
	case 5, 10, 12:
		return 8
	case 16, 17, 18:
		if big {
			return 8
		}
	}
	return 0
}

// tiffStop ends a walk: a rejection reason, or (capped) the reason of a
// bounded verdict.
type tiffStop struct {
	capped bool
	reason string
}

func (s *tiffStop) Error() string { return s.reason }

func tiffReject(reason string) error { return &tiffStop{reason: reason} }
func tiffFail(err error) error       { return &tiffStop{reason: readFailReason(err)} }
func tiffCap(what string) error {
	return &tiffStop{capped: true, reason: "tiff-walk-capped=" + what}
}

// tiffArr is where the value of one entry lies: in the entry itself (inline)
// or at a window offset. Nothing is read until the walk streams it.
type tiffArr struct {
	present bool
	typ     uint16
	count   uint64
	inline  bool
	inl     [8]byte
	off     int64
}

// The image-data arrays of one IFD, by index.
const (
	tiffStripOffsets = iota
	tiffStripCounts
	tiffTileOffsets
	tiffTileCounts
	tiffJPEGOffset
	tiffJPEGLength
	tiffArrays
)

// tiffWalk is the state of one validation. It is iterative: the visited set
// is the work list, fixed at tiffMaxIFDs offsets.
type tiffWalk struct {
	w                          *Window
	bo                         binary.ByteOrder
	big                        bool
	hdr                        int64 // header length: 8 or 16
	entSize, cntSize, nextSize int64
	inl                        int64 // bytes of an entry's value field, as wide as its count
	maxEnd                     int64
	ifds                       [tiffMaxIFDs]int64
	kinds                      [tiffMaxIFDs]uint8
	n, refs                    int
	zero, makerNote, dng       bool
	ent, bufA, bufB            [tiffArrayBuf]byte
}

func (t *tiffWalk) uint(b []byte) uint64 {
	switch len(b) {
	case 1:
		return uint64(b[0])
	case 2:
		return uint64(t.bo.Uint16(b))
	case 4:
		return uint64(t.bo.Uint32(b))
	}
	return t.bo.Uint64(b)
}

// add queues the IFD at off. An offset inside the header is bad-offset, one at
// or past the window truncated, one seen before ifd-cycle, and a 65th IFD a
// capped walk.
func (t *tiffWalk) add(off uint64, kind uint8) error {
	if off < uint64(t.hdr) {
		return tiffReject("bad-offset")
	}
	if off >= uint64(t.w.Len()) {
		return tiffReject("truncated")
	}
	o := int64(off)
	for i := range t.n {
		if t.ifds[i] == o {
			return tiffReject("ifd-cycle")
		}
	}
	if t.n == tiffMaxIFDs {
		return tiffCap("ifds")
	}
	t.ifds[t.n], t.kinds[t.n] = o, kind
	t.n++
	return nil
}

// tiffOffsetType: LONG, IFD, LONG8, IFD8.
func tiffOffsetType(typ uint16) bool { return typ == 4 || typ == 13 || typ == 16 || typ == 18 }

// elements calls f with each value of an array, streamed through buf. count
// is at most tiffMaxValues.
func (t *tiffWalk) elements(a *tiffArr, buf []byte, f func(v uint64) error) error {
	es := tiffTypeSize(a.typ, t.big)
	step := int64(len(buf)) / es
	for done := int64(0); done < int64(a.count); {
		m := min(step, int64(a.count)-done)
		p, err := t.chunk(a, es, buf, done, m)
		if err != nil {
			return err
		}
		for j := range m {
			if err := f(t.uint(p[j*es : (j+1)*es])); err != nil {
				return err
			}
		}
		done += m
	}
	return nil
}

// chunk returns the bytes of values [start, start+m) of a.
func (t *tiffWalk) chunk(a *tiffArr, es int64, buf []byte, start, m int64) ([]byte, error) {
	if a.inline {
		return a.inl[start*es : (start+m)*es], nil
	}
	p := buf[:m*es]
	if err := t.w.readChecked(p, a.off+start*es); err != nil {
		return nil, tiffFail(err)
	}
	return p, nil
}

// pointers follows the offsets of a pointer tag: 330 holds any number, the
// others exactly one.
func (t *tiffWalk) pointers(a *tiffArr, kind uint8, one bool) error {
	if !tiffOffsetType(a.typ) || one && a.count != 1 {
		return tiffReject("bad-entry")
	}
	if a.count > tiffMaxValues {
		return tiffCap("values")
	}
	return t.elements(a, t.bufA[:], func(v uint64) error { return t.add(v, kind) })
}

// entry reads one directory entry: its type, where its value lies (counted
// toward the length), and what the tag asks the walk to follow.
func (t *tiffWalk) entry(e []byte, first bool, kind uint8, arrs *[tiffArrays]tiffArr) error {
	tag := t.bo.Uint16(e)
	typ := t.bo.Uint16(e[2:])
	count := t.uint(e[4 : 4+t.inl])
	field := e[4+t.inl : 4+2*t.inl]
	sz := tiffTypeSize(typ, t.big)
	if sz == 0 {
		return tiffReject("bad-entry")
	}
	a := tiffArr{present: true, typ: typ, count: count}
	if count <= uint64(t.inl/sz) {
		a.inline = true
		copy(a.inl[:], field)
	} else {
		voff := t.uint(field)
		if voff < uint64(t.hdr) {
			return tiffReject("bad-offset")
		}
		if voff > math.MaxInt64 || count > math.MaxInt64/uint64(sz) || !t.w.fits(int64(voff), int64(count)*sz) {
			return tiffReject("truncated")
		}
		a.off = int64(voff)
		t.maxEnd = max(t.maxEnd, a.off+int64(count)*sz)
	}
	if tag == tiffMakerNote {
		t.makerNote = true
	}
	switch kind {
	case tiffKindMain:
		switch tag {
		case tiffDNGVersion:
			t.dng = t.dng || first && !t.big
		case 273:
			arrs[tiffStripOffsets] = a
		case 279:
			arrs[tiffStripCounts] = a
		case 324:
			arrs[tiffTileOffsets] = a
		case 325:
			arrs[tiffTileCounts] = a
		case 513:
			arrs[tiffJPEGOffset] = a
		case 514:
			arrs[tiffJPEGLength] = a
		case 330:
			return t.pointers(&a, tiffKindMain, false)
		case 34665:
			return t.pointers(&a, tiffKindExif, true)
		case 34853:
			return t.pointers(&a, tiffKindOther, true)
		}
	case tiffKindExif:
		if tag == 40965 {
			return t.pointers(&a, tiffKindOther, true)
		}
	}
	return nil
}

// ref books one offset/count pair of image data. A byte count of 0 is an
// unresolved strip: it adds nothing and leaves the walk incomplete.
func (t *tiffWalk) ref(o, c uint64) error {
	t.refs++
	if c == 0 {
		t.zero = true
		return nil
	}
	if o < uint64(t.hdr) {
		return tiffReject("bad-offset")
	}
	if o > math.MaxInt64 || c > math.MaxInt64-o || int64(o+c) > t.w.Len() {
		return tiffReject("truncated")
	}
	t.maxEnd = max(t.maxEnd, int64(o+c))
	return nil
}

// pair streams an offset array and its byte-count array together through two
// fixed buffers. single demands one value each (the JPEG interchange tags).
func (t *tiffWalk) pair(offs, cnts *tiffArr, single bool) error {
	if !offs.present && !cnts.present {
		return nil
	}
	arrType := func(typ uint16) bool { return typ == 3 || typ == 4 || typ == 16 }
	if !offs.present || !cnts.present || !arrType(offs.typ) || !arrType(cnts.typ) || offs.count != cnts.count || single && offs.count != 1 {
		return tiffReject("bad-strips")
	}
	if offs.count > tiffMaxValues {
		return tiffCap("values")
	}
	eo, ec := tiffTypeSize(offs.typ, true), tiffTypeSize(cnts.typ, true)
	step := int64(tiffArrayBuf) / max(eo, ec)
	for done := int64(0); done < int64(offs.count); {
		m := min(step, int64(offs.count)-done)
		po, err := t.chunk(offs, eo, t.bufA[:], done, m)
		if err != nil {
			return err
		}
		pc, err := t.chunk(cnts, ec, t.bufB[:], done, m)
		if err != nil {
			return err
		}
		for j := range m {
			if err := t.ref(t.uint(po[j*eo:(j+1)*eo]), t.uint(pc[j*ec:(j+1)*ec])); err != nil {
				return err
			}
		}
		done += m
	}
	return nil
}

// ifd walks the directory queued at index i.
func (t *tiffWalk) ifd(i int) error {
	off, kind := t.ifds[i], t.kinds[i]
	var cb [8]byte
	if err := t.w.readChecked(cb[:t.cntSize], off); err != nil {
		return tiffFail(err)
	}
	count := t.uint(cb[:t.cntSize])
	if count == 0 {
		return tiffReject("empty-ifd")
	}
	if count > uint64(math.MaxInt64)/uint64(t.entSize)-16 {
		return tiffReject("truncated")
	}
	size := t.cntSize + int64(count)*t.entSize + t.nextSize
	if !t.w.fits(off, size) {
		return tiffReject("truncated")
	}
	t.maxEnd = max(t.maxEnd, off+size)
	var arrs [tiffArrays]tiffArr
	limit := int64(min(count, tiffMaxEntries))
	per := int64(len(t.ent)) / t.entSize
	for done := int64(0); done < limit; {
		m := min(per, limit-done)
		buf := t.ent[:m*t.entSize]
		if err := t.w.readChecked(buf, off+t.cntSize+done*t.entSize); err != nil {
			return tiffFail(err)
		}
		for j := range m {
			if err := t.entry(buf[j*t.entSize:(j+1)*t.entSize], i == 0, kind, &arrs); err != nil {
				return err
			}
		}
		done += m
	}
	if count > tiffMaxEntries {
		return tiffCap("entries")
	}
	if err := t.w.readChecked(cb[:t.nextSize], off+size-t.nextSize); err != nil {
		return tiffFail(err)
	}
	if next := t.uint(cb[:t.nextSize]); next != 0 {
		if err := t.add(next, kind); err != nil {
			return err
		}
	}
	if kind != tiffKindMain {
		return nil
	}
	if err := t.pair(&arrs[tiffStripOffsets], &arrs[tiffStripCounts], false); err != nil {
		return err
	}
	if err := t.pair(&arrs[tiffTileOffsets], &arrs[tiffTileCounts], false); err != nil {
		return err
	}
	return t.pair(&arrs[tiffJPEGOffset], &arrs[tiffJPEGLength], true)
}

// validateTIFF accepts a TIFF or BigTIFF from its IFD structure alone (spec
// D4-D6): the header, IFD0 and its chain, SubIFDs, Exif, GPS and
// Interoperability, every out-of-line value and every strip, tile and JPEG
// interchange range. The length is the maximum end of all of them, so a file
// whose last bytes are an early IFD's value or a strip written before a later
// IFD is measured whole. Offsets and counts only move cursors inside the
// window, arrays are streamed through fixed buffers, no loop is unbounded and
// no pixel data is read. A walk that cannot finish (a cap, or a strip of byte
// count 0) is bounded at the window with a reason, never validated. The
// identity comes from the header and IFD0 (D5). The layouts are from memory
// (docs/limitations.md).
func validateTIFF(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	var head [16]byte
	have := int(min(int64(len(head)), w.Len()))
	if err := w.readChecked(head[:have], 0); err != nil {
		return reject(readFailReason(err))
	}
	if have >= 2 && (head[0] != 'I' && head[0] != 'M' || head[1] != head[0]) {
		return reject("bad-header")
	}
	if have < 8 {
		return reject("truncated")
	}
	t := &tiffWalk{w: w, bo: binary.LittleEndian, hdr: 8, entSize: 12, cntSize: 2, nextSize: 4, inl: 4}
	little := head[0] == 'I'
	if !little {
		t.bo = binary.BigEndian
	}
	magic := t.bo.Uint16(head[2:])
	var ifd0 uint64
	switch {
	case magic == 42, magic == tiffMagicORF, little && (magic == tiffMagicORFS || magic == tiffMagicRW2):
		ifd0 = uint64(t.bo.Uint32(head[4:]))
	case magic == 43:
		if have < 16 {
			return reject("truncated")
		}
		if t.bo.Uint16(head[4:]) != 8 || t.bo.Uint16(head[6:]) != 0 {
			return reject("bad-header")
		}
		t.big, t.hdr, t.entSize, t.cntSize, t.nextSize, t.inl = true, 16, 20, 8, 8, 8
		ifd0 = t.bo.Uint64(head[8:])
	default:
		return reject("bad-header")
	}
	t.maxEnd = t.hdr

	// A CR2 names its raw IFD in header bytes 12..15 (0 = none). It is queued
	// as a main IFD after the chain is walked, so one the chain already holds
	// is the same IFD, not a cycle; any other pointer is checked like every
	// other (bad-offset, truncated, the IFD cap).
	var cr2Raw uint64
	isCR2 := little && magic == 42 && have >= 16 && head[8] == 'C' && head[9] == 'R' && head[10] == 2
	if isCR2 {
		cr2Raw = uint64(t.bo.Uint32(head[12:]))
	}
	err := t.add(ifd0, tiffKindMain)
	next := 0
	walk := func() {
		for err == nil && next < t.n {
			err = t.ifd(next)
			next++
		}
	}
	walk()
	sameMain := false // the raw IFD is a main IFD the chain already holds
	for i := range t.n {
		sameMain = sameMain || t.ifds[i] == int64(cr2Raw) && t.kinds[i] == tiffKindMain
	}
	if err == nil && cr2Raw != 0 && !sameMain {
		if cr2Raw < 16 { // inside the 16-byte CR2 header
			err = tiffReject("bad-offset")
		} else {
			err = t.add(cr2Raw, tiffKindMain)
			walk()
		}
	}
	if err == nil && t.refs == 0 {
		err = tiffReject("no-image-data")
	}

	typ, ext := "tiff", "tif"
	switch {
	case t.big:
	case magic == tiffMagicORF || magic == tiffMagicORFS:
		typ, ext = "orf", "orf"
	case little && magic == tiffMagicRW2:
		typ, ext = "rw2", "rw2"
	case isCR2:
		typ, ext = "cr2", "cr2"
	case t.dng:
		typ, ext = "dng", "dng"
	}
	v := Verdict{Outcome: Validated, Length: t.maxEnd, Type: typ, Ext: ext}
	if stop, ok := err.(*tiffStop); ok {
		if !stop.capped {
			return reject(stop.reason)
		}
		v.Outcome, v.Length, v.Reason = Bounded, w.Len(), stop.reason
	} else if t.zero {
		v.Outcome, v.Length, v.Reason = Bounded, w.Len(), "tiff-zero-bytecount"
	}
	order := "order=MM"
	if little {
		order = "order=II"
	}
	v.Basis = []string{order}
	if t.big {
		v.Basis = append(v.Basis, "bigtiff")
	}
	v.Basis = append(v.Basis, "ifds="+strconv.Itoa(t.n), "data-refs="+strconv.Itoa(t.refs))
	if t.makerNote {
		v.Assumptions = []string{"makernote-private-data-not-followed"}
	}
	return v
}
