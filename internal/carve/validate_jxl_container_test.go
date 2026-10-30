package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func jxlSigBox() []byte { return carvetest.Box("JXL ", []byte{0x0D, 0x0A, 0x87, 0x0A}) }

func jxlFtypBox() []byte {
	return carvetest.Box("ftyp", []byte("jxl "), make([]byte, 4), []byte("jxl "))
}

func jxlpBox(index uint32, data []byte) []byte {
	return carvetest.Box("jxlp", binary.BigEndian.AppendUint32(nil, index), data)
}

func TestJXLContainerValidates(t *testing.T) {
	big := bytes.Repeat([]byte{0xFF, 0x0A, 0x55}, 400)
	for _, c := range []struct {
		name string
		o    carvetest.JXLOpts
	}{
		{"one jxlc", carvetest.JXLOpts{}},
		{"three jxlp", carvetest.JXLOpts{Codestream: big, Parts: 3}},
		{"all extra boxes", carvetest.JXLOpts{Extra: []string{"Exif", "xml ", "jumb", "jxll", "brob"}}},
		{"trailer excluded", carvetest.JXLOpts{Trailer: bytes.Repeat(fillPattern, 4096/len(fillPattern)+1)[:4096]}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			trailer := o.Trailer
			o.Trailer = nil
			obj := carvetest.JXLContainer(o)
			buf := slices.Concat(obj, trailer)
			v := validateBMFF(bmffWindow(buf, newBudget(1<<30)))
			if v.Outcome != Validated || v.Type != "jxl" || v.Ext != "jxl" || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated jxl/jxl length %d", v, len(obj))
			}
			if !slices.Contains(v.Basis, "brand=jxl ") {
				t.Errorf("basis = %v, want brand=jxl ", v.Basis)
			}
		})
	}
}

func TestJXLContainerRejects(t *testing.T) {
	cs := []byte{0xFF, 0x0A, 1, 2, 3, 4, 5, 6}
	fill := bytes.Repeat(fillPattern, 10)
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"brand isom", carvetest.JXLContainer(carvetest.JXLOpts{Brand: "isom"}), "bad-ftyp"},
		{"no codestream", carvetest.JXLContainer(carvetest.JXLOpts{NoCodestream: true}), "missing-codestream"},
		{"jxlc and jxlp", carvetest.JXLContainer(carvetest.JXLOpts{Both: true}), "bad-codestream"},
		{"index 0 then 2", carvetest.JXLContainer(carvetest.JXLOpts{BadIndex: true}), "bad-codestream"},
		{"two last parts", carvetest.JXLContainer(carvetest.JXLOpts{TwoLast: true}), "bad-codestream"},
		{"parts without a last flag", slices.Concat(jxlSigBox(), jxlFtypBox(), jxlpBox(0, cs), jxlpBox(1, cs), fill), "bad-codestream"},
		{"two jxlc", slices.Concat(jxlSigBox(), jxlFtypBox(), carvetest.Box("jxlc", cs), carvetest.Box("jxlc", cs)), "bad-codestream"},
		{"jxlp under 4 bytes", slices.Concat(jxlSigBox(), jxlFtypBox(), carvetest.Box("jxlp", []byte{0, 0})), "bad-codestream"},
		{"size 0 on Exif", slices.Concat(jxlSigBox(), jxlFtypBox(), []byte{0, 0, 0, 0, 'E', 'x', 'i', 'f'}, fill, carvetest.Box("jxlc", cs)), "unbounded-box"},
		{"size 0 on a jxlp not flagged last", slices.Concat(jxlSigBox(), jxlFtypBox(), []byte{0, 0, 0, 0, 'j', 'x', 'l', 'p', 0, 0, 0, 0}, cs), "unbounded-box"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if v := bmffVerdict(c.obj, 0); v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %s", v, c.reason)
			}
		})
	}
	obj := carvetest.JXLContainer(carvetest.JXLOpts{Codestream: bytes.Repeat([]byte{0xFF, 0x0A, 9}, 100)})
	if v := bmffVerdict(obj[:len(obj)-20], 0); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("jxlc cut short = %+v, want rejected truncated", v)
	}
	if v := bmffVerdict(jxlSigBox(), 0); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("signature box only = %+v, want rejected truncated", v)
	}
}

func TestJXLContainerSizeZeroCodestreamIsBounded(t *testing.T) {
	for _, parts := range []int{0, 2} {
		obj := carvetest.JXLContainer(carvetest.JXLOpts{SizeZeroLast: true, Parts: parts})
		buf := slices.Concat(obj, bytes.Repeat(fillPattern, 10))
		v := validateBMFF(bmffWindow(buf, newBudget(1<<30)))
		if v.Outcome != Bounded || v.Length != int64(len(buf)) || !slices.Equal(v.Assumptions, []string{"box-extends-to-end"}) || v.Type != "jxl" {
			t.Fatalf("parts %d: verdict = %+v, want bounded jxl length %d box-extends-to-end", parts, v, len(buf))
		}
	}
}

func TestJXLContainerNeverReadsBrob(t *testing.T) {
	brob := carvetest.Box("brob", []byte("xml "), make([]byte, 64<<10))
	obj := slices.Concat(jxlSigBox(), jxlFtypBox(), brob, carvetest.Box("jxlc", []byte{0xFF, 0x0A, 1, 2, 3}))
	b := newBudget(1 << 30)
	v := validateBMFF(bmffWindow(obj, b))
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated %d", v, len(obj))
	}
	if limit := int64(len(obj)) - 60<<10; b.spent >= limit {
		t.Fatalf("validator read %d bytes of a %d byte object, want fewer than %d (headers only)", b.spent, len(obj), limit)
	}
}

// TestJXLFtypFirstBrandIsRejected (ruling Q2): a jxl brand without the
// signature box in front is not typed jxl.
func TestJXLFtypFirstBrandIsRejected(t *testing.T) {
	obj := slices.Concat(carvetest.MP4(carvetest.BMFFOpts{Brand: "jxl "}), carvetest.Box("jxlc", []byte{0xFF, 0x0A, 1, 2, 3}))
	if v := bmffVerdict(obj, 0); v.Outcome != Rejected || v.Reason != "jxl-without-signature" {
		t.Fatalf("ftyp-first jxl = %+v, want rejected jxl-without-signature", v)
	}
	img := carvetest.Image(16*kib, fillPattern)
	cont := carvetest.JXLContainer(carvetest.JXLOpts{Extra: []string{"Exif"}})
	carvetest.Place(img, 0, cont)
	var hits []Hit
	st, err := Scan(context.Background(), bytes.NewReader(img), whole(img), Builtin(), Options{Align: 4096, Nested: true},
		func(h Hit) error { hits = append(hits, h); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != 0 || hits[0].Type != "jxl" || hits[0].Outcome != Validated || hits[0].Length != int64(len(cont)) {
		t.Fatalf("hits = %+v, want one validated jxl at 0 and no nested hit", hits)
	}
	if st.Rejected["jxl-without-signature"] != 1 {
		t.Fatalf("rejected = %v, want jxl-without-signature once (the inner ftyp at 12)", st.Rejected)
	}
}

func TestJXLContainerNegativeControls(t *testing.T) {
	prose := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 200)
	img := slices.Concat(jxlSigBox(), prose)
	img = append(img, carvetest.Image(8*kib-len(img)%(4*kib), fillPattern)...)
	var hits []Hit
	if _, err := Scan(context.Background(), bytes.NewReader(img), whole(img), Builtin(), Options{Align: 1},
		func(h Hit) error { hits = append(hits, h); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("signature followed by prose gave hits %+v", hits)
	}
	obj := carvetest.JXLContainer(carvetest.JXLOpts{Codestream: bytes.Repeat([]byte{0xFF, 0x0A, 7}, 300)})
	if v := bmffVerdict(obj[:len(obj)*6/10], 0); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("first 60%% = %+v, want rejected truncated", v)
	}
}

// TestJXLCodestreamNeedsItsMagic (T5-m1): the first two bytes of the codestream
// (FF 0A) are checked wherever the codestream starts: an empty jxlc, a jxlp
// holding only its index word, a one-byte payload and a wrong magic are all
// bad-codestream, for a sized box and for a size-0 box.
func TestJXLCodestreamNeedsItsMagic(t *testing.T) {
	head := slices.Concat(jxlSigBox(), jxlFtypBox())
	last := uint32(1 << 31)
	for _, c := range []struct {
		name string
		box  []byte
	}{
		{"empty jxlc", carvetest.Box("jxlc")},
		{"one byte jxlc", carvetest.Box("jxlc", []byte{0xFF})},
		{"wrong magic jxlc", carvetest.Box("jxlc", []byte{0x0A, 0xFF, 1, 2})},
		{"jxlp index word only", jxlpBox(last, nil)},
		{"jxlp one data byte", jxlpBox(last, []byte{0xFF})},
		{"jxlp wrong magic", jxlpBox(last, []byte{0xFF, 0x0B, 1})},
		{"size 0 jxlc empty", []byte{0, 0, 0, 0, 'j', 'x', 'l', 'c'}},
		{"size 0 jxlc wrong magic", []byte{0, 0, 0, 0, 'j', 'x', 'l', 'c', 0xFF, 0x0B, 1}},
		{"size 0 jxlp index word only", []byte{0, 0, 0, 0, 'j', 'x', 'l', 'p', 0x80, 0, 0, 0}},
		{"size 0 jxlp wrong magic", []byte{0, 0, 0, 0, 'j', 'x', 'l', 'p', 0x80, 0, 0, 0, 1, 2, 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if v := bmffVerdict(slices.Concat(head, c.box), 0); v.Outcome != Rejected || v.Reason != "bad-codestream" {
				t.Fatalf("verdict = %+v, want rejected bad-codestream", v)
			}
		})
	}
	// Only the FIRST part carries the magic; later parts are raw codestream bytes.
	ok := slices.Concat(head, jxlpBox(0, []byte{0xFF, 0x0A, 1}), jxlpBox(1|last, []byte{7, 7}))
	if v := bmffVerdict(ok, 0); v.Outcome != Validated {
		t.Fatalf("magic in the first part only = %+v, want validated", v)
	}
	okZero := slices.Concat(head, []byte{0, 0, 0, 0, 'j', 'x', 'l', 'c', 0xFF, 0x0A})
	if v := bmffVerdict(okZero, 0); v.Outcome != Bounded {
		t.Fatalf("size 0 jxlc holding FF 0A = %+v, want bounded", v)
	}
}

// TestJXLWalkRows (T5-m2): a 64-bit largesize jxlc box walks and validates, and
// a size-0 last-flagged jxlp with a wrong index is bad-codestream.
func TestJXLWalkRows(t *testing.T) {
	head := slices.Concat(jxlSigBox(), jxlFtypBox())
	payload := []byte{0xFF, 0x0A, 1, 2, 3, 4}
	large := binary.BigEndian.AppendUint64([]byte{0, 0, 0, 1, 'j', 'x', 'l', 'c'}, uint64(16+len(payload)))
	large = append(large, payload...)
	obj := slices.Concat(head, large)
	if v := bmffVerdict(obj, 0); v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("largesize jxlc = %+v, want validated length %d", v, len(obj))
	}
	wrong := slices.Concat(head, []byte{0, 0, 0, 0, 'j', 'x', 'l', 'p', 0x80, 0, 0, 5}, payload)
	if v := bmffVerdict(wrong, 0); v.Outcome != Rejected || v.Reason != "bad-codestream" {
		t.Fatalf("size 0 last jxlp with index 5 = %+v, want rejected bad-codestream", v)
	}
}
