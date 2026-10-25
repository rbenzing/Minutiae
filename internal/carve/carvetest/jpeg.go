package carvetest

import "encoding/binary"

// JPEGOpts shapes a synthetic JPEG built by an encoder independent of the
// validator. Zero values give a baseline 64x48 file with one scan.
type JPEGOpts struct {
	W, H        uint16
	Progressive bool   // SOF2 and two scans with a DHT between
	Restart     bool   // DRI and RSTn markers inside the entropy data
	FillBytes   bool   // 0xFF fill before markers and inside the entropy data
	Thumbnail   []byte // embedded in an APP1 segment (skipped by length)
	EntropyLen  int    // entropy bytes before stuffing; 0 = 64
	OmitEOI     bool
	ZeroWidth   bool
}

// JPEGSegment returns a marker segment: FF code, big-endian length, payload.
func JPEGSegment(code byte, payload []byte) []byte {
	b := []byte{0xFF, code, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(payload)+2))
	return append(b, payload...)
}

// JPEGEntropy returns n bytes of entropy-coded data with 0xFF stuffed as
// FF 00; restart adds an RSTn marker after every 8 source bytes, fill adds
// FF FF pairs.
func JPEGEntropy(n int, restart, fill bool) []byte {
	var out []byte
	for i := range n {
		v := byte(i*37 + 11)
		if i%17 == 5 {
			v = 0xFF
		}
		out = append(out, v)
		if v == 0xFF {
			out = append(out, 0x00)
		}
		if fill && i%11 == 3 {
			out = append(out, 0xFF, 0xFF)
			// FF FF followed by data: the second FF must be stuffed or a
			// marker; make it a stuffed data byte.
			out = append(out, 0x00)
		}
		if restart && i%8 == 7 && i != n-1 {
			out = append(out, 0xFF, 0xD0+byte(i/8%8))
		}
	}
	return out
}

// JPEG builds a JPEG per o.
func JPEG(o JPEGOpts) []byte {
	if o.W == 0 && !o.ZeroWidth {
		o.W = 64
	}
	if o.ZeroWidth {
		o.W = 0
	}
	if o.H == 0 {
		o.H = 48
	}
	if o.EntropyLen == 0 {
		o.EntropyLen = 64
	}
	fill := func() []byte {
		if o.FillBytes {
			return []byte{0xFF, 0xFF}
		}
		return nil
	}
	b := []byte{0xFF, 0xD8}
	b = append(b, fill()...)
	b = append(b, JPEGSegment(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))...)
	if o.Thumbnail != nil {
		b = append(b, JPEGSegment(0xE1, append([]byte("Exif\x00\x00"), o.Thumbnail...))...)
	}
	b = append(b, fill()...)
	b = append(b, JPEGSegment(0xDB, append([]byte{0}, make([]byte, 64)...))...)
	sof := []byte{8, byte(o.H >> 8), byte(o.H), byte(o.W >> 8), byte(o.W), 1, 1, 0x11, 0}
	code := byte(0xC0)
	if o.Progressive {
		code = 0xC2
	}
	b = append(b, JPEGSegment(code, sof)...)
	dht := append([]byte{0x00}, make([]byte, 16)...)
	b = append(b, JPEGSegment(0xC4, dht)...)
	if o.Restart {
		b = append(b, JPEGSegment(0xDD, []byte{0, 8})...)
	}
	sos := []byte{1, 1, 0, 0, 63, 0}
	scans := 1
	if o.Progressive {
		scans = 2
	}
	for i := range scans {
		if i > 0 {
			b = append(b, JPEGSegment(0xC4, dht)...)
		}
		b = append(b, JPEGSegment(0xDA, sos)...)
		b = append(b, JPEGEntropy(o.EntropyLen, o.Restart, o.FillBytes)...)
		b = append(b, fill()...)
	}
	if !o.OmitEOI {
		b = append(b, 0xFF, 0xD9)
	}
	return b
}
