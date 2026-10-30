package carve

import (
	"encoding/binary"
	"errors"
	"strconv"
)

// icoMaxImages is the largest directory count a validated ICO or CUR may name
// (spec D10: the format's own 16-bit field allows more; none is plausible).
const icoMaxImages = 256

// icoPNGSignature starts a PNG entry image.
const icoPNGSignature = "\x89PNG\r\n\x1a\n"

// validateICO accepts an icon or cursor file from its directory and the
// header of every entry image alone (spec D10): the length is the maximum end
// of the entries, and no pixel or IDAT is read. The ICO width, height, planes
// and bpp fields and the CUR hotspot fields are not judged, as the image
// header carries the real values. The layout is from memory
// (docs/limitations.md).
func validateICO(w *Window) Verdict {
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
	var head [6]byte
	if err := w.readChecked(head[:], 0); err != nil {
		return fail(err)
	}
	le := binary.LittleEndian
	typ := "ico"
	switch {
	case le.Uint16(head[0:]) != 0:
		return reject("bad-header")
	case le.Uint16(head[2:]) == 2:
		typ = "cur"
	case le.Uint16(head[2:]) != 1:
		return reject("bad-header")
	}
	count := int64(le.Uint16(head[4:]))
	if count < 1 || count > icoMaxImages {
		return reject("bad-count")
	}
	dirEnd := 6 + 16*count // at most 4102 bytes
	if dirEnd > w.Len() {
		return reject("truncated")
	}
	dir := make([]byte, dirEnd-6) // bounded by icoMaxImages, not by a declared size
	if err := w.readChecked(dir, 6); err != nil {
		return fail(err)
	}
	var end int64
	var pngs, dibs int
	for e := dir; len(e) >= 16; e = e[16:] {
		size, off := int64(le.Uint32(e[8:])), int64(le.Uint32(e[12:]))
		switch {
		case e[3] != 0 || size == 0:
			return reject("bad-entry")
		case off < dirEnd:
			return reject("bad-offset")
		case off+size > w.Len():
			return reject("truncated")
		}
		var sig [8]byte
		isPNG := false
		if size >= int64(len(sig)) {
			if err := w.readChecked(sig[:], off); err != nil {
				return fail(err)
			}
			isPNG = string(sig[:]) == icoPNGSignature
		}
		var reason string
		var err error
		if isPNG {
			_, _, reason, err = pngHead(w, off, size)
			pngs++
		} else {
			// dibCheck says "truncated" when the DIB header is larger than
			// its avail; here avail is the declared entry size, so that is a
			// bad image, not a cut object.
			_, reason, err = dibCheck(w, off, size)
			dibs++
		}
		if err != nil {
			return fail(err)
		}
		if reason != "" {
			return reject("bad-image")
		}
		end = max(end, off+size)
	}
	return Verdict{Outcome: Validated, Length: end, Type: typ, Basis: []string{
		"images=" + strconv.FormatInt(count, 10),
		"png=" + strconv.Itoa(pngs),
		"dib=" + strconv.Itoa(dibs),
	}}
}
