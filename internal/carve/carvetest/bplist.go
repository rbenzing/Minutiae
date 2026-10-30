package carvetest

// BPlist builds a binary property list: the "bplist00" header, the objects,
// the offset table (offsetSize bytes per entry, big-endian) and the 32-byte
// trailer. refSize is only recorded in the trailer; the objects carry their
// own references.
func BPlist(objects [][]byte, top int, offsetSize, refSize byte) []byte {
	b := []byte("bplist00")
	offs := make([]uint64, len(objects))
	for i, o := range objects {
		offs[i] = uint64(len(b))
		b = append(b, o...)
	}
	table := uint64(len(b))
	for _, off := range offs {
		b = appendBE(b, off, int(offsetSize))
	}
	b = append(b, make([]byte, 6)...) // unused (5) and sort version (1)
	b = append(b, offsetSize, refSize)
	b = appendBE(b, uint64(len(objects)), 8)
	b = appendBE(b, uint64(top), 8)
	return appendBE(b, table, 8)
}

// BPlistASCII returns an ASCII string object (marker 0x5n, n < 15).
func BPlistASCII(s string) []byte { return append([]byte{0x50 | byte(len(s))}, s...) }

// BPlistInt1 returns a one-byte integer object.
func BPlistInt1(v byte) []byte { return []byte{0x10, v} }

// BPlistDict returns a dictionary object with one-byte references (n < 15):
// the key references, then the value references.
func BPlistDict(keys, vals []byte) []byte {
	return append(append([]byte{0xD0 | byte(len(keys))}, keys...), vals...)
}

func appendBE(b []byte, v uint64, n int) []byte {
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*uint(i))))
	}
	return b
}
