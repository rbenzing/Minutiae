package carvetest

import "encoding/binary"

// BMPOpts shapes a synthetic BMP built by an encoder independent of the
// validator. The fields are literal (no defaults), so a test states exactly the
// header it wants, including the invalid ones.
type BMPOpts struct {
	DIBSize       int // the DIB header size, written as given (the header is cut or zero-padded to it)
	W, H          int32
	BPP, Planes   uint16
	Compression   uint32
	SizeImage     uint32
	FileSizeDelta int // added to the correct bfSize
	OffBitsDelta  int // added to the correct bfOffBits
	ZeroFileSize  bool
}

// maxBuiltPixels bounds the pixel bytes the builder writes, so a hostile
// dimension in a test never allocates; the array is then left empty.
const maxBuiltPixels = 1 << 24

// bmpCompressed reports whether the compression code of a header size names a
// compressed array (the OS/2 64-byte header reuses codes 3 and 4).
func bmpCompressed(dibSize int, c uint32) bool {
	switch c {
	case 1, 2, 4, 5:
		return true
	case 3:
		return dibSize == 64
	}
	return false
}

// DIB builds a DIB header, its palette (bpp <= 8) and the pixel bytes, without
// the 14-byte file header (headerless, for ICO). A compressed form gets
// SizeImage opaque bytes.
func DIB(o BMPOpts) []byte {
	hdr := make([]byte, max(o.DIBSize, 40))
	le := binary.LittleEndian
	le.PutUint32(hdr[0:], uint32(o.DIBSize))
	if o.DIBSize == 12 {
		le.PutUint16(hdr[4:], uint16(o.W))
		le.PutUint16(hdr[6:], uint16(o.H))
		le.PutUint16(hdr[8:], o.Planes)
		le.PutUint16(hdr[10:], o.BPP)
	} else {
		le.PutUint32(hdr[4:], uint32(o.W))
		le.PutUint32(hdr[8:], uint32(o.H))
		le.PutUint16(hdr[12:], o.Planes)
		le.PutUint16(hdr[14:], o.BPP)
		le.PutUint32(hdr[16:], o.Compression)
		le.PutUint32(hdr[20:], o.SizeImage)
	}
	out := hdr[:o.DIBSize]
	if o.BPP >= 1 && o.BPP <= 8 {
		entry := 4
		if o.DIBSize == 12 {
			entry = 3
		}
		for i := 0; i < entry<<o.BPP; i++ {
			out = append(out, byte(i*5+1))
		}
	}
	n := int64(0)
	if o.DIBSize != 12 && bmpCompressed(o.DIBSize, o.Compression) {
		n = int64(o.SizeImage)
	} else if o.W > 0 && o.H != 0 {
		h := int64(o.H)
		if h < 0 {
			h = -h
		}
		row := (int64(o.BPP)*int64(o.W) + 31) / 32 * 4
		n = row * h
	}
	if n > maxBuiltPixels {
		n = 0
	}
	for i := int64(0); i < n; i++ {
		out = append(out, byte(i*7+3))
	}
	return out
}

// BMP builds a bitmap file: the file header, the DIB header, a palette for
// bpp <= 8 and the pixel bytes. bfSize and bfOffBits are the correct values
// moved by FileSizeDelta and OffBitsDelta.
func BMP(o BMPOpts) []byte {
	dib := DIB(o)
	out := make([]byte, 14, 14+len(dib))
	out[0], out[1] = 'B', 'M'
	palette := 0
	if o.BPP >= 1 && o.BPP <= 8 {
		entry := 4
		if o.DIBSize == 12 {
			entry = 3
		}
		palette = entry << o.BPP
	}
	size := 14 + len(dib) + o.FileSizeDelta
	if o.ZeroFileSize {
		size = 0
	}
	binary.LittleEndian.PutUint32(out[2:], uint32(size))
	binary.LittleEndian.PutUint32(out[10:], uint32(14+o.DIBSize+palette+o.OffBitsDelta))
	return append(out, dib...)
}
