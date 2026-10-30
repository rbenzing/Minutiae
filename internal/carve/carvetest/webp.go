package carvetest

import "encoding/binary"

// RIFFChunk returns a RIFF chunk: fourcc, little-endian size, data and one pad
// byte when the size is odd.
func RIFFChunk(typ string, data []byte) []byte {
	b := make([]byte, 8, 9+len(data))
	copy(b, typ)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	b = append(b, data...)
	if len(data)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// RIFF wraps chunks in a RIFF container of the given form; sizeDelta is added
// to the declared size to build damaged objects.
func RIFF(form string, sizeDelta int, chunks ...[]byte) []byte {
	body := []byte(form)
	for _, c := range chunks {
		body = append(body, c...)
	}
	b := make([]byte, 8, 8+len(body))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(int64(len(body))+int64(sizeDelta)))
	return append(b, body...)
}

// WebPOpts shapes a synthetic WebP built by an encoder independent of the
// validator. Zero values give a 64x48 lossy (VP8 ) file.
type WebPOpts struct {
	Kind         string   // "VP8 ", "VP8L" or "VP8X"; "" = "VP8 "
	W, H         int      // 0 = 64, 48
	Extra        []string // "ICCP", "ALPH" (before the image), "EXIF", "XMP " (after)
	OddChunk     bool     // the image payload gets an odd size (and a pad byte)
	BadStartCode bool     // the VP8 frame start code is damaged
	ZeroDims     bool     // the VP8 frame dimensions are zero
	SizeDelta    int      // added to the RIFF size field
}

// WebP builds a WebP per o.
func WebP(o WebPOpts) []byte {
	if o.Kind == "" {
		o.Kind = "VP8 "
	}
	if o.W == 0 {
		o.W = 64
	}
	if o.H == 0 {
		o.H = 48
	}
	image := func() []byte {
		var p []byte
		if o.Kind == "VP8L" {
			bits := uint32(o.W-1) | uint32(o.H-1)<<14
			p = append([]byte{0x2F}, binary.LittleEndian.AppendUint32(nil, bits)...)
			p = append(p, 0x01, 0x02, 0x03)
			if o.OddChunk != (len(p)%2 == 1) {
				p = append(p, 0x04)
			}
			return RIFFChunk("VP8L", p)
		}
		p = []byte{0x10, 0x02, 0x00, 0x9D, 0x01, 0x2A}
		if o.BadStartCode {
			p[3] = 0x9E
		}
		w, h := uint16(o.W), uint16(o.H)
		if o.ZeroDims {
			w, h = 0, 0
		}
		p = binary.LittleEndian.AppendUint16(p, w)
		p = binary.LittleEndian.AppendUint16(p, h)
		p = append(p, 0x01, 0x02, 0x03, 0x04)
		if o.OddChunk != (len(p)%2 == 1) {
			p = append(p, 0x05)
		}
		return RIFFChunk("VP8 ", p)
	}
	var chunks [][]byte
	if o.Kind == "VP8X" {
		p := make([]byte, 10)
		p[0] = 0x10
		put24 := func(b []byte, v int) { b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16) }
		put24(p[4:], o.W-1)
		put24(p[7:], o.H-1)
		chunks = append(chunks, RIFFChunk("VP8X", p))
	}
	var after [][]byte
	for _, e := range o.Extra {
		c := RIFFChunk(e, []byte{1, 2, 3, 4, 5, 6})
		if e == "ICCP" || e == "ALPH" {
			chunks = append(chunks, c)
		} else {
			after = append(after, c)
		}
	}
	if o.Kind == "VP8X" {
		o.Kind = "VP8 "
	}
	chunks = append(chunks, image())
	chunks = append(chunks, after...)
	return RIFF("WEBP", o.SizeDelta, chunks...)
}
