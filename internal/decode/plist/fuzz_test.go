package plist

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
	"unicode/utf8"

	howett "howett.net/plist"
)

const fuzzMaxInput = 1 << 16

// fuzzGuard fails the test when one input takes absurdly long: the engine reports a hang as a
// timeout, this reports it with the input's size.
func fuzzGuard(t *testing.T, n int) func() {
	start := time.Now()
	return func() {
		if d := time.Since(start); d > 20*time.Second {
			t.Fatalf("input of %d bytes took %v", n, d)
		}
	}
}

// typedErr reports whether err wraps one of the three validation sentinels (or, with budget,
// ErrNoBudget) and not ErrInternal.
func typedErr(err error, budget bool) bool {
	if errors.Is(err, ErrInternal) {
		return false
	}
	for _, s := range []error{ErrMalformed, ErrLimit, ErrUnsupported} {
		if errors.Is(err, s) {
			return true
		}
	}
	return budget && errors.Is(err, ErrNoBudget)
}

// nanMark stands for a NaN in canon: reflect.DeepEqual is false for NaN against itself.
type nanMark struct{}

// canon returns v with every NaN float64 and every RawDate NaN replaced by nanMark, so that
// reflect.DeepEqual can compare two decodes of the same bytes.
func canon(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = canon(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canon(e)
		}
		return out
	case float64:
		if math.IsNaN(x) {
			return nanMark{}
		}
	case RawDate:
		if math.IsNaN(x.Seconds) {
			return struct{ NaNDate nanMark }{}
		}
	}
	return v
}

func sameValue(a, b any) bool { return reflect.DeepEqual(canon(a), canon(b)) }

// rawStringJustified reports whether a Go string could not have held x: its 8-bit bytes are
// not valid UTF-8, or it is UTF-16 with a surrogate unit that a Go string cannot carry (a lone
// one in either format; in XML a character reference to a surrogate, even one of a valid
// pair, is kept raw by design).
func rawStringJustified(x RawString, xml bool) bool {
	if !x.UTF16 {
		return !utf8.Valid(x.Bytes)
	}
	if len(x.Bytes)%2 != 0 {
		return false
	}
	units := make([]uint16, len(x.Bytes)/2)
	for i := range units {
		units[i] = uint16(x.Bytes[2*i])<<8 | uint16(x.Bytes[2*i+1])
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u < 0xDC00 && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] < 0xE000:
			if xml {
				return true
			}
			i++
		case u >= 0xD800 && u < 0xE000:
			return true
		}
	}
	return false
}

// rawDateJustified reports whether a time.Time (years 1 to 9999) could not have held x: NaN,
// an infinity, or seconds clearly outside the range (one second of slack at each end for the
// float rounding of the boundary).
func rawDateJustified(x RawDate) bool {
	if math.IsNaN(x.Seconds) || math.IsInf(x.Seconds, 0) {
		return true
	}
	const cocoa = 978307200
	lo := float64(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - cocoa)
	hi := float64(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - cocoa)
	return x.Seconds < lo+1 || x.Seconds >= hi-1
}

func fuzzMarshal(v any, format int) []byte {
	b, err := howett.Marshal(v, format)
	if err != nil {
		panic(err)
	}
	return b
}

func richDoc() map[string]any {
	return map[string]any{
		"s": "héllo \U0001F600", "i": int64(-42), "u": uint64(1) << 63, "f": 2.5, "t": true,
		"d": time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC), "b": []byte{1, 2, 3},
		"arr": []any{int64(1), "x", []any{map[string]any{"k": howett.UID(2)}}},
	}
}

func archiveDoc() map[string]any {
	type obj = map[string]any
	return obj{
		"$archiver": "NSKeyedArchiver",
		"$version":  uint64(100000),
		"$top":      obj{"root": howett.UID(1)},
		"$objects": []any{
			"$null",
			obj{"$class": howett.UID(2), "NS.keys": []any{howett.UID(3)}, "NS.objects": []any{howett.UID(4)}},
			obj{"$classname": "NSDictionary", "$classes": []any{"NSDictionary", "NSObject"}},
			"name",
			obj{"$class": howett.UID(5), "NS.objects": []any{howett.UID(6), howett.UID(0)}},
			obj{"$classname": "NSArray", "$classes": []any{"NSArray", "NSObject"}},
			"v",
		},
	}
}

// toLib swaps the package's UID for the library's so the library marshals a CF$UID.
func toLib(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = toLib(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toLib(e)
		}
		return out
	case UID:
		return howett.UID(x)
	}
	return v
}

// archiveSeeds are archives in both formats, plus a cyclic one.
func archiveSeeds() [][]byte {
	cyc := archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{UID(1)}}, classRec("NSArray"))
	return [][]byte{
		fuzzMarshal(archiveDoc(), howett.BinaryFormat),
		fuzzMarshal(archiveDoc(), howett.XMLFormat),
		fuzzMarshal(toLib(cyc), howett.BinaryFormat),
		fuzzMarshal(toLib(cyc), howett.XMLFormat),
	}
}

func truncations(seeds [][]byte) [][]byte {
	var out [][]byte
	for _, s := range seeds {
		for _, n := range []int{1, 2, 7, 8, 9, 16, len(s) / 3, len(s) / 2, len(s) - 33, len(s) - 8, len(s) - 1} {
			if n > 0 && n < len(s) {
				out = append(out, s[:n])
			}
		}
	}
	return out
}

func binarySeeds() [][]byte {
	return [][]byte{
		nestedPlist(8, 2),
		nestedPlist(40, 8),
		nestedTyped(10, 3, 0xD0),
		payloadBomb(),
		objectsPlist(refArray(0)),
		objectsPlist(refArray(1), refArray(0)),
		objectsPlist(dataObj(20)),
		objectsPlist(asciiObj("a\xffb")),
		objectsPlist(utf16Obj(0xD800)),
		objectsPlist(dateObj(math.NaN())),
		objectsPlist([]byte{0xD1, 1, 2}, asciiObj("k"), utf16Obj(0xDC00, 0x41)),
		rawPlist(bigCount(0xA, math.MaxUint64), []byte{0}),
		rawPlist(bigCount(0x5, 1<<62), []byte{0}),
		rawPlist(bigCount(0xD, 1<<40), []byte{0}),
		int16Plist(0, 5),
		fuzzMarshal(richDoc(), howett.BinaryFormat),
		fuzzMarshal(archiveDoc(), howett.BinaryFormat),
	}
}

func xmlSeeds() [][]byte {
	return [][]byte{
		fuzzMarshal(richDoc(), howett.XMLFormat),
		fuzzMarshal(archiveDoc(), howett.XMLFormat),
		append([]byte("\xef\xbb\xbf"), fuzzMarshal(richDoc(), howett.XMLFormat)...),
		[]byte(`<plist version="1.0"><array><string>a&#xD800;</string><date>0000-01-01T00:00:00Z</date></array></plist>`),
		[]byte(`<plist><dict><key>CF$UID</key><integer>-1</integer></dict></plist>`),
		[]byte(`<plist><dict><key>CF$UID</key><integer>3</integer></dict></plist>`),
		[]byte(`<?xml version="1.0"?><!DOCTYPE plist [<!ENTITY a "b">]><plist><string>&a;</string></plist>`),
		[]byte(`<plist><array><array><array><array></array></array></array></array></plist>`),
		[]byte(`<plist><string><![CDATA[x]]><!-- c --></string></plist>`),
		[]byte(`<plist><string>&#0;</string></plist>`),
		[]byte(`<plist><real>nan</real></plist>`),
		[]byte(`<plist><`),
	}
}

// refusalSeeds are inputs the format gate must refuse or the validators reject.
func refusalSeeds() [][]byte {
	return [][]byte{
		{},
		{0},
		{'<'},
		{'b'},
		[]byte("not a plist"),
		[]byte("{a=b;}"), []byte("(1,2)"), []byte("<01ab>"), []byte(`"quoted"`), []byte("<*I5>"),
		[]byte("bplist00"), []byte("bplist15"), []byte("bplist00\x00"),
		[]byte("<?xml version=\"1.0\" encoding=\"UTF-16\"?><plist/>"),
		[]byte(" <?xml version=\"1.0\"?><plist><true/></plist>"),
		[]byte(`<plist><data>bplist00</data></plist>`),
	}
}

func addSeeds(f *testing.F, sets ...[][]byte) {
	for _, set := range sets {
		for _, s := range set {
			f.Add(s)
		}
	}
}

// checkRun runs a validator twice and holds it to its contract: a typed error, no ErrInternal,
// the same answer both times, and an accepted document inside the default caps.
func checkRun(t *testing.T, run func() (uint64, uint64, error)) bool {
	t.Helper()
	n1, p1, err := run()
	n2, p2, err2 := run()
	if (err == nil) != (err2 == nil) || n1 != n2 || p1 != p2 {
		t.Fatalf("not deterministic: (%d,%d,%v) vs (%d,%d,%v)", n1, p1, err, n2, p2, err2)
	}
	if err != nil {
		if !typedErr(err, false) {
			t.Fatalf("untyped or internal error: %v", err)
		}
		return false
	}
	l := DefaultLimits()
	if n1 > l.MaxNodes || p1 > l.MaxPayload {
		t.Fatalf("accepted document over the caps: %d nodes, %d payload", n1, p1)
	}
	return true
}

func FuzzCheckBinary(f *testing.F) {
	addSeeds(f, binarySeeds(), refusalSeeds(), truncations(binarySeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		for _, in := range [][]byte{b, append([]byte(bplistMagic), b...)} {
			checkRun(t, func() (uint64, uint64, error) {
				if err := Check(in, Limits{}); err != nil {
					return 0, 0, err
				}
				return measure(in, Limits{})
			})
		}
	})
}

func FuzzCheckBinaryNoRecover(f *testing.F) {
	addSeeds(f, binarySeeds(), refusalSeeds(), truncations(binarySeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		for _, in := range [][]byte{b, append([]byte(bplistMagic), b...)} {
			checkRun(t, func() (uint64, uint64, error) { return measureCore(in, DefaultLimits()) })
		}
	})
}

func FuzzPrescanXML(f *testing.F) {
	addSeeds(f, xmlSeeds(), refusalSeeds(), truncations(xmlSeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		checkRun(t, func() (uint64, uint64, error) { return 0, 0, PrescanXML(b, Limits{}) })
	})
}

func FuzzPrescanXMLNoRecover(f *testing.F) {
	addSeeds(f, xmlSeeds(), refusalSeeds(), truncations(xmlSeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		checkRun(t, func() (uint64, uint64, error) { return prescanCore(b, DefaultLimits()) })
	})
}

// decodeSeeds are the seeds of every Decode target; the first nine are the ones FuzzDecode
// has carried since Task 6.
func decodeSeeds() [][]byte {
	first := [][]byte{
		objectsPlist(asciiObj("a\xffb")),
		objectsPlist(utf16Obj(0xD800)),
		objectsPlist(dateObj(math.NaN())),
		objectsPlist([]byte{0xD1, 1, 2}, asciiObj("k"), utf16Obj(0xDC00, 0x41)),
		nestedPlist(8, 2),
		[]byte(`<plist version="1.0"><array><string>a&#xD800;</string><date>0000-01-01T00:00:00Z</date></array></plist>`),
		[]byte(`<plist><dict><key>CF$UID</key><integer>-1</integer></dict></plist>`),
		[]byte(`<plist><dict><key>CF$UID</key><integer>3</integer></dict></plist>`),
		[]byte("not a plist"),
	}
	all := append(binarySeeds(), xmlSeeds()...)
	all = append(all, refusalSeeds()...)
	all = append(all, archiveSeeds()...)
	all = append(all, truncations(all)...)
	return append(first, all...)
}

// walkPlain fails when v holds a type outside the documented value set (the third-party
// plist.UID included), a string that is not valid UTF-8 or a date outside years 1 to 9999,
// and bounds the walk at 1<<20 nodes.
func walkPlain(t *testing.T, v any, xml bool) {
	t.Helper()
	left := 1 << 20
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if left--; left < 0 {
			t.Fatalf("value walk exceeds 1<<20 nodes")
		}
		if depth > 4096 {
			t.Fatalf("value nests deeper than 4096")
		}
		switch x := v.(type) {
		case nil:
		case map[string]any:
			for k, e := range x {
				if !utf8.ValidString(k) {
					t.Fatalf("invalid key %q", k)
				}
				walk(e, depth+1)
			}
		case []any:
			for _, e := range x {
				walk(e, depth+1)
			}
		case string:
			if !utf8.ValidString(x) {
				t.Fatalf("invalid string %q", x)
			}
		case time.Time:
			if y := x.Year(); y < 1 || y > 9999 {
				t.Fatalf("date year %d", y)
			}
		case RawString:
			if !rawStringJustified(x, xml) {
				t.Fatalf("RawString for a string a Go string holds: %#v", x)
			}
		case RawDate:
			if !rawDateJustified(x) {
				t.Fatalf("RawDate for a date time.Time holds: %v", x.Seconds)
			}
		case float64, int64, uint64, bool, []byte, UID:
		default:
			t.Fatalf("unexpected %T", v)
		}
	}
	walk(v, 0)
}

// decodeContract holds one Decode-like call to the shared invariants.
func decodeContract(t *testing.T, b []byte, call func(Budget) (any, error)) {
	t.Helper()
	run := func() (any, int64, error) {
		view := newView(256 << 20)
		v, err := call(view)
		return v, view.Used(), err
	}
	v, used, err := run()
	if err != nil {
		if v != nil || used != 0 {
			t.Fatalf("error %v but value %v, used %d", err, v, used)
		}
		if !typedErr(err, true) {
			t.Fatalf("untyped or internal error: %v", err)
		}
		return
	}
	if cerr := Check(b, Limits{}); cerr != nil {
		t.Fatalf("Decode accepted bytes Check refuses: %v", cerr)
	}
	walkPlain(t, v, LooksLikeXML(b))
	v2, used2, err2 := run()
	if err2 != nil || used2 != used || !sameValue(v, v2) {
		t.Fatalf("not deterministic")
	}
}

func FuzzDecode(f *testing.F) {
	addSeeds(f, decodeSeeds())
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		decodeContract(t, b, func(bu Budget) (any, error) { return Decode(b, bu) })
	})
}

func FuzzDecodeNoRecover(f *testing.F) {
	addSeeds(f, decodeSeeds())
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		decodeContract(t, b, func(bu Budget) (any, error) { return decodeCore(b, bu) })
	})
}

// unarchiveContract decodes b first and ignores what is not an archive.
func unarchiveContract(t *testing.T, b []byte, call func(any, Budget) (any, error)) {
	t.Helper()
	v, err := Decode(b, newView(256<<20))
	if err != nil {
		return
	}
	run := func() (any, int64, error) {
		view := newView(256 << 20)
		out, err := call(v, view)
		return out, view.Used(), err
	}
	out, used, err := run()
	if err != nil {
		if out != nil || used != 0 {
			t.Fatalf("error %v but value %v, used %d", err, out, used)
		}
		if !typedErr(err, true) {
			t.Fatalf("untyped or internal error: %v", err)
		}
		return
	}
	walkPlain(t, out, LooksLikeXML(b))
	out2, used2, err2 := run()
	if err2 != nil || used2 != used || !sameValue(out, out2) {
		t.Fatalf("not deterministic")
	}
}

func FuzzUnarchive(f *testing.F) {
	addSeeds(f, decodeSeeds(), archiveSeeds(), truncations(archiveSeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		unarchiveContract(t, b, Unarchive)
	})
}

func FuzzUnarchiveNoRecover(f *testing.F) {
	addSeeds(f, decodeSeeds(), archiveSeeds(), truncations(archiveSeeds()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzMaxInput {
			return
		}
		defer fuzzGuard(t, len(b))()
		unarchiveContract(t, b, unarchiveCore)
	})
}

func TestRawJustification(t *testing.T) {
	if rawStringJustified(RawString{Bytes: []byte("abc")}, false) || !rawStringJustified(RawString{Bytes: []byte{0xff}}, false) {
		t.Fatal("8-bit rule")
	}
	lone := RawString{Bytes: []byte{0xD8, 0x00}, UTF16: true}
	pair := RawString{Bytes: []byte{0xD8, 0x3D, 0xDE, 0x00}, UTF16: true}
	plain := RawString{Bytes: []byte{0x00, 0x41}, UTF16: true}
	if !rawStringJustified(lone, false) || rawStringJustified(plain, false) || rawStringJustified(pair, false) || !rawStringJustified(pair, true) {
		t.Fatal("UTF-16 rule")
	}
	if !rawDateJustified(RawDate{math.NaN()}) || !rawDateJustified(RawDate{math.Inf(-1)}) || !rawDateJustified(RawDate{1e13}) || rawDateJustified(RawDate{7e8}) {
		t.Fatal("date rule")
	}
	if sameValue([]any{1.0}, []any{2.0}) || !sameValue([]any{math.NaN(), RawDate{math.NaN()}}, []any{math.NaN(), RawDate{math.NaN()}}) {
		t.Fatal("sameValue")
	}
}
