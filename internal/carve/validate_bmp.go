package carve

import (
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
)

// dibInfo is what dibCheck learns from a plausible DIB header.
type dibInfo struct {
	HeaderSize, W, H int64 // H = |height|
	BPP, Compression uint32
	TopDown          bool
	PixelBytes       int64 // uncompressed array size; -1 when compressed
	SizeImage        int64
}

// The BMP compression codes (the 64-byte OS/2 header reuses 3 and 4).
const (
	bmpRGB       = 0
	bmpRLE8      = 1
	bmpRLE4      = 2
	bmpBitfields = 3
	bmpJPEG      = 4
	bmpPNG       = 5
	bmpAlphaBits = 6
)

// bmpMaxDim is the largest width or height of this build (spec section 5).
const bmpMaxDim = 65536

// bmpDIBSize reports whether n is a known DIB header size.
func bmpDIBSize(n int64) bool {
	return n == 12 || n == 40 || n == 52 || n == 56 || n == 64 || n == 108 || n == 124
}

// dibCheck checks the DIB header at off, with avail bytes available to it
// (spec D3). The reason is "" for a plausible header. A window refusal (the
// header does not fit avail or the window) is the reason "truncated"; err is
// only a read failure of the media, a spent budget or a cancel, which the
// caller maps to "unreadable". Nothing past the header is read. A caller
// whose avail is a size the file declared (an ICO entry) maps "truncated" for
// a header larger than avail itself: there it is a bad image, not a cut object.
func dibCheck(w *Window, off, avail int64) (dibInfo, string, error) {
	var d dibInfo
	fail := func(err error) (dibInfo, string, error) {
		if errors.Is(err, ErrOutsideWindow) {
			return dibInfo{}, "truncated", nil
		}
		return dibInfo{}, "", err
	}
	if avail < 4 {
		return fail(ErrOutsideWindow)
	}
	var sz [4]byte
	if err := w.readChecked(sz[:], off); err != nil {
		return fail(err)
	}
	d.HeaderSize = int64(binary.LittleEndian.Uint32(sz[:]))
	if !bmpDIBSize(d.HeaderSize) {
		return dibInfo{}, "bad-dib", nil
	}
	if avail < d.HeaderSize {
		return fail(ErrOutsideWindow)
	}
	var h [24]byte // the fields through biSizeImage
	n := min(d.HeaderSize, int64(len(h)))
	if err := w.readChecked(h[:n], off); err != nil {
		return fail(err)
	}
	le := binary.LittleEndian
	d.SizeImage = -1
	if d.HeaderSize == 12 {
		d.W, d.H = int64(le.Uint16(h[4:])), int64(le.Uint16(h[6:]))
		if d.W == 0 || d.H == 0 {
			return dibInfo{}, "bad-dimensions", nil
		}
		if le.Uint16(h[8:]) != 1 {
			return dibInfo{}, "bad-planes", nil
		}
		d.BPP = uint32(le.Uint16(h[10:]))
		if !slices.Contains([]uint32{1, 4, 8, 24}, d.BPP) {
			return dibInfo{}, "bad-bpp", nil
		}
		d.PixelBytes = bmpPixelBytes(d.W, d.H, d.BPP)
		return d, "", nil
	}
	w32, h32 := int64(int32(le.Uint32(h[4:]))), int64(int32(le.Uint32(h[8:])))
	if h32 < 0 {
		d.TopDown = true
		h32 = -h32
	}
	if w32 < 1 || w32 > bmpMaxDim || h32 < 1 || h32 > bmpMaxDim {
		return dibInfo{}, "bad-dimensions", nil
	}
	d.W, d.H = w32, h32
	if le.Uint16(h[12:]) != 1 {
		return dibInfo{}, "bad-planes", nil
	}
	d.BPP = uint32(le.Uint16(h[14:]))
	if !slices.Contains([]uint32{0, 1, 4, 8, 16, 24, 32}, d.BPP) {
		return dibInfo{}, "bad-bpp", nil
	}
	d.Compression = le.Uint32(h[16:])
	d.SizeImage = int64(le.Uint32(h[20:]))
	compressed, ok := bmpCompression(d.HeaderSize, d.Compression, d.BPP)
	if !ok || d.TopDown && compressed {
		return dibInfo{}, "bad-compression", nil
	}
	if compressed {
		d.PixelBytes = -1
	} else {
		d.PixelBytes = bmpPixelBytes(d.W, d.H, d.BPP)
	}
	return d, "", nil
}

// bmpCompression says whether a compression code is valid for the header size
// and bpp, and whether it names a compressed array.
func bmpCompression(headerSize int64, c, bpp uint32) (compressed, ok bool) {
	switch {
	case headerSize == 64 && c == bmpBitfields: // OS/2 Huffman 1D
		return true, bpp == 1
	case headerSize == 64 && c == bmpJPEG: // OS/2 RLE24 (code 4)
		return true, bpp == 24
	case bpp == 0:
		return true, c == bmpJPEG || c == bmpPNG
	case c == bmpJPEG || c == bmpPNG:
		return false, false
	case c == bmpRGB:
		return false, true
	case c == bmpRLE8:
		return true, bpp == 8
	case c == bmpRLE4:
		return true, bpp == 4
	case c == bmpBitfields || c == bmpAlphaBits:
		return false, bpp == 16 || bpp == 32
	}
	return false, false
}

// bmpPixelBytes is the uncompressed array size. W and H are at most 65536 and
// bpp at most 32, so the product stays far inside int64: dibCheck bounds the
// 40-byte-and-longer headers against bmpMaxDim before it calls this, and on
// the core-header path the values are bounded by their u16 field width.
func bmpPixelBytes(w, h int64, bpp uint32) int64 {
	row := (int64(bpp)*w + 31) / 32 * 4
	return row * h
}

// validateBMP accepts a bitmap file from its file header and DIB header alone
// (spec D3): the length is bfSize, never the pixel array, and no pixel is read.
// The layout is from memory (docs/limitations.md).
func validateBMP(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	var head [18]byte // file header (14) and the DIB header size
	have := int(min(int64(len(head)), w.Len()))
	if err := w.readChecked(head[:have], 0); err != nil {
		return fail(err)
	}
	if have >= 2 && (head[0] != 'B' || head[1] != 'M') {
		return reject("bad-signature")
	}
	if have < len(head) {
		return reject("truncated")
	}
	le := binary.LittleEndian
	bfSize, offBits := int64(le.Uint32(head[2:])), int64(le.Uint32(head[10:]))
	dibSize := int64(le.Uint32(head[14:]))
	if !bmpDIBSize(dibSize) {
		return reject("bad-dib")
	}
	if bfSize < 14+dibSize {
		return reject("bad-size")
	}
	if bfSize > w.Len() {
		return reject("truncated")
	}
	d, reason, err := dibCheck(w, 14, bfSize-14)
	if err != nil {
		return fail(err)
	}
	if reason != "" {
		return reject(reason)
	}
	if offBits < 14+dibSize || offBits >= bfSize {
		return reject("bad-offset")
	}
	if d.PixelBytes >= 0 {
		if d.PixelBytes > bfSize-offBits {
			return reject("pixels-exceed-size")
		}
	} else if d.SizeImage == 0 || d.SizeImage > bfSize-offBits {
		return reject("pixels-exceed-size")
	}
	return Verdict{Outcome: Validated, Length: bfSize, Basis: []string{
		"dims=" + strconv.FormatInt(d.W, 10) + "x" + strconv.FormatInt(d.H, 10),
		"bpp=" + strconv.FormatUint(uint64(d.BPP), 10),
		"dib=" + strconv.FormatInt(d.HeaderSize, 10),
		"compression=" + strconv.FormatUint(uint64(d.Compression), 10),
	}}
}
