package filesys

import (
	"bytes"
	"testing"
)

func TestDecodeRawNameAliasRoundTrip(t *testing.T) {
	for _, raw := range [][]byte{{0xff}, {0xfb, 0xff, 0xfe}, []byte("a\x00b"), []byte("abc"), {0x00}, bytes.Repeat([]byte{0xfe, 0x01}, 100)} {
		s := RawNameAlias(raw)
		got, ok := DecodeRawNameAlias(s)
		if !ok || !bytes.Equal(got, raw) {
			t.Errorf("DecodeRawNameAlias(%q) = %x, %v; want %x", s, got, ok, raw)
		}
	}
}

func TestDecodeRawNameAliasRejects(t *testing.T) {
	for _, c := range []struct{ name, s string }{
		{"non-canonical trailing bits", "~raw~_x"},
		{"embedded LF", "~raw~_\nw"},
		{"embedded CR", "~raw~_\rw"},
		{"padding", "~raw~_w=="},
		{"standard alphabet plus", "~raw~+__+"},
		{"standard alphabet slash", "~raw~/w"},
		{"empty payload", "~raw~"},
		{"wrong prefix", "~enc~_w"},
		{"no prefix", "_w"},
		{"upper-case prefix", "~RAW~_w"},
		{"impossible length", "~raw~A"},
	} {
		if got, ok := DecodeRawNameAlias(c.s); ok {
			t.Errorf("%s: DecodeRawNameAlias(%q) = %x, true; want false", c.name, c.s, got)
		}
	}
}
