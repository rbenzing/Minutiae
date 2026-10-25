package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// jpegVerdict validates obj followed by trail bytes of fill, the window being
// the whole buffer (what the scanner hands a validator at the run end).
func jpegVerdict(t *testing.T, obj []byte, trail int) Verdict {
	t.Helper()
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateJPEG(newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30)))
}

func TestJPEGValidatesBuilderVariants(t *testing.T) {
	thumb := carvetest.JPEG(carvetest.JPEGOpts{W: 8, H: 8})
	for name, o := range map[string]carvetest.JPEGOpts{
		"baseline":    {},
		"progressive": {Progressive: true},
		"restart":     {Restart: true},
		"fill":        {FillBytes: true},
		"thumbnail":   {Thumbnail: thumb},
		"all":         {Progressive: true, Restart: true, FillBytes: true, Thumbnail: thumb, EntropyLen: 500},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.JPEG(o)
			v := jpegVerdict(t, obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if !slices.Contains(v.Basis, "dims=64x48") {
				t.Errorf("basis = %v, want dims=64x48", v.Basis)
			}
			if o.Progressive != slices.Contains(v.Basis, "progressive") {
				t.Errorf("basis = %v, progressive flag mismatch", v.Basis)
			}
		})
	}
}

func TestJPEGTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.JPEG(carvetest.JPEGOpts{})
	v := jpegVerdict(t, obj, 4096)
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
}

func TestJPEGRejects(t *testing.T) {
	good := carvetest.JPEG(carvetest.JPEGOpts{})
	seg := carvetest.JPEGSegment
	sof := seg(0xC0, []byte{8, 0, 48, 0, 64, 1, 1, 0x11, 0})
	sos := seg(0xDA, []byte{1, 1, 0, 0, 63, 0})
	soi := []byte{0xFF, 0xD8}
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"omit EOI", carvetest.JPEG(carvetest.JPEGOpts{OmitEOI: true}), "truncated"},
		{"zero width", carvetest.JPEG(carvetest.JPEGOpts{ZeroWidth: true}), "zero-dimensions"},
		{"scan before frame", cat(soi, sos, []byte{1, 2, 3}, []byte{0xFF, 0xD9}), "no-frame"},
		{"segment length 1", cat(soi, []byte{0xFF, 0xE0, 0, 1}, sof), "bad-length"},
		{"segment longer than window", cat(soi, []byte{0xFF, 0xE0, 0xFF, 0xF0}, make([]byte, 100)), "truncated"},
		{"second SOI", cat(soi, []byte{0xFF, 0xD8}), "bad-marker"},
		{"restart outside entropy", cat(soi, []byte{0xFF, 0xD3}), "bad-marker"},
		{"zero marker", cat(soi, []byte{0xFF, 0x00}), "bad-marker"},
		{"non-marker byte", cat(soi, []byte{0x12, 0x34}), "bad-marker"},
		{"EOI right after SOI", cat(soi, []byte{0xFF, 0xD9}), "no-scan"},
		{"frame without components", cat(soi, seg(0xC0, []byte{8, 0, 48, 0, 64, 0})), "bad-frame"},
		{"frame length mismatch", cat(soi, seg(0xC0, []byte{8, 0, 48, 0, 64, 1, 1, 0x11})), "bad-frame"},
		{"zero height", cat(soi, seg(0xC0, []byte{8, 0, 0, 0, 64, 1, 1, 0x11, 0})), "zero-dimensions"},
		{"window ends in entropy", cat(soi, sof, sos, []byte{1, 2, 3, 4}), "truncated"},
		{"window ends after FF in entropy", cat(soi, sof, sos, []byte{1, 2, 3, 0xFF}), "truncated"},
		{"only SOI", soi, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateJPEG(newWindow(context.Background(), bytes.NewReader(c.obj), 0, int64(len(c.obj)), newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	// Control: the good object is not rejected by the table's shared prefix.
	if v := jpegVerdict(t, good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestJPEGWindowBoundsAndBudget: a greedy walk over a huge window of entropy
// bytes is stopped by the budget (a typed rejection, never a loop or an
// allocation from a declared size).
func TestJPEGWindowBoundsAndBudget(t *testing.T) {
	head := slices.Concat([]byte{0xFF, 0xD8}, carvetest.JPEGSegment(0xC0, []byte{8, 0, 48, 0, 64, 1, 1, 0x11, 0}),
		carvetest.JPEGSegment(0xDA, []byte{1, 1, 0, 0, 63, 0}))
	buf := slices.Concat(head, bytes.Repeat([]byte{0x41}, 1<<20))
	b := newBudget(128 << 10)
	w := newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
	v := validateJPEG(w)
	if v.Outcome != Rejected || !b.exhausted {
		t.Fatalf("verdict = %+v exhausted = %v, want rejected with an exhausted budget", v, b.exhausted)
	}
	if b.spent > b.limit {
		t.Fatalf("spent %d over limit %d", b.spent, b.limit)
	}
	// A cancelled context stops the walk.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = newWindow(ctx, bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30))
	if v := validateJPEG(w); v.Outcome != Rejected {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

func TestJPEGRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family string
			Size         int64
			Width        int
			Height       int
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "jpeg" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := jpegVerdict(t, obj, 4096)
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
		t.Fatalf("expected at least 2 jpeg objects, found %d", n)
	}
}
