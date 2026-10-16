package evidence

// SQLiteVarint decodes SQLite's variable-length integer (1 to 9 bytes, big endian; the high bit of
// each of the first eight bytes says another follows; the ninth byte contributes all 8 bits). n is 0
// when b is truncated. It is the encoding FTS5 uses inside its data records.
func SQLiteVarint(b []byte) (v uint64, n int) {
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return v<<8 | uint64(b[8]), 9
}
