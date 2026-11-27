package tl

import (
	"bytes"
	"errors"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// The synthetic table of the walker tests (plan 4I4 Task 3): it holds no Telegram fact.
//
//	point{flags:#, [F.0 x:i32], y:i64, z=F.1 true}                                 Point
//	box{flags:#, [F.0 inner:Point], items:vec<Point>, ids:vec<i64>, key:secret,
//	    who:id32@user}                                                             Box
//	node{next:Node}                                                                Node (self-recursive)
//	half{flags:#, a:i32}                                                           Point, BodyPrefix
//	blank                                                                          Point, BodyOpaque
//	kinds{b:Bool, d:double, s:string, raw:bytes, v32:vec<i32>, cid:id64@channel}   Kinds
//	crate{boxes:vec<Box>, nodes:vec<Node>}                                         Crate (vectors of objects with vectors)
//	left{a:i32}                                                                    Left
//	right{b:i64}                                                                   Right
//	twin{x:i32} as Left, twin{y:i64} as Right                                      one ID in two types (W1)
//	shared as Left (BodyOpaque), shared{c:i32} as Right                            one ID, opaque in the first type
//	pair{l:Left, r:Right}                                                          Pair
//	mixed{u:vec<Left|Right>}                                                       Mixed (a union vector, W2)
//	bare{ids32:vec<id32@user>, ids64:vec<id64@channel>, strs:vec<str>}             Bare (bare vectors, W3)
const (
	idPoint  uint32 = 0x0a0b0c01
	idBox    uint32 = 0x0a0b0c02
	idNode   uint32 = 0x0a0b0c03
	idHalf   uint32 = 0x0a0b0c04
	idBlank  uint32 = 0x0a0b0c05
	idKinds  uint32 = 0x0a0b0c06
	idCrate  uint32 = 0x0a0b0c07
	idLeft   uint32 = 0x0a0b0c08
	idRight  uint32 = 0x0a0b0c09
	idTwin   uint32 = 0x0a0b0c0a
	idShared uint32 = 0x0a0b0c0b
	idPair   uint32 = 0x0a0b0c0c
	idMixed  uint32 = 0x0a0b0c0d
	idBare   uint32 = 0x0a0b0c0e
)

func synthTable() Table {
	flags := Field{Name: "flags", Kind: KFlags}
	return Table{
		{Type: "Point", ID: idPoint}: {ID: idPoint, Name: "point", Type: "Point", Body: BodyFull, Source: "test", Fields: []Field{
			flags,
			{Name: "x", Kind: KInt32, Flags: "flags", Bit: 0},
			{Name: "y", Kind: KInt64},
			{Name: "z", Kind: KTrue, Flags: "flags", Bit: 1},
		}},
		{Type: "Box", ID: idBox}: {ID: idBox, Name: "box", Type: "Box", Body: BodyFull, Source: "test", Fields: []Field{
			flags,
			{Name: "inner", Kind: KObject, Of: "Point", Flags: "flags", Bit: 0},
			{Name: "items", Kind: KVector, Of: "Point"},
			{Name: "ids", Kind: KVecInt64},
			{Name: "key", Kind: KSecret},
			{Name: "who", Kind: KID32, Class: "user"},
		}},
		{Type: "Node", ID: idNode}: {ID: idNode, Name: "node", Type: "Node", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "next", Kind: KObject, Of: "Node"},
		}},
		{Type: "Point", ID: idHalf}: {ID: idHalf, Name: "half", Type: "Point", Body: BodyPrefix, Source: "test", Fields: []Field{
			flags,
			{Name: "a", Kind: KInt32},
		}},
		{Type: "Point", ID: idBlank}: {ID: idBlank, Name: "blank", Type: "Point", Body: BodyOpaque, Source: "test"},
		{Type: "Kinds", ID: idKinds}: {ID: idKinds, Name: "kinds", Type: "Kinds", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "b", Kind: KBool},
			{Name: "d", Kind: KDouble},
			{Name: "s", Kind: KString},
			{Name: "raw", Kind: KBytes},
			{Name: "v32", Kind: KVecInt32},
			{Name: "cid", Kind: KID64, Class: "channel"},
		}},
		{Type: "Crate", ID: idCrate}: {ID: idCrate, Name: "crate", Type: "Crate", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "boxes", Kind: KVector, Of: "Box"},
			{Name: "nodes", Kind: KVector, Of: "Node"},
		}},
		{Type: "Left", ID: idLeft}: {ID: idLeft, Name: "left", Type: "Left", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "a", Kind: KInt32},
		}},
		{Type: "Right", ID: idRight}: {ID: idRight, Name: "right", Type: "Right", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "b", Kind: KInt64},
		}},
		{Type: "Left", ID: idTwin}: {ID: idTwin, Name: "twinLeft", Type: "Left", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "x", Kind: KInt32},
		}},
		{Type: "Right", ID: idTwin}: {ID: idTwin, Name: "twinRight", Type: "Right", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "y", Kind: KInt64},
		}},
		{Type: "Left", ID: idShared}: {ID: idShared, Name: "sharedLeft", Type: "Left", Body: BodyOpaque, Source: "test"},
		{Type: "Right", ID: idShared}: {ID: idShared, Name: "sharedRight", Type: "Right", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "c", Kind: KInt32},
		}},
		{Type: "Pair", ID: idPair}: {ID: idPair, Name: "pair", Type: "Pair", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "l", Kind: KObject, Of: "Left"},
			{Name: "r", Kind: KObject, Of: "Right"},
		}},
		{Type: "Mixed", ID: idMixed}: {ID: idMixed, Name: "mixed", Type: "Mixed", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "u", Kind: KVector, Of: "Left", Alt: "Right"},
		}},
		{Type: "Bare", ID: idBare}: {ID: idBare, Name: "bare", Type: "Bare", Body: BodyFull, Source: "test", Fields: []Field{
			{Name: "ids32", Kind: KVecInt32, Class: "user"},
			{Name: "ids64", Kind: KVecInt64, Class: "channel"},
			{Name: "strs", Kind: KVecString},
		}},
	}
}

// tlBytes frames data as a TL string (spec section 6.5): canonical prefix, zero padding.
func tlBytes(data []byte) []byte {
	var out []byte
	if len(data) <= shortMax {
		out = append([]byte{byte(len(data))}, data...)
	} else {
		out = append([]byte{0xfe, byte(len(data)), byte(len(data) >> 8), byte(len(data) >> 16)}, data...)
	}
	return append(out, make([]byte, (4-len(out)%4)%4)...)
}

// pointBlob is a boxed point; x is written iff flags bit 0.
func pointBlob(flags uint32, x int32, y int64) []byte {
	b := cat(le32(idPoint), le32(flags))
	if flags&1 != 0 {
		b = cat(b, le32(uint32(x)))
	}
	return cat(b, le64(uint64(y)))
}

// boxBlob is a boxed box; inner (a boxed object) is written iff it is non-nil, with bit 0.
func boxBlob(inner []byte, items [][]byte, ids []int64, key []byte, who int32) []byte {
	var flags uint32
	if inner != nil {
		flags = 1
	}
	b := cat(le32(idBox), le32(flags), inner, le32(IDVector), le32(uint32(len(items))))
	for _, it := range items {
		b = append(b, it...)
	}
	b = append(b, cat(le32(IDVector), le32(uint32(len(ids))))...)
	for _, v := range ids {
		b = append(b, le64(uint64(v))...)
	}
	return append(b, cat(tlBytes(key), le32(uint32(who)))...)
}

// crateBlob is a boxed crate of n copies of box and the node elements given.
func crateBlob(box []byte, n int, nodes ...[]byte) []byte {
	b := cat(le32(idCrate), le32(IDVector), le32(uint32(n)), bytes.Repeat(box, n), le32(IDVector), le32(uint32(len(nodes))))
	for _, nd := range nodes {
		b = append(b, nd...)
	}
	return b
}

// pointsBox is a box holding p points of flags 0 and nothing else (6 + 3p reads).
func pointsBox(p int) []byte {
	items := make([][]byte, p)
	for i := range items {
		items[i] = pointBlob(0, 0, int64(i))
	}
	return boxBlob(nil, items, nil, nil, 0)
}

// wantCharge is the budget charge of r's fields computed by the test (I1): 128 bytes and the
// path's length per field.
func wantCharge(r Result) int64 {
	var n int64
	for _, f := range r.Fields {
		n += 128 + int64(len(f.Path))
	}
	return n
}

// fullBox is a box with every gated field set (TestTLDecodeReadsInTableOrder).
func fullBox() []byte {
	return boxBlob(pointBlob(3, 7, 9), [][]byte{pointBlob(0, 0, 1), pointBlob(0, 0, 2)}, []int64{10, 20}, bytes.Repeat([]byte{0x5a}, 32), 42)
}

// countBudget admits limit Allocs (any size), then refuses; it counts what it holds.
type countBudget struct {
	allocs, limit int
	held          int64
}

func (b *countBudget) Alloc(n int64) error {
	if b.allocs >= b.limit {
		return errors.New("refused")
	}
	b.allocs++
	b.held += n
	return nil
}

func (b *countBudget) Free(n int64) { b.held -= n }

func open() *countBudget { return &countBudget{limit: math.MaxInt} }

func paths(fs []Named) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

func field(t *testing.T, r Result, path string) Named {
	t.Helper()
	for _, f := range r.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no field %q in %v", path, paths(r.Fields))
	return Named{}
}

// Spec section 7.2: fields are read in table order; a gated field is read only when its bit
// is set; paths are dotted and indexed; KSecret keeps only its length (spec section 9);
// KID32/KID64 carry their class (T5); every Named charges 128 bytes and its path's length,
// and Decode never frees (F8, Task 3 review I1).
func TestTLDecodeReadsInTableOrder(t *testing.T) {
	blob := fullBox()
	bud := open()
	r := Decode(blob, "Box", synthTable(), bud)
	if r.Stop != nil {
		t.Fatalf("stop %+v", *r.Stop)
	}
	want := []string{
		"flags", "inner", "inner.flags", "inner.x", "inner.y", "inner.z",
		"items", "items[0]", "items[0].flags", "items[0].y", "items[1]", "items[1].flags", "items[1].y",
		"ids", "ids[0]", "ids[1]", "key", "who",
	}
	if got := paths(r.Fields); !slices.Equal(got, want) {
		t.Fatalf("paths\n got %v\nwant %v", got, want)
	}
	if r.Top != idBox || r.End != len(blob) || len(r.Flags) != 0 {
		t.Errorf("top %#x, end %d, flags %v; want %#x, %d, none", r.Top, r.End, r.Flags, idBox, len(blob))
	}
	if bud.held != wantCharge(r) || r.Charged() != bud.held {
		t.Errorf("budget holds %d, Charged %d; want %d", bud.held, r.Charged(), wantCharge(r))
	}
	if f := field(t, r, "key"); f.Kind != KSecret || f.Len != 32 || f.Bytes != nil {
		t.Errorf("key = %+v; want KSecret, Len 32, Bytes nil", f)
	}
	if f := field(t, r, "who"); f.Kind != KID32 || f.Int != 42 || f.Class != "user" {
		t.Errorf("who = %+v; want KID32 42 user", f)
	}
	if f := field(t, r, "inner"); f.Kind != KObject || f.ID != idPoint {
		t.Errorf("inner = %+v; want KObject %#x", f, idPoint)
	}
	if f := field(t, r, "inner.flags"); f.Kind != KFlags || f.Int != 3 {
		t.Errorf("inner.flags = %+v; want KFlags 3", f)
	}
	if f := field(t, r, "inner.x"); f.Kind != KInt32 || f.Int != 7 {
		t.Errorf("inner.x = %+v; want 7", f)
	}
	if f := field(t, r, "inner.z"); f.Kind != KTrue || !f.Bool {
		t.Errorf("inner.z = %+v; want KTrue true", f)
	}
	if f := field(t, r, "items"); f.Kind != KVector || f.Len != 2 {
		t.Errorf("items = %+v; want KVector Len 2", f)
	}
	if f := field(t, r, "items[1].y"); f.Kind != KInt64 || f.Int != 2 {
		t.Errorf("items[1].y = %+v; want 2", f)
	}
	if f := field(t, r, "ids"); f.Kind != KVecInt64 || f.Len != 2 {
		t.Errorf("ids = %+v; want KVecInt64 Len 2", f)
	}
	if f := field(t, r, "ids[1]"); f.Kind != KInt64 || f.Int != 20 {
		t.Errorf("ids[1] = %+v; want 20", f)
	}

	// Bit 0 clear: inner is absent and nothing is read for it.
	blob = boxBlob(nil, nil, nil, nil, 5)
	r = Decode(blob, "Box", synthTable(), open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("bit 0 clear: stop %v, end %d of %d", r.Stop, r.End, len(blob))
	}
	if want := []string{"flags", "items", "ids", "key", "who"}; !slices.Equal(paths(r.Fields), want) {
		t.Errorf("bit 0 clear: paths %v, want %v", paths(r.Fields), want)
	}
	// A point without x and z: neither is present.
	r = Decode(pointBlob(0, 0, -1), "Point", synthTable(), open())
	if want := []string{"flags", "y"}; r.Stop != nil || !slices.Equal(paths(r.Fields), want) {
		t.Errorf("point 0: stop %v, paths %v; want %v", r.Stop, paths(r.Fields), want)
	}

	// Every other kind, and trailing bytes left after the top object (End is where they start).
	kinds := cat(le32(idKinds), le32(IDBoolTrue), le64(math.Float64bits(2.5)), tlBytes([]byte("hi")),
		tlBytes([]byte{1, 2, 3}), le32(IDVector), le32(2), le32(uint32(0xffffffff)), le32(4), le64(uint64(1)<<40))
	r = Decode(cat(kinds, []byte{9, 9, 9, 9}), "Kinds", synthTable(), open())
	if r.Stop != nil || r.End != len(kinds) {
		t.Fatalf("kinds: stop %v, end %d; want nil, %d", r.Stop, r.End, len(kinds))
	}
	if f := field(t, r, "b"); f.Kind != KBool || !f.Bool {
		t.Errorf("b = %+v", f)
	}
	if f := field(t, r, "d"); f.Kind != KDouble || f.Float != 2.5 {
		t.Errorf("d = %+v", f)
	}
	if f := field(t, r, "s"); f.Kind != KString || string(f.Bytes) != "hi" || f.Len != 2 {
		t.Errorf("s = %+v", f)
	}
	if f := field(t, r, "raw"); f.Kind != KBytes || !bytes.Equal(f.Bytes, []byte{1, 2, 3}) || f.Len != 3 {
		t.Errorf("raw = %+v", f)
	}
	if f := field(t, r, "v32[0]"); f.Kind != KInt32 || f.Int != -1 {
		t.Errorf("v32[0] = %+v", f)
	}
	if f := field(t, r, "v32[1]"); f.Kind != KInt32 || f.Int != 4 {
		t.Errorf("v32[1] = %+v", f)
	}
	if f := field(t, r, "cid"); f.Kind != KID64 || f.Int != 1<<40 || f.Class != "channel" {
		t.Errorf("cid = %+v", f)
	}
}

// Spec section 7.2 and section 14 ("the walker never skips"): an ID the table lacks for the
// expected type ends the decode there, with what was read before it kept.
func TestTLDecodeStopsAtUnknownConstructor(t *testing.T) {
	const unknown uint32 = 0xdeadbeef
	r := Decode(cat(le32(unknown), make([]byte, 16)), "Box", synthTable(), open())
	if r.Stop == nil || r.Stop.Kind != StopUnknownConstructor || r.Top != unknown || len(r.Fields) != 0 {
		t.Fatalf("unknown top: %+v", r)
	}
	var ue *UnknownConstructorError
	if !errors.As(r.Stop.Err, &ue) || ue.ID != unknown || ue.Type != "Box" || ue.Offset != 0 || ue.Path != "" {
		t.Errorf("unknown top: err %v", r.Stop.Err)
	}
	if r.End != 0 {
		t.Errorf("unknown top: end %d, want 0", r.End)
	}

	// A known ID of another type at the top is unknown for the expected type.
	r = Decode(pointBlob(0, 0, 1), "Box", synthTable(), open())
	if r.Stop == nil || r.Stop.Kind != StopUnknownConstructor || r.Stop.Type != "Box" || r.Top != idPoint || len(r.Fields) != 0 {
		t.Errorf("point as Box: %+v", r)
	}

	// An unknown ID at items[1]: stop there; items[0] and its fields are kept; nothing after.
	bad := cat(le32(unknown), make([]byte, 12))
	blob := boxBlob(nil, [][]byte{pointBlob(0, 0, 1), bad}, []int64{1}, nil, 1)
	r = Decode(blob, "Box", synthTable(), open())
	s := r.Stop
	if s == nil || s.Kind != StopUnknownConstructor || s.Path != "items[1]" || s.ID != unknown || s.Type != "Point" {
		t.Fatalf("items[1]: stop %+v", s)
	}
	off := bytes.Index(blob, le32(unknown))
	if s.Offset != off || !errors.As(s.Err, &ue) || ue.Path != "items[1]" || ue.Offset != off {
		t.Errorf("items[1]: offset %d, err %v; want offset %d", s.Offset, s.Err, off)
	}
	if want := []string{"flags", "items", "items[0]", "items[0].flags", "items[0].y"}; !slices.Equal(paths(r.Fields), want) {
		t.Errorf("items[1]: paths %v, want %v", paths(r.Fields), want)
	}
	if r.End != off {
		t.Errorf("items[1]: end %d, want %d (after items[0])", r.End, off)
	}

	// A box ID where a Point is expected (inner): unknown for the type Point.
	blob = boxBlob(boxBlob(nil, nil, nil, nil, 0), nil, nil, nil, 0)
	r = Decode(blob, "Box", synthTable(), open())
	if s := r.Stop; s == nil || s.Kind != StopUnknownConstructor || s.Type != "Point" || s.ID != idBox || s.Path != "inner" {
		t.Errorf("box as inner: stop %+v", r.Stop)
	}
	if want := []string{"flags"}; !slices.Equal(paths(r.Fields), want) {
		t.Errorf("box as inner: paths %v, want %v", paths(r.Fields), want)
	}
}

// Spec section 7.2: everything decoded before a stop is returned. A box cut at every byte
// boundary yields a prefix of the full result, an End within the cut and a `truncated` stop
// (ErrTruncated): a blob that ends is never a limit hit, even inside a declared length or
// count (spec ruling T7).
func TestTLDecodePartialKeepsWhatWasRead(t *testing.T) {
	blob := fullBox()
	full := Decode(blob, "Box", synthTable(), open())
	if full.Stop != nil {
		t.Fatalf("full: %+v", *full.Stop)
	}
	for cut := range len(blob) {
		bud := open()
		r := Decode(blob[:cut], "Box", synthTable(), bud)
		if r.Stop == nil {
			t.Fatalf("cut %d: no stop", cut)
		}
		if len(r.Fields) > len(full.Fields) || !slices.EqualFunc(r.Fields, full.Fields[:len(r.Fields)], sameNamed) {
			t.Fatalf("cut %d: fields %v are not a prefix of the full result", cut, paths(r.Fields))
		}
		if r.End > cut {
			t.Errorf("cut %d: end %d", cut, r.End)
		}
		if bud.held != wantCharge(r) || r.Charged() != bud.held {
			t.Errorf("cut %d: budget holds %d, Charged %d; want %d", cut, bud.held, r.Charged(), wantCharge(r))
		}
		var te *Error
		if r.Stop.Kind != StopTruncated || !errors.As(r.Stop.Err, &te) || te.Err != ErrTruncated ||
			te.Offset != r.Stop.Offset || te.Path != r.Stop.Path {
			t.Errorf("cut %d: stop %+v, err %v; want truncated with ErrTruncated", cut, *r.Stop, r.Stop.Err)
		}
	}
}

func sameNamed(a, b Named) bool {
	return a.Path == b.Path && a.Kind == b.Kind && a.Int == b.Int && a.Bool == b.Bool &&
		bytes.Equal(a.Bytes, b.Bytes) && a.Len == b.Len && a.ID == b.ID && a.Type == b.Type && a.Class == b.Class
}

func checkStop(t *testing.T, name string, r Result, kind string, sentinel error, path string, off int) {
	t.Helper()
	s := r.Stop
	if s == nil || s.Kind != kind || s.Path != path || s.Offset != off {
		t.Fatalf("%s: stop %+v; want %s at %q, offset %d", name, s, kind, path, off)
	}
	if sentinel == nil {
		return
	}
	var te *Error
	if !errors.As(s.Err, &te) || te.Err != sentinel || te.Path != path || te.Offset != off {
		t.Errorf("%s: err %#v; want *Error{%v, %d, %q}", name, s.Err, sentinel, off, path)
	}
}

// Spec section 12 and 4I4-P10: each stop kind, with the *Error carrying the offset and the
// field path (the invariant row: "with offset and path").
func TestTLDecodeStopKinds(t *testing.T) {
	tab := synthTable()
	kindsHead := le32(idKinds)

	r := Decode(cat(kindsHead, le32(1)), "Kinds", tab, open())
	checkStop(t, "bad bool", r, StopMalformed, ErrBadBool, "b", 4)

	badVec := cat(le32(idBox), le32(0), le32(0x1cb5c416), le32(0), make([]byte, 16))
	r = Decode(badVec, "Box", tab, open())
	checkStop(t, "bad vector", r, StopMalformed, ErrBadVector, "items", 8)

	huge := cat(le32(idBox), le32(0), le32(IDVector), le32(math.MaxInt32), make([]byte, 16))
	r = Decode(huge, "Box", tab, open())
	checkStop(t, "vector 2^31-1", r, StopCap, ErrVectorCount, "items", 8)

	str := cat(kindsHead, le32(IDBoolFalse), le64(0), []byte{0xfe, 0xff, 0xff, 0xff}, make([]byte, 8))
	r = Decode(str, "Kinds", tab, open())
	// T7: the framing maximum past the end of a short blob is a truncation, not a cap.
	checkStop(t, "string fe ff ff ff", r, StopTruncated, ErrTruncated, "s", 16)
	if te := new(Error); !errors.As(r.Stop.Err, &te) || te.Declared != MaxString || te.Remaining != 8 {
		t.Errorf("string fe ff ff ff: err %#v; want declared %d, remaining 8", r.Stop.Err, MaxString)
	}
	if want := []string{"b", "d"}; !slices.Equal(paths(r.Fields), want) || r.End != 16 {
		t.Errorf("string fe ff ff ff: paths %v end %d; want %v 16", paths(r.Fields), r.End, want)
	}

	// A truncation deep in a vector element names its full path.
	blob := boxBlob(nil, [][]byte{pointBlob(0, 0, 1), pointBlob(0, 0, 2)}, nil, nil, 0)
	cut := bytes.LastIndex(blob, le64(2)) + 3
	r = Decode(blob[:cut], "Box", tab, open())
	checkStop(t, "truncated items[1].y", r, StopTruncated, ErrTruncated, "items[1].y", cut-3)
	if r.Stop.Type != "Point" || r.Stop.ID != idPoint {
		t.Errorf("truncated items[1].y: stop type %q id %#x; want Point %#x", r.Stop.Type, r.Stop.ID, idPoint)
	}
	// A truncated top ID.
	r = Decode([]byte{1, 2}, "Box", tab, open())
	checkStop(t, "top id", r, StopTruncated, ErrTruncated, "", 0)
	if r.Top != 0 || r.Stop.Type != "Box" {
		t.Errorf("top id: top %#x, type %q", r.Top, r.Stop.Type)
	}

	// blank nested: opaque at inner, with its ID and type; nothing after it.
	blob = boxBlob(le32(idBlank), nil, nil, nil, 0)
	r = Decode(blob, "Box", tab, open())
	checkStop(t, "blank nested", r, StopOpaque, nil, "inner", 8)
	if r.Stop.ID != idBlank || r.Stop.Type != "Point" || !slices.Equal(paths(r.Fields), []string{"flags"}) || r.End != 8 {
		t.Errorf("blank nested: %+v, paths %v, end %d", *r.Stop, paths(r.Fields), r.End)
	}
	// blank on top: opaque, Top set, no field.
	r = Decode(cat(le32(idBlank), make([]byte, 8)), "Point", tab, open())
	checkStop(t, "blank top", r, StopOpaque, nil, "", 0)
	if r.Top != idBlank || r.Stop.ID != idBlank || len(r.Fields) != 0 {
		t.Errorf("blank top: %+v", r)
	}

	// half on top: prefix after its second field, the bytes after it unread.
	half := cat(le32(idHalf), le32(0), le32(6))
	r = Decode(cat(half, le32(77), le32(78)), "Point", tab, open())
	checkStop(t, "half top", r, StopPrefix, nil, "", 12)
	if r.End != 12 || r.Top != idHalf || r.Stop.ID != idHalf || r.Stop.Type != "Point" || !slices.Equal(paths(r.Fields), []string{"flags", "a"}) {
		t.Errorf("half top: %+v", r)
	}
	// half nested at items[0]: the WHOLE decode ends there (the walker never skips).
	blob = boxBlob(nil, [][]byte{half, pointBlob(0, 0, 1)}, nil, nil, 0)
	r = Decode(blob, "Box", tab, open())
	checkStop(t, "half nested", r, StopPrefix, nil, "items[0]", 28)
	if want := []string{"flags", "items", "items[0]", "items[0].flags", "items[0].a"}; !slices.Equal(paths(r.Fields), want) || r.End != 28 {
		t.Errorf("half nested: paths %v end %d; want %v 28", paths(r.Fields), r.End, want)
	}

	// The budget refuses the third Alloc: the stop is at the third field, two are kept.
	bud := &countBudget{limit: 2}
	r = Decode(pointBlob(1, 5, 6), "Point", tab, bud)
	checkStop(t, "budget", r, StopBudget, ErrNoBudget, "y", 12)
	if !slices.Equal(paths(r.Fields), []string{"flags", "x"}) || bud.held != (128+5)+(128+1) || r.End != 12 {
		t.Errorf("budget: paths %v, held %d, end %d", paths(r.Fields), bud.held, r.End)
	}
	// A nil budget is a refusal.
	r = Decode(pointBlob(1, 5, 6), "Point", tab, nil)
	checkStop(t, "nil budget", r, StopBudget, ErrNoBudget, "flags", 4)
	if len(r.Fields) != 0 || r.Top != idPoint {
		t.Errorf("nil budget: %+v", r)
	}

	// A blob above MaxBlob is a cap before any read.
	r = Decode(make([]byte, MaxBlob+1), "Box", tab, open())
	checkStop(t, "MaxBlob+1", r, StopCap, ErrTooLarge, "", 0)

	// A panic (here the caller's budget) is recovered into malformed ErrInternal; what was
	// read before it is kept.
	r = Decode(pointBlob(1, 5, 6), "Point", tab, &panicBudget{after: 1})
	checkStop(t, "panic", r, StopMalformed, ErrInternal, "x", 12)
	if te := new(Error); !errors.As(r.Stop.Err, &te) || te.Detail != `panic: "budget"` ||
		len(r.Fields) != 1 || r.Stop.Type != "Point" || r.Stop.ID != idPoint {
		t.Errorf("panic: stop %+v, err %#v, %d fields; want detail %q, Point %#x", *r.Stop, r.Stop.Err, len(r.Fields), `panic: "budget"`, idPoint)
	}
	// A long panic value is quoted and capped at 200 bytes (as typedstream.guard).
	r = Decode(pointBlob(1, 5, 6), "Point", tab, &panicBudget{after: 0, msg: strings.Repeat("\xff", 300)})
	if te := new(Error); !errors.As(r.Stop.Err, &te) || len(te.Detail) != len("panic: ")+200 || !strings.HasPrefix(te.Detail, `panic: "\xff`) {
		t.Errorf("long panic: err %v", r.Stop.Err)
	}

	// A table defect (a gated field before its flags word, a KTrue with no gate, a flags word
	// of another name; plan 4I4c P6: a union type on a kind other than KVector, a union of one
	// type twice or without its first type, a class on a vector of objects or strings, a class
	// outside the four on a bare id vector) is ErrInternal at that field, never a guess.
	for name, c := range map[string]struct {
		f   []Field
		off int
	}{
		"gate without word": {[]Field{{Name: "x", Kind: KInt32, Flags: "flags", Bit: 0}}, 4},
		"ungated true":      {[]Field{{Name: "z", Kind: KTrue}}, 4},
		"bit 32":            {[]Field{{Name: "flags", Kind: KFlags}, {Name: "x", Kind: KInt32, Flags: "flags", Bit: 32}}, 8},
		"word name":         {[]Field{{Name: "other", Kind: KFlags}}, 4},
		"kind 0":            {[]Field{{Name: "k"}}, 4},
		"alt on object":     {[]Field{{Name: "o", Kind: KObject, Of: "T", Alt: "U"}}, 4},
		"alt is of":         {[]Field{{Name: "u", Kind: KVector, Of: "U", Alt: "U"}}, 4},
		"alt without of":    {[]Field{{Name: "u", Kind: KVector, Alt: "U"}}, 4},
		"class on strings":  {[]Field{{Name: "s", Kind: KVecString, Class: "user"}}, 4},
		"class on objects":  {[]Field{{Name: "v", Kind: KVector, Of: "U", Class: "user"}}, 4},
		"class robot":       {[]Field{{Name: "i", Kind: KVecInt32, Class: "robot"}}, 4},
	} {
		bad := Table{{Type: "T", ID: 1}: {ID: 1, Type: "T", Body: BodyFull, Fields: c.f}}
		r := Decode(cat(le32(1), make([]byte, 8)), "T", bad, open())
		checkStop(t, name, r, StopMalformed, ErrInternal, c.f[len(c.f)-1].Name, c.off)
		if te := new(Error); !errors.As(r.Stop.Err, &te) || !strings.HasPrefix(te.Detail, "table: ") || r.Stop.ID != 1 || r.Stop.Type != "T" {
			t.Errorf("%s: stop %+v, err %#v; want a table detail, T 1", name, *r.Stop, r.Stop.Err)
		}
	}

	// M6, kept by ruling C1 (plan 4I4c P4): the Stop names the key read, never a differing
	// Constructor.ID or Constructor.Type: the entries below sit under Key{"T", n} but say ID 99
	// or 98 and type U, and every stop names T and the ID read; under U the ID is unknown.
	skew := Table{
		{Type: "T", ID: 1}: {ID: 99, Type: "U", Body: BodyPrefix, Fields: []Field{{Name: "a", Kind: KInt32}}},
		{Type: "T", ID: 2}: {ID: 98, Type: "U", Body: BodyOpaque},
	}
	r = Decode(cat(le32(1), le32(5)), "T", skew, open())
	checkStop(t, "prefix key", r, StopPrefix, nil, "", 8)
	if r.Stop.ID != 1 || r.Stop.Type != "T" {
		t.Errorf("prefix key: stop ID %d type %q, want 1 T", r.Stop.ID, r.Stop.Type)
	}
	r = Decode(le32(2), "T", skew, open())
	checkStop(t, "opaque key", r, StopOpaque, nil, "", 0)
	if r.Stop.ID != 2 || r.Stop.Type != "T" {
		t.Errorf("opaque key: stop ID %d type %q, want 2 T", r.Stop.ID, r.Stop.Type)
	}
	r = Decode(cat(le32(1), le32(5)), "T", skew, &countBudget{limit: 0})
	checkStop(t, "budget key", r, StopBudget, ErrNoBudget, "a", 4)
	if r.Stop.ID != 1 || r.Stop.Type != "T" {
		t.Errorf("budget key: stop ID %d type %q, want 1 T", r.Stop.ID, r.Stop.Type)
	}
	r = Decode(cat(le32(1), le32(5)), "U", skew, open())
	checkStop(t, "skew as U", r, StopUnknownConstructor, nil, "", 0)
	if r.Stop.Type != "U" || len(r.Fields) != 0 {
		t.Errorf("skew as U: stop %+v, %d fields; want unknown for U, none", *r.Stop, len(r.Fields))
	}

	// M4: ErrSteps through Decode, inside a vector of objects with vectors. A crate of 40
	// boxes of 10,000 points: 2 reads for the crate, 30,006 per box (ID, flags, items header,
	// 3 per point, ids header, key, who), so read MaxSteps+1 falls in box 33 after 3 + 3 x
	// 3,265 + 2 of its reads: the y of point 3,265.
	box := pointsBox(10000)
	r = Decode(crateBlob(box, 40), "Crate", tab, open())
	off := 12 + 33*len(box) + 16 + 3265*16 + 8
	checkStop(t, "steps", r, StopCap, ErrSteps, "boxes[33].items[3265].y", off)
	if r.Steps != MaxSteps || r.Stop.Type != "Point" || r.Stop.ID != idPoint {
		t.Errorf("steps: %d steps, stop %+v", r.Steps, *r.Stop)
	}
	// ErrDepth inside a vector element: nodes[0] is at depth 2, so its 31st next is depth 33.
	r = Decode(crateBlob(nil, 0, bytes.Repeat(le32(idNode), 64)), "Crate", tab, open())
	checkStop(t, "depth in vector", r, StopCap, ErrDepth, "nodes[0]"+strings.Repeat(".next", 31), 20+4*31)
}

type panicBudget struct {
	after int
	msg   string
}

func (b *panicBudget) Alloc(int64) error {
	if b.after == 0 {
		if b.msg != "" {
			panic(b.msg)
		}
		panic("budget")
	}
	b.after--
	return nil
}

func (b *panicBudget) Free(int64) {}

// tableFields is the number of fields of all constructors of t (the "fields of the table"
// of spec section 7.1's termination bound).
func tableFields(t Table) int {
	n := 0
	for _, c := range t {
		n += len(c.Fields)
	}
	return n
}

// Spec section 7.1 termination and section 12 (depth bomb): nesting stops at MaxDepth with
// ErrDepth; Steps stays within len/4 + the table's fields (F14); a declared size never
// drives an allocation.
func TestTLDecodeTerminates(t *testing.T) {
	tab := synthTable()
	bomb := bytes.Repeat(le32(idNode), (1<<20)/4)
	r := Decode(bomb, "Node", tab, open())
	path := strings.TrimSuffix(strings.Repeat("next.", MaxDepth), ".")
	checkStop(t, "depth bomb", r, StopCap, ErrDepth, path, 4*MaxDepth)
	if r.Steps > MaxDepth+1 || len(r.Fields) != MaxDepth-1 {
		t.Errorf("depth bomb: steps %d, fields %d; want <= %d, %d", r.Steps, len(r.Fields), MaxDepth+1, MaxDepth-1)
	}

	// 4 MiB: a box whose vectors hold MaxVector elements each and whose key fills the rest.
	items := make([][]byte, MaxVector)
	ids := make([]int64, MaxVector)
	for i := range items {
		items[i] = pointBlob(0, 0, int64(i))
		ids[i] = int64(i)
	}
	head := boxBlob(nil, items, ids, nil, 0)
	keyLen := 4<<20 - len(head)
	blob := boxBlob(nil, items, ids, make([]byte, keyLen), 0)
	if len(blob) != 4<<20 {
		t.Fatalf("blob %d bytes", len(blob))
	}
	r = Decode(blob, "Box", tab, open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("4 MiB: stop %v, end %d", r.Stop, r.End)
	}
	if bound := len(blob)/4 + tableFields(tab); r.Steps > bound || r.Steps < 3*MaxVector {
		t.Errorf("4 MiB: steps %d; want in [%d, %d]", r.Steps, 3*MaxVector, bound)
	}
	// 4 MiB of empty box vectors (box IDs, zero flags, zero-count vectors): the decode reads
	// the first box and stops at the end of it; the rest is trailing bytes.
	empty := boxBlob(nil, nil, nil, nil, 0)
	blob = bytes.Repeat(empty, (4<<20)/len(empty))
	r = Decode(blob, "Box", tab, open())
	if r.Stop != nil || r.End != len(empty) || r.Steps > len(empty)/4+tableFields(tab) {
		t.Errorf("empty boxes: stop %v, end %d, steps %d", r.Stop, r.End, r.Steps)
	}

	// Plan 4I4c W3: a bare string vector of MaxVector empty strings (4 bytes each, about 40 KB)
	// is read whole, one step per element, within len/4 + the table's fields.
	blob = bareBlob(nil, nil, make([][]byte, MaxVector))
	r = Decode(blob, "Bare", tab, open())
	if r.Stop != nil || r.End != len(blob) || len(r.Fields) != 3+MaxVector {
		t.Fatalf("empty strings: stop %v, end %d of %d, %d fields", r.Stop, r.End, len(blob), len(r.Fields))
	}
	if bound := len(blob)/4 + tableFields(tab); r.Steps > bound {
		t.Errorf("empty strings: steps %d > %d", r.Steps, bound)
	}

	// Hostile declared sizes allocate nothing from the size (as TestTLReaderNeverAllocatesFromDeclaredSize).
	vecBomb := cat(le32(idBox), le32(0), le32(IDVector), le32(MaxVector), make([]byte, 8))
	strBomb := cat(le32(idKinds), le32(IDBoolTrue), le64(0), []byte{0xfe, 0xff, 0xff, 0x7f}, make([]byte, 8))
	allocs := testing.AllocsPerRun(100, func() {
		if r := Decode(vecBomb, "Box", tab, open()); r.Stop == nil || r.Stop.Kind != StopTruncated { // T7
			t.Fatal("vector bomb decoded")
		}
		if r := Decode(strBomb, "Kinds", tab, open()); r.Stop == nil || r.Stop.Kind != StopTruncated {
			t.Fatal("string bomb decoded")
		}
	})
	if allocs > 32 {
		t.Errorf("hostile decodes allocated %v times per run, want <= 32", allocs)
	}
}

// Spec section 7.1: a set bit that gates no field and names no KTrue in a fully pinned
// constructor is stated; a prefix body is not fully pinned, so it states nothing.
func TestTLDecodeFlagsUnknownBits(t *testing.T) {
	tab := synthTable()
	r := Decode(pointBlob(1<<5, 0, 1), "Point", tab, open())
	if r.Stop != nil || !slices.Equal(r.Flags, []string{FlagUnknownFlagBits}) {
		t.Errorf("point bit 5: stop %v flags %v", r.Stop, r.Flags)
	}
	r = Decode(pointBlob(3, 1, 1), "Point", tab, open())
	if r.Stop != nil || len(r.Flags) != 0 {
		t.Errorf("point bits 0,1: stop %v flags %v", r.Stop, r.Flags)
	}
	r = Decode(cat(le32(idHalf), le32(1<<5), le32(1)), "Point", tab, open())
	if r.Stop == nil || r.Stop.Kind != StopPrefix || len(r.Flags) != 0 {
		t.Errorf("half bit 5: stop %v flags %v", r.Stop, r.Flags)
	}
	// A nested point with an unknown bit flags the whole result.
	r = Decode(boxBlob(pointBlob(1<<7, 0, 0), nil, nil, nil, 0), "Box", tab, open())
	if r.Stop != nil || !slices.Equal(r.Flags, []string{FlagUnknownFlagBits}) {
		t.Errorf("nested bit 7: stop %v flags %v", r.Stop, r.Flags)
	}
	// Reader flags reach the result.
	r = Decode(cat(le32(idKinds), le32(IDBoolTrue), le64(0), []byte{1, 'a', 0, 1}, tlBytes(nil), le32(IDVector), le32(0), le64(0)), "Kinds", tab, open())
	if r.Stop != nil || !slices.Equal(r.Flags, []string{FlagPaddingNonzero}) {
		t.Errorf("padding: stop %v flags %v", r.Stop, r.Flags)
	}
}

// liveFactor is the documented gap between the budget charge and the live heap of a result
// (Task 3 review I1; docs/limitations/parsers.md, Decoders): the charge covers each Named
// (120 bytes on 64-bit since plan 4I4c added Named.Type, ruling C5) and its path, while the
// Fields slice's append regrowth (spare capacity up to a quarter of a large slice) and the
// allocator's size classes stay uncharged. Measured 0.94 times with the 120-byte Named.
const liveFactor = 1.25

// Task 3 review I1 (F8): the charge bounds what a decode keeps alive. A large synthetic
// decode (10 boxes of 10,000 points, 300,062 fields) charges 128 bytes and the path's length
// per field, and the live heap of its result is at most liveFactor times that charge.
func TestTLDecodeBudgetCoversLiveHeap(t *testing.T) {
	blob := crateBlob(pointsBox(10000), 10)
	tab := synthTable()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	bud := open()
	r := Decode(blob, "Crate", tab, bud)
	runtime.GC()
	runtime.ReadMemStats(&after)
	live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if r.Stop != nil || len(r.Fields) != 300062 {
		t.Fatalf("stop %v, %d fields", r.Stop, len(r.Fields))
	}
	if bud.held != wantCharge(r) || r.Charged() != bud.held {
		t.Errorf("budget holds %d, Charged %d; want %d", bud.held, r.Charged(), wantCharge(r))
	}
	if float64(live) > liveFactor*float64(bud.held) {
		t.Errorf("live heap %d bytes > %.2f x charge %d (%.2fx)", live, liveFactor, bud.held, float64(live)/float64(bud.held))
	}
	t.Logf("live %d bytes, charged %d (%.2fx)", live, bud.held, float64(live)/float64(bud.held))
	runtime.KeepAlive(r)
}

// bareBlob is a boxed bare: a vector of bare int32 ids, one of bare int64 ids and one of bare
// TL strings.
func bareBlob(ids32 []int32, ids64 []int64, strs [][]byte) []byte {
	b := cat(le32(idBare), le32(IDVector), le32(uint32(len(ids32))))
	for _, v := range ids32 {
		b = append(b, le32(uint32(v))...)
	}
	b = append(b, cat(le32(IDVector), le32(uint32(len(ids64))))...)
	for _, v := range ids64 {
		b = append(b, le64(uint64(v))...)
	}
	b = append(b, cat(le32(IDVector), le32(uint32(len(strs))))...)
	for _, s := range strs {
		b = append(b, tlBytes(s)...)
	}
	return b
}

// mixedBlob is a boxed mixed whose union vector holds the boxed elements given.
func mixedBlob(elems ...[]byte) []byte {
	b := cat(le32(idMixed), le32(IDVector), le32(uint32(len(elems))))
	for _, e := range elems {
		b = append(b, e...)
	}
	return b
}

// Plan 4I4c W1, P4 (design section 6): an ID is looked up by (expected type, ID). One ID in
// two types with different bodies reads by the field's type in each place, and the type it
// was looked up under is returned on its Named; under a third type it is unknown-constructor.
func TestTLDecodeKeysByTypeAndID(t *testing.T) {
	tab := synthTable()
	blob := cat(le32(idPair), le32(idTwin), le32(7), le32(idTwin), le64(1<<40))
	r := Decode(blob, "Pair", tab, open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("pair: stop %v, end %d of %d", r.Stop, r.End, len(blob))
	}
	if want := []string{"l", "l.x", "r", "r.y"}; !slices.Equal(paths(r.Fields), want) {
		t.Fatalf("pair: paths %v, want %v", paths(r.Fields), want)
	}
	if f := field(t, r, "l"); f.Kind != KObject || f.ID != idTwin || f.Type != "Left" {
		t.Errorf("l = %+v; want KObject %#x of type Left", f, idTwin)
	}
	if f := field(t, r, "r"); f.Kind != KObject || f.ID != idTwin || f.Type != "Right" {
		t.Errorf("r = %+v; want KObject %#x of type Right", f, idTwin)
	}
	if f := field(t, r, "l.x"); f.Kind != KInt32 || f.Int != 7 || f.Type != "" {
		t.Errorf("l.x = %+v; want KInt32 7, no type", f)
	}
	if f := field(t, r, "r.y"); f.Kind != KInt64 || f.Int != 1<<40 {
		t.Errorf("r.y = %+v; want KInt64 1<<40", f)
	}

	// Under a third type the ID is unknown, at the top and nested.
	r = Decode(cat(le32(idTwin), le32(7), le32(0)), "Point", tab, open())
	if s := r.Stop; s == nil || s.Kind != StopUnknownConstructor || s.Type != "Point" || s.ID != idTwin || len(r.Fields) != 0 {
		t.Errorf("twin as Point: stop %+v, %d fields", s, len(r.Fields))
	}
	r = Decode(boxBlob(cat(le32(idTwin), le32(7), le32(0)), nil, nil, nil, 0), "Box", tab, open())
	if s := r.Stop; s == nil || s.Kind != StopUnknownConstructor || s.Type != "Point" || s.Path != "inner" {
		t.Errorf("twin as inner Point: stop %+v", s)
	}

	// Every KObject carries the type it was read as (a field's Of, a vector element's Of).
	r = Decode(fullBox(), "Box", tab, open())
	objects := 0
	for _, f := range r.Fields {
		switch {
		case f.Kind == KObject && f.Type != "Point":
			t.Errorf("%s: KObject of type %q, want Point", f.Path, f.Type)
		case f.Kind == KObject:
			objects++
		case f.Type != "":
			t.Errorf("%s: kind %d carries type %q", f.Path, f.Kind, f.Type)
		}
	}
	if objects != 3 {
		t.Errorf("full box: %d objects, want 3", objects)
	}
}

// Plan 4I4c W2, P5 (design section 6, Q3): a union element is read by the first of its two
// types that holds the ID, never by the second when the first holds it (an opaque row there
// stops, naming the first type); an ID in neither is unknown-constructor naming both.
func TestTLDecodeUnionElement(t *testing.T) {
	tab := synthTable()
	left := cat(le32(idLeft), le32(5))
	right := cat(le32(idRight), le64(6))
	blob := mixedBlob(left, right)
	r := Decode(blob, "Mixed", tab, open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("left, right: stop %v, end %d of %d", r.Stop, r.End, len(blob))
	}
	if want := []string{"u", "u[0]", "u[0].a", "u[1]", "u[1].b"}; !slices.Equal(paths(r.Fields), want) {
		t.Fatalf("left, right: paths %v, want %v", paths(r.Fields), want)
	}
	if f := field(t, r, "u"); f.Kind != KVector || f.Len != 2 || f.Type != "" {
		t.Errorf("u = %+v; want KVector Len 2", f)
	}
	if f := field(t, r, "u[0]"); f.Kind != KObject || f.ID != idLeft || f.Type != "Left" {
		t.Errorf("u[0] = %+v; want %#x of type Left", f, idLeft)
	}
	if f := field(t, r, "u[1]"); f.Kind != KObject || f.ID != idRight || f.Type != "Right" {
		t.Errorf("u[1] = %+v; want %#x of type Right", f, idRight)
	}
	if f := field(t, r, "u[1].b"); f.Int != 6 {
		t.Errorf("u[1].b = %+v; want 6", f)
	}

	// An ID in neither type: unknown at u[2], naming both types; u[0] and u[1] are kept.
	const unknown uint32 = 0xdeadbeef
	blob = mixedBlob(left, right, cat(le32(unknown), le32(0)))
	off := len(blob) - 8
	r = Decode(blob, "Mixed", tab, open())
	checkStop(t, "neither", r, StopUnknownConstructor, nil, "u[2]", off)
	var ue *UnknownConstructorError
	if s := r.Stop; s.Type != "Left|Right" || s.ID != unknown || !errors.As(s.Err, &ue) ||
		ue.Type != "Left|Right" || ue.ID != unknown || ue.Path != "u[2]" || ue.Offset != off {
		t.Errorf("neither: stop %+v, err %v; want unknown %#x naming Left|Right", *s, s.Err, unknown)
	}
	if len(r.Fields) != 5 || r.End != off {
		t.Errorf("neither: %d fields, end %d; want 5, %d", len(r.Fields), r.End, off)
	}

	// An ID whose first-type row is opaque stops there, naming the first type, though the
	// second type holds a full row of the same ID.
	blob = mixedBlob(cat(le32(idShared), le32(9)))
	r = Decode(blob, "Mixed", tab, open())
	checkStop(t, "shared", r, StopOpaque, nil, "u[0]", 12)
	if s := r.Stop; s.Type != "Left" || s.ID != idShared || !slices.Equal(paths(r.Fields), []string{"u"}) {
		t.Errorf("shared: stop %+v, paths %v; want opaque Left %#x after u", *s, paths(r.Fields), idShared)
	}

	// An ID both types hold is read as the first (Left: x, an int32), never the second.
	blob = mixedBlob(cat(le32(idTwin), le32(3)))
	r = Decode(blob, "Mixed", tab, open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("twin: stop %v, end %d of %d", r.Stop, r.End, len(blob))
	}
	if f := field(t, r, "u[0]"); f.Type != "Left" || f.ID != idTwin {
		t.Errorf("twin u[0] = %+v; want type Left", f)
	}
	if f := field(t, r, "u[0].x"); f.Kind != KInt32 || f.Int != 3 {
		t.Errorf("twin u[0].x = %+v; want 3", f)
	}
}

// Plan 4I4c W3, P6 (design section 6; VectorLegacy.java:27-37, 48-58 at A-HEAD): a bare id
// vector carries its class on every element (the header keeps the field kind and no class);
// each element consumes at least its minimum size (4 for int32, 8 for int64), and the header
// and count fail as every vector does. A bare int vector without a class is unchanged.
func TestTLDecodeBareIDVector(t *testing.T) {
	tab := synthTable()
	blob := bareBlob([]int32{7, -1}, []int64{1 << 40}, nil)
	r := Decode(blob, "Bare", tab, open())
	if r.Stop != nil || r.End != len(blob) {
		t.Fatalf("ids: stop %v, end %d of %d", r.Stop, r.End, len(blob))
	}
	if want := []string{"ids32", "ids32[0]", "ids32[1]", "ids64", "ids64[0]", "strs"}; !slices.Equal(paths(r.Fields), want) {
		t.Fatalf("ids: paths %v, want %v", paths(r.Fields), want)
	}
	if f := field(t, r, "ids32"); f.Kind != KVecInt32 || f.Len != 2 || f.Class != "" {
		t.Errorf("ids32 = %+v; want KVecInt32 Len 2, no class", f)
	}
	if f := field(t, r, "ids32[0]"); f.Kind != KID32 || f.Int != 7 || f.Class != "user" {
		t.Errorf("ids32[0] = %+v; want KID32 7 user", f)
	}
	if f := field(t, r, "ids32[1]"); f.Kind != KID32 || f.Int != -1 || f.Class != "user" {
		t.Errorf("ids32[1] = %+v; want KID32 -1 user", f)
	}
	if f := field(t, r, "ids64"); f.Kind != KVecInt64 || f.Len != 1 || f.Class != "" {
		t.Errorf("ids64 = %+v; want KVecInt64 Len 1, no class", f)
	}
	if f := field(t, r, "ids64[0]"); f.Kind != KID64 || f.Int != 1<<40 || f.Class != "channel" {
		t.Errorf("ids64[0] = %+v; want KID64 1<<40 channel", f)
	}

	// Empty vectors.
	blob = bareBlob(nil, nil, nil)
	r = Decode(blob, "Bare", tab, open())
	if want := []string{"ids32", "ids64", "strs"}; r.Stop != nil || r.End != len(blob) || !slices.Equal(paths(r.Fields), want) {
		t.Errorf("empty: stop %v, end %d, paths %v", r.Stop, r.End, paths(r.Fields))
	}

	// A count past the end: truncated at the header, declared count and remainder kept.
	r = Decode(cat(le32(idBare), le32(IDVector), le32(3), le32(1)), "Bare", tab, open())
	checkStop(t, "ids32 past end", r, StopTruncated, ErrTruncated, "ids32", 4)
	if te := new(Error); !errors.As(r.Stop.Err, &te) || te.Declared != 3 || te.Remaining != 4 || len(r.Fields) != 0 {
		t.Errorf("ids32 past end: err %#v, %d fields; want declared 3, remaining 4, none", r.Stop.Err, len(r.Fields))
	}
	// Two int64 ids need 16 bytes: 12 remain (enough at 4 bytes each) is truncated.
	r = Decode(cat(le32(idBare), le32(IDVector), le32(0), le32(IDVector), le32(2), make([]byte, 12)), "Bare", tab, open())
	checkStop(t, "ids64 past end", r, StopTruncated, ErrTruncated, "ids64", 12)
	if te := new(Error); !errors.As(r.Stop.Err, &te) || te.Declared != 2 || te.Remaining != 12 {
		t.Errorf("ids64 past end: err %#v; want declared 2, remaining 12", r.Stop.Err)
	}
	// A count above MaxVector is a cap; a bad header is malformed.
	r = Decode(cat(le32(idBare), le32(IDVector), le32(MaxVector+1), make([]byte, 4*(MaxVector+1))), "Bare", tab, open())
	checkStop(t, "ids32 10001", r, StopCap, ErrVectorCount, "ids32", 4)
	r = Decode(cat(le32(idBare), le32(0x1cb5c416), le32(0), make([]byte, 16)), "Bare", tab, open())
	checkStop(t, "ids32 bad header", r, StopMalformed, ErrBadVector, "ids32", 4)

	// A bare int vector without a class keeps KInt32 elements and no class.
	kinds := cat(le32(idKinds), le32(IDBoolTrue), le64(0), tlBytes(nil), tlBytes(nil), le32(IDVector), le32(1), le32(9), le64(0))
	r = Decode(kinds, "Kinds", tab, open())
	if f := field(t, r, "v32[0]"); r.Stop != nil || f.Kind != KInt32 || f.Int != 9 || f.Class != "" {
		t.Errorf("kinds v32[0] = %+v (stop %v); want KInt32 9, no class", f, r.Stop)
	}
}

// Plan 4I4c W3, P6, Q4 (design section 6; Vector.java:161-177, 184-186 at A-HEAD): a bare
// string vector is the boxed header, a count and bare TL strings; each element is a KString
// whose Bytes is a sub-slice of the blob (capacity ending with it) with its length; its
// encoding facts are stated as any string's; a string or count past the end is truncated.
func TestTLDecodeBareStringVector(t *testing.T) {
	tab := synthTable()
	strs := [][]byte{{}, bytes.Repeat([]byte{'a'}, 253), bytes.Repeat([]byte{'b'}, 254)}
	blob := bareBlob(nil, nil, strs)
	r := Decode(blob, "Bare", tab, open())
	if r.Stop != nil || r.End != len(blob) || len(r.Flags) != 0 {
		t.Fatalf("strings: stop %v, end %d of %d, flags %v", r.Stop, r.End, len(blob), r.Flags)
	}
	if f := field(t, r, "strs"); f.Kind != KVecString || f.Len != 3 || f.Class != "" {
		t.Errorf("strs = %+v; want KVecString Len 3", f)
	}
	for i, want := range strs {
		f := field(t, r, "strs["+strconv.Itoa(i)+"]")
		if f.Kind != KString || !bytes.Equal(f.Bytes, want) || f.Len != len(want) || cap(f.Bytes) != len(f.Bytes) || f.Class != "" {
			t.Errorf("strs[%d]: kind %d, %d bytes (cap %d), Len %d; want KString of %d bytes, cap = len",
				i, f.Kind, len(f.Bytes), cap(f.Bytes), f.Len, len(want))
		}
	}

	// A non-zero padding byte is stated.
	blob = cat(le32(idBare), le32(IDVector), le32(0), le32(IDVector), le32(0), le32(IDVector), le32(1), []byte{1, 'a', 0, 1})
	r = Decode(blob, "Bare", tab, open())
	if r.Stop != nil || !slices.Equal(r.Flags, []string{FlagPaddingNonzero}) || string(field(t, r, "strs[0]").Bytes) != "a" {
		t.Errorf("padding: stop %v, flags %v", r.Stop, r.Flags)
	}

	// A length past the end: truncated at strs[1], strs[0] kept.
	blob = cat(le32(idBare), le32(IDVector), le32(0), le32(IDVector), le32(0), le32(IDVector), le32(2), tlBytes([]byte("a")), []byte{200, 0, 0, 0})
	r = Decode(blob, "Bare", tab, open())
	checkStop(t, "strs[1] past end", r, StopTruncated, ErrTruncated, "strs[1]", len(blob)-4)
	if te := new(Error); !errors.As(r.Stop.Err, &te) || te.Declared != 200 || te.Remaining != 3 || r.Fields[len(r.Fields)-1].Path != "strs[0]" {
		t.Errorf("strs[1] past end: err %#v, paths %v; want declared 200, remaining 3, strs[0] last", r.Stop.Err, paths(r.Fields))
	}
	// A count whose 4-byte minimum overruns: truncated at strs before any element.
	blob = cat(le32(idBare), le32(IDVector), le32(0), le32(IDVector), le32(0), le32(IDVector), le32(3), tlBytes(nil), tlBytes(nil))
	r = Decode(blob, "Bare", tab, open())
	checkStop(t, "strs count past end", r, StopTruncated, ErrTruncated, "strs", 20)
	if want := []string{"ids32", "ids64"}; !slices.Equal(paths(r.Fields), want) {
		t.Errorf("strs count past end: paths %v, want %v", paths(r.Fields), want)
	}
}

// Ruling C5 (plan 4I4c W1): a Named, grown by its Type, still fits the fixed charge of one
// returned field.
func TestTLNamedFitsFieldOverhead(t *testing.T) {
	if s := unsafe.Sizeof(Named{}); s > FieldOverhead {
		t.Errorf("Named is %d bytes, FieldOverhead %d", s, FieldOverhead)
	}
}
