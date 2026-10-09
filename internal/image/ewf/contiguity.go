package ewf

// zlibStartsAt reports whether b begins like a zlib stream: method 8, a window
// of at most 32 KiB, valid header check bits, no preset dictionary, and room
// for the 2-byte header and the 4-byte Adler-32. Padding (zeros, a few bytes of
// alignment) does not, a following stored chunk does. decode uses it to tell a
// skipped stored chunk (a forged table) from padding after a chunk's stream.
func zlibStartsAt(b []byte) bool {
	if len(b) < 6 {
		return false
	}
	cmf, flg := b[0], b[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint(cmf)<<8|uint(flg))%31 == 0 && flg&0x20 == 0
}
