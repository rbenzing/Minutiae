package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// bmpBase is a valid 64x48 24-bit BMP with a 40-byte DIB header.
func bmpBase() carvetest.BMPOpts {
	return carvetest.BMPOpts{DIBSize: 40, W: 64, H: 48, BPP: 24, Planes: 1}
}

func bmpVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateBMP(newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30)))
}

func TestBMPValidatesBuilderVariants(t *testing.T) {
	with := func(f func(*carvetest.BMPOpts)) carvetest.BMPOpts {
		o := bmpBase()
		f(&o)
		return o
	}
	for name, o := range map[string]carvetest.BMPOpts{
		"dib12 24bpp":          with(func(o *carvetest.BMPOpts) { o.DIBSize = 12 }),
		"dib40 1bpp":           with(func(o *carvetest.BMPOpts) { o.BPP = 1 }),
		"dib40 4bpp":           with(func(o *carvetest.BMPOpts) { o.BPP = 4 }),
		"dib40 8bpp":           with(func(o *carvetest.BMPOpts) { o.BPP = 8 }),
		"dib40 24bpp":          bmpBase(),
		"dib40 32bpp":          with(func(o *carvetest.BMPOpts) { o.BPP = 32 }),
		"dib40 top-down":       with(func(o *carvetest.BMPOpts) { o.H = -48 }),
		"dib52 alphabitfields": with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression = 52, 32, 6 }),
		"dib56 alphabitfields": with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression = 56, 32, 6 }),
		"dib64 os2":            with(func(o *carvetest.BMPOpts) { o.DIBSize = 64 }),
		"dib108 bitfields":     with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression = 108, 32, 3 }),
		"dib124 bitfields":     with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression = 124, 32, 3 }),
		"rle8":                 with(func(o *carvetest.BMPOpts) { o.BPP, o.Compression, o.SizeImage = 8, 1, 500 }),
		"rle4":                 with(func(o *carvetest.BMPOpts) { o.BPP, o.Compression, o.SizeImage = 4, 2, 500 }),
		"jpeg":                 with(func(o *carvetest.BMPOpts) { o.BPP, o.Compression, o.SizeImage = 0, 4, 300 }),
		"png":                  with(func(o *carvetest.BMPOpts) { o.BPP, o.Compression, o.SizeImage = 0, 5, 300 }),
		"os2 huffman 1bpp":     with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression, o.SizeImage = 64, 1, 3, 200 }),
		"os2 rle24":            with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression, o.SizeImage = 64, 24, 4, 200 }),
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.BMP(o)
			v := bmpVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			for _, want := range []string{
				"dims=64x48", "bpp=" + strconv.Itoa(int(o.BPP)), "dib=" + strconv.Itoa(o.DIBSize),
				"compression=" + strconv.Itoa(int(o.Compression)),
			} {
				if !slices.Contains(v.Basis, want) {
					t.Errorf("basis = %v, missing %s", v.Basis, want)
				}
			}
		})
	}
}

func TestBMPTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.BMP(bmpBase())
	v := bmpVerdict(obj, 4096)
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
}

func TestBMPRejects(t *testing.T) {
	with := func(f func(*carvetest.BMPOpts)) []byte {
		o := bmpBase()
		f(&o)
		return carvetest.BMP(o)
	}
	// withBytes grows the file by extra bytes and bfSize with it, so the array
	// the header declares is the only thing that is wrong.
	withBytes := func(extra int, f func(*carvetest.BMPOpts)) []byte {
		b := with(f)
		b = append(b, make([]byte, extra)...)
		binary.LittleEndian.PutUint32(b[2:], uint32(len(b)))
		return b
	}
	patch := func(off int, v uint32) []byte {
		b := carvetest.BMP(bmpBase())
		binary.LittleEndian.PutUint32(b[off:], v)
		return b
	}
	size := uint32(len(carvetest.BMP(bmpBase())))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"dib size 20", with(func(o *carvetest.BMPOpts) { o.DIBSize = 20 }), "bad-dib"},
		{"zero bfSize", with(func(o *carvetest.BMPOpts) { o.ZeroFileSize = true }), "bad-size"},
		{"bfSize 20", patch(2, 20), "bad-size"},
		{"bfSize past the window", with(func(o *carvetest.BMPOpts) { o.FileSizeDelta = 1000 }), "truncated"},
		{"width 0", with(func(o *carvetest.BMPOpts) { o.W = 0 }), "bad-dimensions"},
		{"height 0", with(func(o *carvetest.BMPOpts) { o.H = 0 }), "bad-dimensions"},
		{"width -1", with(func(o *carvetest.BMPOpts) { o.W = -1 }), "bad-dimensions"},
		{"width 65537", with(func(o *carvetest.BMPOpts) { o.W = 65537 }), "bad-dimensions"},
		{"height MinInt32", with(func(o *carvetest.BMPOpts) { o.H = math.MinInt32 }), "bad-dimensions"},
		{"core width 0", with(func(o *carvetest.BMPOpts) { o.DIBSize, o.W = 12, 0 }), "bad-dimensions"},
		{"planes 2", with(func(o *carvetest.BMPOpts) { o.Planes = 2 }), "bad-planes"},
		{"bpp 7", with(func(o *carvetest.BMPOpts) { o.BPP = 7 }), "bad-bpp"},
		{"core bpp 32", with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP = 12, 32 }), "bad-bpp"},
		{"compression 7", with(func(o *carvetest.BMPOpts) { o.Compression = 7 }), "bad-compression"},
		{"rle8 at 24 bpp", with(func(o *carvetest.BMPOpts) { o.Compression, o.SizeImage = 1, 100 }), "bad-compression"},
		{"bpp 0 with RGB", with(func(o *carvetest.BMPOpts) { o.BPP = 0 }), "bad-compression"},
		{"bitfields at 24 bpp", with(func(o *carvetest.BMPOpts) { o.Compression = 3 }), "bad-compression"},
		{"os2 huffman at 8 bpp", with(func(o *carvetest.BMPOpts) { o.DIBSize, o.BPP, o.Compression, o.SizeImage = 64, 8, 3, 100 }), "bad-compression"},
		{"top-down rle8", with(func(o *carvetest.BMPOpts) { o.H, o.BPP, o.Compression, o.SizeImage = -48, 8, 1, 100 }), "bad-compression"},
		{"bfOffBits 10", patch(10, 10), "bad-offset"},
		{"bfOffBits == bfSize", patch(10, size), "bad-offset"},
		{"pixels one byte over bfSize", with(func(o *carvetest.BMPOpts) { o.FileSizeDelta = -1 }), "pixels-exceed-size"},
		{"rle8 with SizeImage 0", withBytes(100, func(o *carvetest.BMPOpts) { o.BPP, o.Compression = 8, 1 }), "pixels-exceed-size"},
		{"rle8 SizeImage past bfSize", with(func(o *carvetest.BMPOpts) { o.BPP, o.Compression, o.SizeImage, o.FileSizeDelta = 8, 1, 500, -1 }), "pixels-exceed-size"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := bmpVerdict(c.obj, 0)
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %s", v, c.reason)
			}
		})
	}
}

func TestBMPNegativeControls(t *testing.T) {
	const slot = 4096
	valid := carvetest.BMP(bmpBase())
	cut := valid[:len(valid)*6/10]
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a seeded generator for reproducible buffers
	img := carvetest.Image(65*slot+len(cut), fillPattern)
	headers := 0
	put := func(i int, b []byte) {
		carvetest.Place(img, i*slot, b)
		headers++
	}
	put(0, []byte("BM is the abbreviation of a common phrase, followed by prose that goes on."))
	for i := 1; i <= 64; i++ {
		b := make([]byte, slot)
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		b[0], b[1] = 'B', 'M'
		put(i, b)
	}
	put(65, cut)
	set, filter, err := Builtin().Select([]string{"bmp"})
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
	if len(hits) != 0 || total != headers || st.Rejected["truncated"] != 1 {
		t.Errorf("hits %d, rejected %v (sum %d), want no hits, sum %d and truncated 1", len(hits), st.Rejected, total, headers)
	}
}

func TestDIBCheckAtOffset(t *testing.T) {
	dib := carvetest.DIB(bmpBase())
	buf := slices.Concat(bytes.Repeat([]byte{0xAA}, 22), dib)
	w := newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30))
	d, reason, err := dibCheck(w, 22, int64(len(dib)))
	if err != nil || reason != "" || d.W != 64 || d.H != 48 || d.HeaderSize != 40 || d.BPP != 24 || d.TopDown || d.PixelBytes != 64*3*48 {
		t.Fatalf("dibCheck = %+v, %q, %v", d, reason, err)
	}
	if _, reason, err := dibCheck(w, 22, 39); err != nil || reason != "truncated" {
		t.Errorf("avail 39: reason %q, err %v, want truncated", reason, err)
	}
}

func TestBMPReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.BMP(bmpBase())
	w := newWindow(context.Background(), failAt{obj, 30}, 0, int64(len(obj)), newBudget(1<<30))
	if v := validateBMP(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("verdict = %+v, want rejected unreadable", v)
	}
}

// TestDIBCheckMediaFailure: a failing medium is an error, never a reason: the
// caller maps it to "unreadable" and the scanner re-labels it.
func TestDIBCheckMediaFailure(t *testing.T) {
	dib := carvetest.DIB(bmpBase())
	for _, at := range []int64{0, 20} { // the size field, the rest of the header
		w := newWindow(context.Background(), failAt{dib, at}, 0, int64(len(dib)), newBudget(1<<30))
		d, reason, err := dibCheck(w, 0, int64(len(dib)))
		if !errors.Is(err, errMedia) || reason != "" || d != (dibInfo{}) {
			t.Errorf("fail at %d: dibCheck = %+v, %q, %v, want errMedia and no reason", at, d, reason, err)
		}
	}
}
