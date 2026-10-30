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
