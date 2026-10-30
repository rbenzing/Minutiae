package carvetest

import "encoding/binary"

// WAVOpts shapes a synthetic PCM WAV built by an encoder independent of the
// validator. Zero values give mono 8000 Hz 16-bit audio with 160 data bytes.
type WAVOpts struct {
	Channels, Rate, Bits int
	DataLen              int
	NoData               bool // no data chunk
	BadFmt               bool // the fmt chunk has zero channels
	ListBefore           bool // a LIST chunk between fmt and data
	OddChunk             bool // an odd-sized JUNK chunk (with its pad) before data
	SizeDelta            int  // added to the RIFF size field
}

// WAV builds a WAV per o.
func WAV(o WAVOpts) []byte {
	if o.Channels == 0 {
		o.Channels = 1
	}
	if o.Rate == 0 {
		o.Rate = 8000
	}
	if o.Bits == 0 {
		o.Bits = 16
	}
	if o.DataLen == 0 {
		o.DataLen = 160
	}
	align := o.Channels * o.Bits / 8
	f := binary.LittleEndian.AppendUint16(nil, 1)
	ch := o.Channels
	if o.BadFmt {
		ch = 0
	}
	f = binary.LittleEndian.AppendUint16(f, uint16(ch))
	f = binary.LittleEndian.AppendUint32(f, uint32(o.Rate))
	f = binary.LittleEndian.AppendUint32(f, uint32(o.Rate*align))
	f = binary.LittleEndian.AppendUint16(f, uint16(align))
	f = binary.LittleEndian.AppendUint16(f, uint16(o.Bits))
	chunks := [][]byte{RIFFChunk("fmt ", f)}
	if o.ListBefore {
		chunks = append(chunks, RIFFChunk("LIST", append([]byte("INFOISFT\x05\x00\x00\x00Lavf\x00"), 0)))
	}
	if o.OddChunk {
		chunks = append(chunks, RIFFChunk("JUNK", []byte{1, 2, 3}))
	}
	if !o.NoData {
		chunks = append(chunks, RIFFChunk("data", make([]byte, o.DataLen)))
	}
	return RIFF("WAVE", o.SizeDelta, chunks...)
}
