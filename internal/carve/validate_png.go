package carve

import (
	"errors"
	"hash/crc32"
	"strconv"
)

// pngCRCPiece is the size of the pieces a chunk is read in for its CRC.
const pngCRCPiece = 64 << 10

// validatePNG accepts a PNG from its chunk structure alone: the signature, an
// IHDR with valid fields first, chunks whose CRC-32 is verified, and an IEND
// after at least one IDAT. A chunk length only moves a cursor inside the
// window and sizes the reads of a fixed 64 KiB buffer; nothing is allocated
// from it, every iteration advances at least 12 bytes, and no IDAT is
// decompressed. The layout is from memory (docs/limitations.md).
func validatePNG(w *Window) Verdict {
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
	var sig [8]byte
	if err := w.readChecked(sig[:], 0); err != nil {
		return fail(err)
	}
	if string(sig[:]) != "\x89PNG\r\n\x1a\n" {
		return reject("bad-signature")
	}
	var (
		pos    int64 = 8
		chunks int
		idats  int
		dims   string
		winLen = w.Len()
		buf    []byte // allocated at the first chunk body, never from a declared size
	)
	for {
		var hdr [8]byte
		if err := w.readChecked(hdr[:], pos); err != nil {
			return fail(err)
		}
		length := int64(hdr[0])<<24 | int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if length > 0x7FFFFFFF {
			return reject("bad-length")
		}
		for _, c := range hdr[4:] {
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return reject("bad-chunk-type")
			}
		}
		typ := string(hdr[4:])
		if chunks == 0 && (typ != "IHDR" || length != 13) {
			return reject("no-ihdr")
		}
		if chunks > 0 && typ == "IHDR" {
			return reject("bad-ihdr")
		}
		if typ == "IEND" && length != 0 {
			return reject("bad-iend")
		}
		if length+12 > winLen-pos {
			return reject("truncated")
		}
		if buf == nil {
			buf = make([]byte, pngCRCPiece)
		}
		crc := crc32.Update(0, crc32.IEEETable, hdr[4:])
		var ihdr [13]byte
		for done := int64(0); done < length; {
			n := min(length-done, pngCRCPiece)
			if err := w.readChecked(buf[:n], pos+8+done); err != nil {
				return fail(err)
			}
			if chunks == 0 {
				copy(ihdr[:], buf[:n]) // length is 13: one piece
			}
			crc = crc32.Update(crc, crc32.IEEETable, buf[:n])
			done += n
		}
		var tail [4]byte
		if err := w.readChecked(tail[:], pos+8+length); err != nil {
			return fail(err)
		}
		if crc != uint32(tail[0])<<24|uint32(tail[1])<<16|uint32(tail[2])<<8|uint32(tail[3]) {
			return reject("crc")
		}
		if chunks == 0 {
			width := uint32(ihdr[0])<<24 | uint32(ihdr[1])<<16 | uint32(ihdr[2])<<8 | uint32(ihdr[3])
			height := uint32(ihdr[4])<<24 | uint32(ihdr[5])<<16 | uint32(ihdr[6])<<8 | uint32(ihdr[7])
			if width == 0 || height == 0 {
				return reject("zero-dimensions")
			}
			if width > 0x7FFFFFFF || height > 0x7FFFFFFF || !pngColourDepthOK(ihdr[9], ihdr[8]) ||
				ihdr[10] != 0 || ihdr[11] != 0 || ihdr[12] > 1 {
				return reject("bad-ihdr")
			}
			dims = strconv.FormatUint(uint64(width), 10) + "x" + strconv.FormatUint(uint64(height), 10)
		}
		chunks++
		pos += 12 + length
		switch typ {
		case "IDAT":
			idats++
		case "IEND":
			if idats == 0 {
				return reject("no-idat")
			}
			return Verdict{Outcome: Validated, Length: pos, Basis: []string{"dims=" + dims, "chunks=" + strconv.Itoa(chunks)}}
		}
	}
}

// pngColourDepthOK: the allowed colour type and bit depth pairs.
func pngColourDepthOK(colour, depth byte) bool {
	switch colour {
	case 0:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case 2, 4, 6:
		return depth == 8 || depth == 16
	case 3:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	}
	return false
}
