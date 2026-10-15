package plist

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// A processing instruction at the very start that is not the XML declaration is refused
// (4C-4 review I1, M8).
func TestPrescanLeadingProcessingInstructionIsRefused(t *testing.T) {
	for name, in := range map[string]string{
		"foo":             `<?foo x?><plist/>`,
		"foo-bom":         "\xef\xbb\xbf" + `<?foo x?><plist/>`,
		"xml-stylesheet":  `<?xml-stylesheet href="a"?><plist/>`,
		"stylesheet-bom":  "\xef\xbb\xbf" + `<?xml-stylesheet href="a"?><plist/>`,
		"xmlfoo-space":    `<?xmlfoo x?><plist/>`,
		"pi-alone-spaced": `<?php echo 1; ?><plist/>`,
	} {
		t.Run(name, func(t *testing.T) { wantIs(t, PrescanXML([]byte(in), DefaultLimits()), ErrMalformed) })
	}
	// control: the real declaration, with and without a BOM
	for _, in := range []string{`<?xml version="1.0"?><plist/>`, "\xef\xbb\xbf" + `<?xml version="1.0"?><plist/>`} {
		if err := PrescanXML([]byte(in), DefaultLimits()); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// A UTF-16 or UTF-32 byte order mark without a NUL byte is still refused as non-UTF-8, and a
// tag name must start with a name character (4C-4 review M8).
func TestPrescanBOMWithoutNULAndBadNameStart(t *testing.T) {
	for name, in := range map[string]string{
		"utf16le-bom-no-nul": "\xff\xfe<plist/>",
		"utf16be-bom-no-nul": "\xfe\xff<plist/>",
		"utf32be-bom":        "\x00\x00\xfe\xff",
	} {
		t.Run(name, func(t *testing.T) { wantIs(t, PrescanXML([]byte(in), DefaultLimits()), ErrUnsupported) })
	}
	wantIs(t, PrescanXML([]byte(`<plist><1/></plist>`), DefaultLimits()), ErrMalformed)
	wantIs(t, PrescanXML([]byte(`<plist><-a/></plist>`), DefaultLimits()), ErrMalformed)
}

// OpenStep data that starts with a digit is an unsupported format, not a malformed XML
// document (4C-4 review M6).
func TestCheckOpenStepDataIsUnsupported(t *testing.T) {
	for _, in := range []string{`<01ab cd>`, `<0123>`, "  <ff>", "<  >"} {
		wantIs(t, Check([]byte(in), DefaultLimits()), ErrUnsupported)
	}
	// still malformed: a lone '<' at the end of a document
	wantIs(t, Check([]byte(`<plist/><`), DefaultLimits()), ErrMalformed)
}

// The recursion depth is capped internally whatever MaxDepth the caller passes (4C-4
// review M5).
func TestHugeMaxDepthIsCappedInternally(t *testing.T) {
	huge := Limits{MaxNodes: 1 << 20, MaxDepth: math.MaxInt, MaxPayload: 1 << 20}
	const n = 5000
	// binary: object 0 is an integer, object i an array of one reference to i-1
	b := []byte("bplist00")
	offsets := make([]byte, 0, 4*(n+1))
	offsets = binary.BigEndian.AppendUint32(offsets, uint32(len(b)))
	b = append(b, 0x10, 0x00)
	for i := 1; i <= n; i++ {
		offsets = binary.BigEndian.AppendUint32(offsets, uint32(len(b)))
		b = append(b, 0xA1)
		b = binary.BigEndian.AppendUint16(b, uint16(i-1))
	}
	tableOff := len(b)
	b = append(b, offsets...)
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 4, 2
	binary.BigEndian.PutUint64(trailer[8:], uint64(n+1))
	binary.BigEndian.PutUint64(trailer[16:], uint64(n))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	wantIs(t, checkBinary(append(b, trailer...), huge), ErrLimit)

	x := "<plist>" + strings.Repeat("<array>", n) + strings.Repeat("</array>", n) + "</plist>"
	wantIs(t, PrescanXML([]byte(x), huge), ErrLimit)
}
