package plist

import (
	"bytes"
	"encoding/binary"
)

// The builders below are copied from internal/ios/mb2/hardening_test.go (test helpers
// cannot be imported across packages).

// nestedPlist builds a binary plist whose object i is an array of fan references to
// object i-1 (object 0 is the integer 0). The file is tiny but expands to fan^n nodes.
func nestedPlist(n, fan int) []byte { return nestedTyped(n, fan, 0xA0) }

// nestedTyped is nestedPlist with the container marker nibble chosen by the caller.
func nestedTyped(n, fan int, marker byte) []byte {
	b := []byte("bplist00")
	offsets := []byte{byte(len(b))}
	b = append(b, 0x10, 0x00)
	for i := 1; i <= n; i++ {
		offsets = append(offsets, byte(len(b)))
		b = append(b, marker|byte(fan))
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

// objectsPlist assembles a binary plist from whole objects (object 0 is the top object),
// with one-byte references and offsets; it fails the test when the file outgrows one byte.
func objectsPlist(objs ...[]byte) []byte {
	b := []byte("bplist00")
	var offsets []byte
	for _, o := range objs {
		offsets = append(offsets, byte(len(b)))
		b = append(b, o...)
	}
	tableOff := len(b)
	if tableOff > 255 {
		panic("objectsPlist: offsets need more than one byte")
	}
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 1, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(objs)))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(b, trailer...)
}

// refArray is an array object holding the given one-byte references.
func refArray(refs ...byte) []byte { return append([]byte{0xA0 | byte(len(refs))}, refs...) }

// dataObj is a data object of n bytes (n < 256).
func dataObj(n int) []byte {
	body := bytes.Repeat([]byte{7}, n)
	if n < 15 {
		return append([]byte{0x40 | byte(n)}, body...)
	}
	return append([]byte{0x4f, 0x10, byte(n)}, body...)
}
