package carvetest

// GzipHeader returns a gzip member header (RFC 1952 layout) followed by a
// short stored deflate body and a zero trailer, for the flag bits given: 0x02
// FHCRC, 0x04 FEXTRA (a 4-byte extra field is written), 0x08 FNAME (name),
// 0x10 FCOMMENT (comment); any other bit is written as given, so a test can
// build a header with a reserved bit set. The method is always 8.
func GzipHeader(flags byte, name, comment string) []byte {
	b := []byte{0x1F, 0x8B, 0x08, flags, 0, 0, 0, 0, 0, 3}
	if flags&0x04 != 0 {
		b = append(b, 4, 0, 'X', 'T', 0, 0)
	}
	if flags&0x08 != 0 {
		b = append(append(b, name...), 0)
	}
	if flags&0x10 != 0 {
		b = append(append(b, comment...), 0)
	}
	if flags&0x02 != 0 {
		b = append(b, 0x12, 0x34)
	}
	b = append(b, 0x03, 0x00)
	return append(b, make([]byte, 8)...)
}
