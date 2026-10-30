package carve

import (
	"errors"
	"strconv"
)

// validateBPlist accepts a binary property list by finding its trailer: from
// offset 41 forward, the first end E whose preceding 32 bytes are a trailer
// consistent with the offset table in front of it (sizes 1..8, at least one
// object, a top object inside the table, the table ending exactly at the
// trailer, every entry pointing at a valid object marker inside the object
// area). The search is bounded by the window and every read is budgeted; the
// object bodies are never parsed. The layout is from memory
// (docs/limitations.md).
func validateBPlist(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	var sig [8]byte
	if n, err := w.ReadAt(sig[:], 0); err != nil {
		if n >= 6 && string(sig[:6]) != "bplist" {
			return reject("bad-signature")
		}
		return fail(err)
	}
	if string(sig[:]) != "bplist00" {
		return reject("bad-signature")
	}
	for end := int64(41); end <= w.Len(); end++ {
		start := end - 32
		// Cheap filter on the two size bytes before the 32-byte read.
		os, err := w.Byte(start + 6)
		if err != nil {
			return fail(err)
		}
		rs, err := w.Byte(start + 7)
		if err != nil {
			return fail(err)
		}
		if os < 1 || os > 8 || rs < 1 || rs > 8 {
			continue
		}
		var t [32]byte
		if _, err := w.ReadAt(t[:], start); err != nil {
			return fail(err)
		}
		n, ok, err := bplistCheck(w, t[:], int64(os), start)
		if err != nil {
			return fail(err)
		}
		if ok {
			return Verdict{Outcome: Validated, Length: end, Basis: []string{"objects=" + strconv.FormatUint(n, 10)}}
		}
	}
	return reject("no-trailer")
}

// bplistCheck judges the trailer t whose first byte is at window offset
// start: the declared sizes only bound the arithmetic, which is checked
// against the window before it is used.
func bplistCheck(w *Window, t []byte, offSize, start int64) (objects uint64, ok bool, err error) {
	be64 := func(o int) uint64 {
		var v uint64
		for _, b := range t[o : o+8] {
			v = v<<8 | uint64(b)
		}
		return v
	}
	if offSize < 1 || offSize > 8 {
		return 0, false, nil
	}
	n, top, tab := be64(8), be64(16), be64(24)
	if n < 1 || top >= n || tab < 9 || tab > uint64(start) {
		return 0, false, nil
	}
	if n > (uint64(start)-tab)/uint64(offSize) || tab+n*uint64(offSize) != uint64(start) {
		return 0, false, nil
	}
	var buf [4096]byte
	for done := uint64(0); done < n; {
		k := min(n-done, uint64(len(buf))/uint64(offSize))
		p := buf[:k*uint64(offSize)]
		if _, err := w.ReadAt(p, int64(tab+done*uint64(offSize))); err != nil {
			return 0, false, err
		}
		for i := uint64(0); i < k; i++ {
			var off uint64
			for _, b := range p[i*uint64(offSize) : (i+1)*uint64(offSize)] {
				off = off<<8 | uint64(b)
			}
			if off < 8 || off >= tab {
				return 0, false, nil
			}
			m, err := w.Byte(int64(off))
			if err != nil {
				return 0, false, err
			}
			switch m >> 4 {
			case 0, 1, 2, 3, 4, 5, 6, 8, 0xA, 0xC, 0xD:
			default:
				return 0, false, nil
			}
		}
		done += k
	}
	return n, true, nil
}
