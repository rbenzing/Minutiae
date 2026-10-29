package carve

import (
	"errors"
	"strconv"
)

// validateJPEG accepts a JPEG from its marker structure alone: SOI, a marker
// loop over length-prefixed segments, entropy-coded data after each SOS, and
// an EOI after at least one scan. A segment length only moves a cursor inside
// the window; nothing is allocated from it, every loop iteration advances at
// least one byte, and nothing is decoded. The layout is from memory
// (docs/limitations.md).
func validateJPEG(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	// fail maps a window error: past the window is a truncated object; any
	// other error (media read, spent budget, cancel) is rejected unreadable
	// and the scanner re-labels it from the window's own state.
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	if b, err := w.byteChecked(0); err != nil {
		return fail(err)
	} else if b != 0xFF {
		return reject("bad-marker")
	}
	if b, err := w.byteChecked(1); err != nil {
		return fail(err)
	} else if b != 0xD8 {
		return reject("bad-marker")
	}
	var (
		pos           int64 = 2
		segments      int
		scans         int
		dims          string
		progressive   bool
		haveFrame     bool
		hdr           [6]byte
		winLen        = w.Len()
		needFrameBody = func(code byte) bool {
			return code >= 0xC0 && code <= 0xCF && code != 0xC4 && code != 0xC8 && code != 0xCC
		}
	)
	for {
		b, err := w.byteChecked(pos)
		if err != nil {
			return fail(err)
		}
		if b != 0xFF {
			return reject("bad-marker")
		}
		pos++
		var code byte
		for { // fill: further FF before the code
			if code, err = w.byteChecked(pos); err != nil {
				return fail(err)
			}
			pos++
			if code != 0xFF {
				break
			}
		}
		switch {
		case code == 0xD9:
			if scans == 0 {
				return reject("no-scan")
			}
			basis := []string{"dims=" + dims, "segments=" + strconv.Itoa(segments), "scans=" + strconv.Itoa(scans)}
			if progressive {
				basis = append(basis, "progressive")
			}
			return Verdict{Outcome: Validated, Length: pos, Basis: basis}
		case code == 0x01:
			continue
		case code == 0x00 || code == 0xD8 || (code >= 0xD0 && code <= 0xD7):
			return reject("bad-marker")
		}
		var lb [2]byte
		if err := w.readChecked(lb[:], pos); err != nil {
			return fail(err)
		}
		l := int64(lb[0])<<8 | int64(lb[1])
		if l < 2 {
			return reject("bad-length")
		}
		// Early exit only: a segment running past the window would also be
		// found truncated by the next read, so this is not a safety bound.
		if l > winLen-pos {
			return reject("truncated")
		}
		segments++
		if needFrameBody(code) {
			// L = 8 + 3 x components; the fixed part is read, the component
			// table is skipped by the length check alone.
			if l < 8 {
				return reject("bad-frame")
			}
			if err := w.readChecked(hdr[:], pos+2); err != nil {
				return fail(err)
			}
			nf := int64(hdr[5])
			if nf < 1 || nf > 4 || l != 8+3*nf {
				return reject("bad-frame")
			}
			h := int(hdr[1])<<8 | int(hdr[2])
			wd := int(hdr[3])<<8 | int(hdr[4])
			if h == 0 || wd == 0 {
				return reject("zero-dimensions")
			}
			if !haveFrame {
				haveFrame = true
				dims = strconv.Itoa(wd) + "x" + strconv.Itoa(h)
				progressive = code == 0xC2
			}
		}
		sosAt := pos
		pos += l
		if code != 0xDA {
			continue
		}
		if !haveFrame {
			return reject("no-frame")
		}
		// SOS: L = 6 + 2 x Ns, Ns 1..4 (the scan header was skipped by length).
		if l < 6 {
			return reject("bad-scan")
		}
		var ns [1]byte
		if err := w.readChecked(ns[:], sosAt+2); err != nil {
			return fail(err)
		}
		if ns[0] < 1 || ns[0] > 4 || l != 6+2*int64(ns[0]) {
			return reject("bad-scan")
		}
		scans++
		data := false
		// Entropy-coded data: FF 00 stuffing, FF D0-D7 restart and FF FF fill
		// stay inside; FF followed by any other code ends it at that FF.
		for {
			b, err := w.byteChecked(pos)
			if err != nil {
				return fail(err)
			}
			if b != 0xFF {
				data = true
				pos++
				continue
			}
			n, err := w.byteChecked(pos + 1)
			if err != nil {
				return fail(err)
			}
			switch {
			case n == 0x00:
				data = true
				pos += 2
			case n >= 0xD0 && n <= 0xD7:
				pos += 2
			case n == 0xFF:
				pos++
			default:
				if !data {
					return reject("empty-scan")
				}
				goto marker
			}
		}
	marker:
	}
}
