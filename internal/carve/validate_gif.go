package carve

import (
	"errors"
	"strconv"
)

// validateGIF accepts a GIF from its block structure alone: the header, a
// logical screen descriptor, then extension and image blocks made of
// sub-blocks up to the trailer, after at least one image. A sub-block size
// only moves a cursor inside the window, every iteration advances at least
// one byte, and no image data is decoded. The layout is from memory
// (docs/limitations.md).
func validateGIF(w *Window) Verdict {
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
	var head [13]byte // header (6) and logical screen descriptor (7)
	if n, err := w.ReadAt(head[:], 0); err != nil {
		// The signature and version are judged on what was read, so a short
		// non-GIF is not called truncated.
		if n >= 3 && string(head[:3]) != "GIF" {
			return reject("bad-signature")
		}
		return fail(err)
	}
	if string(head[:3]) != "GIF" {
		return reject("bad-signature")
	}
	if v := string(head[3:6]); v != "87a" && v != "89a" {
		return reject("bad-version")
	}
	sw := int(head[6]) | int(head[7])<<8
	sh := int(head[8]) | int(head[9])<<8
	if sw == 0 || sh == 0 {
		return reject("zero-dimensions")
	}
	pos := int64(13)
	// skipTable moves past a colour table when the packed flag says there is
	// one, proving its last byte lies inside the window.
	skipTable := func(packed byte) error {
		if packed&0x80 == 0 {
			return nil
		}
		size := int64(3) << ((packed & 7) + 1)
		if _, err := w.Byte(pos + size - 1); err != nil {
			return err
		}
		pos += size
		return nil
	}
	// skipSubBlocks moves past sub-blocks up to and including the zero size.
	skipSubBlocks := func() error {
		for {
			n, err := w.Byte(pos)
			if err != nil {
				return err
			}
			pos += 1 + int64(n)
			if n == 0 {
				return nil
			}
		}
	}
	if err := skipTable(head[10]); err != nil {
		return fail(err)
	}
	frames := 0
	for {
		b, err := w.Byte(pos)
		if err != nil {
			return fail(err)
		}
		pos++
		switch b {
		case 0x3B:
			if frames == 0 {
				return reject("no-image")
			}
			return Verdict{Outcome: Validated, Length: pos, Basis: []string{
				"dims=" + strconv.Itoa(sw) + "x" + strconv.Itoa(sh), "frames=" + strconv.Itoa(frames),
			}}
		case 0x21:
			if _, err := w.Byte(pos); err != nil { // the label
				return fail(err)
			}
			pos++
			if err := skipSubBlocks(); err != nil {
				return fail(err)
			}
		case 0x2C:
			var d [9]byte
			if _, err := w.ReadAt(d[:], pos); err != nil {
				return fail(err)
			}
			if d[4] == 0 && d[5] == 0 || d[6] == 0 && d[7] == 0 {
				return reject("zero-dimensions")
			}
			pos += 9
			if err := skipTable(d[8]); err != nil {
				return fail(err)
			}
			lzw, err := w.Byte(pos)
			if err != nil {
				return fail(err)
			}
			pos++
			if lzw < 2 || lzw > 11 {
				return reject("bad-lzw")
			}
			if err := skipSubBlocks(); err != nil {
				return fail(err)
			}
			frames++
		default:
			return reject("bad-block")
		}
	}
}
