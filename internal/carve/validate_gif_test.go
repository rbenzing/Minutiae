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

func gifWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// gifVerdict validates obj followed by trail bytes of fill, the window being
// the whole buffer.
func gifVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateGIF(gifWindow(buf, newBudget(1<<30)))
}

func TestGIFValidatesBuilderVariants(t *testing.T) {
	ext := slices.Concat(
		carvetest.GIFExtension(0xFE, []byte("a comment")),
		carvetest.GIFExtension(0xFF, []byte("NETSCAPE2.0")),
		carvetest.GIFExtension(0xFF, nil),
	)
	for name, c := range map[string]struct {
		o      carvetest.GIFOpts
		frames string
	}{
		"87a":              {carvetest.GIFOpts{Version: "GIF87a"}, "frames=1"},
		"89a with GCT":     {carvetest.GIFOpts{GCT: true}, "frames=1"},
		"with LCT":         {carvetest.GIFOpts{LCT: true}, "frames=1"},
		"with GCE":         {carvetest.GIFOpts{}, "frames=1"},
		"extensions":       {carvetest.GIFOpts{Extensions: ext}, "frames=1"},
		"three frames":     {carvetest.GIFOpts{GCT: true, LCT: true, Frames: 3}, "frames=3"},
		"short image data": {carvetest.GIFOpts{DataLen: 1}, "frames=1"},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.GIF(c.o)
			v := gifVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if !slices.Contains(v.Basis, "dims=64x48") || !slices.Contains(v.Basis, c.frames) {
				t.Errorf("basis = %v, want dims=64x48 and %s", v.Basis, c.frames)
			}
		})
	}
}

func TestGIFTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.GIF(carvetest.GIFOpts{})
	v := gifVerdict(obj, 4096)
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
}

func TestGIFRejects(t *testing.T) {
	good := carvetest.GIF(carvetest.GIFOpts{})
	hdr := carvetest.GIF(carvetest.GIFOpts{NoFrames: true, NoTrailer: true})
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"not GIF", cat([]byte("PNG89a"), good[6:]), "bad-signature"},
		{"version GIF88a", carvetest.GIF(carvetest.GIFOpts{Version: "GIF88a"}), "bad-version"},
		{"zero screen width", carvetest.GIF(carvetest.GIFOpts{ZeroDims: true}), "zero-dimensions"},
		{"zero byte at block start", cat(hdr, []byte{0x00}, good[len(hdr):]), "bad-block"},
		{"unknown block 0x40", cat(hdr, []byte{0x40, 0, 0, 0}), "bad-block"},
		{"no trailer", carvetest.GIF(carvetest.GIFOpts{NoTrailer: true}), "truncated"},
		{"trailer with no image", carvetest.GIF(carvetest.GIFOpts{NoFrames: true}), "no-image"},
		{"extension only then trailer", carvetest.GIF(carvetest.GIFOpts{NoFrames: true, Extensions: carvetest.GIFExtension(0xFE, []byte("x"))}), "no-image"},
		{"LZW size 12", carvetest.GIF(carvetest.GIFOpts{LZWMin: 12}), "bad-lzw"},
		{"LZW size 1", carvetest.GIF(carvetest.GIFOpts{LZWMin: 1}), "bad-lzw"},
		{"zero image width", cat(hdr, []byte{0x2C, 0, 0, 0, 0, 0, 0, 8, 0, 0, 8, 0}, []byte{0x3B}), "zero-dimensions"},
		{"cut inside image data", good[:len(good)-20], "truncated"},
		{"cut inside global table", carvetest.GIF(carvetest.GIFOpts{GCT: true})[:15], "truncated"},
		{"cut inside image descriptor", cat(hdr, []byte{0x2C, 0, 0, 0}), "truncated"},
		{"sub-block longer than window", cat(hdr, []byte{0x21, 0xFE, 0xFF}, make([]byte, 50)), "truncated"},
		{"only the signature", []byte("GIF89a"), "truncated"},
		{"one byte", []byte("G"), "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateGIF(gifWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := gifVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestGIFWindowBoundsAndBudget: a long run of one-byte sub-blocks is stopped
// by the budget, a cancelled context by the context; both are rejected
// "unreadable" (ruling 3E-C1). No loop iteration fails to advance.
func TestGIFWindowBoundsAndBudget(t *testing.T) {
	hdr := carvetest.GIF(carvetest.GIFOpts{NoFrames: true, NoTrailer: true})
	buf := slices.Concat(hdr, []byte{0x2C, 0, 0, 0, 0, 8, 0, 8, 0, 0, 8}, bytes.Repeat([]byte{1, 0x41}, 1<<19))
	b := newBudget(128 << 10)
	v := validateGIF(gifWindow(buf, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30))
	if v := validateGIF(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

// TestGIFReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// header, inside the block walk and inside image data is rejected "unreadable"
// (never "truncated") and recorded in the window.
func TestGIFReadErrorIsUnreadable(t *testing.T) {
	frames := carvetest.GIF(carvetest.GIFOpts{Frames: 5, DataLen: 30000})
	one := carvetest.GIF(carvetest.GIFOpts{DataLen: 200000})
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at header", carvetest.GIF(carvetest.GIFOpts{}), 0},
		{"mid blocks", frames, 70000},
		{"mid image data", one, 131072},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateGIF(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

func TestGIFRealEncoderObjects(t *testing.T) {
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
			Facts         struct {
				NFrames int `json:"n_frames"`
			}
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "gif" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := gifVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
				t.Fatalf("verdict = %+v, want validated length %d", v, o.Size)
			}
			want := "dims=" + strconv.Itoa(o.Width) + "x" + strconv.Itoa(o.Height)
			if !slices.Contains(v.Basis, want) {
				t.Errorf("basis = %v, want %s", v.Basis, want)
			}
			if wf := "frames=" + strconv.Itoa(o.Facts.NFrames); !slices.Contains(v.Basis, wf) {
				t.Errorf("basis = %v, want %s", v.Basis, wf)
			}
		})
	}
	if n < 2 {
		t.Fatalf("expected at least 2 gif objects, found %d", n)
	}
}
