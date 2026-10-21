package androidmms

// trimPartialRune drops the end of b that may be a cut character: its continuation
// bytes and one lead byte. It is only applied to a buffer that was cut anyway.
func trimPartialRune(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] >= 0xC0 {
		b = b[:len(b)-1]
	}
	return b
}

// joinText joins text parts with "\n", keeping at most 2*maxBody bytes, and reports
// the full length. The cut is made on a character boundary, so the cut itself is
// never reported as invalid UTF-8.
func joinText(parts [][]byte) (joined []byte, total int64) {
	limit := 2 * maxBody
	for i, p := range parts {
		sep := 0
		if i > 0 {
			sep = 1
		}
		total += int64(sep + len(p))
		if len(joined) >= limit {
			continue
		}
		if sep == 1 {
			joined = append(joined, '\n')
		}
		if room := limit - len(joined); len(p) > room {
			p = p[:room]
		}
		joined = append(joined, p...)
	}
	if total > int64(len(joined)) {
		joined = trimPartialRune(joined)
	}
	return joined, total
}

// asciiLower lower-cases ASCII letters of b.
func asciiLower(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
