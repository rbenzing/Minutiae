package plist

import (
	"bytes"
	"fmt"
	"math"
	"unicode/utf16"
	"unicode/utf8"
)

// Cocoa-epoch seconds of the first and the one-past-last representable instant: 0001-01-01
// and 10000-01-01 (UTC), the years a time.Time built from a plist date may have.
const (
	cocoaToUnix  = 978307200
	dateFirst    = -62135596800 - cocoaToUnix
	dateEndAfter = 253402300800 - cocoaToUnix
)

func dateInRange(secs float64) bool {
	return !math.IsNaN(secs) && secs >= dateFirst && secs < dateEndAfter
}

// normalizeBinary normalizes the library's result lib for object i of a validated binary
// plist, walking the document's own structure beside it so that what the library alters
// (a lone UTF-16 surrogate becomes U+FFFD, an out-of-range date becomes a wrong time.Time)
// is read from the stored bytes instead. The recursion depth is bounded by the validator.
func (w *bplistWalker) normalizeBinary(i uint64, lib any) (any, error) {
	off := w.uint(w.tableOff+i*w.offSize, w.offSize)
	marker := w.b[off]
	switch marker >> 4 {
	case 0xA, 0xC:
		l, ok := lib.([]any)
		count, hdr, err := w.header(off)
		if err != nil {
			return nil, err
		}
		if !ok || uint64(len(l)) != count {
			return nil, w.mismatch()
		}
		out := make([]any, len(l))
		for k := range l {
			ref := w.uint(off+hdr+uint64(k)*w.refSize, w.refSize)
			if out[k], err = w.normalizeBinary(ref, l[k]); err != nil {
				return nil, err
			}
		}
		return out, nil
	case 0xD:
		m, ok := lib.(map[string]any)
		count, hdr, err := w.header(off)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, w.mismatch()
		}
		last := make(map[string]uint64, len(m)) // a duplicate key keeps the last value
		for k := range count {
			kref := w.uint(off+hdr+k*w.refSize, w.refSize)
			key, err := w.keyString(kref)
			if err != nil {
				return nil, err
			}
			last[key] = w.uint(off+hdr+(count+k)*w.refSize, w.refSize)
		}
		if len(last) != len(m) {
			return nil, w.mismatch()
		}
		out := make(map[string]any, len(m))
		for key, ref := range last {
			lv, ok := m[key]
			if !ok {
				return nil, w.mismatch()
			}
			if out[key], err = w.normalizeBinary(ref, lv); err != nil {
				return nil, err
			}
		}
		return out, nil
	case 0x5, 0x6:
		s, raw, err := w.text(off)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			return *raw, nil
		}
		if ls, ok := lib.(string); !ok || ls != s {
			return nil, w.mismatch()
		}
		return s, nil
	case 0x3:
		secs := math.Float64frombits(w.uint(off+1, 8))
		if !dateInRange(secs) || math.IsInf(secs, 0) {
			return RawDate{Seconds: secs}, nil
		}
	}
	return normalizer{}.value(lib)
}

func (w *bplistWalker) mismatch() error {
	return fmt.Errorf("%w: decoding library result does not match the document structure", ErrInternal)
}

// text reads the string object at off. It returns the Go string when the text is valid, or
// the RawString that keeps the stored bytes when it is not valid UTF-8 or holds a lone
// surrogate.
func (w *bplistWalker) text(off uint64) (string, *RawString, error) {
	count, hdr, err := w.header(off)
	if err != nil {
		return "", nil, err
	}
	if w.b[off]>>4 == 0x5 {
		raw := w.b[off+hdr : off+hdr+count]
		if !utf8.Valid(raw) {
			return "", &RawString{Bytes: bytes.Clone(raw)}, nil
		}
		return string(raw), nil, nil
	}
	raw := w.b[off+hdr : off+hdr+count*2]
	units := make([]uint16, count)
	for k := range units {
		units[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
	}
	if hasLoneSurrogate(units) {
		return "", &RawString{Bytes: bytes.Clone(raw), UTF16: true}, nil
	}
	return string(utf16.Decode(units)), nil, nil
}

// keyString returns the text of the dictionary key object ref, refusing a key that is not a
// valid string.
func (w *bplistWalker) keyString(ref uint64) (string, error) {
	off := w.uint(w.tableOff+ref*w.offSize, w.offSize)
	if t := w.b[off] >> 4; t != 0x5 && t != 0x6 {
		return "", fmt.Errorf("%w: dictionary key is not a string", ErrUnsupported)
	}
	s, raw, err := w.text(off)
	if err != nil {
		return "", err
	}
	if raw != nil {
		return "", fmt.Errorf("%w: dictionary key is not valid text", ErrUnsupported)
	}
	return s, nil
}

func hasLoneSurrogate(u []uint16) bool {
	for i := 0; i < len(u); i++ {
		switch {
		case u[i] >= 0xD800 && u[i] < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			i++
		case u[i] >= 0xD800 && u[i] < 0xE000:
			return true
		}
	}
	return false
}
