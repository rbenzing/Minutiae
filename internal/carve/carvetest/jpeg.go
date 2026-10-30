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
	EntropyLen  int    // approximate entropy bytes of the (AC) scan before stuffing; 0 = 64 (a taller frame if W x H is too small)
	OmitEOI     bool
	ZeroWidth   bool
	// ZeroSafe makes every block after the first eight a block of zero bits, so a zero-filled stretch of the
	// entropy data (an acquisition gap) still decodes to the same block count; the frame grows to hold
	// EntropyLen bytes.
	ZeroSafe bool
}

// JPEGSegment returns a marker segment: FF code, big-endian length, payload.
func JPEGSegment(code byte, payload []byte) []byte {
	b := []byte{0xFF, code, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(payload)+2))
	return append(b, payload...)
}

// JPEGBits writes an entropy-coded bit stream: Put appends bits MSB first, Restart pads the byte with 1 bits and
// writes an RSTn marker, Bytes pads the last byte with 1 bits. A 0xFF byte is stuffed with a 0x00.
type JPEGBits struct {
	out []byte
	acc uint32
	n   uint
}

// Put appends the low n bits of v (n <= 24).
func (b *JPEGBits) Put(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		b.acc = b.acc<<1 | (v>>uint(i))&1
		b.n++
		if b.n == 8 {
			b.flushByte()
		}
	}
}

func (b *JPEGBits) flushByte() {
	c := byte(b.acc)
	b.out = append(b.out, c)
	if c == 0xFF {
		b.out = append(b.out, 0)
	}
	b.acc, b.n = 0, 0
}

func (b *JPEGBits) pad() {
	for b.n != 0 {
		b.acc = b.acc<<1 | 1
		b.n++
		if b.n == 8 {
			b.flushByte()
		}
	}
}

// Restart pads to a byte boundary and writes RSTn (n is taken modulo 8).
func (b *JPEGBits) Restart(n int) {
	b.pad()
	b.out = append(b.out, 0xFF, 0xD0+byte(n&7))
}

// Bytes pads the last byte and returns the stream.
func (b *JPEGBits) Bytes() []byte {
	b.pad()
	return b.out
}

// JPEGDHT returns a DHT segment defining one table: class 0 (DC) or 1 (AC), id 0..3, the 16 code-length counts
// and the symbols.
func JPEGDHT(class, id byte, counts [16]byte, syms []byte) []byte {
	p := append([]byte{class<<4 | id}, counts[:]...)
	return JPEGSegment(0xC4, append(p, syms...))
}

// The tables of JPEG: DC has category 0 (code 0) and category 8 (code 10); AC has EOB (code 0), run 0 size 8
// (code 10) and ZRL (code 110). A block of zero bits is therefore a DC of category 0 and an EOB.
var (
	jpegDC = JPEGDHT(0, 0, [16]byte{1, 1}, []byte{0, 8})
	jpegAC = JPEGDHT(1, 0, [16]byte{1, 1, 1}, []byte{0x00, 0x08, 0xF0})
)

// jpegBlocks encodes n blocks of a scan covering coefficients ss..se: the DC part (ss == 0) is category 8 with
// eight pseudo-random bits, the AC part acN coefficients of size 8 (code 01) and an EOB unless the band is full.
func jpegBlocks(n, ss, se, acN, ri, realBlocks int) []byte {
	var b JPEGBits
	seed := uint32(12345)
	val := func() uint32 {
		seed = seed*1103515245 + 12345
		return seed >> 16 & 0xFF
	}
	rst := 0
	for i := range n {
		if ri > 0 && i > 0 && i%ri == 0 {
			b.Restart(rst)
			rst++
		}
		first := max(ss, 1)
		if i >= realBlocks { // a block of zero bits: DC category 0, end of block
			if ss == 0 {
				b.Put(0, 1)
			}
			if se >= first {
				b.Put(0, 1)
			}
			continue
		}
		if ss == 0 {
			b.Put(2, 2)
			b.Put(val(), 8)
		}
		if se < first {
			continue
		}
		k := min(acN, se-first+1)
		for range k {
			b.Put(2, 2)
			b.Put(val(), 8)
		}
		if k < se-first+1 {
			b.Put(0, 1)
		}
	}
	return b.Bytes()
}

// JPEG builds a JPEG per o: one component, a valid Huffman-coded baseline scan (or a progressive DC scan and AC
// scan), so a validator that walks the entropy-coded data accepts it.
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
	bw := (int(o.W) + 7) / 8
	// Each block carries at most 9 bits of DC and 63 coefficients of 10 bits: a request for more entropy than
	// the frame can hold makes the frame taller.
	if bw > 0 {
		const maxBits = 10 + 63*10
		if need := (o.EntropyLen*8 + maxBits - 1) / maxBits; need > bw*((int(o.H)+7)/8) {
			o.H = uint16(min(65535, (need+bw-1)/bw*8))
		}
	}
	blocks := bw * ((int(o.H) + 7) / 8)
	realBlocks := blocks
	acN := 0
	if o.ZeroSafe && bw > 0 {
		realBlocks = 8
		acN = 20
		// 8 realBlocks blocks of 10+200+1 bits, the rest two bits each.
		if need := 8 + max(0, o.EntropyLen*8-8*211)/2; need > blocks {
			o.H = uint16(min(65535, (need+bw-1)/bw*8))
			blocks = bw * ((int(o.H) + 7) / 8)
		}
	} else if blocks > 0 {
		acN = max(0, min(63, (o.EntropyLen*8/blocks-11)/10))
	}
	fill := func() []byte {
		if o.FillBytes {
			return []byte{0xFF, 0xFF}
		}
		return nil
	}
	ri := 0
	if o.Restart {
		ri = 8
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
	b = append(b, jpegDC...)
	b = append(b, jpegAC...)
	if o.Restart {
		b = append(b, JPEGSegment(0xDD, []byte{0, byte(ri)})...)
	}
	type scan struct{ ss, se int }
	scans := []scan{{0, 63}}
	if o.Progressive {
		scans = []scan{{0, 0}, {1, 63}}
	}
	for i, sc := range scans {
		if i > 0 {
			b = append(b, jpegDC...)
			b = append(b, jpegAC...)
		}
		b = append(b, JPEGSegment(0xDA, []byte{1, 1, 0, byte(sc.ss), byte(sc.se), 0})...)
		b = append(b, jpegBlocks(blocks, sc.ss, sc.se, acN, ri, realBlocks)...)
		b = append(b, fill()...)
	}
	if !o.OmitEOI {
		b = append(b, 0xFF, 0xD9)
	}
	return b
}
