package sqlitetest

import (
	"fmt"
	"math"
	"unicode/utf16"
)

// Records, local payload sizes and overflow chains, written from the file
// format description (never from sqlitefile's decoders).

// normalize converts a column value to one of nil, int64, float64, string or
// []byte; anything else panics (this is a test helper).
func normalize(v any) any {
	switch x := v.(type) {
	case nil, int64, float64, string, []byte:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case uint32:
		return int64(x)
	}
	panic(fmt.Sprintf("sqlitetest: unsupported column value %T", v))
}

// encodeText returns the bytes of s in database encoding enc (1 UTF-8,
// 2 UTF-16le, 3 UTF-16be).
func encodeText(s string, enc int) []byte {
	if enc == 1 {
		return []byte(s)
	}
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2*len(units))
	for _, u := range units {
		if enc == 2 {
			out = append(out, byte(u), byte(u>>8))
		} else {
			out = append(out, byte(u>>8), byte(u))
		}
	}
	return out
}

// encodeRecord returns the record holding vals: header length, one serial type
// per value, then the values.
func encodeRecord(vals []any, enc int) []byte {
	var serials, body []byte
	for _, raw := range vals {
		switch v := normalize(raw).(type) {
		case nil:
			serials = append(serials, putVarint(0)...)
		case int64:
			s, b := intSerial(v)
			serials = append(serials, putVarint(s)...)
			body = append(body, b...)
		case float64:
			serials = append(serials, putVarint(7)...)
			bits := math.Float64bits(v)
			for i := 7; i >= 0; i-- {
				body = append(body, byte(bits>>(8*i)))
			}
		case string:
			t := encodeText(v, enc)
			serials = append(serials, putVarint(uint64(2*len(t)+13))...)
			body = append(body, t...)
		case []byte:
			serials = append(serials, putVarint(uint64(2*len(v)+12))...)
			body = append(body, v...)
		}
	}
	return joinRecord(serials, body)
}

// joinRecord prefixes serial type bytes with the header length varint, which
// counts itself.
func joinRecord(serials, body []byte) []byte {
	hl := len(serials) + 1
	if len(putVarint(uint64(hl))) == 2 { // the length itself needs two bytes
		hl = len(serials) + 2
	}
	out := append(putVarint(uint64(hl)), serials...)
	return append(out, body...)
}

// localSize returns how many payload bytes of a p-byte payload stay in the
// page and whether the rest spills, for usable size u: table leaf cells keep
// up to u-35; index cells up to (u-12)*64/255-23; a larger payload keeps
// m + (p-m) mod (u-4) bytes (m = (u-12)*32/255-23) when that fits, else m.
func localSize(u int, tableLeaf bool, p int) (local int, spills bool) {
	maxLocal := (u-12)*64/255 - 23
	if tableLeaf {
		maxLocal = u - 35
	}
	minLocal := (u-12)*32/255 - 23
	if p <= maxLocal {
		return p, false
	}
	k := minLocal + (p-minLocal)%(u-4)
	if k <= maxLocal {
		return k, true
	}
	return minLocal, true
}

// spill writes payload[local:] to a fresh chain of overflow pages (a 4-byte
// next page, 0 on the last, then u-4 content bytes each) and returns the
// pages in order. Nothing is written for an empty rest.
func (b *Builder) spill(payload []byte, local int) []uint32 {
	rest := payload[local:]
	chunk := b.usable() - 4
	n := (len(rest) + chunk - 1) / chunk
	pages := make([]uint32, n)
	for i := range pages {
		pages[i] = b.alloc()
	}
	for i, pg := range pages {
		p := b.pages[pg-1]
		if i+1 < n {
			put32(p, pages[i+1])
		}
		end := min((i+1)*chunk, len(rest))
		copy(p[4:], rest[i*chunk:end])
	}
	return pages
}
