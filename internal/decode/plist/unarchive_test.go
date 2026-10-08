package plist

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	howett "howett.net/plist"

	"github.com/rbenzing/minutiae/internal/parse"
)

func classRec(name string) map[string]any {
	return map[string]any{"$classname": name, "$classes": []any{name, "NSObject"}}
}

// archive builds the plain value of an archive whose $top is {root: UID(rootIdx)}; objs
// start at index 1 ($objects[0] is "$null").
func archive(rootIdx uint64, objs ...any) map[string]any {
	return map[string]any{
		"$archiver": "NSKeyedArchiver",
		"$version":  int64(100000),
		"$top":      map[string]any{"root": UID(rootIdx)},
		"$objects":  append([]any{"$null"}, objs...),
	}
}

func unarchive(t *testing.T, v any) any {
	t.Helper()
	out, err := Unarchive(v, newView(1<<30))
	if err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	return out
}

func wantErr(t *testing.T, v any, target error) {
	t.Helper()
	view := newView(1 << 30)
	out, err := Unarchive(v, view)
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
	if out != nil || view.Used() != 0 {
		t.Fatalf("returned %v, used %d with an error", out, view.Used())
	}
}

func TestUnarchiveRootString(t *testing.T) {
	if got := unarchive(t, archive(1, "hello")); got != any("hello") {
		t.Fatalf("%#v", got)
	}
	// NSString class form
	got := unarchive(t, archive(1, map[string]any{"$class": UID(2), "NS.string": "x"}, classRec("NSMutableString")))
	if got != any("x") {
		t.Fatalf("NSString: %#v", got)
	}
	// more than one $top entry: a map
	a := archive(1, "a", "b")
	a["$top"] = map[string]any{"root": UID(1), "other": UID(2)}
	if got := unarchive(t, a); !reflect.DeepEqual(got, map[string]any{"root": "a", "other": "b"}) {
		t.Fatalf("two tops: %#v", got)
	}
	// a single entry that is not named root is still a map
	a["$top"] = map[string]any{"x": UID(1)}
	if got := unarchive(t, a); !reflect.DeepEqual(got, map[string]any{"x": "a"}) {
		t.Fatalf("one non-root top: %#v", got)
	}
}

func TestUnarchiveArrayDictSet(t *testing.T) {
	objs := []any{
		/*1*/ map[string]any{"$class": UID(2), "NS.objects": []any{UID(5), UID(6), UID(0)}},
		/*2*/ classRec("NSMutableArray"),
		/*3*/ map[string]any{"$class": UID(4), "NS.keys": []any{UID(7)}, "NS.objects": []any{UID(5)}},
		/*4*/ classRec("NSDictionary"),
		/*5*/ "one",
		/*6*/ int64(2),
		/*7*/ "k",
		/*8*/ map[string]any{"$class": UID(9), "NS.objects": []any{UID(3)}},
		/*9*/ classRec("NSSet"),
		/*10*/ map[string]any{"$class": UID(11), "NS.data": []byte{1, 2}},
		/*11*/ classRec("NSData"),
	}
	a := archive(1, objs...)
	a["$top"] = map[string]any{"arr": UID(1), "dict": UID(3), "set": UID(8), "data": UID(10)}
	want := map[string]any{
		"arr":  []any{"one", int64(2), nil},
		"dict": map[string]any{"k": "one"},
		"set":  []any{map[string]any{"k": "one"}},
		"data": []byte{1, 2},
	}
	if got := unarchive(t, a); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveDuplicateDictKeyLastWins(t *testing.T) {
	a := archive(1,
		map[string]any{"$class": UID(2), "NS.keys": []any{UID(3), UID(3)}, "NS.objects": []any{UID(4), UID(5)}},
		classRec("NSDictionary"), "k", "first", "second")
	if got := unarchive(t, a); !reflect.DeepEqual(got, map[string]any{"k": "second"}) {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveNSDateKeepsRaw(t *testing.T) {
	a := archive(1, map[string]any{"$class": UID(2), "NS.time": float64(7.5e8)}, classRec("NSDate"))
	want := map[string]any{"$class": "NSDate", "NS.time": float64(7.5e8)}
	got := unarchive(t, a)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	if _, isTime := got.(time.Time); isTime {
		t.Fatal("built a time")
	}
	a = archive(1, map[string]any{"$class": UID(2), "NS.relative": UID(3), "NS.base": UID(0)}, classRec("NSURL"), "http://x/")
	want = map[string]any{"$class": "NSURL", "NS.base": nil, "NS.relative": "http://x/"}
	if got := unarchive(t, a); !reflect.DeepEqual(got, want) {
		t.Fatalf("NSURL: %#v", got)
	}
}

func TestUnarchiveUnknownClassBecomesMap(t *testing.T) {
	a := archive(1,
		map[string]any{"$class": UID(2), "name": UID(3), "n": int64(4), "$flag": true, "list": []any{UID(3)}},
		classRec("MyThing"), "bob")
	want := map[string]any{"$class": "MyThing", "name": "bob", "n": int64(4), "$flag": true, "list": []any{"bob"}}
	if got := unarchive(t, a); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveNull(t *testing.T) {
	a := archive(0)
	if got := unarchive(t, a); got != nil {
		t.Fatalf("%#v", got)
	}
	// only index 0 is $null; the string elsewhere is a string
	a = archive(1, "$null")
	if got := unarchive(t, a); got != any("$null") {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveCycle(t *testing.T) {
	self := archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{UID(1)}}, classRec("NSArray"))
	wantErr(t, self, ErrMalformed)
	_, err := Unarchive(self, newView(1<<20))
	if err == nil || !strings.Contains(err.Error(), "object 1") {
		t.Fatalf("text does not name the object: %v", err)
	}
	two := archive(1,
		map[string]any{"$class": UID(3), "NS.objects": []any{UID(2)}},
		map[string]any{"$class": UID(3), "NS.objects": []any{UID(1)}},
		classRec("NSArray"))
	wantErr(t, two, ErrMalformed)
}

func TestUnarchiveSharedObjectExpansionCap(t *testing.T) {
	// object i (i>=2) is an array referencing object i-1 twice; object 1 is "x".
	objs := []any{"x"}
	const levels = 40
	classIdx := uint64(levels + 2)
	for i := 2; i <= levels+1; i++ {
		objs = append(objs, map[string]any{"$class": UID(classIdx), "NS.objects": []any{UID(i - 1), UID(i - 1)}})
	}
	objs = append(objs, classRec("NSArray"))
	a := archive(uint64(levels+1), objs...)
	start := time.Now()
	wantErr(t, a, ErrLimit)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	// a small diamond is fine and shares the value
	small := archive(3,
		"x",
		map[string]any{"$class": UID(4), "NS.objects": []any{UID(1), UID(1)}},
		map[string]any{"$class": UID(4), "NS.objects": []any{UID(2), UID(2)}},
		classRec("NSArray"))
	got := unarchive(t, small).([]any)
	if len(got) != 2 || len(got[0].([]any)) != 2 {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveSharedObjectResolvedOnce(t *testing.T) {
	a := archive(1,
		map[string]any{"$class": UID(2), "NS.objects": []any{UID(3), UID(4), UID(3)}},
		classRec("NSArray"), "shared",
		map[string]any{"$class": UID(2), "NS.objects": []any{UID(3)}})
	got := unarchive(t, a).([]any)
	if got[0] != any("shared") || got[2] != any("shared") {
		t.Fatalf("%#v", got)
	}
	if inner := got[1].([]any); inner[0] != any("shared") {
		t.Fatalf("%#v", got)
	}
	// the same object is resolved once: a repeated reference yields the very same slice
	b := archive(1,
		map[string]any{"$class": UID(2), "NS.objects": []any{UID(3), UID(3)}},
		classRec("NSArray"),
		map[string]any{"$class": UID(2), "NS.objects": []any{UID(0)}})
	pair := unarchive(t, b).([]any)
	x, y := pair[0].([]any), pair[1].([]any)
	if len(x) != 1 || &x[0] != &y[0] {
		t.Fatalf("not shared: %#v", pair)
	}
}

func TestUnarchiveBadUIDs(t *testing.T) {
	wantErr(t, archive(5, "a"), ErrMalformed)
	wantErr(t, archive(1<<63, "a"), ErrMalformed)
	wantErr(t, archive(^uint64(0), "a"), ErrMalformed)
	// a reference inside an object
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{UID(9)}}, classRec("NSArray")), ErrMalformed)
	// $top entry that is not a UID
	a := archive(1, "a")
	a["$top"] = map[string]any{"root": int64(1)}
	wantErr(t, a, ErrMalformed)
	a["$top"] = map[string]any{"root": int64(-1)}
	wantErr(t, a, ErrMalformed)
}

func TestUnarchiveMalformedClassRecords(t *testing.T) {
	wantErr(t, archive(1, map[string]any{"$class": "NSArray"}), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2)}, map[string]any{"$classes": []any{"A"}}), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2)}, map[string]any{"$classname": int64(3)}), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2)}, "not a record"), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"name": "no class"}), ErrMalformed)
	wantErr(t, archive(1, []any{"x"}), ErrMalformed)
	wantErr(t, archive(1, UID(1)), ErrMalformed)
	// $classes missing is tolerated
	ok := archive(1, map[string]any{"$class": UID(2), "a": int64(1)}, map[string]any{"$classname": "Thing"})
	if got := unarchive(t, ok); !reflect.DeepEqual(got, map[string]any{"$class": "Thing", "a": int64(1)}) {
		t.Fatalf("%#v", got)
	}
}

func TestUnarchiveMissingParts(t *testing.T) {
	good := func() map[string]any { return archive(1, "a") }
	drop := func(k string) map[string]any { m := good(); delete(m, k); return m }
	wantErr(t, drop("$top"), ErrMalformed)
	wantErr(t, drop("$objects"), ErrMalformed)
	wantErr(t, drop("$archiver"), ErrMalformed)
	wantErr(t, drop("$version"), ErrMalformed)
	m := good()
	m["$objects"] = "nope"
	wantErr(t, m, ErrMalformed)
	m = good()
	m["$top"] = []any{}
	wantErr(t, m, ErrMalformed)
	m = good()
	m["$version"] = "100000"
	wantErr(t, m, ErrMalformed)
	for _, other := range []any{"NSArchiver", "nskeyedarchiver", "NSKeyedArchiver ", "X"} {
		m = good()
		m["$archiver"] = other
		wantErr(t, m, ErrUnsupported)
	}
	m = good()
	m["$archiver"] = int64(1)
	wantErr(t, m, ErrMalformed)
}

func TestUnarchiveNSDictionaryMismatch(t *testing.T) {
	dict := func(keys, vals []any) map[string]any {
		return archive(1, map[string]any{"$class": UID(2), "NS.keys": keys, "NS.objects": vals}, classRec("NSDictionary"), "k", "v", int64(5))
	}
	wantErr(t, dict([]any{UID(3)}, []any{UID(4), UID(4)}), ErrMalformed)
	wantErr(t, dict([]any{UID(5)}, []any{UID(4)}), ErrUnsupported) // key resolves to an integer
	wantErr(t, dict([]any{UID(0)}, []any{UID(4)}), ErrUnsupported) // key resolves to null
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.objects": []any{}}, classRec("NSDictionary")), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2)}, classRec("NSArray")), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.objects": "x"}, classRec("NSArray")), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.data": "x"}, classRec("NSData")), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.string": int64(1)}, classRec("NSString")), ErrMalformed)
	wantErr(t, archive(1, map[string]any{"$class": UID(2), "NS.time": "x"}, classRec("NSDate")), ErrMalformed)
}

func TestUnarchiveDepthCap(t *testing.T) {
	chain := func(n int) map[string]any {
		var objs []any
		classIdx := uint64(n + 1)
		objs = append(objs, "leaf")
		for i := 2; i <= n; i++ {
			objs = append(objs, map[string]any{"$class": UID(classIdx), "NS.objects": []any{UID(i - 1)}})
		}
		objs = append(objs, classRec("NSArray"))
		return archive(uint64(n), objs...)
	}
	wantErr(t, chain(70), ErrLimit)
	if got := unarchive(t, chain(MaxUnarchiveDepth-2)); got == nil {
		t.Fatal("a chain within the cap failed")
	}
}

func TestUnarchiveBudgetCharged(t *testing.T) {
	a := archive(1, "hello")
	view := newView(1 << 20)
	if out, err := Unarchive(a, view); err != nil || out == nil || view.Used() <= 0 {
		t.Fatalf("%v %v used %d", out, err, view.Used())
	}
	small := newView(16)
	out, err := Unarchive(a, small)
	if !errors.Is(err, parse.ErrBudget) || !errors.Is(err, ErrNoBudget) {
		t.Fatalf("err = %v", err)
	}
	if out != nil || small.Used() != 0 {
		t.Fatalf("returned %v, used %d", out, small.Used())
	}
	// freed on a later failure too
	bad := archive(1, "hello", map[string]any{"$class": UID(3)})
	bad["$top"] = map[string]any{"a": UID(1), "b": UID(2)}
	view = newView(1 << 20)
	if _, err := Unarchive(bad, view); err == nil || view.Used() != 0 {
		t.Fatalf("%v used %d", err, view.Used())
	}
}

func TestUnarchiveNilBudgetFailsClosed(t *testing.T) {
	out, err := Unarchive(archive(1, "a"), nil)
	if !errors.Is(err, ErrNoBudget) || out != nil {
		t.Fatalf("%v %v", out, err)
	}
}

func TestUnarchiveNonMapInput(t *testing.T) {
	for _, v := range []any{nil, []any{}, "x", int64(1), map[string]any{}} {
		wantErr(t, v, ErrMalformed)
	}
}

func TestUnarchiveFromDecodedXMLAndBinary(t *testing.T) {
	type obj = map[string]any
	doc := obj{
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
	want := map[string]any{"name": []any{"v", nil}}
	for _, format := range []int{howett.BinaryFormat, howett.XMLFormat} {
		decoded := mustDecode(t, marshalAs(t, doc, format))
		if got := unarchive(t, decoded); !reflect.DeepEqual(got, want) {
			t.Fatalf("format %d: %#v", format, got)
		}
	}
}
