package plist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	howett "howett.net/plist"

	"github.com/rbenzing/minutiae/internal/parse"
)

func newView(limit int64) *parse.BudgetView { return parse.NewBudget(limit).View() }

func mustDecode(t *testing.T, b []byte) any {
	t.Helper()
	v, err := Decode(b, newView(1<<30))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return v
}

func marshalAs(t *testing.T, v any, format int) []byte {
	t.Helper()
	b, err := howett.Marshal(v, format)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeBinaryAndXMLAgree(t *testing.T) {
	doc := map[string]any{
		"s": "héllo 世界 😀", "i": int64(42), "neg": int64(-7), "f": 2.5, "t": true, "fl": false,
		"d": time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC), "b": []byte{1, 2, 3},
		"arr": []any{int64(1), "x", []any{map[string]any{"k": int64(2)}}}, "empty": map[string]any{},
	}
	bin := mustDecode(t, marshalAs(t, doc, howett.BinaryFormat))
	xml := mustDecode(t, marshalAs(t, doc, howett.XMLFormat))
	if !reflect.DeepEqual(bin, xml) {
		t.Fatalf("binary %#v\nxml    %#v", bin, xml)
	}
	if !reflect.DeepEqual(bin, any(doc)) {
		t.Fatalf("decoded %#v\nwant    %#v", bin, doc)
	}
}

func int16Plist(hi, lo uint64) []byte {
	obj := append([]byte{0x14}, binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, hi), lo)...)
	return rawPlist(obj, []byte{8})
}

func TestDecodeTypes(t *testing.T) {
	both := func(v any) []any {
		return []any{
			mustDecode(t, marshalAs(t, v, howett.BinaryFormat)),
			mustDecode(t, marshalAs(t, v, howett.XMLFormat)),
		}
	}
	for name, tc := range map[string]struct {
		in   any
		want any
	}{
		"small int":      {int64(5), int64(5)},
		"uint8":          {uint8(200), int64(200)},
		"negative":       {int64(-1 << 40), int64(-1 << 40)},
		"max int64":      {int64(math.MaxInt64), int64(math.MaxInt64)},
		"min int64":      {int64(math.MinInt64), int64(math.MinInt64)},
		"above maxint64": {uint64(math.MaxInt64) + 1, uint64(math.MaxInt64) + 1},
		"max uint64":     {uint64(math.MaxUint64), uint64(math.MaxUint64)},
		"float":          {-0.125, -0.125},
		"true":           {true, true},
		"empty data":     {[]byte{}, []byte{}},
		"data":           {[]byte{0, 255, 7}, []byte{0, 255, 7}},
		"empty string":   {"", ""},
		"surrogate pair": {"a😀b", "a😀b"},
		"uid":            {howett.UID(9), UID(9)},
	} {
		t.Run(name, func(t *testing.T) {
			for i, got := range both(tc.in) {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("format %d: %#v (%T), want %#v (%T)", i, got, got, tc.want, tc.want)
				}
			}
		})
	}
	t.Run("float32 is widened", func(t *testing.T) {
		got := mustDecode(t, marshalAs(t, float32(1.5), howett.BinaryFormat))
		if got != any(1.5) {
			t.Fatalf("%#v (%T)", got, got)
		}
	})
	t.Run("16-byte integers", func(t *testing.T) {
		if got := mustDecode(t, int16Plist(0, 1<<63)); got != any(uint64(1)<<63) {
			t.Fatalf("%#v", got)
		}
		if got := mustDecode(t, int16Plist(0, 17)); got != any(int64(17)) {
			t.Fatalf("%#v", got)
		}
		if got := mustDecode(t, int16Plist(math.MaxUint64, 1<<63)); got != any(int64(math.MinInt64)) {
			t.Fatalf("%#v", got)
		}
	})
	t.Run("date is UTC", func(t *testing.T) {
		in := time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
		for i, got := range both(in) {
			tm, ok := got.(time.Time)
			if !ok || tm.Location() != time.UTC || !tm.Equal(in) {
				t.Fatalf("format %d: %#v", i, got)
			}
		}
		x, _ := mustDecode(t, []byte(`<plist><date>2020-01-02T03:04:05+01:00</date></plist>`)).(time.Time)
		if x.Location() != time.UTC || !x.Equal(in) {
			t.Fatalf("xml offset date: %v", x)
		}
	})
	t.Run("NaN passes through", func(t *testing.T) {
		got := mustDecode(t, marshalAs(t, math.NaN(), howett.BinaryFormat))
		if f, ok := got.(float64); !ok || !math.IsNaN(f) {
			t.Fatalf("%#v", got)
		}
	})
	t.Run("60 deep", func(t *testing.T) {
		var v any = "leaf"
		for range 60 {
			v = []any{v}
		}
		for i, got := range both(v) {
			if !reflect.DeepEqual(got, v) {
				t.Fatalf("format %d differs", i)
			}
		}
	})
	t.Run("output is independent of the input", func(t *testing.T) {
		b := marshalAs(t, map[string]any{"k": "value", "d": []byte{1, 2, 3}}, howett.BinaryFormat)
		got, _ := mustDecode(t, b).(map[string]any)
		for i := range b {
			b[i] = 0xEE
		}
		if d, _ := got["d"].([]byte); got["k"] != "value" || !bytes.Equal(d, []byte{1, 2, 3}) {
			t.Fatalf("decoded values alias the input: %#v", got)
		}
	})
}

func TestDecodeDuplicateKeysLastWins(t *testing.T) {
	// Known property, not a guarantee worth relying on: the library keeps the last value of a
	// duplicate key. Detecting duplicates is a non-goal.
	got := mustDecode(t, []byte(`<plist><dict><key>a</key><integer>1</integer><key>a</key><integer>2</integer></dict></plist>`))
	if !reflect.DeepEqual(got, map[string]any{"a": int64(2)}) {
		t.Fatalf("%#v", got)
	}
}

func TestDecodeUIDForms(t *testing.T) {
	if got := mustDecode(t, []byte(`<plist><dict><key>CF$UID</key><integer>3</integer></dict></plist>`)); got != any(UID(3)) {
		t.Fatalf("xml: %#v", got)
	}
	if got := mustDecode(t, marshalAs(t, howett.UID(3), howett.BinaryFormat)); got != any(UID(3)) {
		t.Fatalf("binary: %#v", got)
	}
	two := mustDecode(t, []byte(`<plist><dict><key>CF$UID</key><integer>3</integer><key>x</key><integer>1</integer></dict></plist>`))
	if !reflect.DeepEqual(two, map[string]any{"CF$UID": int64(3), "x": int64(1)}) {
		t.Fatalf("two keys: %#v", two)
	}
	str := mustDecode(t, []byte(`<plist><dict><key>CF$UID</key><string>3</string></dict></plist>`))
	if !reflect.DeepEqual(str, map[string]any{"CF$UID": "3"}) {
		t.Fatalf("string value: %#v", str)
	}
	nested := mustDecode(t, []byte(`<plist><array><dict><key>CF$UID</key><integer>1</integer></dict></array></plist>`))
	if !reflect.DeepEqual(nested, []any{UID(1)}) {
		t.Fatalf("nested: %#v", nested)
	}
}

func TestDecodeBudgetCharged(t *testing.T) {
	b := marshalAs(t, map[string]any{"a": []any{"x", int64(1)}}, howett.BinaryFormat)
	view := newView(1 << 20)
	v, err := Decode(b, view)
	if err != nil || v == nil {
		t.Fatalf("Decode: %v %v", v, err)
	}
	if view.Used() <= 0 {
		t.Fatalf("Used = %d after success, want a charge", view.Used())
	}
	t.Run("too small", func(t *testing.T) {
		view := newView(16)
		v, err := Decode(b, view)
		if !errors.Is(err, parse.ErrBudget) || !errors.Is(err, ErrNoBudget) {
			t.Fatalf("err = %v, want parse.ErrBudget and ErrNoBudget", err)
		}
		if v != nil || view.Used() != 0 {
			t.Fatalf("decoded %v, used %d", v, view.Used())
		}
	})
	t.Run("freed on failure", func(t *testing.T) {
		for _, in := range []string{
			`<plist><integer>abc</integer></plist>`,
			`<plist><data>!!!</data></plist>`,
			`<plist><date>yesterday</date></plist>`,
			`<plist><dict><key>a</key></dict></plist>`,
		} {
			view := newView(1 << 20)
			if _, err := Decode([]byte(in), view); err == nil {
				t.Fatalf("%q accepted", in)
			}
			if view.Used() != 0 {
				t.Fatalf("%q: Used = %d after failure", in, view.Used())
			}
		}
	})
}

// The estimate counts what the decoder itself measured: expanded nodes and expanded payload.
func TestDecodeChargesExpandedNodesAndPayload(t *testing.T) {
	// a node bomb that stays within the node cap: ~1M nodes from a tiny file
	nodes := nestedPlist(19, 2)
	view := newView(16 << 20)
	v, err := Decode(nodes, view)
	if !errors.Is(err, ErrNoBudget) || v != nil || view.Used() != 0 {
		t.Fatalf("node bomb: v=%v used=%d err=%v", v != nil, view.Used(), err)
	}
	// above the node cap it is a limit, before any charge
	view = newView(1 << 40)
	if _, err := Decode(nestedPlist(21, 2), view); !errors.Is(err, ErrLimit) || view.Used() != 0 {
		t.Fatalf("over node cap: used=%d err=%v", view.Used(), err)
	}
	// a payload bomb: one 200-byte data object shared 14^4 times (~7.7 MB), nodes ~41k
	pay := payloadBomb()
	if len(pay) > 4096 {
		t.Fatalf("bomb file is %d bytes", len(pay))
	}
	view = newView(8 << 20)
	v, err = Decode(pay, view)
	if !errors.Is(err, ErrNoBudget) || v != nil || view.Used() != 0 {
		t.Fatalf("payload bomb: v=%v used=%d err=%v", v != nil, view.Used(), err)
	}
	// the same file with enough budget decodes and is charged for the payload
	view = newView(64 << 20)
	if _, err := Decode(pay, view); err != nil {
		t.Fatalf("payload bomb with budget: %v", err)
	}
	if view.Used() < 7_000_000 {
		t.Fatalf("Used = %d, want at least the expanded payload", view.Used())
	}
	n, p, err := measure(pay, DefaultLimits())
	if err != nil || n != 41371 || p != 14*14*14*14*200 {
		t.Fatalf("measure = %d nodes, %d payload, %v", n, p, err)
	}
}

// payloadBomb: object 0 is the top (an array of 14 references to object 4, a copy of level 5), 1 a 200-byte data
// object, 2..5 arrays of 14 references to the previous level.
func payloadBomb() []byte {
	const w = 2 // reference and offset size
	data := append([]byte{0x4f, 0x10, 200}, bytes.Repeat([]byte{7}, 200)...)
	objs := [][]byte{nil, data}
	for lvl := 2; lvl <= 5; lvl++ {
		a := []byte{0xAE}
		for range 14 {
			a = binary.BigEndian.AppendUint16(a, uint16(lvl-1))
		}
		objs = append(objs, a)
	}
	objs[0] = objs[len(objs)-1]
	b := []byte("bplist00")
	var offs []byte
	for _, o := range objs {
		offs = binary.BigEndian.AppendUint16(offs, uint16(len(b)))
		b = append(b, o...)
	}
	tableOff := len(b)
	b = append(b, offs...)
	tr := make([]byte, 32)
	tr[6], tr[7] = w, w
	binary.BigEndian.PutUint64(tr[8:], uint64(len(objs)))
	binary.BigEndian.PutUint64(tr[24:], uint64(tableOff))
	return append(b, tr...)
}

func TestDecodeNilBudgetFailsClosed(t *testing.T) {
	b := marshalAs(t, "x", howett.BinaryFormat)
	v, err := Decode(b, nil)
	if !errors.Is(err, ErrNoBudget) || v != nil {
		t.Fatalf("v=%v err=%v", v, err)
	}
	var typedNil *parse.BudgetView
	if v, err := Decode(b, typedNil); err == nil || v != nil {
		t.Fatalf("typed nil view accepted: %v %v", v, err)
	}
}

func TestDecodeRefusesOpenStepText(t *testing.T) {
	for name, in := range map[string]string{
		"dict":     `{ a = b; }`,
		"array":    `( a, b )`,
		"string":   `hello`,
		"quoted":   `"hello"`,
		"gnustep":  `{ a = <*I1>; }`,
		"hex-data": `<deadbeef>`,
		"digit":    `<01ab cd>`,
		"version":  "bplist01" + strings.Repeat("\x00", 40),
		"bom-only": "\xef\xbb\xbf",
	} {
		t.Run(name, func(t *testing.T) {
			view := newView(1 << 20)
			v, err := Decode([]byte(in), view)
			wantIs(t, err, ErrUnsupported)
			if v != nil || view.Used() != 0 {
				t.Fatalf("v=%v used=%d", v, view.Used())
			}
		})
	}
	view := newView(1 << 20)
	_, err := Decode(nil, view)
	wantIs(t, err, ErrMalformed)
	_, err = Decode([]byte{}, view)
	wantIs(t, err, ErrMalformed)
	if view.Used() != 0 {
		t.Fatal("charged for empty input")
	}
}

func TestDecodeMalformedXMLIsError(t *testing.T) {
	for name, in := range map[string]string{
		"mismatched tags":  `<plist><array><string>x</array></string></plist>`,
		"bad integer":      `<plist><integer>12x</integer></plist>`,
		"empty integer":    `<plist><integer/></plist>`,
		"bad base64":       `<plist><data>%%%%</data></plist>`,
		"bad date":         `<plist><date>not a date</date></plist>`,
		"bad real":         `<plist><real>1.2.3</real></plist>`,
		"key without val":  `<plist><dict><key>a</key></dict></plist>`,
		"val without key":  `<plist><dict><integer>1</integer></dict></plist>`,
		"unknown element":  `<plist><blob/></plist>`,
		"truncated":        `<plist><array><string>x</string>`,
		"integer overflow": `<plist><integer>99999999999999999999999</integer></plist>`,
	} {
		t.Run(name, func(t *testing.T) {
			view := newView(1 << 20)
			v, err := Decode([]byte(in), view)
			if err == nil || v != nil {
				t.Fatalf("accepted: %#v", v)
			}
			if errors.Is(err, ErrInternal) {
				t.Fatalf("recover guard reached: %v", err)
			}
			if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrLimit) {
				t.Fatalf("untyped error: %v", err)
			}
			if view.Used() != 0 {
				t.Fatalf("Used = %d", view.Used())
			}
		})
	}
}

func TestDecodeInputOverCapIsRefused(t *testing.T) {
	big := make([]byte, MaxInput+1)
	copy(big, "bplist00")
	view := newView(1 << 40)
	v, err := Decode(big, view)
	wantIs(t, err, ErrLimit)
	if v != nil || view.Used() != 0 {
		t.Fatalf("v=%v used=%d", v, view.Used())
	}
}

func TestDecodeBombIsBoundedInTimeAndMemory(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	view := newView(1 << 30)
	for name, in := range map[string][]byte{
		"nested 21x2":   nestedPlist(21, 2),
		"nested set":    nestedTyped(21, 2, 0xC0),
		"xml 100k dict": []byte("<plist>" + strings.Repeat("<dict>", 100000)),
		"xml 100k full": []byte("<plist>" + strings.Repeat("<array>", 100000) + strings.Repeat("</array>", 100000) + "</plist>"),
	} {
		v, err := Decode(in, view)
		if err == nil || v != nil {
			t.Fatalf("%s: accepted", name)
		}
		wantIs(t, err, ErrLimit)
	}
	runtime.ReadMemStats(&after)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if grown := int64(after.TotalAlloc) - int64(before.TotalAlloc); grown > 64<<20 {
		t.Fatalf("allocated %d bytes", grown)
	}
	if view.Used() != 0 {
		t.Fatalf("Used = %d", view.Used())
	}
}

func TestDecodeDeterministic(t *testing.T) {
	doc := map[string]any{"a": []any{int64(1), "x", 2.5}, "b": map[string]any{"c": []byte{9}}, "d": time.Unix(5, 0).UTC()}
	for _, f := range []int{howett.BinaryFormat, howett.XMLFormat} {
		b := marshalAs(t, doc, f)
		a, c := mustDecode(t, b), mustDecode(t, b)
		if a == nil || !reflect.DeepEqual(a, c) {
			t.Fatalf("format %d differs between runs", f)
		}
	}
}

// mutations returns a deterministic corpus of damaged copies of the valid plists in seeds.
func mutations(seeds [][]byte, n int) [][]byte {
	state := uint64(0x9E3779B97F4A7C15)
	next := func(m int) int {
		state = state*6364136223846793005 + 1442695040888963407
		return int(state>>33) % m
	}
	out := make([][]byte, 0, n)
	for len(out) < n {
		b := bytes.Clone(seeds[next(len(seeds))])
		for range 1 + next(3) {
			if len(b) == 0 {
				break
			}
			switch next(4) {
			case 0:
				b[next(len(b))] ^= byte(1 << next(8))
			case 1:
				i := next(len(b) + 1)
				b = append(b[:i], append([]byte{byte(next(256))}, b[i:]...)...)
			case 2:
				i := next(len(b))
				b = append(b[:i], b[i+1:]...)
			case 3:
				b = b[:next(len(b)+1)]
			}
		}
		out = append(out, b)
	}
	return out
}

func mutationSeeds(t *testing.T) [][]byte {
	doc := map[string]any{
		"s": "héllo 😀", "i": int64(-42), "u": uint64(1) << 63, "f": 2.5, "t": true,
		"d": time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC), "b": []byte{1, 2, 3},
		"arr": []any{int64(1), "x", []any{map[string]any{"k": howett.UID(2)}}},
	}
	return [][]byte{
		marshalAs(t, doc, howett.BinaryFormat), marshalAs(t, doc, howett.XMLFormat),
		nestedPlist(4, 2), int16Plist(0, 5),
	}
}

func TestDecodeNeverReturnsErrInternalOnGeneratedInputs(t *testing.T) {
	corpus := mutations(mutationSeeds(t), 2000)
	var accepted int
	for i, b := range corpus {
		v, err := Decode(b, newView(1<<30))
		if errors.Is(err, ErrInternal) {
			t.Fatalf("input %d reached the recover guard: %v\n%q", i, err, b)
		}
		if err == nil {
			accepted++
			if v == nil {
				t.Fatalf("input %d: nil value without error", i)
			}
		}
		// the same input without the guard: a panic here is a bug fixed by validating first
		if _, err := decodeCore(b, newView(1<<30)); errors.Is(err, ErrInternal) {
			t.Fatalf("input %d: %v", i, err)
		}
	}
	if accepted == 0 || accepted == len(corpus) {
		t.Fatalf("corpus is degenerate: %d of %d accepted", accepted, len(corpus))
	}
}
