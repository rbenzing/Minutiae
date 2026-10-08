package plist

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	bplistMagic   = "bplist00"
	bplistTrailer = 32
)

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

func limited(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrLimit, fmt.Sprintf(format, a...))
}

// guard runs f and converts a panic into ErrInternal (defence in depth: the checks below are
// written so that no input panics, and the fuzz targets run without this guard).
func guard(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: recovered panic: %v", ErrInternal, r)
		}
	}()
	return f()
}

// checkBinary rejects anything that is not a well-formed binary plist whose fully expanded
// form stays within l.MaxNodes nodes, l.MaxPayload bytes of string and data payload and
// l.MaxDepth levels of nesting. Shared references are memoized, so a reference bomb costs
// O(objects), not its expanded size. Every error wraps ErrMalformed or ErrLimit. The zero
// Limits means DefaultLimits. Counters saturate, so limits near MaxUint64 cannot wrap.
func checkBinary(b []byte, l Limits) error {
	_, _, err := measureBinary(b, l)
	return err
}

// measureBinary is checkBinary that also returns the expanded node count and the expanded
// string and data payload bytes of the document.
func measureBinary(b []byte, l Limits) (nodes, payload uint64, err error) {
	err = guard(func() error {
		var e error
		nodes, payload, e = checkCore(b, l)
		return e
	})
	return
}

func checkCore(b []byte, l Limits) (nodes, payload uint64, err error) {
	w, top, err := newBplistWalker(b, l, true)
	if err != nil {
		return 0, 0, err
	}
	res, err := w.visit(top, 0)
	return res.nodes, res.payload, err
}

// newBplistWalker parses the trailer of b and returns a walker over it and the index of the
// top object. It reads no object; memo allocates the table visit needs.
func newBplistWalker(b []byte, l Limits, memo bool) (*bplistWalker, uint64, error) {
	l = l.effective()
	if len(b) < len(bplistMagic)+bplistTrailer || string(b[:len(bplistMagic)]) != bplistMagic {
		return nil, 0, malformed("not a binary plist")
	}
	t := b[len(b)-bplistTrailer:]
	w := &bplistWalker{
		b:       b,
		l:       l,
		offSize: uint64(t[6]),
		refSize: uint64(t[7]),
	}
	numObjects := binary.BigEndian.Uint64(t[8:16])
	top := binary.BigEndian.Uint64(t[16:24])
	tableOff := binary.BigEndian.Uint64(t[24:32])
	dataEnd := uint64(len(b) - bplistTrailer)
	switch {
	case w.offSize < 1 || w.offSize > 8 || w.refSize < 1 || w.refSize > 8:
		return nil, 0, malformed("bad trailer sizes")
	case numObjects == 0 || numObjects > dataEnd:
		return nil, 0, malformed("bad object count")
	case numObjects > l.MaxNodes:
		// A legitimate plist references every object, so numObjects never exceeds the
		// expanded node count; this also bounds the memo table.
		return nil, 0, limited("object count %d above %d", numObjects, l.MaxNodes)
	case tableOff < uint64(len(bplistMagic)) || tableOff > dataEnd || numObjects*w.offSize > dataEnd-tableOff:
		return nil, 0, malformed("bad offset table")
	case top >= numObjects:
		return nil, 0, malformed("bad top object")
	}
	w.numObjects, w.tableOff = numObjects, tableOff
	if memo {
		w.memo = make([]bplistNode, numObjects)
	}
	return w, top, nil
}

type bplistNode struct {
	state   uint8 // 0 unvisited, 1 in progress, 2 done
	nodes   uint64
	payload uint64
	height  int
}

type bplistWalker struct {
	b          []byte
	l          Limits
	offSize    uint64
	refSize    uint64
	numObjects uint64
	tableOff   uint64
	memo       []bplistNode
}

func (w *bplistWalker) uint(off, size uint64) uint64 {
	var v uint64
	for _, c := range w.b[off : off+size] {
		v = v<<8 | uint64(c)
	}
	return v
}

// header returns the element count and the header length of the object at off.
func (w *bplistWalker) header(off uint64) (count, hdr uint64, err error) {
	marker := w.b[off]
	if low := uint64(marker & 0x0f); low != 0x0f {
		return low, 1, nil
	}
	if off+2 > w.tableOff {
		return 0, 0, malformed("truncated length")
	}
	m := w.b[off+1]
	if m>>4 != 0x1 || m&0x0f > 3 {
		return 0, 0, malformed("bad length marker")
	}
	size := uint64(1) << (m & 0x0f)
	if off+2+size > w.tableOff {
		return 0, 0, malformed("truncated length")
	}
	return w.uint(off+2, size), 2 + size, nil
}

func (w *bplistWalker) tooDeep() error {
	return limited("nested deeper than %d", w.l.MaxDepth)
}

// sat adds two counters, saturating at MaxUint64 so caller limits near it cannot wrap.
func sat(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxUint64
}

// scalarSize returns the number of content bytes that follow the marker of a fixed-size
// object (integer, real, date, UID), and false for a marker that is not valid.
func scalarSize(marker byte) (uint64, bool) {
	low := uint64(marker & 0x0f)
	switch marker >> 4 {
	case 0x1:
		return 1 << low, low <= 4
	case 0x2:
		return 1 << low, low == 2 || low == 3
	case 0x3:
		return 8, low == 3
	case 0x8:
		return low + 1, true
	}
	return 0, true
}

func (w *bplistWalker) visit(i uint64, depth int) (bplistNode, error) {
	if depth > w.l.MaxDepth {
		return bplistNode{}, w.tooDeep()
	}
	n := &w.memo[i]
	switch n.state {
	case 1:
		return bplistNode{}, malformed("reference cycle")
	case 2:
		if depth+n.height > w.l.MaxDepth {
			return bplistNode{}, w.tooDeep()
		}
		return *n, nil
	}
	n.state = 1
	off := w.uint(w.tableOff+i*w.offSize, w.offSize)
	if off < uint64(len(bplistMagic)) || off >= w.tableOff {
		return bplistNode{}, malformed("object offset out of range")
	}
	res := bplistNode{state: 2, nodes: 1}
	marker := w.b[off]
	switch typ := marker >> 4; typ {
	case 0xA, 0xC, 0xD: // array, set, dict
		count, hdr, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		perElem := w.refSize
		if typ == 0xD {
			perElem *= 2 // a key and a value reference
		}
		// off+hdr <= tableOff, and the division cannot wrap, unlike count*perElem.
		if count > (w.tableOff-(off+hdr))/perElem {
			return bplistNode{}, malformed("truncated container")
		}
		count *= perElem / w.refSize
		start := off + hdr
		for k := range count {
			ref := w.uint(start+k*w.refSize, w.refSize)
			if ref >= w.numObjects {
				return bplistNode{}, malformed("reference out of range")
			}
			c, err := w.visit(ref, depth+1)
			if err != nil {
				return bplistNode{}, err
			}
			res.nodes = sat(res.nodes, c.nodes)
			res.payload = sat(res.payload, c.payload)
			res.height = max(res.height, c.height+1)
			if res.nodes > w.l.MaxNodes {
				return bplistNode{}, limited("expands beyond %d nodes", w.l.MaxNodes)
			}
			if res.payload > w.l.MaxPayload {
				return bplistNode{}, limited("expands beyond %d payload bytes", w.l.MaxPayload)
			}
		}
	case 0x4, 0x5, 0x6: // data, ASCII string, UTF-16 string
		count, hdr, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		limit := w.l.MaxPayload
		if typ == 0x6 {
			limit /= 2 // compared before doubling, so the doubling cannot wrap
		}
		if count > limit {
			return bplistNode{}, limited("payload above %d bytes", w.l.MaxPayload)
		}
		if typ == 0x6 {
			count *= 2
		}
		if count > w.tableOff-(off+hdr) {
			return bplistNode{}, malformed("string or data runs past the object area")
		}
		res.payload = count
	default:
		size, ok := scalarSize(marker)
		if !ok || size > w.tableOff-off-1 {
			return bplistNode{}, malformed("scalar runs past the object area")
		}
		if marker == 0x14 {
			// A 16-byte integer is read as hi:lo. The decoding library keeps only lo, so a
			// value that does not fit 64 bits (hi neither 0 nor all ones with lo's sign bit
			// set) would silently decode to a wrong number: refuse it.
			hi, lo := w.uint(off+1, 8), w.uint(off+9, 8)
			if hi != 0 && (hi != math.MaxUint64 || lo>>63 == 0) {
				return bplistNode{}, fmt.Errorf("%w: integer does not fit 64 bits", ErrUnsupported)
			}
		}
	}
	*n = res
	return res, nil
}

// Check validates a plist document of either accepted format before any decoding library
// sees it: XML (PrescanXML) or Apple binary (checkBinary). Anything else, including OpenStep
// and GNUstep text plists, is refused with ErrUnsupported; empty input is ErrMalformed. The
// zero Limits means DefaultLimits. Every error wraps ErrMalformed, ErrLimit or ErrUnsupported.
func Check(b []byte, l Limits) error {
	_, _, err := measure(b, l)
	return err
}

// measure is Check that also returns the document's own counts: the expanded node count and
// the expanded string and data payload bytes (for XML, element count and text, CDATA and
// comment bytes). Decode charges its memory budget from these.
func measure(b []byte, l Limits) (nodes, payload uint64, err error) {
	err = guard(func() error {
		var e error
		nodes, payload, e = measureCore(b, l)
		return e
	})
	return nodes, payload, err
}

// measureCore is measure without the recover guard: decodeCore and the fuzz targets reach the
// validators through it, so a panic in a validator is a failure rather than a recovered error.
func measureCore(b []byte, l Limits) (nodes, payload uint64, err error) {
	switch {
	case len(b) == 0:
		return 0, 0, malformed("empty input")
	case LooksLikeXML(b):
		if !startsLikeXMLMarkup(b) {
			// "<01ab>" is an OpenStep data literal, not a start tag.
			return 0, 0, fmt.Errorf("%w: neither XML nor binary plist", ErrUnsupported)
		}
		return prescanCore(b, l)
	case bytes.HasPrefix(b, []byte(bplistMagic)):
		return checkCore(b, l)
	}
	return 0, 0, fmt.Errorf("%w: neither XML nor binary plist", ErrUnsupported)
}

// startsLikeXMLMarkup reports whether the first '<' of b (after an optional BOM and ASCII
// whitespace, as LooksLikeXML found it) is followed by something XML can start with: a name
// character, '?' or '!'. A '<' at the very end counts as markup (the pre-scan calls it
// malformed).
func startsLikeXMLMarkup(b []byte) bool {
	b = bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n")
	return len(b) < 2 || b[1] == '?' || b[1] == '!' || nameStart(b[1])
}
