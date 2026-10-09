// Package typedstream extracts the message text from a legacy NSArchiver "typedstream"
// blob, the format of the iOS Messages attributedBody column.
//
// Only one thing is read: the bytes of the first NSString or NSMutableString object. The
// attribute runs, dictionaries and numbers around it are ignored. The text is returned
// verbatim: nothing is repaired, and invalid UTF-8 and NUL bytes are kept, so the text may be
// invalid UTF-8 and a caller must pass it through recordtypes/common.CleanText (or otherwise
// treat it as untrusted bytes) before storing or printing it.
//
// LIMITATION: the layout below is from knowledge of the legacy format, not from a specification
// or from a real blob; no real iOS attributedBody value was available when this was written.
// It is validated by builder streams written from the format description, so a real blob must
// be checked by hand before the extraction is relied on.
//
// Layout as implemented. Header: version byte 0x04, a length-prefixed (0x0B) ASCII
// "streamtyped", then a small integer (the system version, for example 0x81 0xE8 0x03 =
// 1000). Integers: a byte below 0x80 is the value; 0x81 is followed by a 2-byte
// little-endian value, 0x82 by 4 bytes, 0x83 by 8 bytes (refused for a length, ErrLimit).
// Extraction rule, the only one: within MaxScan bytes after the header find the first class name
// "NSString" or "NSMutableString" preceded by its length byte (the 0x84 class-record prefix
// is not required), then the first type-encoding string "+" (bytes 0x01 0x2B) after it, then
// the integer length, then exactly that many bytes. The first match wins and the search never
// looks for a better one, so text that itself contains the marker cannot redirect the
// extraction.
//
// A marker may START at any of the MaxScan bytes after the header (the scan window) and is
// then read in full. "No string object" (ok false, nil error) is a claim and is made only when
// the whole window was examined without finding a marker and the input does not end inside a
// marker that started in it (that is ErrTruncated). A stream
// that ends before a marker within the window is ErrTruncated ("stream ended before a string
// object"), so a cut stream is never reported as text-less (a short complete stream without
// a string is ErrTruncated too: conservative, a real attributedBody carries a string). A
// marker without its "+" encoding is never "no string object": ErrTruncated when the input
// ends first, ErrLimit when the encoding lies past the scan window.
//
// Every error wraps one of the sentinels of errors.go. The package imports no other
// Minutiae package: callers charge memory through the local Budget interface.
package typedstream

import (
	"bytes"
	"fmt"
	"strconv"
)

const (
	// MaxInput is the largest blob ExtractText reads; a larger one is ErrLimit.
	MaxInput = 16 << 20
	// MaxText is the largest text ExtractText returns; a longer declared length is ErrLimit.
	MaxText = 4 << 20
	// MaxScan is how many bytes of structure are examined for the string object: the class
	// name and the "+" encoding must lie inside the MaxScan bytes after the header.
	MaxScan = 1 << 16
)

// Budget is the two-method memory budget the caller passes (a *parse.BudgetView satisfies it).
type Budget interface {
	Alloc(n int64) error
	Free(n int64)
}

// guard runs f and converts a panic into ErrInternal.
func guard(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			msg := strconv.QuoteToASCII(fmt.Sprint(r))
			if len(msg) > 200 {
				msg = msg[:200]
			}
			err = fmt.Errorf("%w: %s", ErrInternal, msg)
		}
	}()
	return f()
}

// ExtractText returns the text of the first NSString or NSMutableString object of the
// typedstream b. ok is false whenever no text was recovered; then text is "" and err says why
// (ErrNotTypedstream, ErrTruncated, ErrLimit, ErrNoBudget, ErrInternal), except that ok false
// with a nil err means a well-formed stream with no string object. ok true with an empty text
// is a string object of length zero. A partial text is never returned.
//
// The budget is charged len(text) before the text is copied; on success the charge stays (the
// caller's per-invocation budget is released by the host), on failure nothing stays charged.
func ExtractText(b []byte, budget Budget) (text string, ok bool, err error) {
	err = guard(func() error {
		var e error
		text, ok, e = extractCore(b, budget)
		return e
	})
	if err != nil {
		text, ok = "", false
	}
	return text, ok, err
}

func tooLong(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrLimit, fmt.Sprintf(format, a...))
}

func truncated(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrTruncated, fmt.Sprintf(format, a...))
}

// extractCore is ExtractText without the recover guard.
func extractCore(b []byte, budget Budget) (string, bool, error) {
	if budget == nil {
		return "", false, fmt.Errorf("%w: no budget", ErrNoBudget)
	}
	if len(b) > MaxInput {
		return "", false, tooLong("input of %d bytes above %d", len(b), MaxInput)
	}
	pos, err := readHeader(b)
	if err != nil {
		return "", false, err
	}
	scanEnd := min(len(b), pos+MaxScan)
	nameEnd, cut := findString(b, pos, scanEnd)
	if nameEnd < 0 {
		if cut {
			return "", false, truncated("stream ended inside a string class name")
		}
		if len(b)-pos >= MaxScan {
			// The whole window was examined: the claim "no string object" is justified.
			return "", false, nil
		}
		return "", false, truncated("stream ended before a string object")
	}
	plus := -1
	for j := nameEnd; j+1 < scanEnd; j++ {
		if b[j] == 0x01 && b[j+1] == '+' {
			plus = j + 2
			break
		}
	}
	if plus < 0 {
		if len(b) <= scanEnd {
			return "", false, truncated("string class without a text encoding")
		}
		return "", false, tooLong("text encoding past the scan window of %d bytes", MaxScan)
	}
	n, p, err := readLength(b, plus)
	if err != nil {
		return "", false, err
	}
	if n > MaxText {
		return "", false, tooLong("text of %d bytes above %d", n, MaxText)
	}
	if n > uint64(len(b)-p) {
		return "", false, truncated("text of %d bytes, %d left", n, len(b)-p)
	}
	if n == 0 {
		return "", true, nil
	}
	if err := budget.Alloc(int64(n)); err != nil {
		return "", false, fmt.Errorf("%w: %w", ErrNoBudget, err)
	}
	return string(b[p : p+int(n)]), true, nil
}

func headerBytes() []byte { return append([]byte{0x04, 0x0B}, "streamtyped"...) }

// readHeader checks the header and skips the system version; it returns the position after.
func readHeader(b []byte) (int, error) {
	hdr := headerBytes()
	n := min(len(b), len(hdr))
	if len(b) == 0 || !bytes.Equal(b[:n], hdr[:n]) {
		return 0, fmt.Errorf("%w: bad header", ErrNotTypedstream)
	}
	if len(b) < len(hdr) {
		return 0, truncated("header cut")
	}
	_, p, err := readInt(b, len(hdr))
	return p, err
}

// readInt reads a typedstream integer at p: below 0x80 the value itself, 0x81 two, 0x82 four
// and 0x83 eight little-endian bytes. Any other tag is not an integer.
func readInt(b []byte, p int) (uint64, int, error) {
	if p >= len(b) {
		return 0, 0, truncated("integer cut")
	}
	c := b[p]
	if c < 0x80 {
		return uint64(c), p + 1, nil
	}
	size := 0
	switch c {
	case 0x81:
		size = 2
	case 0x82:
		size = 4
	case 0x83:
		size = 8
	default:
		return 0, 0, fmt.Errorf("%w: tag 0x%02x is not an integer", ErrNotTypedstream, c)
	}
	if len(b)-p-1 < size {
		return 0, 0, truncated("integer cut")
	}
	var v uint64
	for i := size - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[p+1+i])
	}
	return v, p + 1 + size, nil
}

// readLength is readInt for a length: the 8-byte form is refused.
func readLength(b []byte, p int) (uint64, int, error) {
	if p < len(b) && b[p] == 0x83 {
		return 0, 0, tooLong("8-byte length")
	}
	return readInt(b, p)
}

// findString returns the end of the first class name NSString or NSMutableString (preceded by
// its length byte) that starts before end, or -1. A marker may start at any offset of the
// window and is read in full, past end if need be. cut reports that no marker was found but
// the input ends inside one that started in the window.
func findString(b []byte, from, end int) (next int, cut bool) {
	for i := from; i < end; i++ {
		var name string
		switch b[i] {
		case 8:
			name = "NSString"
		case 15:
			name = "NSMutableString"
		default:
			continue
		}
		rest := b[i+1:]
		if bytes.HasPrefix(rest, []byte(name)) {
			return i + 1 + len(name), false
		}
		if len(rest) < len(name) && bytes.HasPrefix([]byte(name), rest) {
			cut = true
		}
	}
	return -1, cut
}
