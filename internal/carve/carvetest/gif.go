package carvetest

// GIFOpts shapes a synthetic GIF built by an encoder independent of the
// validator. Zero values give a GIF89a 64x48 file with one frame, no tables.
type GIFOpts struct {
	Version    string // "GIF87a" or "GIF89a" (default); anything else is written as given
	W, H       uint16
	GCT, LCT   bool
	Frames     int    // 0 = 1 unless NoFrames
	NoFrames   bool   // a header and a trailer only
	Extensions []byte // raw extension blocks written before the first frame
	NoTrailer  bool
	ZeroDims   bool
	LZWMin     byte // 0 = 8
	DataLen    int  // image data bytes per frame; 0 = 300 (two sub-blocks)
}

// GIFSubBlocks returns data cut into sub-blocks of at most 255 bytes and the
// zero-size terminator.
func GIFSubBlocks(data []byte) []byte {
	var b []byte
	for len(data) > 0 {
		n := min(len(data), 255)
		b = append(b, byte(n))
		b = append(b, data[:n]...)
		data = data[n:]
	}
	return append(b, 0)
}

// GIFExtension returns an extension block: 0x21, label, sub-blocks.
func GIFExtension(label byte, data []byte) []byte {
	return append([]byte{0x21, label}, GIFSubBlocks(data)...)
}

// GIF builds a GIF per o.
func GIF(o GIFOpts) []byte {
	if o.Version == "" {
		o.Version = "GIF89a"
	}
	if o.W == 0 {
		o.W = 64
	}
	if o.H == 0 {
		o.H = 48
	}
	if o.ZeroDims {
		o.W = 0
	}
	if o.Frames == 0 && !o.NoFrames {
		o.Frames = 1
	}
	if o.LZWMin == 0 {
		o.LZWMin = 8
	}
	if o.DataLen == 0 {
		o.DataLen = 300
	}
	b := []byte(o.Version)
	packed := byte(0)
	if o.GCT {
		packed = 0x80 | 1 // 4 entries
	}
	b = append(b, byte(o.W), byte(o.W>>8), byte(o.H), byte(o.H>>8), packed, 0, 0)
	if o.GCT {
		b = append(b, make([]byte, 12)...)
	}
	b = append(b, o.Extensions...)
	for f := range o.Frames {
		if o.Version == "GIF89a" {
			b = append(b, GIFExtension(0xF9, []byte{4, 10, 0, 0})...)
		}
		lp := byte(0)
		if o.LCT {
			lp = 0x80 | 2 // 8 entries
		}
		b = append(b, 0x2C, 0, 0, 0, 0, 8, 0, 8, 0, lp)
		if o.LCT {
			b = append(b, make([]byte, 24)...)
		}
		b = append(b, o.LZWMin)
		data := make([]byte, o.DataLen)
		for i := range data {
			data[i] = byte(i*13 + f)
		}
		b = append(b, GIFSubBlocks(data)...)
	}
	if !o.NoTrailer {
		b = append(b, 0x3B)
	}
	return b
}
