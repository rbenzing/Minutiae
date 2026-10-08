package sqlitefile

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Cell is one parsed b-tree cell. Local aliases the page and is read-only.
type Cell struct {
	Index        int
	Offset       int // in the page
	Length       int // bytes of the cell in the page: header + local payload + overflow pointer
	LeftChild    uint32
	PayloadLen   int64
	Rowid        int64
	HasRowid     bool
	Local        []byte
	OverflowHead uint32 // 0: no overflow pointer, or a pointer to nothing
}

// LocalPayload returns how many bytes of a payload of the given length stay
// in the page of type t when the usable size is usable, and whether the rest
// spills to an overflow chain. Table leaf: X = U-35; index: X =
// (U-12)*64/255-23; M = (U-12)*32/255-23. A payload up to X is wholly local;
// else K = M + (P-M) mod (U-4), local is K when K <= X and M otherwise. A
// table interior cell has no payload. A usable size below 480 (never valid) yields 0, false.
func LocalPayload(usable int, t PageType, payload int64) (local int64, spills bool) {
	if payload <= 0 || t == PageTableInterior || usable < 480 {
		return 0, false
	}
	u := int64(usable)
	var x int64
	switch t {
	case PageTableLeaf:
		x = u - 35
	default:
		x = (u-12)*64/255 - 23
	}
	m := (u-12)*32/255 - 23
	if payload <= x {
		return payload, false
	}
	k := m + (payload-m)%(u-4)
	if k <= x {
		return k, true
	}
	return m, true
}

// ParseCell parses the cell at off of page (the bytes present, aliased by the
// result). usable is the usable size and h the page's header. The cell must
// lie wholly inside min(usable, len(page)): a cell that reaches past the
// bytes present (a truncated file) or past the usable size is an error
// wrapping ErrCorrupt. The payload length is returned as stored (only values
// that fit an int64 are accepted); caps are the caller's.
func ParseCell(page []byte, usable int, h PageHeader, off int) (_ Cell, err error) {
	defer guard(&err)
	pureAt("ParseCell")
	hi := min(usable, len(page))
	bad := func(format string, a ...any) (Cell, error) {
		return Cell{}, &CorruptError{File: FileDB, Reason: fmt.Sprintf("cell at %d: "+format, append([]any{off}, a...)...)}
	}
	if off < 0 || off >= hi {
		return bad("offset outside [0, %d)", hi)
	}
	c := Cell{Offset: off}
	pos := off
	varint := func() (uint64, bool) {
		v, n := GetVarint(page[pos:hi])
		if n == 0 {
			return 0, false
		}
		pos += n
		return v, true
	}
	if h.Type.interior() {
		if hi-pos < 4 {
			return bad("the child pointer does not fit")
		}
		c.LeftChild = binary.BigEndian.Uint32(page[pos:])
		pos += 4
	}
	if h.Type == PageTableInterior {
		v, ok := varint()
		if !ok {
			return bad("the rowid varint runs past the page")
		}
		c.Rowid, c.HasRowid = int64(v), true
		c.Length = pos - off
		return c, nil
	}
	p, ok := varint()
	if !ok {
		return bad("the payload length varint runs past the page")
	}
	if p > math.MaxInt64 {
		return bad("payload length %d does not fit an int64", p)
	}
	c.PayloadLen = int64(p)
	if h.Type == PageTableLeaf {
		v, ok := varint()
		if !ok {
			return bad("the rowid varint runs past the page")
		}
		c.Rowid, c.HasRowid = int64(v), true
	}
	local, spills := LocalPayload(usable, h.Type, c.PayloadLen)
	need := int64(pos) + local
	if spills {
		need += 4
	}
	if need > int64(hi) {
		return bad("payload of %d local bytes ends at %d, past the %d bytes available", local, need, hi)
	}
	c.Local = page[pos : pos+int(local) : pos+int(local)]
	pos += int(local)
	if spills {
		c.OverflowHead = binary.BigEndian.Uint32(page[pos:])
		pos += 4
	}
	c.Length = pos - off
	return c, nil
}
