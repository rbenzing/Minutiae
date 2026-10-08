package plist

import "encoding/binary"

// The builders below are copied from internal/ios/mb2/hardening_test.go (test helpers
// cannot be imported across packages).

// nestedPlist builds a binary plist whose object i is an array of fan references to
// object i-1 (object 0 is the integer 0). The file is tiny but expands to fan^n nodes.
func nestedPlist(n, fan int) []byte {
	b := []byte("bplist00")
	offsets := []byte{byte(len(b))}
	b = append(b, 0x10, 0x00)
	for i := 1; i <= n; i++ {
		offsets = append(offsets, byte(len(b)))
		b = append(b, 0xA0|byte(fan))
		for range fan {
			b = append(b, byte(i-1))
		}
	}
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(n+1))
	binary.BigEndian.PutUint64(trailer[16:], uint64(n))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

// rawPlist assembles a binary plist from obj (placed at offset 8) and a one-byte offset
// table; object 0 is the top object. The reference size is 3.
func rawPlist(obj, offsets []byte) []byte {
	b := append([]byte("bplist00"), obj...)
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 3
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(offsets)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

// bigCount returns a marker for type nibble typ followed by an 8-byte length.
func bigCount(typ byte, n uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{typ<<4 | 0x0f, 0x13}, n)
}
