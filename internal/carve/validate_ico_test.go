package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func icoPNG() []byte { return carvetest.PNG(carvetest.PNGOpts{}) }

// icoDIB is a 16x16 32-bit DIB with the doubled height of the AND mask.
func icoDIB() []byte {
	return carvetest.DIB(carvetest.BMPOpts{DIBSize: 40, W: 16, H: 32, BPP: 32, Planes: 1})
}

func icoVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateICO(newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30)))
}

func TestICOValidatesBuilderVariants(t *testing.T) {
	for _, c := range []struct {
		name               string
		o                  carvetest.ICOOpts
		typ                string
		images, pngs, dibs int
		trail              int
	}{
		{"one png", carvetest.ICOOpts{Images: [][]byte{icoPNG()}}, "ico", 1, 1, 0, 0},
		{"one dib", carvetest.ICOOpts{Images: [][]byte{icoDIB()}}, "ico", 1, 0, 1, 0},
		{"three mixed", carvetest.ICOOpts{Images: [][]byte{icoPNG(), icoDIB(), icoPNG()}}, "ico", 3, 2, 1, 0},
		{"reversed order", carvetest.ICOOpts{Images: [][]byte{icoPNG(), icoDIB(), icoPNG()}, Order: []int{2, 1, 0}}, "ico", 3, 2, 1, 0},
		{"cursor", carvetest.ICOOpts{Cursor: true, Images: [][]byte{icoDIB(), icoPNG()}}, "cur", 2, 1, 1, 0},
		{"trailer", carvetest.ICOOpts{Images: [][]byte{icoPNG()}}, "ico", 1, 1, 0, 4096},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := carvetest.ICO(c.o)
			v := icoVerdict(obj, c.trail)
			if v.Outcome != Validated || v.Type != c.typ || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated %s length %d", v, c.typ, len(obj))
			}
			for _, want := range []string{
				"images=" + strconv.Itoa(c.images), "png=" + strconv.Itoa(c.pngs), "dib=" + strconv.Itoa(c.dibs),
			} {
				if !slices.Contains(v.Basis, want) {
					t.Errorf("basis = %v, missing %s", v.Basis, want)
				}
			}
		})
	}
}

func TestICORejects(t *testing.T) {
	one := func(f func(*carvetest.ICOOpts)) []byte {
		o := carvetest.ICOOpts{Images: [][]byte{icoPNG()}}
		f(&o)
		return carvetest.ICO(o)
	}
	const sizeOff, offsetOff = 6 + 8, 6 + 12
	count0 := one(func(*carvetest.ICOOpts) {})
	count0[4], count0[5] = 0, 0
	overflow := one(func(*carvetest.ICOOpts) {})
	binary.LittleEndian.PutUint32(overflow[sizeOff:], 0xFFFFFFF0)
	binary.LittleEndian.PutUint32(overflow[offsetOff:], 0xFFFFFFF0)
	longHeader := carvetest.DIB(carvetest.BMPOpts{DIBSize: 124, W: 16, H: 32, BPP: 32, Planes: 1, Compression: 3})[:60]
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"count 0", count0, "bad-count"},
		{"count 257", one(func(o *carvetest.ICOOpts) { o.Count = 257 }), "bad-count"},
		{"directory past window", one(func(o *carvetest.ICOOpts) { o.Count = 200 }), "truncated"},
		{"reserved byte", one(func(o *carvetest.ICOOpts) { o.ReservedByte = 1 }), "bad-entry"},
		{"zero size", one(func(o *carvetest.ICOOpts) { o.ZeroSize = true }), "bad-entry"},
		{"offset into directory", one(func(o *carvetest.ICOOpts) { o.OffsetIntoDir = true }), "bad-offset"},
		{"size past window", one(func(o *carvetest.ICOOpts) { o.SizeDelta = 1000 }), "truncated"},
		{"offset + size overflows u32", overflow, "truncated"},
		{"random entry", carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{bytes.Repeat([]byte{0xA5}, 64)}}), "bad-image"},
		{"png width 0", carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{carvetest.PNG(carvetest.PNGOpts{ZeroWidth: true})}}), "bad-image"},
		// dibCheck says "truncated" when the header exceeds the entry; for an
		// entry that is a bad image, not a cut object.
		{"dib header larger than entry", carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{longHeader}}), "bad-image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := icoVerdict(c.obj, 0)
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %s", v, c.reason)
			}
		})
	}
}

func TestICONegativeControls(t *testing.T) {
	const slot = 4096
	valid := carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{icoPNG(), icoDIB(), icoPNG()}})
	cut := valid[:len(valid)*6/10]
	if v := icoVerdict(cut, 0); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("cut object verdict = %+v, want rejected truncated", v)
	}
	rng := rand.New(rand.NewPCG(13, 17)) //nolint:gosec // a seeded generator for reproducible buffers
	img := carvetest.Image((2+128)*slot+len(cut), fillPattern)
	headers := 0
	put := func(b []byte) {
		carvetest.Place(img, headers*slot, b)
		headers++
	}
	put(append([]byte{0, 0, 1, 0}, "00 00 01 00 is the ICO magic, quoted in prose that goes on."...))
	put(append([]byte{0, 0, 2, 0}, "00 00 02 00 is the CUR magic, quoted in prose that goes on."...))
	for _, magic := range [][]byte{{0, 0, 1, 0}, {0, 0, 2, 0}} {
		for range 64 {
			b := make([]byte, slot)
			for j := range b {
				b[j] = byte(rng.Uint32())
			}
			copy(b, magic)
			put(b)
		}
	}
	put(cut)
	set, filter, err := Builtin().Select([]string{"ico", "cur"})
	if err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	st, err := Scan(context.Background(), bytes.NewReader(img), []Run{{0, int64(len(img))}}, set,
		Options{Align: slot, TypeFilter: filter}, func(h Hit) error { hits = append(hits, h); return nil })
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range st.Rejected {
		total += n
	}
	if len(hits) != 0 || total != headers || st.Rejected["bad-count"] < 2 {
		t.Errorf("hits %d, rejected %v (sum %d), want no hits and sum %d", len(hits), st.Rejected, total, headers)
	}
}

// TestICOMagicInBMFFSizeFieldStillCarvesMP4: an ftyp box of size 256 or 512
// begins 00 00 01 00 or 00 00 02 00, the ICO and CUR magic. The ICO row
// rejects it (its count field is "ft") and the MP4 is carved validated.
func TestICOMagicInBMFFSizeFieldStillCarvesMP4(t *testing.T) {
	for _, size := range []int{256, 512} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			compat := make([]string, (size-16)/4)
			for i := range compat {
				compat[i] = "mp42"
			}
			obj := carvetest.MP4(carvetest.BMFFOpts{Compat: compat})
			if binary.BigEndian.Uint32(obj) != uint32(size) {
				t.Fatalf("ftyp size %d, want %d", binary.BigEndian.Uint32(obj), size)
			}
			img := carvetest.Image(16384, fillPattern)
			carvetest.Place(img, 4096, obj)
			var hits []Hit
			st, err := Scan(context.Background(), bytes.NewReader(img), []Run{{0, int64(len(img))}}, Builtin(),
				Options{Align: 4096}, func(h Hit) error { hits = append(hits, h); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 1 || hits[0].Family != "isobmff" || hits[0].Outcome != Validated || hits[0].Length != int64(len(obj)) {
				t.Fatalf("hits = %+v, want one validated isobmff of length %d", hits, len(obj))
			}
			if st.Rejected["bad-count"] != 1 {
				t.Errorf("rejected = %v, want bad-count 1", st.Rejected)
			}
		})
	}
}

func TestPNGHeadMatchesValidatePNG(t *testing.T) {
	for name, o := range map[string]carvetest.PNGOpts{
		"rgb8":     {},
		"palette4": {ColorType: 3, BitDepth: 4},
		"rgba16":   {ColorType: 6, BitDepth: 16},
		"wide":     {W: 1000, H: 7},
		"apng":     {APNG: true, IDATs: 2},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.PNG(o)
			v := validatePNG(pngWindow(obj, newBudget(1<<30)))
			if v.Outcome != Validated {
				t.Fatalf("validatePNG = %+v", v)
			}
			w, h, reason, err := pngHead(pngWindow(obj, newBudget(1<<30)), 0, int64(len(obj)))
			if err != nil || reason != "" {
				t.Fatalf("pngHead reason %q, err %v", reason, err)
			}
			if want := "dims=" + strconv.FormatUint(uint64(w), 10) + "x" + strconv.FormatUint(uint64(h), 10); !slices.Contains(v.Basis, want) {
				t.Errorf("validatePNG basis %v, pngHead says %s", v.Basis, want)
			}
		})
	}
}

func TestPNGHeadRejects(t *testing.T) {
	good := carvetest.PNG(carvetest.PNGOpts{})
	for _, c := range []struct {
		name   string
		obj    []byte
		avail  int64
		reason string
	}{
		{"zero width", carvetest.PNG(carvetest.PNGOpts{ZeroWidth: true}), 1 << 20, "zero-dimensions"},
		{"bad signature", slices.Concat([]byte("\x89PNG\r\n\x1a\x0b"), good[8:]), 1 << 20, "bad-signature"},
		{"IHDR not first", carvetest.PNG(carvetest.PNGOpts{IHDRNotFirst: true}), 1 << 20, "no-ihdr"},
		{"avail one short", good, 28, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, reason, err := pngHead(pngWindow(c.obj, newBudget(1<<30)), 0, min(c.avail, int64(len(c.obj))))
			if err != nil || reason != c.reason {
				t.Fatalf("reason %q, err %v, want %s", reason, err, c.reason)
			}
		})
	}
}

func TestICOReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.ICO(carvetest.ICOOpts{Images: [][]byte{icoPNG()}})
	for _, at := range []int64{10, 30} { // inside the directory, inside the entry image
		w := newWindow(context.Background(), failAt{obj, at}, 0, int64(len(obj)), newBudget(1<<30))
		if v := validateICO(w); v.Outcome != Rejected || v.Reason != "unreadable" {
			t.Fatalf("fail at %d: verdict = %+v, want rejected unreadable", at, v)
		}
	}
}
