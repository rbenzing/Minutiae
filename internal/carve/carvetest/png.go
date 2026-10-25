package carvetest

import (
	"encoding/binary"
	"hash/crc32"
)

// PNGSignature is the 8-byte PNG file signature.
var PNGSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// PNGOpts shapes a synthetic PNG built by an encoder independent of the
// validator. Zero values give a 64x48 RGB 8-bit file with one IDAT.
type PNGOpts struct {
	W, H                 uint32
	ColorType, BitDepth  byte // BitDepth 0 = colour type 2 (RGB), depth 8
	IDATs                int  // number of IDAT chunks; 0 = 1
	IDATData             []byte
	Ancillary            []string // chunk types emitted between IHDR and IDAT
	APNG                 bool     // acTL, fcTL and a trailing fcTL and fdAT
	BadCRC               bool     // the first IDAT carries a wrong CRC
	NoIDAT, IHDRNotFirst bool
	ZeroWidth, OmitIEND  bool
	IENDPayload          bool // IEND with a 1-byte payload
}

// PNGChunk returns a chunk: big-endian length, type, data and the CRC-32
// (IEEE) over type and data.
func PNGChunk(typ string, data []byte) []byte {
	b := make([]byte, 8, 12+len(data))
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	copy(b[4:], typ)
	b = append(b, data...)
	crc := crc32.ChecksumIEEE(b[4:])
	return binary.BigEndian.AppendUint32(b, crc)
}

// PNG builds a PNG per o.
func PNG(o PNGOpts) []byte {
	if o.W == 0 && !o.ZeroWidth {
		o.W = 64
	}
	if o.ZeroWidth {
		o.W = 0
	}
	if o.H == 0 {
		o.H = 48
	}
	if o.BitDepth == 0 {
		o.ColorType, o.BitDepth = 2, 8
	}
	if o.IDATs == 0 {
		o.IDATs = 1
	}
	ihdr := binary.BigEndian.AppendUint32(nil, o.W)
	ihdr = binary.BigEndian.AppendUint32(ihdr, o.H)
	ihdr = append(ihdr, o.BitDepth, o.ColorType, 0, 0, 0)
	b := append([]byte(nil), PNGSignature...)
	if o.IHDRNotFirst {
		b = append(b, PNGChunk("tEXt", []byte("k\x00v"))...)
	}
	b = append(b, PNGChunk("IHDR", ihdr)...)
	if o.ColorType == 3 {
		b = append(b, PNGChunk("PLTE", []byte{0, 0, 0, 255, 0, 0, 0, 255, 0, 0, 0, 255})...)
	}
	for _, a := range o.Ancillary {
		b = append(b, PNGChunk(a, []byte("0123456789"))...)
	}
	if o.APNG {
		b = append(b, PNGChunk("acTL", make([]byte, 8))...)
		b = append(b, PNGChunk("fcTL", make([]byte, 26))...)
	}
	if !o.NoIDAT {
		data := o.IDATData
		if data == nil {
			data = make([]byte, 40*o.IDATs)
			for i := range data {
				data[i] = byte(i*29 + 7)
			}
		}
		per := max(1, (len(data)+o.IDATs-1)/o.IDATs)
		for i := range o.IDATs {
			lo := min(i*per, len(data))
			hi := min(lo+per, len(data))
			c := PNGChunk("IDAT", data[lo:hi])
			if o.BadCRC && i == 0 {
				c[len(c)-1] ^= 0xFF
			}
			b = append(b, c...)
		}
	}
	if o.APNG {
		b = append(b, PNGChunk("fcTL", make([]byte, 26))...)
		b = append(b, PNGChunk("fdAT", make([]byte, 12))...)
	}
	if o.IENDPayload {
		return append(b, PNGChunk("IEND", []byte{0})...)
	}
	if !o.OmitIEND {
		b = append(b, PNGChunk("IEND", nil)...)
	}
	return b
}
