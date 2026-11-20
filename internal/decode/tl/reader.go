package tl

import (
	"encoding/binary"
	"math"
)

// Constructor IDs the Reader itself decides (doc.go: NBB:447-462, VEC:8).
const (
	IDBoolTrue  uint32 = 0x997275b5
	IDBoolFalse uint32 = 0xbc799737
	IDVector    uint32 = 0x1cb5c415
)

// Flags: facts about the encoding that are not errors (spec section 7.1). Flags returns them
// sorted; the constants are in that order.
const (
	FlagLengthPrefix255    = "length-prefix-255"
	FlagNonCanonicalLength = "non-canonical-length"
	FlagPaddingNonzero     = "padding-nonzero"
	FlagUnknownFlagBits    = "unknown-flag-bits"
)

// flag bits of Reader.flags, in the sorted order of their names.
const (
	flagLengthPrefix255 uint8 = 1 << iota
	flagNonCanonicalLength
	flagPaddingNonzero
	flagUnknownFlagBits
)

// shortMax is the largest length of the 1-byte prefix; a first byte above it starts the long
// form (254, and 255 as the app's reader accepts it, NBB:531).
const shortMax = 253

// Reader reads TL primitives from a byte slice. A failed read consumes nothing and returns an
// *Error carrying the offset where it began; every reading call is one step of MaxSteps.
type Reader struct {
	b     []byte
	off   int
	steps int
	flags uint8
}

// NewReader returns a Reader over b; a b above MaxBlob is ErrTooLarge before any read.
func NewReader(b []byte) (*Reader, error) {
	if len(b) > MaxBlob {
		return nil, &Error{Err: ErrTooLarge}
	}
	return &Reader{b: b}, nil
}

// fail is the error of a read that began at the current offset.
func (r *Reader) fail(err error) error { return &Error{Err: err, Offset: r.off} }

// step charges one read to the work budget.
func (r *Reader) step() error {
	if r.steps >= MaxSteps {
		return r.fail(ErrSteps)
	}
	r.steps++
	return nil
}

// fixed charges one step and returns the next n bytes without consuming them.
func (r *Reader) fixed(n int) ([]byte, error) {
	if err := r.step(); err != nil {
		return nil, err
	}
	if r.Remaining() < n {
		return nil, r.fail(ErrTruncated)
	}
	return r.b[r.off : r.off+n], nil
}

// Int32 reads a little-endian int32 (SD:121-153).
func (r *Reader) Int32() (int32, error) {
	v, err := r.Uint32()
	return int32(v), err //nolint:gosec // the TL int32 is the two's-complement reading of the bits
}

// Uint32 reads a little-endian uint32 (a constructor ID or a flags word).
func (r *Reader) Uint32() (uint32, error) {
	p, err := r.fixed(4)
	if err != nil {
		return 0, err
	}
	r.off += 4
	return binary.LittleEndian.Uint32(p), nil
}

// Int64 reads a little-endian int64.
func (r *Reader) Int64() (int64, error) {
	p, err := r.fixed(8)
	if err != nil {
		return 0, err
	}
	r.off += 8
	return int64(binary.LittleEndian.Uint64(p)), nil //nolint:gosec // two's-complement reading of the bits
}

// Double reads the IEEE-754 bits of a little-endian int64 (SD:318-327).
func (r *Reader) Double() (float64, error) {
	p, err := r.fixed(8)
	if err != nil {
		return 0, err
	}
	r.off += 8
	return math.Float64frombits(binary.LittleEndian.Uint64(p)), nil
}

// Bool reads a boxed bool: IDBoolTrue or IDBoolFalse, any other value ErrBadBool (NBB:447-462).
func (r *Reader) Bool() (bool, error) {
	p, err := r.fixed(4)
	if err != nil {
		return false, err
	}
	var v bool
	switch binary.LittleEndian.Uint32(p) {
	case IDBoolTrue:
		v = true
	case IDBoolFalse:
	default:
		return false, r.fail(ErrBadBool)
	}
	r.off += 4
	return v, nil
}

// Bytes reads a TL bytes value and returns a sub-slice of the input, never a copy (its
// capacity ends with it, so an append by the caller cannot write into the input).
func (r *Reader) Bytes() ([]byte, error) {
	if err := r.step(); err != nil {
		return nil, err
	}
	rem := r.Remaining()
	if rem < 1 {
		return nil, r.fail(ErrTruncated)
	}
	var flags uint8
	first := r.b[r.off]
	prefix, n := 1, int(first)
	if first > shortMax {
		if rem < 4 {
			return nil, r.fail(ErrTruncated)
		}
		prefix = 4
		n = int(r.b[r.off+1]) | int(r.b[r.off+2])<<8 | int(r.b[r.off+3])<<16
		if first == 0xff {
			flags |= flagLengthPrefix255
		}
		if n <= shortMax {
			flags |= flagNonCanonicalLength
		}
	}
	if n > MaxString || n > rem-prefix {
		return nil, r.fail(ErrStringLength)
	}
	pad := (4 - (prefix+n)%4) % 4
	end := r.off + prefix + n
	if len(r.b)-end < pad {
		return nil, r.fail(ErrTruncated)
	}
	for _, c := range r.b[end : end+pad] {
		if c != 0 {
			flags |= flagPaddingNonzero
		}
	}
	data := r.b[r.off+prefix : end : end]
	r.off = end + pad
	r.flags |= flags
	return data, nil
}

// String reads a TL string: the framing of Bytes. The bytes are returned as stored; UTF-8
// validity is the caller's to state (CleanText).
func (r *Reader) String() ([]byte, error) { return r.Bytes() }

// VectorHeader reads a boxed vector's constructor (IDVector, else ErrBadVector) and count. A
// count below 0, above MaxVector, or whose count elements of at least minElem bytes each
// cannot fit the bytes after the header is ErrVectorCount, before any element is read.
func (r *Reader) VectorHeader(minElem int) (int, error) {
	if minElem < 0 {
		return 0, r.fail(ErrInternal)
	}
	p, err := r.fixed(4)
	if err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(p) != IDVector {
		return 0, r.fail(ErrBadVector)
	}
	if r.Remaining() < 8 {
		return 0, r.fail(ErrTruncated)
	}
	p = r.b[r.off : r.off+8]
	count := int(int32(binary.LittleEndian.Uint32(p[4:]))) //nolint:gosec // the TL count is a signed int32
	rem := r.Remaining() - 8
	if count < 0 || count > MaxVector || (minElem > 0 && count > rem/minElem) {
		return 0, r.fail(ErrVectorCount)
	}
	r.off += 8
	return count, nil
}

// Offset is the offset of the next read.
func (r *Reader) Offset() int { return r.off }

// Remaining is the number of bytes after Offset.
func (r *Reader) Remaining() int { return len(r.b) - r.off }

// Steps is the number of reading calls charged so far.
func (r *Reader) Steps() int { return r.steps }

// Flags are the encoding facts met so far, sorted and distinct.
func (r *Reader) Flags() []string {
	var out []string
	for _, f := range [...]struct {
		bit  uint8
		name string
	}{
		{flagLengthPrefix255, FlagLengthPrefix255},
		{flagNonCanonicalLength, FlagNonCanonicalLength},
		{flagPaddingNonzero, FlagPaddingNonzero},
		{flagUnknownFlagBits, FlagUnknownFlagBits},
	} {
		if r.flags&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return out
}
