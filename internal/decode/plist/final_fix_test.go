package plist

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// FA-1: a binary UID with a 16-byte payload keeps its high word or is refused.
func TestBinaryUID16ByteHighWordIsRefused(t *testing.T) {
	uid16 := func(hi, lo uint64) []byte {
		obj := append([]byte{0x8F}, binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, hi), lo)...)
		return rawPlist(obj, []byte{8})
	}
	for _, hi := range []uint64{1, 1 << 63, ^uint64(0)} {
		doc := uid16(hi, 7)
		wantIs(t, Check(doc, DefaultLimits()), ErrUnsupported)
		if _, err := Decode(doc, newView(1<<20)); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("hi=%#x: Decode err = %v, want ErrUnsupported", hi, err)
		}
	}
	if got := mustDecode(t, uid16(0, 7)); got != any(UID(7)) {
		t.Fatalf("hi=0: %#v", got)
	}
	// UIDs of 9 to 15 bytes are not a size the library reads: refused as malformed.
	for n := 9; n <= 15; n++ {
		obj := append([]byte{0x80 | byte(n-1)}, make([]byte, n)...)
		if _, err := Decode(rawPlist(obj, []byte{8}), newView(1<<20)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%d-byte UID: err = %v, want ErrMalformed", n, err)
		}
	}
}

// FA-2: a child element inside <key> or <integer> cannot hide a negative CF$UID.
func TestDecodeXMLNegativeUIDWithChildElements(t *testing.T) {
	for name, body := range map[string]string{
		"child in key":      `<dict><key>CF$UID<b/></key><integer>-5</integer></dict>`,
		"child in integer":  `<dict><key>CF$UID</key><integer><x/>-5</integer></dict>`,
		"child in both":     `<dict><key>CF$UID<b/></key><integer><x/>-5</integer></dict>`,
		"text child":        `<dict><key>CF$UID</key><integer>-<x>7</x>5</integer></dict>`,
		"child after":       `<dict><key>CF$UID</key><integer>-5<x/></integer></dict>`,
		"nested in child":   `<dict><key>CF$UID<b><c/></b></key><integer>-5</integer></dict>`,
		"unrelated key too": `<dict><key>a<b/></key><string>x</string></dict>`,
	} {
		_, err := Decode([]byte(`<plist version="1.0">`+body+`</plist>`), newView(1<<20))
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

// FA-3: consecutive keys never drop the first silently.
func TestDecodeXMLConsecutiveKeysAreMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"two keys":      `<dict><key>a</key><key>b</key><integer>1</integer></dict>`,
		"nested second": `<dict><key>x</key><string>y</string><key>a</key><key>b</key><true/></dict>`,
		"in array":      `<array><dict><key>a</key><key>b</key><integer>1</integer></dict></array>`,
	} {
		_, err := Decode([]byte(`<plist version="1.0">`+body+`</plist>`), newView(1<<20))
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	ok := `<plist><dict><key>a</key><dict><key>b</key><integer>1</integer></dict><key>c</key><array><dict/></array></dict></plist>`
	if _, err := Decode([]byte(ok), newView(1<<20)); err != nil {
		t.Fatalf("well-formed dicts refused: %v", err)
	}
}

// FA-3: a binary set and the binary null/fill byte are one class: ErrMalformed (the decoding
// library does not read them), pinned so the documentation and the code agree.
func TestBinarySetAndNullAreMalformed(t *testing.T) {
	for name, obj := range map[string][]byte{
		"set":  {0xC0},
		"set1": {0xC1, 0},
		"null": {0x00},
		"fill": {0x0F},
	} {
		doc := rawPlist(obj, []byte{8})
		if name == "set1" {
			doc = objectsPlist([]byte{0xC1, 1}, []byte{0x10, 1})
		}
		if _, err := Decode(doc, newView(1<<20)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

// FA-3: a dictionary key that is valid but not a string is ErrUnsupported in Unarchive too.
func TestUnarchiveNonStringKeyIsUnsupported(t *testing.T) {
	a := archive(1,
		map[string]any{"$class": UID(2), "NS.keys": []any{UID(3)}, "NS.objects": []any{UID(4)}},
		classRec("NSDictionary"), int64(5), "v")
	wantErr(t, a, ErrUnsupported)
	a = archive(1,
		map[string]any{"$class": UID(2), "NS.keys": []any{UID(3)}, "NS.objects": []any{UID(4)}},
		classRec("NSDictionary"), RawString{Bytes: []byte{0xff}}, "v")
	wantErr(t, a, ErrUnsupported)
}

// FA-3: the depth of the RESULT is bounded, also through memoized objects and inline arrays.
func TestUnarchiveDepthHoldsForMemoHits(t *testing.T) {
	const n = 600
	objs := []any{}
	for k := 1; k <= n; k++ {
		inner := UID(k - 1) // object 1 holds $null
		objs = append(objs, map[string]any{"$class": UID(n + 1), "NS.objects": []any{inner}})
	}
	objs = append(objs, classRec("NSArray"))
	a := archive(1, objs...)
	top := map[string]any{}
	for k := 1; k <= n; k++ {
		top[fmt.Sprintf("t%04d", k)] = UID(k)
	}
	a["$top"] = top
	wantErr(t, a, ErrLimit)

	var deep any = "x"
	for range MaxUnarchiveDepth + 10 {
		deep = []any{deep}
	}
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "f": deep}, classRec("Thing")), ErrLimit)
}

// FB-4: the text of a recovered panic is bounded and escaped, like typedstream's.
func TestGuardBoundsAndEscapesThePanicValue(t *testing.T) {
	err := guard(func() error { panic("\x1b[31m" + strings.Repeat("x", 5000) + "\n") })
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	if len(msg) > 300 {
		t.Fatalf("message of %d bytes is not bounded", len(msg))
	}
	if strings.ContainsAny(msg, "\x1b\n") {
		t.Fatalf("message holds raw control characters: %q", msg)
	}
}
