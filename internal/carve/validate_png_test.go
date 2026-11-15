package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func pngWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// pngVerdict validates obj followed by trail bytes of fill, the window being
// the whole buffer (what the scanner hands a validator at the run end).
func pngVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validatePNG(pngWindow(buf, newBudget(1<<30)))
}

func TestPNGValidatesBuilderVariants(t *testing.T) {
	for name, o := range map[string]carvetest.PNGOpts{
		"rgb8":        {},
		"palette4":    {ColorType: 3, BitDepth: 4},
		"rgba16":      {ColorType: 6, BitDepth: 16},
		"gray1":       {ColorType: 0, BitDepth: 1},
		"three IDATs": {IDATs: 3},
		"ancillary":   {Ancillary: []string{"tEXt", "pHYs"}},
		"apng":        {APNG: true, IDATs: 2},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.PNG(o)
			v := pngVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if !slices.Contains(v.Basis, "dims=64x48") {
				t.Errorf("basis = %v, want dims=64x48", v.Basis)
			}
			if !slices.ContainsFunc(v.Basis, func(s string) bool { return strings.HasPrefix(s, "chunks=") }) {
				t.Errorf("basis = %v, want chunks=", v.Basis)
			}
		})
	}
}

func TestPNGTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.PNG(carvetest.PNGOpts{})
	v := pngVerdict(obj, 4096)
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
}

func TestPNGRejects(t *testing.T) {
	ch := carvetest.PNGChunk
	sig := carvetest.PNGSignature
	good := carvetest.PNG(carvetest.PNGOpts{})
	ihdrData := func(w, h uint32, depth, colour, comp, filt, ilace byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, w)
		b = binary.BigEndian.AppendUint32(b, h)
		return append(b, depth, colour, comp, filt, ilace)
	}
	idat, iend := ch("IDAT", []byte{1, 2, 3}), ch("IEND", nil)
	withIHDR := func(d []byte) []byte { return slices.Concat(sig, ch("IHDR", d), idat, iend) }
	okIHDR := ihdrData(64, 48, 8, 2, 0, 0, 0)
	// A chunk whose declared length is 0x80000000: the length is checked
	// before the data is, so a short tail is enough.
	hugeLen := slices.Concat(good[:33], []byte{0x80, 0, 0, 0, 'I', 'D', 'A', 'T'}, make([]byte, 64))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"bad signature", slices.Concat([]byte("\x89PNG\r\n\x1a\x0b"), good[8:]), "bad-signature"},
		{"bad CRC", carvetest.PNG(carvetest.PNGOpts{BadCRC: true}), "crc"},
		{"IHDR not first", carvetest.PNG(carvetest.PNGOpts{IHDRNotFirst: true}), "no-ihdr"},
		{"IHDR wrong length", slices.Concat(sig, ch("IHDR", make([]byte, 12)), idat, iend), "no-ihdr"},
		{"zero width", carvetest.PNG(carvetest.PNGOpts{ZeroWidth: true}), "zero-dimensions"},
		{"zero height", withIHDR(ihdrData(64, 0, 8, 2, 0, 0, 0)), "zero-dimensions"},
		{"no IDAT", carvetest.PNG(carvetest.PNGOpts{NoIDAT: true}), "no-idat"},
		{"colour 2 depth 4", carvetest.PNG(carvetest.PNGOpts{ColorType: 2, BitDepth: 4}), "bad-ihdr"},
		{"colour 3 depth 16", carvetest.PNG(carvetest.PNGOpts{ColorType: 3, BitDepth: 16}), "bad-ihdr"},
		{"colour 5", carvetest.PNG(carvetest.PNGOpts{ColorType: 5, BitDepth: 8}), "bad-ihdr"},
		{"compression 1", withIHDR(ihdrData(64, 48, 8, 2, 1, 0, 0)), "bad-ihdr"},
		{"filter 1", withIHDR(ihdrData(64, 48, 8, 2, 0, 1, 0)), "bad-ihdr"},
		{"interlace 2", withIHDR(ihdrData(64, 48, 8, 2, 0, 0, 2)), "bad-ihdr"},
		{"width over 2^31-1", withIHDR(ihdrData(0x80000000, 48, 8, 2, 0, 0, 0)), "bad-ihdr"},
		{"second IHDR", slices.Concat(sig, ch("IHDR", okIHDR), ch("IHDR", okIHDR), idat, iend), "bad-ihdr"},
		{"chunk length 0x80000000", hugeLen, "bad-length"},
		{"chunk type IH1R", slices.Concat(sig, ch("IH1R", okIHDR), idat, iend), "bad-chunk-type"},
		{"IEND with payload", carvetest.PNG(carvetest.PNGOpts{IENDPayload: true}), "bad-iend"},
		{"cut 10 bytes short", good[:len(good)-10], "truncated"},
		{"IEND omitted", carvetest.PNG(carvetest.PNGOpts{OmitIEND: true}), "truncated"},
		{"only the signature", slices.Clone(sig), "truncated"},
		{"one byte", []byte{0x89}, "truncated"},
		{"chunk longer than window", slices.Concat(sig, ch("IHDR", okIHDR), []byte{0x00, 0x10, 0, 0, 'I', 'D', 'A', 'T'}, make([]byte, 100)), "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validatePNG(pngWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := pngVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestCarveNeverInflates: the validator proves the length from the chunk
// structure and never decompresses an IDAT. A 1,000-byte deflate stream that
// would inflate to 1 MiB is read once and no more than its own bytes. The
// package purity table (Task 2) bounds the import closure; this test
// additionally checks that no file of the package imports a compress/ package
// directly.
func TestCarveNeverInflates(t *testing.T) {
	stream := make([]byte, 1000) // stands for 1 MiB of zeros deflated
	stream[0], stream[1] = 0x78, 0x9C
	obj := carvetest.PNG(carvetest.PNGOpts{IDATData: stream})
	b := newBudget(1 << 30)
	v := validatePNG(pngWindow(obj, b))
	if v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
	if b.spent > int64(len(obj)) {
		t.Fatalf("read %d bytes of a %d-byte object", b.spent, len(obj))
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, "compress/") || strings.HasPrefix(p, "archive/") {
				t.Errorf("%s imports %s", f, imp.Path.Value)
			}
		}
	}
}

// TestPNGWindowBoundsAndBudget: a chunk declaring more than the window holds
// is a typed truncation (no allocation from the declared length); a CRC pass
// that outruns the budget, and a cancelled context, are rejected "unreadable"
// (ruling 3E-C1).
func TestPNGWindowBoundsAndBudget(t *testing.T) {
	good := carvetest.PNG(carvetest.PNGOpts{})
	// IHDR (33 bytes with the signature) then an IDAT declaring 0x7FFFFFFF.
	buf := slices.Concat(good[:33], []byte{0x7F, 0xFF, 0xFF, 0xFF, 'I', 'D', 'A', 'T'}, bytes.Repeat([]byte{0x41}, 1<<20))
	if v := validatePNG(pngWindow(buf, newBudget(128<<10))); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("oversized chunk verdict = %+v", v)
	}
	big := carvetest.PNG(carvetest.PNGOpts{IDATData: bytes.Repeat([]byte{7}, 1<<20)})
	b := newBudget(128 << 10)
	v := validatePNG(pngWindow(big, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(big), 0, int64(len(big)), newBudget(1<<30))
	if v := validatePNG(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

// TestPNGReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// signature, in the chunk headers and inside chunk data is rejected
// "unreadable" (never "truncated") and recorded in the window.
func TestPNGReadErrorIsUnreadable(t *testing.T) {
	many := carvetest.PNG(carvetest.PNGOpts{IDATs: 40, IDATData: bytes.Repeat([]byte{9}, 40*3000)})
	data := carvetest.PNG(carvetest.PNGOpts{IDATData: bytes.Repeat([]byte{9}, 200000)})
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at signature", carvetest.PNG(carvetest.PNGOpts{}), 0},
		{"mid chunk headers", many, 60000},
		{"mid chunk data", data, 100000},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validatePNG(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

func TestPNGRealEncoderObjects(t *testing.T) {
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
		if o.Family != "png" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := pngVerdict(obj, 4096)
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
		t.Fatalf("expected at least 2 png objects, found %d", n)
	}
}
