package plist

// RawString is a string value Decode refuses to turn into a Go string because doing so would
// alter it: either it is not valid UTF-8 (a binary ASCII string with bytes the text rules
// reject), or it holds a lone UTF-16 surrogate. Bytes holds the stored bytes exactly. When
// UTF16 is true Bytes are big-endian UTF-16 code units (the binary UTF-16 form, or the units
// of an XML string with a character reference to a surrogate); otherwise they are the stored
// 8-bit bytes. A dictionary key that would be a RawString is ErrUnsupported instead (a Go
// map key must be a string).
type RawString struct {
	Bytes []byte
	UTF16 bool
}

// RawDate is a date value that is not a representable instant: NaN, an infinity, or an
// instant outside the years 0001 to 9999. Seconds is the stored Cocoa-epoch double (seconds
// since 2001-01-01 UTC), exactly (for an XML date, computed from its text).
type RawDate struct {
	Seconds float64
}
