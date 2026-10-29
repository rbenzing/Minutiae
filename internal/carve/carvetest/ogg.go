package carvetest

import "encoding/binary"

// OggPage is one page for Ogg: header-type flags (1 continued, 2 first of
// stream, 4 last of stream), the logical stream serial, the sequence number
// and the segments (each at most 255 bytes; a 255-byte segment continues the
// packet into the next segment or page). BadCRC writes a wrong checksum.
type OggPage struct {
	Flags    byte
	Serial   uint32
	Seq      uint32
	Segments [][]byte
	BadCRC   bool
}

// oggCRC computes the Ogg page checksum: CRC-32, polynomial 0x04C11DB7, not
// reflected, initial value 0, no final xor, from its own table (independent of
// the validator's).
func oggCRC(b []byte) uint32 {
	var tab [256]uint32
	for i := range tab {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		tab[i] = r
	}
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ tab[byte(crc>>24)^c]
	}
	return crc
}

// Ogg builds the concatenation of pages. It panics on a segment above 255
// bytes or more than 255 segments in a page: that is a test bug.
func Ogg(pages []OggPage) []byte {
	var out []byte
	for _, p := range pages {
		if len(p.Segments) > 255 {
			panic("carvetest: more than 255 segments in an Ogg page")
		}
		b := append([]byte("OggS"), 0, p.Flags)
		b = binary.LittleEndian.AppendUint64(b, 0)
		b = binary.LittleEndian.AppendUint32(b, p.Serial)
		b = binary.LittleEndian.AppendUint32(b, p.Seq)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, byte(len(p.Segments)))
		for _, s := range p.Segments {
			if len(s) > 255 {
				panic("carvetest: Ogg segment above 255 bytes")
			}
			b = append(b, byte(len(s)))
		}
		for _, s := range p.Segments {
			b = append(b, s...)
		}
		crc := oggCRC(b)
		if p.BadCRC {
			crc ^= 0xDEADBEEF
		}
		binary.LittleEndian.PutUint32(b[22:], crc)
		out = append(out, b...)
	}
	return out
}
