package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func riffWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// webpVerdict validates obj followed by trail bytes of fill, the window being
// the whole buffer (what the scanner hands a validator at the run end).
func webpVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateRIFFWebP(riffWindow(buf, newBudget(1<<30)))
}

func TestWebPValidatesBuilderVariants(t *testing.T) {
	for name, o := range map[string]carvetest.WebPOpts{
		"lossy":            {},
		"lossless":         {Kind: "VP8L"},
		"extended alpha":   {Kind: "VP8X", Extra: []string{"ALPH"}},
		"extended all":     {Kind: "VP8X", Extra: []string{"ICCP", "ALPH", "EXIF", "XMP "}},
		"odd lossy":        {OddChunk: true},
		"odd lossless":     {Kind: "VP8L", OddChunk: true},
		"odd extended":     {Kind: "VP8X", OddChunk: true, Extra: []string{"EXIF"}},
		"other dimensions": {W: 100, H: 7, Kind: "VP8L"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.WebP(o)
			v := webpVerdict(obj, 0)
			w, h := 64, 48
			if o.W != 0 {
				w, h = o.W, o.H
			}
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if !slices.Contains(v.Basis, "dims="+strconv.Itoa(w)+"x"+strconv.Itoa(h)) {
				t.Errorf("basis = %v, want dims=%dx%d", v.Basis, w, h)
			}
			if !slices.ContainsFunc(v.Basis, func(s string) bool { return strings.HasPrefix(s, "chunks=") }) {
				t.Errorf("basis = %v, want chunks=", v.Basis)
			}
		})
	}
}

// TestWebPTrailingDataNotIncluded: the RIFF size bounds the object; whatever
// follows it in the window is excluded.
func TestWebPTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.WebP(carvetest.WebPOpts{Kind: "VP8X", Extra: []string{"EXIF"}})
	if v := webpVerdict(obj, 4096); v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
	two := slices.Concat(obj, carvetest.WebP(carvetest.WebPOpts{}))
	if v := webpVerdict(two, 0); v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("two objects verdict = %+v, want length %d", v, len(obj))
	}
}

func TestWebPRejects(t *testing.T) {
	ch := carvetest.RIFFChunk
	vp8 := func() []byte { return ch("VP8 ", []byte{0x10, 2, 0, 0x9D, 1, 0x2A, 64, 0, 48, 0, 1, 2}) }
	vp8x := ch("VP8X", []byte{0x10, 0, 0, 0, 63, 0, 0, 47, 0, 0})
	good := carvetest.WebP(carvetest.WebPOpts{})
	header := func(size uint32, form string) []byte {
		return append(binary.LittleEndian.AppendUint32([]byte("RIFF"), size), form...)
	}
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"not RIFF", slices.Concat([]byte("RIFX"), good[4:]), "bad-signature"},
		{"form WAVE", slices.Concat(good[:8], []byte("WAVE"), good[12:]), "bad-form"},
		{"size +2 with data after", slices.Concat(carvetest.WebP(carvetest.WebPOpts{SizeDelta: 2}), make([]byte, 64)), "bad-length"},
		{"size +2 exact buffer", carvetest.WebP(carvetest.WebPOpts{SizeDelta: 2}), "truncated"},
		{"size -2", carvetest.WebP(carvetest.WebPOpts{SizeDelta: -2}), "bad-length"},
		{"size 2", slices.Concat(header(2, "WEBP"), vp8()), "bad-length"},
		{"size 0", slices.Concat(header(0, "WEBP"), vp8()), "bad-length"},
		{"bad start code", carvetest.WebP(carvetest.WebPOpts{BadStartCode: true}), "bad-vp8"},
		{"zero dimensions", carvetest.WebP(carvetest.WebPOpts{ZeroDims: true}), "zero-dimensions"},
		{"VP8 payload 9", carvetest.RIFF("WEBP", 0, ch("VP8 ", []byte{0x10, 2, 0, 0x9D, 1, 0x2A, 64, 0, 48})), "bad-vp8"},
		{"VP8L bad signature", carvetest.RIFF("WEBP", 0, ch("VP8L", []byte{0x2E, 1, 2, 3, 4, 5})), "bad-vp8l"},
		{"VP8L payload 4", carvetest.RIFF("WEBP", 0, ch("VP8L", []byte{0x2F, 1, 2, 3})), "bad-vp8l"},
		{"first chunk ICCP", carvetest.RIFF("WEBP", 0, ch("ICCP", []byte{1, 2, 3, 4}), vp8()), "bad-first-chunk"},
		{"no chunks", carvetest.RIFF("WEBP", 0), "bad-first-chunk"},
		{"VP8X alone", carvetest.RIFF("WEBP", 0, vp8x), "no-image"},
		{"VP8X with ICCP and EXIF only", carvetest.RIFF("WEBP", 0, vp8x, ch("ICCP", []byte{1, 2}), ch("EXIF", []byte{1, 2})), "no-image"},
		{"VP8X short payload", carvetest.RIFF("WEBP", 0, ch("VP8X", make([]byte, 9)), vp8()), "bad-vp8x"},
		// The identity rule: an image chunk without a valid header does not count.
		{"VP8X with empty VP8", carvetest.RIFF("WEBP", 0, vp8x, ch("VP8 ", nil)), "bad-vp8"},
		{"VP8X with empty VP8L", carvetest.RIFF("WEBP", 0, vp8x, ch("VP8L", nil)), "bad-vp8l"},
		{"VP8X with bad VP8 start code", carvetest.RIFF("WEBP", 0, vp8x, ch("VP8 ", []byte{0x10, 2, 0, 1, 2, 3, 64, 0, 48, 0})), "bad-vp8"},
		{"VP8X with empty ANMF", carvetest.RIFF("WEBP", 0, vp8x, ch("ANMF", nil)), "no-image"},
		{"VP8X with ANMF without frame", carvetest.RIFF("WEBP", 0, vp8x, ch("ANMF", make([]byte, 24))), "no-image"},
		{"chunk type with control byte", carvetest.RIFF("WEBP", 0, vp8x, ch("V\x01X ", []byte{1, 2})), "bad-chunk-type"},
		{"chunk overruns the RIFF size", slices.Concat(header(24, "WEBP"), []byte{'V', 'P', '8', ' ', 0x40, 0, 0, 0}, make([]byte, 12)), "bad-length"},
		{"chunk header does not fit", slices.Concat(header(8, "WEBP"), []byte{'V', 'P', '8', ' '}), "bad-length"},
		{"odd chunk without its pad", slices.Concat(header(4+8+11, "WEBP"), []byte{'V', 'P', '8', ' ', 11, 0, 0, 0, 0x10, 2, 0, 0x9D, 1, 0x2A, 64, 0, 48, 0, 1}, make([]byte, 40)), "bad-length"},
		{"cut short", good[:len(good)-3], "truncated"},
		{"only the header", slices.Clone(good[:12]), "truncated"},
		{"one byte", []byte{'R'}, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateRIFFWebP(riffWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := webpVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestWebPAnimationValidates: an animated file with an ANMF frame holding a
// real image chunk counts as an image; an empty frame does not.
func TestWebPAnimationValidates(t *testing.T) {
	ch := carvetest.RIFFChunk
	frame := ch("VP8 ", []byte{0x10, 2, 0, 0x9D, 1, 0x2A, 64, 0, 48, 0, 1, 2})
	anmf := ch("ANMF", slices.Concat(make([]byte, 16), frame))
	obj := carvetest.RIFF("WEBP", 0, ch("VP8X", []byte{0x12, 0, 0, 0, 63, 0, 0, 47, 0, 0}), ch("ANIM", make([]byte, 6)), anmf)
	if v := webpVerdict(obj, 0); v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
	bad := carvetest.RIFF("WEBP", 0, ch("VP8X", []byte{0x12, 0, 0, 0, 63, 0, 0, 47, 0, 0}),
		ch("ANMF", slices.Concat(make([]byte, 16), ch("VP8 ", nil))))
	if v := webpVerdict(bad, 0); v.Outcome != Rejected || v.Reason != "bad-vp8" {
		t.Fatalf("empty frame verdict = %+v, want rejected bad-vp8", v)
	}
}

// TestWebPWindowBoundsAndBudget: a RIFF size declaring more than the window
// holds is a typed rejection with no allocation from it; a walk that outruns
// the budget or a cancelled context is rejected "unreadable" (ruling 3E-C1).
func TestWebPWindowBoundsAndBudget(t *testing.T) {
	huge := slices.Concat([]byte("RIFF\xff\xff\xff\xffWEBP"), []byte("VP8 \xf0\xff\xff\xff"), make([]byte, 100))
	if v := validateRIFFWebP(riffWindow(huge, newBudget(1<<30))); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("huge RIFF size verdict = %+v", v)
	}
	// Thousands of empty chunks: the walk advances 8 bytes per step and the
	// budget stops it.
	chunks := make([][]byte, 0, 50001)
	chunks = append(chunks, carvetest.RIFFChunk("VP8X", make([]byte, 10)))
	for range 50000 {
		chunks = append(chunks, carvetest.RIFFChunk("JUNK", nil))
	}
	obj := carvetest.RIFF("WEBP", 0, chunks...)
	b := newBudget(4 << 10)
	v := validateRIFFWebP(riffWindow(obj, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(obj), 0, int64(len(obj)), newBudget(1<<30))
	if v := validateRIFFWebP(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
	// With the budget the same walk is complete and rejected no-image.
	if v := webpVerdict(obj, 0); v.Outcome != Rejected || v.Reason != "no-image" {
		t.Fatalf("walk verdict = %+v, want no-image", v)
	}
}

// TestWebPReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// start, in the middle of the chunk structure and at a late header is
// rejected "unreadable" (never "truncated") and recorded in the window.
func TestWebPReadErrorIsUnreadable(t *testing.T) {
	chunks := [][]byte{carvetest.RIFFChunk("VP8X", []byte{0x10, 0, 0, 0, 63, 0, 0, 47, 0, 0})}
	for range 200 {
		chunks = append(chunks, carvetest.RIFFChunk("JUNK", make([]byte, 2)))
	}
	chunks = append(chunks, carvetest.RIFFChunk("VP8 ", slices.Concat([]byte{0x10, 2, 0, 0x9D, 1, 0x2A, 64, 0, 48, 0}, make([]byte, 200000))))
	chunks = append(chunks, carvetest.RIFFChunk("EXIF", make([]byte, 100000)))
	big := carvetest.RIFF("WEBP", 0, chunks...)
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at start", carvetest.WebP(carvetest.WebPOpts{}), 0},
		{"mid chunk headers", big, 1000},
		{"mid image payload header", big, 2040},
		{"late chunk header", big, int64(len(big)) - 100000 - 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			if v := validateRIFFWebP(riffWindow(c.obj, newBudget(1<<30))); v.Outcome != Validated {
				t.Fatalf("control verdict = %+v", v)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateRIFFWebP(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

func TestWebPRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family  string
			Size          int64
			Width, Height int
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "webp" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := webpVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
				t.Fatalf("verdict = %+v, want validated length %d", v, o.Size)
			}
			want := "dims=" + strconv.Itoa(o.Width) + "x" + strconv.Itoa(o.Height)
			if !slices.Contains(v.Basis, want) {
				t.Errorf("basis = %v, want %s", v.Basis, want)
			}
		})
	}
	if n < 2 {
		t.Fatalf("expected at least 2 webp objects, found %d", n)
	}
}
