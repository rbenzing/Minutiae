package tl

import (
	"errors"
	"testing"
)

// fuzzOps is the op alphabet of FuzzTLReader: one op byte selects one method (modulo).
const fuzzOps = 8

// FuzzTLReader (spec section 12): an op string drives the Reader over the input. It never
// panics; Offset()+Remaining() equals the input length after every call; the offset never
// decreases; every error is an *Error whose Err is one of the sentinels.
func FuzzTLReader(f *testing.F) {
	for _, c := range stringCases() {
		f.Add([]byte{4, 5}, c.in)
	}
	for _, c := range boundCases() {
		f.Add([]byte{0, 6, 4, 3}, c.in)
	}
	f.Add([]byte{0, 1, 2, 3}, []byte{1, 0, 0, 0, 8, 7, 6, 5, 4, 3, 2, 1, 0xb5, 0x75, 0x72, 0x99})
	f.Add([]byte{6, 0, 0}, vec(2, 8))
	sentinels := []error{
		ErrTruncated, ErrBadBool, ErrBadVector, ErrVectorCount, ErrStringLength,
		ErrDepth, ErrSteps, ErrTooLarge, ErrNoBudget, ErrInternal,
	}
	f.Fuzz(func(t *testing.T, ops, in []byte) {
		r, err := NewReader(in)
		if err != nil {
			if len(in) <= MaxBlob {
				t.Fatalf("NewReader(%d bytes): %v", len(in), err)
			}
			return
		}
		prev := 0
		for i, op := range ops {
			var err error
			switch op % fuzzOps {
			case 0:
				_, err = r.Int32()
			case 1:
				_, err = r.Int64()
			case 2:
				_, err = r.Double()
			case 3:
				_, err = r.Bool()
			case 4:
				var b []byte
				b, err = r.Bytes()
				if err == nil && len(b) > MaxString {
					t.Fatalf("op %d: Bytes returned %d bytes", i, len(b))
				}
			case 5:
				_, err = r.String()
			case 6:
				var n int
				n, err = r.VectorHeader(int(op>>3) % 9)
				if err == nil && (n < 0 || n > MaxVector) {
					t.Fatalf("op %d: VectorHeader = %d", i, n)
				}
			case 7:
				_, err = r.Uint32()
			}
			if r.Offset()+r.Remaining() != len(in) {
				t.Fatalf("op %d: offset %d + remaining %d != %d", i, r.Offset(), r.Remaining(), len(in))
			}
			if r.Offset() < prev {
				t.Fatalf("op %d: offset went back from %d to %d", i, prev, r.Offset())
			}
			if err != nil {
				var te *Error
				if !errors.As(err, &te) {
					t.Fatalf("op %d: %v is not an *Error", i, err)
				}
				known := false
				for _, s := range sentinels {
					known = known || te.Err == s
				}
				if !known {
					t.Fatalf("op %d: %v wraps no sentinel", i, err)
				}
				if te.Offset != prev {
					t.Fatalf("op %d: error offset %d, read began at %d", i, te.Offset, prev)
				}
			}
			prev = r.Offset()
		}
	})
}
