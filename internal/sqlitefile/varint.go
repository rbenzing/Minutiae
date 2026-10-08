package sqlitefile

// GetVarint decodes the SQLite variable-length integer at the start of b: up
// to nine bytes, big-endian groups of seven bits with a continuation bit,
// the ninth byte (if reached) contributing all eight of its bits. Negative
// numbers are stored as their 64-bit two's complement pattern, so the result
// is returned as a uint64. n is the number of bytes used; n == 0 means b ends
// inside the varint (v is then 0). Trailing bytes are ignored.
func GetVarint(b []byte) (v uint64, n int) {
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		c := b[i]
		v = v<<7 | uint64(c&0x7f)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return v<<8 | uint64(b[8]), 9
}
