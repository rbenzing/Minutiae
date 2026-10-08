package plist

import (
	"encoding/binary"
	"fmt"
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

// Check rejects anything that is not a well-formed binary plist whose fully expanded
// form stays within l.MaxNodes nodes, l.MaxPayload bytes of string and data payload and
// l.MaxDepth levels of nesting. Shared references are memoized, so a reference bomb costs
// O(objects), not its expanded size. Every error wraps ErrMalformed or ErrLimit.
func Check(b []byte, l Limits) error {
	return guard(func() error { return checkCore(b, l) })
}

func checkCore(b []byte, l Limits) error {
	if len(b) < len(bplistMagic)+bplistTrailer || string(b[:len(bplistMagic)]) != bplistMagic {
		return malformed("not a binary plist")
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
		return malformed("bad trailer sizes")
	case numObjects == 0 || numObjects > dataEnd:
		return malformed("bad object count")
	case numObjects > l.MaxNodes:
		// A legitimate plist references every object, so numObjects never exceeds the
		// expanded node count; this also bounds the memo table.
		return limited("object count %d above %d", numObjects, l.MaxNodes)
	case tableOff < uint64(len(bplistMagic)) || tableOff > dataEnd || numObjects*w.offSize > dataEnd-tableOff:
		return malformed("bad offset table")
	case top >= numObjects:
		return malformed("bad top object")
	}
	w.numObjects, w.tableOff = numObjects, tableOff
	w.memo = make([]bplistNode, numObjects)
	_, err := w.visit(top, 0)
	return err
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
	switch typ := w.b[off] >> 4; typ {
	case 0xA, 0xB, 0xD: // array, set, dict
		count, hdr, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		if count > w.tableOff { // bound before doubling so a dict count cannot wrap
			return bplistNode{}, malformed("truncated container")
		}
		if typ == 0xD {
			count *= 2
		}
		start := off + hdr
		if start+count*w.refSize > w.tableOff {
			return bplistNode{}, malformed("truncated container")
		}
		for k := range count {
			ref := w.uint(start+k*w.refSize, w.refSize)
			if ref >= w.numObjects {
				return bplistNode{}, malformed("reference out of range")
			}
			c, err := w.visit(ref, depth+1)
			if err != nil {
				return bplistNode{}, err
			}
			res.nodes += c.nodes
			res.payload += c.payload
			res.height = max(res.height, c.height+1)
			if res.nodes > w.l.MaxNodes {
				return bplistNode{}, limited("expands beyond %d nodes", w.l.MaxNodes)
			}
			if res.payload > w.l.MaxPayload {
				return bplistNode{}, limited("expands beyond %d payload bytes", w.l.MaxPayload)
			}
		}
	case 0x4, 0x5, 0x6: // data, ASCII string, UTF-16 string
		count, _, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		if count > w.l.MaxPayload { // bound before doubling so a UTF-16 count cannot wrap
			return bplistNode{}, limited("payload above %d bytes", w.l.MaxPayload)
		}
		if typ == 0x6 {
			count *= 2
		}
		if count > w.l.MaxPayload {
			return bplistNode{}, limited("payload above %d bytes", w.l.MaxPayload)
		}
		res.payload = count
	}
	*n = res
	return res, nil
}
