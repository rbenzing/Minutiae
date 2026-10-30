package carve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// Tests of the entropy-coded data walk (3E ruling 3E-C5): the validator decodes every scan with the frame's
// Huffman tables and rejects what a fragmented or spliced file cannot satisfy.

func jpegWalk(obj []byte) Verdict {
	return validateJPEG(newWindow(context.Background(), bytes.NewReader(obj), 0, int64(len(obj)), newBudget(1<<30)))
}

// jpegPiece is a progressive 8x8 frame of one block whose scans are written by hand: the DC table has category
// 0 as code 0; the AC table has RS 0x01 (code 00), EOB0 (code 01) and EOB1 (code 10).
type jpegPieces struct {
	soi, sof, tables []byte
}

func newJPEGPieces(progressive bool, w, h int) jpegPieces {
	code := byte(0xC0)
	if progressive {
		code = 0xC2
	}
	return jpegPieces{
		soi: []byte{0xFF, 0xD8},
		sof: carvetest.JPEGSegment(code, []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 1, 1, 0x11, 0}),
		tables: slices.Concat(
			carvetest.JPEGDHT(0, 0, [16]byte{1}, []byte{0}),
			carvetest.JPEGDHT(1, 0, [16]byte{0, 3}, []byte{0x01, 0x00, 0x10})),
	}
}

func jpegSOS(ss, se, ahal byte) []byte {
	return carvetest.JPEGSegment(0xDA, []byte{1, 1, 0, ss, se, ahal})
}

var jpegEOI = []byte{0xFF, 0xD9}

func TestJPEGWalkProgressiveRefinement(t *testing.T) {
	p := newJPEGPieces(true, 8, 8)
	build := func(dc, first, refine []byte) []byte {
		return slices.Concat(p.soi, p.sof, p.tables,
			jpegSOS(0, 0, 0), dc,
			jpegSOS(1, 63, 0x01), first, // Al 1
			jpegSOS(1, 63, 0x10), refine, // Ah 1, Al 0
			jpegEOI)
	}
	dc := []byte{0x7F}     // DC category 0, then 1 padding bits
	first := []byte{0x2F}  // RS 0x01 (00), value bit 1, EOB0 (01), padding 111: coefficient 1 becomes nonzero
	refine := []byte{0x37} // RS 0x01 (00), sign 1, correction bit of coefficient 1 (1), EOB0 (01), padding 11
	obj := build(dc, first, refine)
	v := jpegWalk(obj)
	if v.Outcome != Validated || v.Length != int64(len(obj)) || !slices.Contains(v.Basis, "scans=3") || !slices.Contains(v.Basis, "entropy-walked") {
		t.Fatalf("verdict = %+v, want validated, 3 scans, walked", v)
	}
	for name, c := range map[string]struct {
		dc, first, refine []byte
		reason            string
	}{
		"zero padding":                 {dc, first, []byte{0x34}, "bad-padding"},
		"extra data after the block":   {dc, first, []byte{0x37, 0x00}, "scan-overrun"},
		"missing EOB after refinement": {dc, first, []byte{0x3F}, "scan-short"},
		"unknown code":                 {dc, []byte{0xFF, 0x00, 0xFF, 0x00}, refine, "bad-code"},
		"DC with extra block bits":     {[]byte{0x00}, first, refine, "bad-padding"},
	} {
		t.Run(name, func(t *testing.T) {
			v := jpegWalk(build(c.dc, c.first, c.refine))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	// A refinement scan symbol with a size other than 1 is no refinement.
	p2 := p
	p2.tables = slices.Concat(carvetest.JPEGDHT(0, 0, [16]byte{1}, []byte{0}), carvetest.JPEGDHT(1, 0, [16]byte{0, 3}, []byte{0x02, 0x00, 0x10}))
	o2 := slices.Concat(p2.soi, p2.sof, p2.tables, jpegSOS(0, 0, 0), dc, jpegSOS(1, 63, 0x01), []byte{0x2F}, jpegSOS(1, 63, 0x10), []byte{0x37}, jpegEOI)
	if v := jpegWalk(o2); v.Outcome != Rejected {
		t.Fatalf("size-2 refinement verdict = %+v, want rejected", v)
	}
}

func TestJPEGWalkRejects(t *testing.T) {
	seg := carvetest.JPEGSegment
	base := carvetest.JPEG(carvetest.JPEGOpts{Restart: true, EntropyLen: 200})
	if v := jpegWalk(base); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
	rst := bytes.Index(base, []byte{0xFF, 0xD0})
	if rst < 0 {
		t.Fatal("the builder wrote no restart marker")
	}
	swapped := slices.Clone(base)
	swapped[rst+1] = 0xD3
	noRst := slices.Clone(base)
	noRst = slices.Delete(noRst, rst, rst+2)
	plain := carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 200})
	sofAt := bytes.Index(plain, []byte{0xFF, 0xC0})
	taller, shorter := slices.Clone(plain), slices.Clone(plain)
	taller[sofAt+5] = 1 // height 48 -> 304
	shorter[sofAt+6] = 16
	noTables := slices.Clone(base)
	dht := bytes.Index(noTables, []byte{0xFF, 0xC4})
	noTables = slices.Delete(noTables, dht, dht+len(carvetest.JPEGDHT(0, 0, [16]byte{1, 1}, []byte{0, 8})))
	progMissingAC := newJPEGPieces(true, 8, 8)
	for name, c := range map[string]struct {
		obj    []byte
		reason string
	}{
		"restart out of order":  {swapped, "restart-order"},
		"restart missing":       {noRst, "restart-order"},
		"more blocks declared":  {taller, "scan-short"},
		"fewer blocks declared": {shorter, "scan-overrun"},
		"missing DC table":      {noTables, "missing-table"},
		"progressive without AC table": {slices.Concat(progMissingAC.soi, progMissingAC.sof,
			carvetest.JPEGDHT(0, 0, [16]byte{1}, []byte{0}), jpegSOS(1, 63, 0), []byte{0}, jpegEOI), "missing-table"},
		"progressive AC over several components": {slices.Concat(progMissingAC.soi, seg(0xC2, []byte{8, 0, 8, 0, 8, 2, 1, 0x11, 0, 2, 0x11, 0}),
			progMissingAC.tables, seg(0xDA, []byte{2, 1, 0, 2, 0, 1, 63, 0}), []byte{0}, jpegEOI), "bad-scan"},
		"overflowing Huffman counts": {slices.Concat(progMissingAC.soi, progMissingAC.sof,
			carvetest.JPEGDHT(0, 0, [16]byte{3}, []byte{0, 1, 2}), jpegSOS(0, 0, 0), []byte{0}, jpegEOI), "bad-huffman"},
	} {
		t.Run(name, func(t *testing.T) {
			v := jpegWalk(c.obj)
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
}

// TestJPEGWalkRejectsFragmentFillTail is the fragmented-file control of the fixtures: the first cluster of a
// file, a cluster of filler and the rest of the file is no JPEG, whatever markers it ends with.
func TestJPEGWalkRejectsFragmentFillTail(t *testing.T) {
	objs := map[string][]byte{
		"builder baseline":    carvetest.JPEG(carvetest.JPEGOpts{EntropyLen: 5000}),
		"builder progressive": carvetest.JPEG(carvetest.JPEGOpts{Progressive: true, EntropyLen: 5000}),
	}
	for _, f := range []string{"jpeg-baseline.jpg", "jpeg-progressive.jpg"} {
		b, err := os.ReadFile(filepath.Join("testdata", "objects", f))
		if err != nil {
			t.Fatal(err)
		}
		objs[f] = b
	}
	for name, obj := range objs {
		t.Run(name, func(t *testing.T) {
			if v := jpegWalk(obj); v.Outcome != Validated {
				t.Fatalf("whole object verdict = %+v", v)
			}
			cut := min(len(obj)/3, 4096)
			fill := bytes.Repeat(fillPattern, 4096/len(fillPattern)+1)[:4096]
			spliced := slices.Concat(obj[:cut], fill, obj[cut:])
			if v := jpegWalk(spliced); v.Outcome != Rejected {
				t.Fatalf("fragment + fill + tail verdict = %+v, want rejected", v)
			}
		})
	}
}

// TestJPEGWalkHostileDimensionsAreAFooterWithoutAllocation: a progressive frame declaring 65535 x 65535 would
// need a refinement state far over the cap; the hit is a footer with the reason, and nothing large is allocated.
func TestJPEGWalkHostileDimensionsAreAFooterWithoutAllocation(t *testing.T) {
	p := newJPEGPieces(true, 0xFFFF, 0xFFFF)
	obj := slices.Concat(p.soi, p.sof, p.tables, jpegSOS(0, 0, 0), []byte{0x7F}, jpegSOS(1, 63, 0), []byte{0x00}, jpegEOI)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	v := jpegWalk(obj)
	runtime.ReadMemStats(&after)
	if v.Outcome != Footer || v.Length != int64(len(obj)) || !slices.Contains(v.Assumptions, "entropy-not-walked=refinement-state-cap") {
		t.Fatalf("verdict = %+v, want a footer over the refinement cap", v)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("the hostile frame allocated %d bytes", got)
	}
	// The same frame as baseline allocates no state at all: its walk ends at the data.
	b := newJPEGPieces(false, 0xFFFF, 0xFFFF)
	obj = slices.Concat(b.soi, b.sof, b.tables, jpegSOS(0, 63, 0), []byte{0x00}, jpegEOI)
	runtime.ReadMemStats(&before)
	v = jpegWalk(obj)
	runtime.ReadMemStats(&after)
	if v.Outcome != Rejected || v.Reason != "scan-short" {
		t.Fatalf("hostile baseline verdict = %+v, want rejected scan-short", v)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("the hostile baseline allocated %d bytes", got)
	}
}

// TestJPEGWalkEOBRunsAreCheap: a few bytes of EOB runs cover 62500 blocks, in the first scan and in a
// refinement scan, in time linear in the blocks.
func TestJPEGWalkEOBRunsAreCheap(t *testing.T) {
	const w, h = 2000, 2000 // 250 x 250 blocks
	const blocks = 250 * 250
	var dc carvetest.JPEGBits
	for range blocks {
		dc.Put(0, 1) // DC category 0
	}
	var eob carvetest.JPEGBits
	eob.Put(0, 1) // RS 0xE0: EOB run of 2^14 + 14 bits
	eob.Put(32767-16384, 14)
	eob.Put(0, 1)
	eob.Put(blocks-32767-16384, 14)
	tables := slices.Concat(
		carvetest.JPEGDHT(0, 0, [16]byte{1}, []byte{0}),
		carvetest.JPEGDHT(1, 0, [16]byte{1}, []byte{0xE0}))
	p := newJPEGPieces(true, w, h)
	obj := slices.Concat(p.soi, p.sof, tables, jpegSOS(0, 0, 0), dc.Bytes(),
		jpegSOS(1, 63, 0x01), eob.Bytes(), jpegSOS(1, 63, 0x10), eob.Bytes(), jpegEOI)
	start := time.Now()
	v := jpegWalk(obj)
	if v.Outcome != Validated || !slices.Contains(v.Basis, "scans=3") {
		t.Fatalf("verdict = %+v, want validated", v)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the EOB-run walk took %v", d)
	}
	// A run longer than the blocks left is rejected, not extended.
	var over carvetest.JPEGBits
	over.Put(0, 1)
	over.Put(32767-16384, 14)
	over.Put(0, 1)
	over.Put(blocks-32767-16384+1, 14)
	obj = slices.Concat(p.soi, p.sof, tables, jpegSOS(0, 0, 0), dc.Bytes(), jpegSOS(1, 63, 0x01), over.Bytes(), jpegEOI)
	if v := jpegWalk(obj); v.Outcome != Rejected || v.Reason != "eobrun-overrun" {
		t.Fatalf("overlong run verdict = %+v", v)
	}
}

// TestJPEGWalkUnsupportedFramesAreFooters: a frame type the walk does not cover keeps its marker length but is no
// validated hit.
func TestJPEGWalkUnsupportedFramesAreFooters(t *testing.T) {
	p := newJPEGPieces(false, 8, 8)
	for name, code := range map[string]byte{"lossless": 0xC3, "arithmetic": 0xC9} {
		sof := carvetest.JPEGSegment(code, []byte{8, 0, 8, 0, 8, 1, 1, 0x11, 0})
		obj := slices.Concat(p.soi, sof, jpegSOS(0, 63, 0), []byte{1, 2, 3}, jpegEOI)
		v := jpegWalk(obj)
		if v.Outcome != Footer || !slices.Contains(v.Assumptions, "entropy-not-walked=frame-type") {
			t.Fatalf("%s verdict = %+v, want a footer", name, v)
		}
	}
}
