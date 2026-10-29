package carve

import (
	"errors"
	"strconv"
)

// readFailReason is the one mapping of a failed window read to a typed rejection
// reason (ruling 3E-C1): a read that reached past the window is "truncated";
// every other failure (media error, spent budget, cancelled context) is
// "unreadable". The scanner rejects any hit whose window saw a media error.
func readFailReason(err error) string {
	if errors.Is(err, ErrOutsideWindow) {
		return "truncated"
	}
	return "unreadable"
}

// riffChunk is one chunk of a RIFF walk: its fourcc and the window offset and
// size of its payload.
type riffChunk struct {
	typ       string
	off, size int64
}

// riffWalk is the RIFF container walker shared by the WebP and WAV validators. It
// checks the 12-byte header (RIFF, the form), the declared size against the window, and walks the chunks (printable
// fourcc, little-endian size, even padding) until the cursor lands exactly on
// 8 + the RIFF size. A chunk size only moves a cursor inside the window; no
// allocation is made from it and every step advances at least 8 bytes. visit
// is called for each chunk in order (index from 0) and returns a rejection
// reason, or "" to go on. It returns the object length, the chunk count and a
// rejection reason ("" when the walk completed). A window or media failure is
// "truncated" past the window and "unreadable" otherwise.
func riffWalk(w *Window, form string, visit func(i int, c riffChunk) string) (length int64, chunks int, reason string) {
	var hdr [12]byte
	if _, err := w.ReadAt(hdr[:], 0); err != nil {
		return 0, 0, readFailReason(err)
	}
	if string(hdr[:4]) != "RIFF" {
		return 0, 0, "bad-signature"
	}
	if string(hdr[8:]) != form {
		return 0, 0, "bad-form"
	}
	size := int64(hdr[4]) | int64(hdr[5])<<8 | int64(hdr[6])<<16 | int64(hdr[7])<<24
	if size < 4 {
		return 0, 0, "bad-length"
	}
	total := 8 + size
	if total > w.Len() {
		return 0, 0, "truncated"
	}
	for pos := int64(12); pos < total; chunks++ {
		var ch [8]byte
		if total-pos < 8 {
			return 0, 0, "bad-length"
		}
		if _, err := w.ReadAt(ch[:], pos); err != nil {
			return 0, 0, readFailReason(err)
		}
		for _, c := range ch[:4] {
			if c < 0x20 || c > 0x7E {
				return 0, 0, "bad-chunk-type"
			}
		}
		csize := int64(ch[4]) | int64(ch[5])<<8 | int64(ch[6])<<16 | int64(ch[7])<<24
		next := pos + 8 + csize + csize&1
		if next > total {
			return 0, 0, "bad-length"
		}
		if r := visit(chunks, riffChunk{typ: string(ch[:4]), off: pos + 8, size: csize}); r != "" {
			return 0, 0, r
		}
		pos = next
	}
	return total, chunks, ""
}

// webpImageHeader checks the payload header of a VP8 or VP8L image chunk and
// returns its dimensions; a chunk too short for the header, or with a wrong
// signature, is rejected, so an empty image chunk never counts as an image.
func webpImageHeader(w *Window, c riffChunk) (width, height int, reason string) {
	if c.typ == "VP8L" {
		if c.size < 5 {
			return 0, 0, "bad-vp8l"
		}
		var p [5]byte
		if _, err := w.ReadAt(p[:], c.off); err != nil {
			return 0, 0, readFailReason(err)
		}
		if p[0] != 0x2F {
			return 0, 0, "bad-vp8l"
		}
		bits := uint32(p[1]) | uint32(p[2])<<8 | uint32(p[3])<<16 | uint32(p[4])<<24
		return int(bits&0x3FFF) + 1, int(bits>>14&0x3FFF) + 1, ""
	}
	if c.size < 10 {
		return 0, 0, "bad-vp8"
	}
	var p [10]byte
	if _, err := w.ReadAt(p[:], c.off); err != nil {
		return 0, 0, readFailReason(err)
	}
	if p[3] != 0x9D || p[4] != 0x01 || p[5] != 0x2A {
		return 0, 0, "bad-vp8"
	}
	width = int(uint16(p[6])|uint16(p[7])<<8) & 0x3FFF
	height = int(uint16(p[8])|uint16(p[9])<<8) & 0x3FFF
	if width == 0 || height == 0 {
		return 0, 0, "zero-dimensions"
	}
	return width, height, ""
}

// validateRIFFWebP accepts a WebP from its RIFF structure: a VP8, VP8L or VP8X
// first chunk, each image chunk with a valid payload header (a VP8X file needs
// at least one, directly or inside an ANMF frame), and a chunk walk that lands
// exactly on the RIFF size, which bounds the object. The layout is from memory
// (docs/limitations.md).
func validateRIFFWebP(w *Window) Verdict {
	var dims string
	var vp8x, image bool
	var canvas string
	length, chunks, reason := riffWalk(w, "WEBP", func(i int, c riffChunk) string {
		if i == 0 {
			switch c.typ {
			case "VP8 ", "VP8L":
				wd, ht, r := webpImageHeader(w, c)
				if r != "" {
					return r
				}
				dims, image = strconv.Itoa(wd)+"x"+strconv.Itoa(ht), true
			case "VP8X":
				if c.size < 10 {
					return "bad-vp8x"
				}
				var p [10]byte
				if _, err := w.ReadAt(p[:], c.off); err != nil {
					return readFailReason(err)
				}
				cw := 1 + (int(p[4]) | int(p[5])<<8 | int(p[6])<<16)
				ch := 1 + (int(p[7]) | int(p[8])<<8 | int(p[9])<<16)
				canvas, vp8x = strconv.Itoa(cw)+"x"+strconv.Itoa(ch), true
			default:
				return "bad-first-chunk"
			}
			return ""
		}
		if !vp8x || image {
			return ""
		}
		switch c.typ {
		case "VP8 ", "VP8L":
			if _, _, r := webpImageHeader(w, c); r != "" {
				return r
			}
			image = true
		case "ANMF":
			// 16 bytes of frame header, then the frame's own chunks; the
			// first must be an image chunk or its ALPH.
			if c.size < 24 {
				return ""
			}
			var sub [8]byte
			if _, err := w.ReadAt(sub[:], c.off+16); err != nil {
				return readFailReason(err)
			}
			typ := string(sub[:4])
			ssize := int64(sub[4]) | int64(sub[5])<<8 | int64(sub[6])<<16 | int64(sub[7])<<24
			if ssize > c.size-24 || (typ != "VP8 " && typ != "VP8L" && typ != "ALPH") {
				return ""
			}
			if typ != "ALPH" {
				if _, _, r := webpImageHeader(w, riffChunk{typ: typ, off: c.off + 24, size: ssize}); r != "" {
					return r
				}
			}
			image = true
		}
		return ""
	})
	if reason != "" {
		return Verdict{Outcome: Rejected, Reason: reason}
	}
	if chunks == 0 {
		return Verdict{Outcome: Rejected, Reason: "bad-first-chunk"}
	}
	if !image {
		return Verdict{Outcome: Rejected, Reason: "no-image"}
	}
	if vp8x {
		dims = canvas
	}
	return Verdict{Outcome: Validated, Length: length, Basis: []string{"dims=" + dims, "chunks=" + strconv.Itoa(chunks)}}
}

// validateRIFFWave accepts a WAV from its RIFF structure: form WAVE, a first
// chunk "fmt " of at least 16 bytes whose audio format, channel count, sample
// rate and block align are all non-zero, a "data" chunk somewhere, and a chunk
// walk that lands exactly on the RIFF size, which bounds the object. A file
// with no chunk at all has no fmt chunk (bad-fmt). Samples are never read. The
// layout is from memory (docs/limitations.md).
func validateRIFFWave(w *Window) Verdict {
	var channels, rate uint32
	var data bool
	length, chunks, reason := riffWalk(w, "WAVE", func(i int, c riffChunk) string {
		switch {
		case i == 0:
			if c.typ != "fmt " || c.size < 16 {
				return "bad-fmt"
			}
			var p [16]byte
			if _, err := w.ReadAt(p[:], c.off); err != nil {
				return readFailReason(err)
			}
			format := uint32(p[0]) | uint32(p[1])<<8
			channels = uint32(p[2]) | uint32(p[3])<<8
			rate = uint32(p[4]) | uint32(p[5])<<8 | uint32(p[6])<<16 | uint32(p[7])<<24
			align := uint32(p[12]) | uint32(p[13])<<8
			if format == 0 || channels == 0 || rate == 0 || align == 0 {
				return "bad-fmt"
			}
		case c.typ == "data":
			data = true
		}
		return ""
	})
	switch {
	case reason != "":
		return Verdict{Outcome: Rejected, Reason: reason}
	case chunks == 0:
		return Verdict{Outcome: Rejected, Reason: "bad-fmt"}
	case !data:
		return Verdict{Outcome: Rejected, Reason: "no-data"}
	}
	return Verdict{Outcome: Validated, Length: length, Basis: []string{"channels=" + strconv.FormatUint(uint64(channels), 10), "rate=" + strconv.FormatUint(uint64(rate), 10)}}
}
