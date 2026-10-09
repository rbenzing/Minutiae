package typedstream

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

func view(limit int64) *parse.BudgetView { return parse.NewBudget(limit).View() }

func extract(t *testing.T, b []byte) (string, bool, error) {
	t.Helper()
	return ExtractText(b, view(64<<20))
}

func mustText(t *testing.T, b []byte, want string) {
	t.Helper()
	got, ok, err := extract(t, b)
	if err != nil || !ok || got != want {
		t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", got, ok, err, want)
	}
}

func TestExtractTextSimple(t *testing.T) { mustText(t, buildStream("hello world"), "hello world") }

func TestExtractTextMutableString(t *testing.T) {
	mustText(t, buildStream("changed", mutable()), "changed")
}

func TestExtractTextUnicode(t *testing.T) {
	for _, s := range []string{
		"café naïve",
		"你好，世界",
		"\U0001F468\xe2\x80\x8d\U0001F469\xe2\x80\x8d\U0001F467 family \U0001F600",
		"שלום مرحبا abc",
	} {
		mustText(t, buildStream(s), s)
	}
}

func TestExtractTextKeepsBytesExactly(t *testing.T) {
	for _, s := range []string{
		"a\x00b\x00",
		"bad\xff\xfe\xc3(utf8",
		"lone \xed\xa0\x80 surrogate \xed\xb0\x80",
		"trailing   ",
		"\x00",
		" \t\r\n",
	} {
		got, ok, err := extract(t, buildStream(s))
		if err != nil || !ok || !bytes.Equal([]byte(got), []byte(s)) {
			t.Fatalf("%q: got (%q, %v, %v)", s, got, ok, err)
		}
	}
}

func TestExtractTextLengthForms(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 255, 256, 65535, 65536, 70000, MaxText} {
		s := strings.Repeat("x", n)
		want := 1
		switch {
		case n >= 1<<16:
			want = 5
		case n >= 0x80:
			want = 3
		}
		if got := len(lengthBytes(n)); got != want {
			t.Fatalf("builder length form for %d: %d bytes, want %d", n, got, want)
		}
		got, ok, err := extract(t, buildStream(s))
		if err != nil || !ok || got != s {
			t.Fatalf("length %d: ok=%v err=%v len=%d", n, ok, err, len(got))
		}
	}
	text, ok, err := extract(t, buildStream(strings.Repeat("x", MaxText+1)))
	if ok || text != "" || !errors.Is(err, ErrLimit) {
		t.Fatalf("MaxText+1: (%d bytes, %v, %v)", len(text), ok, err)
	}
}

func TestExtractTextLengthLies(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want error
	}{
		{"short buffer 1-byte", buildStream("abc", rawLen(0x7F)), ErrTruncated},
		{"short buffer 2-byte", buildStream("abc", rawLen(0x81, 0xFF, 0xFF)), ErrTruncated},
		{"short buffer 4-byte", buildStream("abc", rawLen(0x82, 0x00, 0x10, 0x00, 0x00)), ErrTruncated},
		{"4-byte all ones", buildStream("abc", rawLen(0x82, 0xFF, 0xFF, 0xFF, 0xFF)), ErrLimit},
		{"8-byte form", buildStream("abc", rawLen(0x83, 3, 0, 0, 0, 0, 0, 0, 0)), ErrLimit},
		{"8-byte form huge", buildStream("abc", rawLen(0x83, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F)), ErrLimit},
		{"int-overflow on 32-bit", buildStream("abc", rawLen(0x82, 0x00, 0x00, 0x00, 0x80)), ErrLimit},
		{"length cut", lengthCut(), ErrTruncated},
	}
	for _, c := range cases {
		text, ok, err := extract(t, c.b)
		if ok || text != "" || !errors.Is(err, c.want) {
			t.Errorf("%s: (%q, %v, %v), want %v", c.name, text, ok, err, c.want)
		}
	}
	// A zero length on an explicit string object is a present, empty text; no string object
	// at all is not.
	if text, ok, err := extract(t, buildStream("")); err != nil || !ok || text != "" {
		t.Fatalf("zero length: (%q, %v, %v)", text, ok, err)
	}
}

func TestExtractTextTruncatedEverywhere(t *testing.T) {
	const text = "truncate me please"
	full := buildStream(text)
	payloadStart := bytes.Index(full, []byte(text))
	for n := 0; n < len(full); n++ {
		got, ok, err := ExtractText(full[:n], view(1<<20))
		switch {
		case ok:
			if got != text || n < payloadStart+len(text) {
				t.Fatalf("cut %d: partial or early text %q", n, got)
			}
		case got != "":
			t.Fatalf("cut %d: text %q with ok=false", n, got)
		case errors.Is(err, ErrTruncated), errors.Is(err, ErrNotTypedstream):
		default:
			t.Fatalf("cut %d: (%q, %v, %v)", n, got, ok, err)
		}
	}
}

func TestExtractTextNotTypedstream(t *testing.T) {
	good := buildStream("x")
	wrongVersion := append([]byte{0x03}, good[1:]...)
	wrongTag := bytes.Replace(good, []byte("streamtyped"), []byte("streamtypee"), 1)
	for name, b := range map[string][]byte{
		"empty":         {},
		"bplist":        []byte("bplist00\x00\x00"),
		"protobuf":      {0x0a, 0x05, 'h', 'e', 'l', 'l', 'o', 0x10, 0x01},
		"xml":           []byte(`<?xml version="1.0"?><plist/>`),
		"wrong version": wrongVersion,
		"wrong tag":     wrongTag,
		"header cut":    good[:6],
		"one byte":      {0x04},
	} {
		text, ok, err := extract(t, b)
		if ok || text != "" || (!errors.Is(err, ErrNotTypedstream) && !errors.Is(err, ErrTruncated)) {
			t.Errorf("%s: (%q, %v, %v)", name, text, ok, err)
		}
	}
	if _, _, err := extract(t, wrongTag); !errors.Is(err, ErrNotTypedstream) {
		t.Errorf("wrong tag: %v", err)
	}
	if _, _, err := extract(t, wrongVersion); !errors.Is(err, ErrNotTypedstream) {
		t.Errorf("wrong version: %v", err)
	}
}

func TestExtractTextMarkerInsidePayload(t *testing.T) {
	for _, s := range []string{
		"NSString",
		"\x08NSString\x01+\x03abc",
		"x \x0fNSMutableString\x01+\x02zz",
	} {
		mustText(t, buildStream(s), s)
	}
}

func TestExtractTextNoStringObject(t *testing.T) {
	// The claim "no string object" needs the whole scan window examined (P38).
	text, ok, err := extract(t, buildStream("ignored", withoutString(), padding(MaxScan)))
	if ok || text != "" || err != nil {
		t.Fatalf("(%q, %v, %v)", text, ok, err)
	}
}

func TestExtractTextShortStreamWithoutStringIsTruncated(t *testing.T) {
	text, ok, err := extract(t, buildStream("ignored", withoutString()))
	if ok || text != "" || !errors.Is(err, ErrTruncated) {
		t.Fatalf("(%q, %v, %v)", text, ok, err)
	}
}

// markerNoPayload is a header and the NSString class name followed by pad zero bytes and no
// "+" encoding.
func markerNoPayload(pad int) []byte {
	b := header()
	b = append(b, make([]byte, 10)...)
	b = append(b, classRecord("NSString")...)
	return append(b, make([]byte, pad)...)
}

func TestExtractTextMarkerWithoutPayload(t *testing.T) {
	// P41: the input ends first.
	for _, pad := range []int{0, 5, MaxScan - 100} {
		text, ok, err := extract(t, markerNoPayload(pad))
		if ok || text != "" || !errors.Is(err, ErrTruncated) {
			t.Errorf("pad %d: (%q, %v, %v)", pad, text, ok, err)
		}
	}
	// P41: the encoding lies past the scan window.
	b := append(markerNoPayload(MaxScan), 0x01, '+', 0x01, 'x')
	text, ok, err := extract(t, b)
	if ok || text != "" || !errors.Is(err, ErrLimit) {
		t.Fatalf("payload past window: (%q, %v, %v)", text, ok, err)
	}
}

func TestExtractTextScanLimit(t *testing.T) {
	mustText(t, buildStream("inside", padding(MaxScan-200)), "inside")
	text, ok, err := extract(t, buildStream("beyond", padding(MaxScan)))
	if ok || text != "" || err != nil {
		t.Fatalf("beyond the scan limit: (%q, %v, %v)", text, ok, err)
	}
}

func TestExtractTextBudget(t *testing.T) {
	v := view(1 << 20)
	got, ok, err := ExtractText(buildStream("twelve bytes"), v)
	if err != nil || !ok || got != "twelve bytes" || v.Used() != int64(len(got)) {
		t.Fatalf("success: (%q, %v, %v) used %d", got, ok, err, v.Used())
	}
	v = view(1 << 20)
	if _, ok, err := ExtractText(buildStream("abc", rawLen(0x7F)), v); ok || err == nil || v.Used() != 0 {
		t.Fatalf("failure: ok=%v err=%v used %d", ok, err, v.Used())
	}
	if _, ok, err := ExtractText(buildStream("x"), nil); ok || !errors.Is(err, ErrNoBudget) {
		t.Fatalf("nil budget: %v %v", ok, err)
	}
	small := view(4)
	text, ok, err := ExtractText(buildStream("far too long for it"), small)
	if ok || text != "" || !errors.Is(err, parse.ErrBudget) || !errors.Is(err, ErrNoBudget) || small.Used() != 0 {
		t.Fatalf("small budget: (%q, %v, %v) used %d", text, ok, err, small.Used())
	}
}

func TestGuardConvertsPanicToError(t *testing.T) {
	var nilMap map[string]int
	var s []int
	idx := 3
	for name, f := range map[string]func(){
		"string": func() { panic("boom") },
		"error":  func() { panic(errors.New("an error")) },
		"nilmap": func() { nilMap["a"] = 1 },
		"index":  func() { _ = s[idx] },
		"custom": func() { panic(struct{ A, B int }{1, 2}) },
		"nil":    func() { panic(nil) },
		"long":   func() { panic(strings.Repeat("x\n\x1b", 500)) },
	} {
		err := guard(func() error { f(); return nil })
		if !errors.Is(err, ErrInternal) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(err.Error()) > 300 || strings.ContainsAny(err.Error(), "\n\x1b") {
			t.Errorf("%s: message not bounded or printable: %q", name, err.Error())
		}
	}
	if err := guard(func() error { return ErrTruncated }); !errors.Is(err, ErrTruncated) {
		t.Fatalf("plain error changed: %v", err)
	}
}

func TestExtractTextInputOverCap(t *testing.T) {
	v := view(1 << 20)
	text, ok, err := ExtractText(make([]byte, MaxInput+1), v)
	if ok || text != "" || !errors.Is(err, ErrLimit) || v.Used() != 0 {
		t.Fatalf("(%q, %v, %v) used %d", text, ok, err, v.Used())
	}
}

func TestExtractTextDeterministic(t *testing.T) {
	st := buildStream("same every time é")
	a, oka, erra := extract(t, st)
	b, okb, errb := extract(t, st)
	if a != b || oka != okb || (erra == nil) != (errb == nil) {
		t.Fatal("two runs differ")
	}
}

// lengthCut is a stream that ends inside the 4-byte length.
func lengthCut() []byte {
	st := buildStream("abc")
	i := bytes.Index(st, []byte{0x01, '+'}) + 2
	return append(st[:i], 0x82, 0x01)
}
