package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// jpegTinyTables defines the smallest walkable Huffman tables: DC category 0 and AC end-of-block, both code 0,
// so a block is two 0 bits and a 64x48 frame (48 blocks) is 12 zero bytes.
func jpegTinyTables() []byte {
	return slices.Concat(carvetest.JPEGDHT(0, 0, [16]byte{1}, []byte{0}), carvetest.JPEGDHT(1, 0, [16]byte{1}, []byte{0}))
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
	tab := jpegTinyTables()
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
		{"window ends in entropy", cat(soi, sof, tab, sos, []byte{0, 0, 0, 0}), "truncated"},
		{"window ends after FF in entropy", cat(soi, sof, tab, sos, []byte{0, 0, 0, 0xFF}), "truncated"},
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
	// 65535 x 65535 declares far more blocks than the megabyte holds, so the walk keeps reading.
	head := slices.Concat([]byte{0xFF, 0xD8}, carvetest.JPEGSegment(0xC0, []byte{8, 0xFF, 0xFF, 0xFF, 0xFF, 1, 1, 0x11, 0}),
		jpegTinyTables(), carvetest.JPEGSegment(0xDA, []byte{1, 1, 0, 0, 63, 0}))
	buf := slices.Concat(head, make([]byte, 1<<20))
	b := newBudget(128 << 10)
	w := newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
	v := validateJPEG(w)
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted {
		t.Fatalf("verdict = %+v exhausted = %v, want rejected with an exhausted budget", v, b.exhausted)
	}
	if b.spent > b.limit {
		t.Fatalf("spent %d over limit %d", b.spent, b.limit)
	}
	// A cancelled context stops the walk.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = newWindow(ctx, bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30))
	if v := validateJPEG(w); v.Outcome != Rejected || v.Reason != "unreadable" {
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

func init() { implementedValidators["jpeg"] = true }

// failAt is a ReaderAt that fails with a non-EOF error from byte at on.
type failAt struct {
	data []byte
	at   int64
}

var errMedia = errors.New("media failure")

func (f failAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.at {
		return 0, errMedia
	}
	return copy(p, f.data[off:]), nil
}

// TestJPEGReadErrorIsUnreadable (ruling 3E-C1): a media read error at the SOI,
// inside a segment walk and inside entropy data is rejected "unreadable"
// (never "truncated") and is recorded in the window.
func TestJPEGReadErrorIsUnreadable(t *testing.T) {
	seg := carvetest.JPEGSegment
	app := func() []byte { return seg(0xE1, make([]byte, 30000)) }
	segWalk := slices.Concat([]byte{0xFF, 0xD8}, app(), app(), app(), app(), carvetest.JPEG(carvetest.JPEGOpts{})[2:])
	entropy := carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 200000})
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at SOI", carvetest.JPEG(carvetest.JPEGOpts{}), 0},
		{"mid segments", segWalk, 65536},
		{"mid entropy", entropy, 131072},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateJPEG(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

func TestJPEGScanHeaderAndEntropyIdentity(t *testing.T) {
	seg := carvetest.JPEGSegment
	soi := []byte{0xFF, 0xD8}
	sof := seg(0xC0, []byte{8, 0, 48, 0, 64, 1, 1, 0x11, 0})
	eoi := []byte{0xFF, 0xD9}
	tab := jpegTinyTables()
	ent := make([]byte, 12)
	run := func(parts ...[]byte) Verdict {
		o := slices.Concat(parts...)
		return validateJPEG(newWindow(context.Background(), bytes.NewReader(o), 0, int64(len(o)), newBudget(1<<30)))
	}
	data := []byte{1, 2, 3}
	for name, c := range map[string]struct {
		sos    []byte
		reason string
	}{
		"Ns 0":              {seg(0xDA, []byte{0, 0, 63, 0}), "bad-scan"},
		"Ns 5":              {seg(0xDA, []byte{5, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 0, 63, 0}), "bad-scan"},
		"length vs Ns":      {seg(0xDA, []byte{2, 1, 0, 0, 63, 0}), "bad-scan"},
		"length too short":  {seg(0xDA, []byte{1}), "bad-scan"},
		"length too long":   {seg(0xDA, []byte{1, 1, 0, 0, 63, 0, 9}), "bad-scan"},
		"no entropy at all": {seg(0xDA, []byte{1, 1, 0, 0, 63, 0}), "scan-short"},
	} {
		t.Run(name, func(t *testing.T) {
			parts := [][]byte{soi, sof, tab, c.sos}
			if c.reason == "bad-scan" {
				parts = append(parts, data)
			}
			v := run(append(parts, eoi)...)
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	// Only fill and a restart marker is still no entropy-coded data.
	sos := seg(0xDA, []byte{1, 1, 0, 0, 63, 0})
	if v := run(soi, sof, tab, sos, []byte{0xFF, 0xFF, 0xFF, 0xD0}, eoi); v.Outcome != Rejected || v.Reason != "scan-short" {
		t.Fatalf("fill-only scan verdict = %+v", v)
	}
	// A frame the walk does not cover (lossless, SOF3) keeps the marker-only scan and its empty-scan rule.
	sof3 := seg(0xC3, []byte{8, 0, 48, 0, 64, 1, 1, 0x11, 0})
	if v := run(soi, sof3, sos, eoi); v.Outcome != Rejected || v.Reason != "empty-scan" {
		t.Fatalf("unwalked empty scan verdict = %+v", v)
	}
	// Control: the 48 blocks validate.
	if v := run(soi, sof, tab, sos, ent, eoi); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

func TestJPEGMarkersInsideAndBetweenScans(t *testing.T) {
	seg := carvetest.JPEGSegment
	soi := []byte{0xFF, 0xD8}
	sof := seg(0xC0, []byte{8, 0, 48, 0, 64, 1, 1, 0x11, 0})
	sos := seg(0xDA, []byte{1, 1, 0, 0, 63, 0})
	dht := jpegTinyTables()
	ent := make([]byte, 12)
	eoi := []byte{0xFF, 0xD9}
	run := func(parts ...[]byte) Verdict {
		o := slices.Concat(parts...)
		return validateJPEG(newWindow(context.Background(), bytes.NewReader(o), 0, int64(len(o)), newBudget(1<<30)))
	}
	// A mid-entropy FF followed by a non-restart marker ends the scan and the
	// marker loop resumes: DHT then a second SOS is a valid two-scan file.
	v := run(soi, sof, dht, sos, ent, dht, sos, ent, eoi)
	if v.Outcome != Validated || !slices.Contains(v.Basis, "scans=2") {
		t.Fatalf("two-scan verdict = %+v", v)
	}
	// FF D8 inside entropy data ends it before the blocks are done (no nesting).
	v = run(soi, sof, dht, sos, []byte{0, 0, 0xFF, 0xD8, 0}, eoi)
	if v.Outcome != Rejected || v.Reason != "scan-short" {
		t.Fatalf("SOI-in-entropy verdict = %+v", v)
	}
	// Pinned: a second SOF is accepted but the walk stops covering the file (a footer hit); the dimensions
	// are the first frame's.
	sof2 := seg(0xC0, []byte{8, 0, 10, 0, 10, 1, 1, 0x11, 0})
	v = run(soi, sof, sof2, dht, sos, []byte{1}, eoi)
	if v.Outcome != Footer || !slices.Contains(v.Basis, "dims=64x48") || !slices.Contains(v.Assumptions, "entropy-not-walked=multiple-frames") {
		t.Fatalf("second-SOF verdict = %+v", v)
	}
	// Boundary: a segment ending exactly at the window end is truncated; the
	// same bytes plus an EOI inside the window validate.
	head := slices.Concat(soi, sof, dht, sos, ent, seg(0xFE, []byte("c")))
	if v := run(head); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("segment-at-window-end verdict = %+v", v)
	}
	if v := run(head, eoi); v.Outcome != Validated || v.Length != int64(len(head)+2) {
		t.Fatalf("segment + EOI verdict = %+v", v)
	}
}
