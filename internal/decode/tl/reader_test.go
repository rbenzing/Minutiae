package tl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

func mustReader(t *testing.T, b []byte) *Reader {
	t.Helper()
	r, err := NewReader(b)
	if err != nil {
		t.Fatalf("NewReader(%d bytes): %v", len(b), err)
	}
	return r
}

// le64 is the little-endian encoding of v.
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// le32 is the little-endian encoding of v.
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// Spec section 6.5: int32/int64 little-endian (SD:121-153), double = IEEE-754 bits of an LE
// int64 (SD:318-327), bool boxed by the two IDs only (NBB:447-462).
func TestTLReaderPrimitivesAreLittleEndian(t *testing.T) {
	if v, err := mustReader(t, []byte{1, 0, 0, 0}).Int32(); err != nil || v != 1 {
		t.Errorf("Int32(01 00 00 00) = %d, %v; want 1", v, err)
	}
	if v, err := mustReader(t, []byte{0xff, 0xff, 0xff, 0xff}).Int32(); err != nil || v != -1 {
		t.Errorf("Int32(ff ff ff ff) = %d, %v; want -1", v, err)
	}
	if v, err := mustReader(t, []byte{0xff, 0xff, 0xff, 0xff}).Uint32(); err != nil || v != 4294967295 {
		t.Errorf("Uint32(ff ff ff ff) = %d, %v; want 4294967295", v, err)
	}
	if v, err := mustReader(t, []byte{8, 7, 6, 5, 4, 3, 2, 1}).Int64(); err != nil || v != 0x0102030405060708 {
		t.Errorf("Int64 = %#x, %v; want 0x0102030405060708", v, err)
	}
	if v, err := mustReader(t, le64(math.Float64bits(1.5))).Double(); err != nil || v != 1.5 {
		t.Errorf("Double = %v, %v; want 1.5", v, err)
	}
	if v, err := mustReader(t, []byte{0xb5, 0x75, 0x72, 0x99}).Bool(); err != nil || !v {
		t.Errorf("Bool(b5 75 72 99) = %v, %v; want true", v, err)
	}
	if v, err := mustReader(t, []byte{0x37, 0x97, 0x79, 0xbc}).Bool(); err != nil || v {
		t.Errorf("Bool(37 97 79 bc) = %v, %v; want false", v, err)
	}
	r := mustReader(t, cat([]byte{1, 0, 0, 0}, []byte{8, 7, 6, 5, 4, 3, 2, 1}))
	_, _ = r.Int32()
	_, _ = r.Int64()
	if r.Offset() != 12 || r.Remaining() != 0 || r.Steps() != 2 {
		t.Errorf("after Int32, Int64: offset %d, remaining %d, steps %d; want 12, 0, 2", r.Offset(), r.Remaining(), r.Steps())
	}
}

// stringCase is one framing vector of spec section 6.5 (SD:227-265, NBB:234-312,526-596).
type stringCase struct {
	name  string
	in    []byte
	want  []byte
	off   int
	flags []string
}

func stringCases() []stringCase {
	short253 := cat([]byte{253}, bytes.Repeat([]byte{'x'}, 253), []byte{0, 0})
	long254 := cat([]byte{0xfe, 0xfe, 0, 0}, bytes.Repeat([]byte{'y'}, 254), []byte{0, 0})
	prefix255 := cat([]byte{0xff, 0xfe, 0, 0}, bytes.Repeat([]byte{'z'}, 254), []byte{0, 0})
	return []stringCase{
		{"short", []byte{3, 'a', 'b', 'c'}, []byte("abc"), 4, nil},
		{"empty", []byte{0, 0, 0, 0}, []byte{}, 4, nil},
		{"short-253", short253, bytes.Repeat([]byte{'x'}, 253), 256, nil},
		{"long-254", long254, bytes.Repeat([]byte{'y'}, 254), 260, nil},
		{"prefix-255", []byte{0xff, 3, 0, 0, 'a', 'b', 'c', 0}, []byte("abc"), 8, []string{FlagLengthPrefix255, FlagNonCanonicalLength}},
		{"padding-nonzero", []byte{1, 'a', 0, 1}, []byte("a"), 4, []string{FlagPaddingNonzero}},
		// The two framing flags apart (Task 2 review M1): the 0xfe long form of a short length,
		// and the 0xff prefix of a canonical long length.
		{"non-canonical-fe", []byte{0xfe, 3, 0, 0, 'a', 'b', 'c', 0}, []byte("abc"), 8, []string{FlagNonCanonicalLength}},
		{"prefix-255-long", prefix255, bytes.Repeat([]byte{'z'}, 254), 260, []string{FlagLengthPrefix255}},
	}
}

func TestTLReaderStringFraming(t *testing.T) {
	for _, c := range stringCases() {
		for _, method := range []string{"Bytes", "String"} {
			r := mustReader(t, c.in)
			var got []byte
			var err error
			if method == "Bytes" {
				got, err = r.Bytes()
			} else {
				got, err = r.String()
			}
			if err != nil || !bytes.Equal(got, c.want) || got == nil {
				t.Errorf("%s %s = %q, %v; want %q", c.name, method, got, err, c.want)
				continue
			}
			if r.Offset() != c.off {
				t.Errorf("%s %s: offset %d, want %d", c.name, method, r.Offset(), c.off)
			}
			if !slices.Equal(r.Flags(), c.flags) {
				t.Errorf("%s %s: flags %v, want %v", c.name, method, r.Flags(), c.flags)
			}
		}
	}
	// The returned slice aliases the input: never a copy (spec section 7.1).
	in := []byte{3, 'a', 'b', 'c'}
	got, err := mustReader(t, in).String()
	if err != nil || &got[0] != &in[1] {
		t.Errorf("String does not alias the input: %v", err)
	}
	// Flags are sorted and distinct over several reads.
	r := mustReader(t, cat([]byte{1, 'a', 0, 1}, []byte{0xff, 1, 0, 0, 'b', 0, 0, 0}, []byte{1, 'c', 2, 0}))
	for range 3 {
		if _, err := r.Bytes(); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{FlagLengthPrefix255, FlagNonCanonicalLength, FlagPaddingNonzero}; !slices.Equal(r.Flags(), want) {
		t.Errorf("flags %v, want %v", r.Flags(), want)
	}
}

// boundCase is one hostile vector of spec section 7.1 and section 12.
type boundCase struct {
	name string
	in   []byte
	op   func(r *Reader) error
	want error
	off  int  // the offset where the failed read began
	pre  int  // primitive Int32 reads made before the failing one
	fop  byte // the FuzzTLReader op byte of op (fuzz_test.go)
}

func opInt32(r *Reader) error  { _, err := r.Int32(); return err }
func opBool(r *Reader) error   { _, err := r.Bool(); return err }
func opString(r *Reader) error { _, err := r.String(); return err }
func opVector(minElem int) func(*Reader) error {
	return func(r *Reader) error { _, err := r.VectorHeader(minElem); return err }
}

func vec(count int32, tail int) []byte {
	return cat(le32(IDVector), le32(uint32(count)), make([]byte, tail))
}

func boundCases() []boundCase {
	return []boundCase{
		{"int32-3-bytes", []byte{1, 2, 3}, opInt32, ErrTruncated, 0, 0, 0},
		{"int32-after-one", []byte{1, 0, 0, 0, 2, 0}, opInt32, ErrTruncated, 4, 1, 0},
		{"bool-not-an-id", []byte{1, 0, 0, 0}, opBool, ErrBadBool, 0, 0, 3},
		{"bool-truncated", []byte{0xb5, 0x75}, opBool, ErrTruncated, 0, 0, 3},
		{"vector-bad-id", []byte{0x16, 0xc4, 0xb5, 0x1c, 0, 0, 0, 0}, opVector(4), ErrBadVector, 0, 0, 38},
		{"vector-count-missing", le32(IDVector), opVector(4), ErrTruncated, 0, 0, 38},
		{"vector-count-negative", vec(-1, 0), opVector(4), ErrVectorCount, 0, 0, 38},
		{"vector-count-above-max", vec(MaxVector+1, 0), opVector(0), ErrVectorCount, 0, 0, 6},
		{"vector-count-past-end", vec(5, 8), opVector(4), ErrVectorCount, 0, 0, 38},
		{"vector-count-2^31-1", vec(math.MaxInt32, 8), opVector(4), ErrVectorCount, 0, 0, 38},
		{"string-long-past-end", []byte{0xfe, 0xff, 0xff, 0xff}, opString, ErrStringLength, 0, 0, 5},
		{"string-short-past-end", []byte{5, 'a'}, opString, ErrStringLength, 0, 0, 5},
		{"string-pad-missing", []byte{2, 'a', 'b'}, opString, ErrTruncated, 0, 0, 5},
		{"string-empty-input", []byte{}, opString, ErrTruncated, 0, 0, 5},
		{"string-long-prefix-cut", []byte{0xfe, 1, 0}, opString, ErrTruncated, 0, 0, 5},
		{"string-after-one", []byte{1, 0, 0, 0, 9, 'a'}, opString, ErrStringLength, 4, 1, 5},
	}
}

// Spec section 7.1: each bound is a typed *Error with its sentinel and the offset where the
// read began; a failed read consumes nothing.
func TestTLReaderBoundsAreTypedErrors(t *testing.T) {
	for _, c := range boundCases() {
		r := mustReader(t, c.in)
		for range c.pre {
			if _, err := r.Int32(); err != nil {
				t.Fatalf("%s: pre-read: %v", c.name, err)
			}
		}
		err := c.op(r)
		checkBoundError(t, c.name, err, c.want, c.off)
		if r.Offset() != c.off {
			t.Errorf("%s: a failed read moved the offset to %d, want %d", c.name, r.Offset(), c.off)
		}
		// The FuzzTLReader seed of this case (pre zero ops, then fop) reaches the same error.
		r = mustReader(t, c.in)
		var ferr error
		for i, op := range append(make([]byte, c.pre), c.fop) {
			ferr = fuzzOp(t, r, i, op)
		}
		checkBoundError(t, c.name+" fuzz seed", ferr, c.want, c.off)
	}
	// "03 61 62 63" needs no padding: it is not a truncation.
	if _, err := mustReader(t, []byte{3, 'a', 'b', 'c'}).String(); err != nil {
		t.Errorf("03 61 62 63: %v", err)
	}
	// A blob above MaxBlob is refused before any read.
	_, err := NewReader(make([]byte, MaxBlob+1))
	checkBoundError(t, "MaxBlob+1", err, ErrTooLarge, 0)
	if _, err := NewReader(make([]byte, MaxBlob)); err != nil {
		t.Errorf("NewReader(MaxBlob): %v", err)
	}
	// Step MaxSteps+1 is ErrSteps, at the offset where it began.
	r := mustReader(t, make([]byte, 4*(MaxSteps+1)))
	for i := range MaxSteps {
		if _, err := r.Int32(); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	_, err = r.Int32()
	checkBoundError(t, "MaxSteps+1", err, ErrSteps, 4*MaxSteps)
	if r.Steps() != MaxSteps || r.Offset() != 4*MaxSteps {
		t.Errorf("after ErrSteps: steps %d, offset %d; want %d, %d", r.Steps(), r.Offset(), MaxSteps, 4*MaxSteps)
	}
	// The Error text names the sentinel, the offset and the path.
	e := &Error{Err: ErrTruncated, Offset: 12, Path: "Message.media"}
	if got, want := e.Error(), `tl: truncated at offset 12 (path "Message.media")`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func checkBoundError(t *testing.T, name string, err, want error, off int) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err %v, want %v", name, err, want)
		return
	}
	var te *Error
	if !errors.As(err, &te) {
		t.Errorf("%s: %v is not an *Error", name, err)
		return
	}
	if te.Offset != off {
		t.Errorf("%s: offset %d, want %d", name, te.Offset, off)
	}
}

// A negative minimum element size is a caller's bug: ErrInternal, never a pass.
func TestTLReaderVectorHeader(t *testing.T) {
	r := mustReader(t, vec(2, 8))
	n, err := r.VectorHeader(4)
	if err != nil || n != 2 || r.Offset() != 8 {
		t.Errorf("VectorHeader = %d, %v, offset %d; want 2, nil, 8", n, err, r.Offset())
	}
	r = mustReader(t, vec(MaxVector, 0))
	if n, err := r.VectorHeader(0); err != nil || n != MaxVector {
		t.Errorf("VectorHeader(MaxVector, minElem 0) = %d, %v", n, err)
	}
	_, err = mustReader(t, vec(1, 4)).VectorHeader(-1)
	checkBoundError(t, "minElem -1", err, ErrInternal, 0)
	_, err = mustReader(t, vec(2, 8)).VectorHeader(math.MaxInt)
	checkBoundError(t, "minElem MaxInt", err, ErrVectorCount, 0)
}

// A failed read charges its step and states no flag (Task 2 review M4): the 0xff prefix is
// stated only by a read that succeeds.
func TestTLReaderFailedReadChargesAStep(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want error
	}{
		{[]byte{0xff, 3, 0, 0, 'a'}, ErrStringLength},
		{[]byte{0xff, 3, 0, 0, 'a', 'b', 'c'}, ErrTruncated}, // the padding byte is missing
	} {
		r := mustReader(t, c.in)
		_, err := r.String()
		checkBoundError(t, fmt.Sprintf("% x", c.in), err, c.want, 0)
		if r.Steps() != 1 || len(r.Flags()) != 0 || r.Offset() != 0 {
			t.Errorf("% x: steps %d, flags %v, offset %d; want 1, none, 0", c.in, r.Steps(), r.Flags(), r.Offset())
		}
	}
}
