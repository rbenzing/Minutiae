package mb2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Limits applied to a device-supplied binary plist before it is decoded.
// The plist decoder rebuilds a container once per reference to it, so a tiny
// document whose objects reference each other can expand exponentially.
const (
	maxExpandedNodes = 1 << 20
	maxNesting       = 64
)

const (
	bplistMagic   = "bplist00"
	bplistTrailer = 32
)

// checkBinaryPlist rejects anything that is not a well-formed binary plist
// whose fully expanded form stays within maxExpandedNodes nodes,
// maxMessage bytes of string/data payload, and maxNesting levels.
func checkBinaryPlist(b []byte) error {
	if len(b) < len(bplistMagic)+bplistTrailer || string(b[:len(bplistMagic)]) != bplistMagic {
		return errors.New("mb2: message is not a binary plist")
	}
	t := b[len(b)-bplistTrailer:]
	w := &bplistWalker{
		b:       b,
		offSize: uint64(t[6]),
		refSize: uint64(t[7]),
	}
	numObjects := binary.BigEndian.Uint64(t[8:16])
	top := binary.BigEndian.Uint64(t[16:24])
	tableOff := binary.BigEndian.Uint64(t[24:32])
	dataEnd := uint64(len(b) - bplistTrailer)
	switch {
	case w.offSize < 1 || w.offSize > 8 || w.refSize < 1 || w.refSize > 8:
		return errors.New("mb2: bad binary plist trailer sizes")
	case numObjects == 0 || numObjects > dataEnd || numObjects > maxExpandedNodes:
		// A legitimate plist references every object, so numObjects never
		// exceeds the expanded node count; this also bounds the memo table.
		return errors.New("mb2: bad binary plist object count")
	case tableOff < uint64(len(bplistMagic)) || tableOff > dataEnd || numObjects*w.offSize > dataEnd-tableOff:
		return errors.New("mb2: bad binary plist offset table")
	case top >= numObjects:
		return errors.New("mb2: bad binary plist top object")
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
		return 0, 0, errors.New("mb2: truncated binary plist length")
	}
	m := w.b[off+1]
	if m>>4 != 0x1 || m&0x0f > 3 {
		return 0, 0, errors.New("mb2: bad binary plist length marker")
	}
	size := uint64(1) << (m & 0x0f)
	if off+2+size > w.tableOff {
		return 0, 0, errors.New("mb2: truncated binary plist length")
	}
	return w.uint(off+2, size), 2 + size, nil
}

func (w *bplistWalker) visit(i uint64, depth int) (bplistNode, error) {
	if depth > maxNesting {
		return bplistNode{}, fmt.Errorf("mb2: binary plist nested deeper than %d", maxNesting)
	}
	n := &w.memo[i]
	switch n.state {
	case 1:
		return bplistNode{}, errors.New("mb2: binary plist contains a reference cycle")
	case 2:
		if depth+n.height > maxNesting {
			return bplistNode{}, fmt.Errorf("mb2: binary plist nested deeper than %d", maxNesting)
		}
		return *n, nil
	}
	n.state = 1
	off := w.uint(w.tableOff+i*w.offSize, w.offSize)
	if off < uint64(len(bplistMagic)) || off >= w.tableOff {
		return bplistNode{}, errors.New("mb2: binary plist object offset out of range")
	}
	res := bplistNode{state: 2, nodes: 1}
	switch typ := w.b[off] >> 4; typ {
	case 0xA, 0xB, 0xD: // array, set, dict
		count, hdr, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		if count > w.tableOff { // bound before doubling so a dict count cannot wrap
			return bplistNode{}, errors.New("mb2: truncated binary plist container")
		}
		if typ == 0xD {
			count *= 2
		}
		start := off + hdr
		if start+count*w.refSize > w.tableOff {
			return bplistNode{}, errors.New("mb2: truncated binary plist container")
		}
		for k := range count {
			ref := w.uint(start+k*w.refSize, w.refSize)
			if ref >= w.numObjects {
				return bplistNode{}, errors.New("mb2: binary plist reference out of range")
			}
			c, err := w.visit(ref, depth+1)
			if err != nil {
				return bplistNode{}, err
			}
			res.nodes += c.nodes
			res.payload += c.payload
			res.height = max(res.height, c.height+1)
			if res.nodes > maxExpandedNodes || res.payload > maxMessage {
				return bplistNode{}, errors.New("mb2: binary plist expands beyond the size limit")
			}
		}
	case 0x4, 0x5, 0x6: // data, ASCII string, UTF-16 string
		count, _, err := w.header(off)
		if err != nil {
			return bplistNode{}, err
		}
		if count > maxMessage { // bound before doubling so a UTF-16 count cannot wrap
			return bplistNode{}, errors.New("mb2: binary plist expands beyond the size limit")
		}
		if typ == 0x6 {
			count *= 2
		}
		res.payload = count
	}
	*n = res
	return res, nil
}
