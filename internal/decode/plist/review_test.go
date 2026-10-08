package plist

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func wantLimit(t *testing.T, err error) {
	t.Helper()
	wantKind(t, err)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("error %v, want ErrLimit", err)
	}
}

func wantMalformed(t *testing.T, err error) {
	t.Helper()
	wantKind(t, err)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("error %v, want ErrMalformed", err)
	}
}

// P12: the aggregate payload of shared references is charged, with exact edges.
func TestCheckAggregatePayloadEdge(t *testing.T) {
	l := Limits{MaxNodes: 100, MaxDepth: 8, MaxPayload: 100}
	// 50 + 50 = 100 bytes through two references to one shared object: accepted.
	if err := Check(objectsPlist(refArray(1, 1), dataObj(50)), l); err != nil {
		t.Fatalf("at the aggregate limit: %v", err)
	}
	// 50 + 50 + 1 = 101: refused although no single object exceeds the limit.
	wantLimit(t, Check(objectsPlist(refArray(1, 1, 2), dataObj(50), dataObj(1)), l))
	// three references to one 50 byte object: 150.
	wantLimit(t, Check(objectsPlist(refArray(1, 1, 1), dataObj(50)), l))
}

// P12: depth edges, direct and through a node first reached shallow, then deep.
func TestCheckDepthEdges(t *testing.T) {
	l := Limits{MaxNodes: 1 << 10, MaxDepth: 5, MaxPayload: 1 << 10}
	if err := Check(nestedPlist(5, 1), l); err != nil {
		t.Fatalf("depth == MaxDepth: %v", err)
	}
	wantLimit(t, Check(nestedPlist(6, 1), l))

	// top (0) = array[1, 2]; 1 = head of a chain of h arrays above an int; 2 = array[1].
	shared := func(h int) []byte {
		objs := [][]byte{refArray(1, 2), refArray(3), refArray(1)}
		// objects 3.. form the chain below object 1: h-1 more arrays and the int leaf.
		for i := 1; i < h; i++ {
			objs = append(objs, refArray(byte(3+i)))
		}
		objs = append(objs, []byte{0x10, 0x01})
		return objectsPlist(objs...)
	}
	// leaf depth on the first visit is 1+h, through object 2 it is 2+h.
	if err := Check(shared(3), l); err != nil { // 2+3 == MaxDepth
		t.Fatalf("shared node at the edge: %v", err)
	}
	wantLimit(t, Check(shared(4), l)) // first path ok (5), second path 6
}

// P12: node and object-count edges.
func TestCheckNodeAndObjectEdges(t *testing.T) {
	l := Limits{MaxNodes: 10, MaxDepth: 8, MaxPayload: 1 << 10}
	refs := func(k int) []byte { return bytes.Repeat([]byte{1}, k) }
	// array of k references to one int: 1+k nodes.
	if err := Check(objectsPlist(refArray(refs(9)...), []byte{0x10, 1}), l); err != nil {
		t.Fatalf("10 nodes: %v", err)
	}
	wantLimit(t, Check(objectsPlist(refArray(refs(10)...), []byte{0x10, 1}), l))
	// object table: 10 objects fine, 11 refused (all but the top unreferenced).
	pad := func(n int) [][]byte {
		o := [][]byte{refArray(1)}
		for range n - 1 {
			o = append(o, []byte{0x10, 1})
		}
		return o
	}
	if err := Check(objectsPlist(pad(10)...), l); err != nil {
		t.Fatalf("10 objects: %v", err)
	}
	wantLimit(t, Check(objectsPlist(pad(11)...), l))
}

// P12: payload edges of one object, and UTF-16 and dict doubling.
func TestCheckPayloadEdgesAndDoubling(t *testing.T) {
	l := Limits{MaxNodes: 100, MaxDepth: 8, MaxPayload: 100}
	utf16 := func(units int) []byte {
		return append([]byte{0x6f, 0x10, byte(units)}, bytes.Repeat([]byte{0, 'a'}, units)...)
	}
	if err := Check(objectsPlist(dataObj(100)), l); err != nil {
		t.Fatalf("data at limit: %v", err)
	}
	wantLimit(t, Check(objectsPlist(dataObj(101)), l))
	if err := Check(objectsPlist(utf16(50)), l); err != nil { // 100 bytes
		t.Fatalf("utf16 at limit: %v", err)
	}
	wantLimit(t, Check(objectsPlist(utf16(51)), l)) // 102 bytes
	// A dict of count 1 holds a key AND a value reference: with room for one only it is
	// truncated, which an array-style count would accept.
	obj := []byte{0xD1, 0, 0, 1, 0x10, 0x00}
	wantMalformed(t, Check(rawPlist(obj, []byte{8, 12}), DefaultLimits()))
}

// P13: the validator refuses lengths that run past the object area.
func TestCheckRefusesLengthsPastObjectArea(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  []byte
	}{
		{"ascii-5-has-2", []byte{0x55, 'a', 'b'}},
		{"data-5-has-2", []byte{0x45, 1, 2}},
		{"utf16-2-has-2-bytes", []byte{0x62, 0, 'a'}},
		{"int-8-has-1", []byte{0x13, 1}},
		{"int-1-has-0", []byte{0x10}},
		{"real-8-has-3", []byte{0x23, 1, 2, 3}},
		{"real-4-has-0", []byte{0x22}},
		{"date-has-4", []byte{0x33, 1, 2, 3, 4}},
		{"uid-4-has-1", []byte{0x83, 1}},
		{"int-size-5", []byte{0x15, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantMalformed(t, checkBinaryTest(objectsPlist(tc.obj)))
		})
	}
	// exact fits are accepted
	for _, obj := range [][]byte{
		{0x52, 'a', 'b'},
		{0x42, 1, 2},
		{0x61, 0, 'a'},
		{0x11, 1, 2},
		{0x23, 1, 2, 3, 4, 5, 6, 7, 8},
		{0x33, 1, 2, 3, 4, 5, 6, 7, 8},
		{0x81, 1, 2},
		{0x08},
		{0x09},
		{0x00},
	} {
		if err := checkBinaryTest(objectsPlist(obj)); err != nil {
			t.Fatalf("% x: %v", obj, err)
		}
	}
}

func checkBinaryTest(b []byte) error { return Check(b, DefaultLimits()) }

// P14: limits near MaxUint64 never wrap.
func TestCheckHugeLimitsDoNotWrap(t *testing.T) {
	l := Limits{MaxNodes: math.MaxUint64, MaxDepth: math.MaxInt, MaxPayload: math.MaxUint64}
	for _, obj := range [][]byte{
		bigCount(0x6, 1<<63+1), bigCount(0x6, 1<<63), bigCount(0x4, math.MaxUint64),
		bigCount(0xD, 1<<63), bigCount(0xA, math.MaxUint64),
	} {
		wantKind(t, Check(rawPlist(obj, []byte{8}), l))
	}
	// a real document still passes under such limits
	if err := Check(objectsPlist(refArray(1, 1), dataObj(50)), l); err != nil {
		t.Fatal(err)
	}
}

// P14: the zero value of Limits means DefaultLimits.
func TestZeroLimitsMeanDefaults(t *testing.T) {
	good := objectsPlist(refArray(1, 1), dataObj(50))
	if err := Check(good, Limits{}); err != nil {
		t.Fatalf("zero Limits refused a small document: %v", err)
	}
	wantLimit(t, Check(nestedPlist(21, 2), Limits{}))
	wantLimit(t, Check(nestedPlist(70, 1), Limits{}))
}

// P15: a set (0xC) is walked like an array.
func TestCheckWalksSets(t *testing.T) {
	wantLimit(t, Check(nestedTyped(21, 2, 0xC0), DefaultLimits()))
	if err := Check(nestedTyped(10, 2, 0xC0), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	wantMalformed(t, Check(rawPlist([]byte{0xC1, 0, 0, 0}, []byte{8}), DefaultLimits())) // self cycle
	wantMalformed(t, Check(rawPlist([]byte{0xC1, 0, 0, 5}, []byte{8}), DefaultLimits())) // ref out of range
	wantMalformed(t, Check(rawPlist([]byte{0xC3, 0, 0, 0}, []byte{8}), DefaultLimits())) // truncated refs
	l := Limits{MaxNodes: 1 << 10, MaxDepth: 5, MaxPayload: 1 << 10}
	wantLimit(t, Check(nestedTyped(6, 1, 0xC0), l))
}
