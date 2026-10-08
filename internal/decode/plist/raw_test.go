package plist

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func asciiObj(s string) []byte { return append([]byte{0x50 | byte(len(s))}, s...) }

func utf16Obj(units ...uint16) []byte {
	o := []byte{0x60 | byte(len(units))}
	for _, u := range units {
		o = binary.BigEndian.AppendUint16(o, u)
	}
	return o
}

func dateObj(secs float64) []byte {
	return binary.BigEndian.AppendUint64([]byte{0x33}, math.Float64bits(secs))
}

func beBytes(units ...uint16) []byte {
	var o []byte
	for _, u := range units {
		o = binary.BigEndian.AppendUint16(o, u)
	}
	return o
}

// P17: a string that is not valid UTF-8 is never a Go string and never repaired.
func TestDecodeInvalidUTF8StringIsRawString(t *testing.T) {
	want := RawString{Bytes: []byte("a\xffb")}
	if got := mustDecode(t, objectsPlist(asciiObj("a\xffb"))); !reflect.DeepEqual(got, any(want)) {
		t.Fatalf("top level: %#v", got)
	}
	// inside an array, a dict value and a shared reference
	arr := mustDecode(t, objectsPlist(refArray(1, 1), asciiObj("a\xffb")))
	if !reflect.DeepEqual(arr, []any{want, want}) {
		t.Fatalf("array: %#v", arr)
	}
	dict := mustDecode(t, objectsPlist([]byte{0xD1, 1, 2}, asciiObj("k"), asciiObj("a\xffb")))
	if !reflect.DeepEqual(dict, map[string]any{"k": want}) {
		t.Fatalf("dict: %#v", dict)
	}
	if got := mustDecode(t, objectsPlist(asciiObj("caf\xc3\xa9"))); got != any("caf\xc3\xa9") {
		t.Fatalf("valid high bytes: %#v", got)
	}
	// the stored bytes are copied, not aliased
	in := objectsPlist(asciiObj("a\xffb"))
	got := mustDecode(t, in).(RawString)
	in[9] = 'Z'
	if string(got.Bytes) != "a\xffb" {
		t.Fatalf("aliases the input: %q", got.Bytes)
	}
}

func TestDecodeInvalidUTF8KeyIsRefused(t *testing.T) {
	_, err := Decode(objectsPlist([]byte{0xD1, 1, 2}, asciiObj("k\xff"), asciiObj("v")), newView(1<<20))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	_, err = Decode(objectsPlist([]byte{0xD1, 1, 2}, utf16Obj(0xD800), asciiObj("v")), newView(1<<20))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("surrogate key: err = %v, want ErrUnsupported", err)
	}
}

// P22 (binary): a lone surrogate keeps its code units.
func TestDecodeBinaryLoneSurrogateIsRawString(t *testing.T) {
	cases := map[string][]uint16{
		"high alone":     {0xD800},
		"low alone":      {0xDC00},
		"high then text": {0x61, 0xD83D, 0x62},
		"reversed pair":  {0xDE00, 0xD83D},
	}
	for name, units := range cases {
		want := RawString{Bytes: beBytes(units...), UTF16: true}
		if got := mustDecode(t, objectsPlist(utf16Obj(units...))); !reflect.DeepEqual(got, any(want)) {
			t.Fatalf("%s: %#v", name, got)
		}
	}
	want := RawString{Bytes: beBytes(0xD800), UTF16: true}
	got := mustDecode(t, objectsPlist([]byte{0xD1, 1, 2}, asciiObj("k"), utf16Obj(0xD800)))
	if !reflect.DeepEqual(got, map[string]any{"k": want}) {
		t.Fatalf("dict value: %#v", got)
	}
	// a valid pair and a literal U+FFFD stay strings
	got = mustDecode(t, objectsPlist(refArray(1, 2), utf16Obj(0xD83D, 0xDE00), utf16Obj(0xFFFD)))
	if !reflect.DeepEqual(got, []any{string(rune(0x1F600)), string(rune(0xFFFD))}) {
		t.Fatalf("valid strings: %#v", got)
	}
	// a damaged string beside a good one with the same replacement text
	got = mustDecode(t, objectsPlist(refArray(1, 2), utf16Obj(0xFFFD), utf16Obj(0xD800)))
	if !reflect.DeepEqual(got, []any{string(rune(0xFFFD)), want}) {
		t.Fatalf("mixed: %#v", got)
	}
}

// P21 (binary): a date that is not a representable instant is never a time.Time.
func TestDecodeBinaryDateOutOfRangeIsRawDate(t *testing.T) {
	const cocoaToUnix = 978307200
	const lastValid = 253402300799 - cocoaToUnix  // 9999-12-31T23:59:59
	const firstValid = -62135596800 - cocoaToUnix // 0001-01-01T00:00:00
	bad := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300, -1e300, lastValid + 1, firstValid - 1}
	for _, s := range bad {
		got := mustDecode(t, objectsPlist(dateObj(s)))
		rd, ok := got.(RawDate)
		if !ok {
			t.Fatalf("%v: %#v, want RawDate", s, got)
		}
		if math.Float64bits(rd.Seconds) != math.Float64bits(s) {
			t.Fatalf("%v: kept %v", s, rd.Seconds)
		}
	}
	for secs, want := range map[float64]time.Time{
		lastValid:       time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		firstValid:      time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		0:               time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		lastValid + 0.5: time.Date(9999, 12, 31, 23, 59, 59, 500000000, time.UTC),
	} {
		got := mustDecode(t, objectsPlist(dateObj(secs)))
		if tm, ok := got.(time.Time); !ok || !tm.Equal(want) {
			t.Fatalf("%v: %#v, want %v", secs, got, want)
		}
	}
	// inside containers, next to a good date
	got := mustDecode(t, objectsPlist(refArray(1, 2), dateObj(math.NaN()), dateObj(0)))
	l := got.([]any)
	if _, ok := l[0].(RawDate); !ok {
		t.Fatalf("array: %#v", got)
	}
	if _, ok := l[1].(time.Time); !ok {
		t.Fatalf("array: %#v", got)
	}
}

func TestDecodeXMLDateOutOfRangeIsRawDate(t *testing.T) {
	got := mustDecode(t, []byte(`<plist version="1.0"><date>0000-01-01T00:00:00Z</date></plist>`))
	// 0000-01-01 is 62167219200 s before the Unix epoch.
	if want := (RawDate{Seconds: -62167219200 - 978307200}); !reflect.DeepEqual(got, any(want)) {
		t.Fatalf("%#v", got)
	}
	got = mustDecode(t, []byte(`<plist version="1.0"><date>9999-12-31T23:59:59Z</date></plist>`))
	if _, ok := got.(time.Time); !ok {
		t.Fatalf("%#v", got)
	}
}

// P22 (XML): a character reference to a surrogate keeps its code unit.
func TestDecodeXMLLoneSurrogateIsRawString(t *testing.T) {
	want := RawString{Bytes: beBytes('a', 0xD800, 'b'), UTF16: true}
	for _, in := range []string{
		`<plist version="1.0"><string>a&#xD800;b</string></plist>`,
		`<plist version="1.0"><string>a&#55296;b</string></plist>`,
		`<plist version="1.0"><string>a&#x0d800;b</string></plist>`,
	} {
		if got := mustDecode(t, []byte(in)); !reflect.DeepEqual(got, any(want)) {
			t.Fatalf("%q: %#v", in, got)
		}
	}
	got := mustDecode(t, []byte(`<plist version="1.0"><array><string>&#xDFFF;</string><string>&#xFFFD;</string><string>x</string></array></plist>`))
	if !reflect.DeepEqual(got, []any{RawString{Bytes: beBytes(0xDFFF), UTF16: true}, string(rune(0xFFFD)), "x"}) {
		t.Fatalf("array: %#v", got)
	}
	// text that uses the characters the implementation might borrow stays as it is
	plane16 := string(rune(0x10F000)) + string(rune(0x10F001))
	doc := `<plist version="1.0"><array><string>&#xD800;</string><string>` + plane16 + `</string><string>&#x10F002;</string></array></plist>`
	got = mustDecode(t, []byte(doc))
	if !reflect.DeepEqual(got, []any{RawString{Bytes: beBytes(0xD800), UTF16: true}, plane16, string(rune(0x10F002))}) {
		t.Fatalf("collision: %#v", got)
	}
	_, err := Decode([]byte(`<plist version="1.0"><dict><key>&#xD800;</key><string>v</string></dict></plist>`), newView(1<<20))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("key: %v", err)
	}
}

// P23
func TestDecodeXMLNegativeUIDIsMalformed(t *testing.T) {
	for _, in := range []string{
		`<plist><dict><key>CF$UID</key><integer>-1</integer></dict></plist>`,
		`<plist><dict><key>CF$UID</key>  <integer> -5</integer></dict></plist>`,
	} {
		if _, err := Decode([]byte(in), newView(1<<20)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	got := mustDecode(t, []byte(`<plist><dict><key>CF$UID</key><integer>18446744073709551615</integer></dict></plist>`))
	if got != any(UID(math.MaxUint64)) {
		t.Fatalf("max uid: %#v", got)
	}
}

// P24: the charge is exactly len*8 + nodes*64 + payload*2, so the input-byte term is pinned.
func TestDecodeChargeIsExactlyTheModel(t *testing.T) {
	b := objectsPlist(asciiObj("hi")) // 1 node, 2 payload bytes
	view := newView(1 << 20)
	if _, err := Decode(b, view); err != nil {
		t.Fatal(err)
	}
	if want := int64(len(b))*8 + 1*64 + 2*2; view.Used() != want {
		t.Fatalf("Used = %d, want %d", view.Used(), want)
	}
}
