package sqlitefile_test

import (
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// encodeVarint is the test's own encoder (not shared with the builder): the
// engine format is big-endian groups of 7 bits with a continuation bit, and at
// 9 bytes the last byte carries 8 bits.
func encodeVarint(v uint64) []byte {
	if v>>56 != 0 {
		out := make([]byte, 9)
		out[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return out
	}
	var rev []byte
	for {
		rev = append(rev, byte(v&0x7f))
		v >>= 7
		if v == 0 {
			break
		}
	}
	out := make([]byte, len(rev))
	for i, b := range rev {
		if i != 0 {
			b |= 0x80
		}
		out[len(rev)-1-i] = b
	}
	return out
}

var varintVectors = []struct {
	name string
	v    uint64
	b    []byte
}{
	{"zero", 0, []byte{0x00}},
	{"0x7f", 0x7f, []byte{0x7f}},
	{"0x80", 0x80, []byte{0x81, 0x00}},
	{"0x3fff", 0x3fff, []byte{0xff, 0x7f}},
	{"0x4000", 0x4000, []byte{0x81, 0x80, 0x00}},
	{"1<<56-1", 1<<56 - 1, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}},
	{"1<<56", 1 << 56, []byte{0x80, 0xc0, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00}},
	{"maxuint64", math.MaxUint64, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	{"minus-two", 0xfffffffffffffffe, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}},
}

func TestVarintGolden(t *testing.T) {
	for _, tc := range varintVectors {
		t.Run(tc.name, func(t *testing.T) {
			if enc := encodeVarint(tc.v); string(enc) != string(tc.b) {
				t.Fatalf("test encoder disagrees with golden bytes: % x vs % x", enc, tc.b)
			}
			v, n := sqlitefile.GetVarint(tc.b)
			if v != tc.v || n != len(tc.b) {
				t.Fatalf("GetVarint(% x) = %#x, %d; want %#x, %d", tc.b, v, n, tc.v, len(tc.b))
			}
		})
	}
	// Stored negative numbers are the two's complement 64-bit pattern.
	v, n := sqlitefile.GetVarint(encodeVarint(math.MaxUint64))
	if int64(v) != -1 || n != 9 {
		t.Fatalf("-1 decoded as %d, %d", int64(v), n)
	}
}

func TestVarintTruncated(t *testing.T) {
	for _, tc := range varintVectors {
		for k := 0; k < len(tc.b); k++ {
			if v, n := sqlitefile.GetVarint(tc.b[:k]); n != 0 || v != 0 {
				t.Errorf("%s: prefix of %d bytes returned %#x, %d; want n == 0", tc.name, k, v, n)
			}
		}
	}
	if _, n := sqlitefile.GetVarint(nil); n != 0 {
		t.Fatalf("nil input: n = %d", n)
	}
}

func TestVarintIgnoresTrailingBytes(t *testing.T) {
	for _, tc := range varintVectors {
		in := append(append([]byte{}, tc.b...), 0xff, 0x80, 0x00, 0x7f)
		v, n := sqlitefile.GetVarint(in)
		if v != tc.v || n != len(tc.b) {
			t.Errorf("%s: with trailing bytes got %#x, %d; want %#x, %d", tc.name, v, n, tc.v, len(tc.b))
		}
	}
}

func TestVarintRoundTripSweep(t *testing.T) {
	for shift := 0; shift < 64; shift++ {
		for _, d := range []uint64{0, 1, 2} {
			for _, base := range []uint64{1 << shift, 1<<shift - 1} {
				v := base + d
				enc := encodeVarint(v)
				got, n := sqlitefile.GetVarint(enc)
				if got != v || n != len(enc) {
					t.Fatalf("%#x: got %#x, %d for % x", v, got, n, enc)
				}
			}
		}
	}
}
