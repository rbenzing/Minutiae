package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

var fillPattern = []byte("MINUTIAE-CARVE-FILL-")

const kib = 1 << 10

const toyHdr = "TOYOBJ\x00\x01"

// toyObj builds a toy object: an 8-byte header, a 4-byte big-endian total
// length, filler and the trailer "END!". total must be at least 16.
func toyObj(hdr string, total int) []byte {
	b := bytes.Repeat([]byte{'.'}, total)
	copy(b, hdr)
	binary.BigEndian.PutUint32(b[8:], uint32(total))
	copy(b[total-4:], "END!")
	return b
}

// testValidators holds the validators the scanner tests inject.
func testValidators() map[string]validatorFunc {
	toy := func(w *Window) Verdict {
		var h [12]byte
		if _, err := w.ReadAt(h[:], 0); err != nil {
			return Verdict{Outcome: Rejected}
		}
		l := int64(binary.BigEndian.Uint32(h[8:]))
		if l < 16 {
			return Verdict{Outcome: Rejected, Reason: "bad-length"}
		}
		var e [4]byte
		if _, err := w.ReadAt(e[:], l-4); err != nil {
			return Verdict{Outcome: Rejected}
		}
		if string(e[:]) != "END!" {
			return Verdict{Outcome: Rejected, Reason: "bad-end"}
		}
		return Verdict{Outcome: Validated, Length: l}
	}
	return map[string]validatorFunc{
		"test": toy,
		"test-refine": func(w *Window) Verdict {
			v := toy(w)
			if v.Outcome == Validated {
				v.Type, v.Ext = "refined", "rfn"
			}
			return v
		},
		// swallows a read error and still validates the length the header declares
		"test-swallow": func(w *Window) Verdict {
			var h [12]byte
			if _, err := w.ReadAt(h[:], 0); err != nil {
				return Verdict{Outcome: Rejected}
			}
			l := int64(binary.BigEndian.Uint32(h[8:]))
			var mid [4]byte
			_, _ = w.ReadAt(mid[:], l-300)
			return Verdict{Outcome: Validated, Length: l}
		},
		// reads an unreadable byte, then a byte outside the window, then rejects
		"test-both": func(w *Window) Verdict {
			var b [4]byte
			_, _ = w.ReadAt(b[:], 1000)
			_, _ = w.ReadAt(b[:], w.Len()+8)
			return Verdict{Outcome: Rejected}
		},
		// validates from the header alone and never reads the body
		"test-blind": func(w *Window) Verdict {
			var h [12]byte
			if _, err := w.ReadAt(h[:], 0); err != nil {
				return Verdict{Outcome: Rejected}
			}
			return Verdict{Outcome: Validated, Length: int64(binary.BigEndian.Uint32(h[8:]))}
		},
		"test-panic": func(*Window) Verdict { panic("validator bug") },
		"test-onebyte": func(w *Window) Verdict {
			if c, err := w.Byte(8); err != nil || c != 'K' {
				return Verdict{Outcome: Rejected, Reason: "bad-byte"}
			}
			return Verdict{Outcome: Validated, Length: 16}
		},
		"test-long": func(*Window) Verdict {
			return Verdict{Outcome: Validated, Length: 16, Basis: []string{strings.Repeat("b", 400), "short"}, Assumptions: []string{strings.Repeat("a", 300)}}
		},
		"test-greedy": func(w *Window) Verdict {
			buf := make([]byte, 256*kib)
			for off := int64(0); ; off += int64(len(buf)) {
				if _, err := w.ReadAt(buf, off); err != nil {
					return Verdict{Outcome: Rejected}
				}
			}
		},
		"test-walk": func(w *Window) Verdict {
			for off := int64(0); off < w.Len(); off++ {
				if _, err := w.Byte(off); err != nil {
					return Verdict{Outcome: Rejected}
				}
			}
			return Verdict{Outcome: Rejected, Reason: "walked"}
		},
	}
}

func toyType(name, hdr, validate string) Type {
	return Type{
		Name: name, Ext: name, MIME: "application/x-" + name,
		Headers: []Pattern{{Offset: 0, Bytes: []byte(hdr)}},
		MinLen:  16, MaxLen: 1 << 20, Validate: validate, Align: "block",
	}
}

func toySet(types ...Type) *Set {
	if len(types) == 0 {
		types = []Type{toyType("toy", toyHdr, "test")}
	}
	return &Set{ID: "test-set", Types: types}
}

func runScan(t *testing.T, r io.ReaderAt, runs []Run, set *Set, o Options) ([]Hit, Stats, error) {
	t.Helper()
	var hits []Hit
	st, err := scan(context.Background(), r, runs, set, o, testValidators(), func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	return hits, st, err
}

func whole(img []byte) []Run { return []Run{{0, int64(len(img))}} }

func TestCarveNeverSpansRuns(t *testing.T) {
	obj := toyObj(toyHdr, 100)

	// Split across two adjacent runs: the object is not found and is counted.
	img := carvetest.Image(64*kib, fillPattern)
	carvetest.Place(img, 4096+3000, obj)
	split := int64(4096 + 3050)
	hits, st, err := runScan(t, bytes.NewReader(img), []Run{{0, split}, {split, int64(len(img)) - split}}, toySet(), Options{Align: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || st.Rejected["truncated"] != 1 {
		t.Errorf("split object: %d hits, rejected %v, want 0 hits and truncated 1", len(hits), st.Rejected)
	}

	// The same object inside the second run.
	hits, _, err = runScan(t, bytes.NewReader(img), []Run{{0, 4096}, {4096, int64(len(img)) - 4096}}, toySet(), Options{Align: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Run != 1 || hits[0].Offset != 4096+3000 || hits[0].Length != 100 {
		t.Fatalf("object inside run 1: %+v", hits)
	}

	// 50 random layouts: every hit lies inside its run.
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a seeded generator for reproducible layouts
	for it := range 50 {
		img := carvetest.Image(32*kib, fillPattern)
		for range 6 {
			carvetest.Place(img, rng.IntN(len(img)-200), toyObj(toyHdr, 20+rng.IntN(150)))
		}
		var runs []Run
		for pos := int64(0); pos < int64(len(img)); {
			if rng.IntN(3) == 0 {
				pos += int64(rng.IntN(300)) // a gap
			}
			l := min(int64(1+rng.IntN(5000)), int64(len(img))-pos)
			if l > 0 {
				runs = append(runs, Run{pos, l})
			}
			pos += max(l, 1)
		}
		hits, _, err := runScan(t, bytes.NewReader(img), runs, toySet(), Options{Align: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			r := runs[h.Run]
			if h.Offset < r.Offset || h.Offset+h.Length > r.Offset+r.Length {
				t.Fatalf("layout %d: hit %d+%d leaves run %d (%d+%d)", it, h.Offset, h.Length, h.Run, r.Offset, r.Length)
			}
		}
	}
}

func TestScanAlignment(t *testing.T) {
	// The run starts at 1000: the grid is 1000 + k*4096, not absolute multiples.
	img := carvetest.Image(64*kib, fillPattern)
	carvetest.Place(img, 1000+4096, toyObj(toyHdr, 64))
	carvetest.Place(img, 1000+4096*3+7, toyObj(toyHdr, 64))
	carvetest.Place(img, 4096*5, toyObj(toyHdr, 64)) // an absolute multiple, off the run grid
	runs := []Run{{1000, int64(len(img)) - 1000}}

	hits, _, err := runScan(t, bytes.NewReader(img), runs, toySet(), Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != 1000+4096 {
		t.Fatalf("Align 4096: %+v, want only offset %d", hits, 1000+4096)
	}
	hits, _, err = runScan(t, bytes.NewReader(img), runs, toySet(), Options{Align: 1})
	if err != nil {
		t.Fatal(err)
	}
	var offs []int64
	for _, h := range hits {
		offs = append(offs, h.Offset)
	}
	if !slices.Equal(offs, []int64{1000 + 4096, 1000 + 4096*3 + 7, 4096 * 5}) {
		t.Errorf("Align 1 offsets = %v", offs)
	}
}

func TestScanAlignAnyTypeIgnoresGrid(t *testing.T) {
	anyType := toyType("anyt", "ANYOBJ!!", "test")
	anyType.Align = "any"
	img := carvetest.Image(16*kib, fillPattern)
	carvetest.Place(img, 4096+13, toyObj("ANYOBJ!!", 40))
	carvetest.Place(img, 8192+13, toyObj(toyHdr, 40))
	hits, _, err := runScan(t, bytes.NewReader(img), whole(img), toySet(toyType("toy", toyHdr, "test"), anyType), Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Family != "anyt" || hits[0].Offset != 4096+13 {
		t.Fatalf("hits = %+v, want only the any-aligned type at 4109", hits)
	}
}

func TestScanHeaderAcrossChunkBoundary(t *testing.T) {
	img := carvetest.Image(3*DefaultBufSize, fillPattern)
	at := DefaultBufSize - 3
	carvetest.Place(img, at, toyObj(toyHdr, 200))
	hits, _, err := runScan(t, bytes.NewReader(img), whole(img), toySet(), Options{Align: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != int64(at) || hits[0].Length != 200 {
		t.Fatalf("hits = %+v, want one at %d", hits, at)
	}
}

// fillReader is a synthetic reader of a claimed size: the fill pattern with an
// optional prefix, a read counter and a hook run on every read.
type fillReader struct {
	size    int64
	prefix  []byte
	hook    func(reads int)
	reads   int
	maxRead int
}

func (f *fillReader) ReadAt(p []byte, off int64) (int, error) {
	f.reads++
	f.maxRead = max(f.maxRead, len(p))
	if f.hook != nil {
		f.hook(f.reads)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := len(p)
	var err error
	if int64(n) > f.size-off {
		n = int(f.size - off)
		err = io.EOF
	}
	for i := range n {
		pos := off + int64(i)
		if pos < int64(len(f.prefix)) {
			p[i] = f.prefix[pos]
		} else {
			p[i] = fillPattern[pos%int64(len(fillPattern))]
		}
	}
	return n, err
}

func TestScanMemoryIndependentOfScope(t *testing.T) {
	for _, size := range []int64{1 << 20, 64 << 20} {
		fr := &fillReader{size: size}
		hits, st, err := runScan(t, fr, []Run{{0, size}}, toySet(), Options{Align: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 || st.Scanned != size {
			t.Errorf("size %d: %d hits, scanned %d", size, len(hits), st.Scanned)
		}
		if limit := DefaultBufSize + 8; fr.maxRead > limit || fr.maxRead == 0 {
			t.Errorf("size %d: largest read %d, want 1..%d", size, fr.maxRead, limit)
		}
	}
}

func TestScanResumeAfterValidated(t *testing.T) {
	third := toyObj(toyHdr, 24)
	inner := toyObj(toyHdr, 60)
	copy(inner[20:], third)
	outer := toyObj(toyHdr, 200)
	copy(outer[53:], inner)
	img := carvetest.Image(32*kib, fillPattern)
	carvetest.Place(img, 8192, outer)
	set := toySet()

	hits, st, err := runScan(t, bytes.NewReader(img), whole(img), set, Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != 8192 || hits[0].NestedIn != -1 || st.NestedDepthSkipped != 0 {
		t.Fatalf("default: %+v skipped %d, want the outer object only", hits, st.NestedDepthSkipped)
	}

	hits, st, err = runScan(t, bytes.NewReader(img), whole(img), set, Options{Align: 4096, Nested: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("nested: %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Offset != 8192 || hits[0].NestedIn != -1 {
		t.Errorf("outer = %+v", hits[0])
	}
	if hits[1].Offset != 8192+53 || hits[1].NestedIn != 8192 || hits[1].Length != 60 {
		t.Errorf("inner = %+v", hits[1])
	}
	if want := "nested-in=0000000000002000"; !slices.Contains(hits[1].Assumptions, want) {
		t.Errorf("inner assumptions %v lack %q", hits[1].Assumptions, want)
	}
	if slices.ContainsFunc(hits[0].Assumptions, func(a string) bool { return strings.HasPrefix(a, "nested-in") }) {
		t.Errorf("outer carries a nested-in assumption: %v", hits[0].Assumptions)
	}
	if st.NestedDepthSkipped != 1 {
		t.Errorf("NestedDepthSkipped = %d, want 1", st.NestedDepthSkipped)
	}
}

func TestScanRejectedResumesNextCandidate(t *testing.T) {
	img := carvetest.Image(16*kib, fillPattern)
	bad := toyObj(toyHdr, 100)
	copy(bad[96:], "XXXX")
	carvetest.Place(img, 0, bad)
	carvetest.Place(img, 4096, toyObj(toyHdr, 100))
	hits, st, err := runScan(t, bytes.NewReader(img), whole(img), toySet(), Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != 4096 {
		t.Fatalf("hits = %+v, want one at 4096", hits)
	}
	if st.Rejected["bad-end"] != 1 {
		t.Errorf("Rejected = %v, want bad-end 1", st.Rejected)
	}
}

func TestScanOutcomesAndConfidence(t *testing.T) {
	foot := toyType("foot", "FOOTHDR!", "")
	foot.Footer, foot.MinLen, foot.MaxLen = []byte("ZZEND"), 8, 4096
	capped := toyType("capt", "CAPHDR!!", "")
	capped.MinLen, capped.MaxLen = 8, 512
	minl := toyType("minl", "MINHDR!!", "test")
	minl.MinLen = 100
	set := toySet(toyType("toy", toyHdr, "test"), foot, capped, minl)

	img := carvetest.Image(64*kib, fillPattern)
	carvetest.Place(img, 0, toyObj(toyHdr, 100))
	carvetest.Place(img, 4096, []byte("FOOTHDR!"))
	carvetest.Place(img, 4096+95, []byte("ZZEND"))
	carvetest.Place(img, 8192, []byte("CAPHDR!!"))
	carvetest.Place(img, 12288, toyObj("MINHDR!!", 40))
	// A capped type at the start of a short run.
	carvetest.Place(img, 20480, []byte("CAPHDR!!"))
	runs := []Run{{0, 16384}, {20480, 300}}

	hits, st, err := runScan(t, bytes.NewReader(img), runs, set, Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	byFam := map[string]Hit{}
	for _, h := range hits {
		key := h.Family
		if h.Offset == 20480 {
			key = "capt-end"
		}
		if _, dup := byFam[key]; dup {
			t.Errorf("duplicate hit for %s", key)
		}
		byFam[key] = h
	}
	if h := byFam["toy"]; h.Outcome != Validated || h.Confidence != 70 || h.Method != "carve-validated" || h.Length != 100 {
		t.Errorf("toy hit = %+v", h)
	}
	if h := byFam["foot"]; h.Outcome != Footer || h.Confidence != 50 || h.Method != "carve-footer" || h.Length != 100 {
		t.Errorf("footer hit = %+v, want length 100 (to the footer's end)", h)
	}
	if h := byFam["capt"]; h.Outcome != Bounded || h.Confidence != 25 || h.Method != "carve-bounded" || h.Length != 512 ||
		!slices.Contains(h.Assumptions, "length-capped") {
		t.Errorf("capped hit = %+v", h)
	}
	if h := byFam["capt-end"]; h.Outcome != Bounded || h.Length != 300 || !slices.Contains(h.Assumptions, "cut-at-run-end") ||
		slices.Contains(h.Assumptions, "length-capped") {
		t.Errorf("run-end hit = %+v", h)
	}
	if _, ok := byFam["minl"]; ok || st.Rejected["too-short"] != 1 {
		t.Errorf("minl: hit %v, rejected %v, want none and too-short 1", ok, st.Rejected)
	}
	if st.Accepted[Validated] != 1 || st.Accepted[Footer] != 1 || st.Accepted[Bounded] != 2 {
		t.Errorf("Accepted = %v", st.Accepted)
	}
}

func TestConfidenceAndMethod(t *testing.T) {
	for _, c := range []struct {
		o      Outcome
		conf   int
		method string
	}{
		{Validated, 70, "carve-validated"}, {Footer, 50, "carve-footer"}, {Bounded, 25, "carve-bounded"}, {Rejected, 0, ""},
	} {
		if Confidence(c.o) != c.conf || MethodOf(c.o) != c.method {
			t.Errorf("%s: %d %q, want %d %q", c.o, Confidence(c.o), MethodOf(c.o), c.conf, c.method)
		}
	}
}

func TestScanFooterSearchStartsAfterHeader(t *testing.T) {
	// A footer equal to the header bytes does not end the object at its own start.
	ty := toyType("self", "SELF", "")
	ty.Footer, ty.MinLen, ty.MaxLen = []byte("SELF"), 4, 4096
	img := carvetest.Image(8*kib, fillPattern)
	carvetest.Place(img, 0, []byte("SELF"))
	carvetest.Place(img, 200, []byte("SELF"))
	hits, _, err := runScan(t, bytes.NewReader(img), whole(img), toySet(ty), Options{Align: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Length != 204 || hits[0].Outcome != Footer {
		t.Fatalf("hits = %+v, want one footer hit of length 204", hits)
	}
}

func TestScanHitCapPerType(t *testing.T) {
	img := carvetest.Image(64*kib, fillPattern)
	for i := range 5 {
		carvetest.Place(img, i*4096, toyObj(toyHdr, 64))
	}
	hits, st, err := runScan(t, bytes.NewReader(img), whole(img), toySet(), Options{Align: 4096, MaxHitsPerType: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || !slices.Equal(st.TypeCapped, []string{"toy"}) {
		t.Fatalf("%d hits, TypeCapped %v, want 3 and [toy]", len(hits), st.TypeCapped)
	}
	// Exactly at the cap with nothing lost: not capped.
	hits, st, err = runScan(t, bytes.NewReader(img), whole(img), toySet(), Options{Align: 4096, MaxHitsPerType: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 || len(st.TypeCapped) != 0 {
		t.Errorf("%d hits, TypeCapped %v, want 5 and none", len(hits), st.TypeCapped)
	}
}

func TestScanTypeFilter(t *testing.T) {
	set := toySet(toyType("toy2", toyHdr, "test-refine"))
	img := carvetest.Image(8*kib, fillPattern)
	carvetest.Place(img, 0, toyObj(toyHdr, 64))
	for _, c := range []struct {
		filter   map[string][]string
		filtered bool
	}{
		{nil, false},
		{map[string][]string{"toy2": {"refined"}}, false},
		{map[string][]string{"toy2": {"other"}}, true},
		{map[string][]string{"elsewhere": {"x"}}, false},
	} {
		hits, _, err := runScan(t, bytes.NewReader(img), whole(img), set, Options{Align: 4096, TypeFilter: c.filter})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Filtered != c.filtered || hits[0].Type != "refined" || hits[0].Ext != "rfn" || hits[0].Family != "toy2" {
			t.Errorf("filter %v: %+v, want Filtered=%v, type refined", c.filter, hits, c.filtered)
		}
	}
}

func TestScanValidateBudgetStopsLoudly(t *testing.T) {
	// 2,000 greedy headers, one on every 4 KiB block of an 8 MiB scope.
	img := carvetest.Image(8<<20, fillPattern)
	for i := range 2000 {
		carvetest.Place(img, i*4096, []byte("GREEDYHD"))
	}
	greedy := toyType("greedy", "GREEDYHD", "test-greedy")
	greedy.MaxLen = 8 << 20
	set := toySet(greedy)
	cr := &countingReader{r: bytes.NewReader(img)}
	hits, st, err := runScan(t, cr, whole(img), set, Options{Align: 4096, ValidateBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || !st.BudgetExhausted {
		t.Errorf("%d hits, BudgetExhausted %v", len(hits), st.BudgetExhausted)
	}
	if st.Rejected["validate-budget"] != 2000 {
		t.Errorf("Rejected = %v, want validate-budget 2000", st.Rejected)
	}
	if st.ValidateBytes > 1<<20+64*kib || st.ValidateBytes == 0 {
		t.Errorf("ValidateBytes = %d, want 1..%d", st.ValidateBytes, 1<<20+64*kib)
	}
	if limit := 3 * (len(img) / DefaultBufSize); cr.reads >= limit {
		t.Errorf("%d reads, want fewer than %d", cr.reads, limit)
	}
}

func TestScanCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := &fillReader{size: 16 << 20, hook: func(n int) {
		if n == 1 {
			cancel()
		}
	}}
	_, err := scan(ctx, fr, []Run{{0, fr.size}}, toySet(), Options{Align: 4096}, testValidators(), func(Hit) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fr.reads > 2 {
		t.Errorf("%d reads after a cancel in the first, want at most 2", fr.reads)
	}

	// A validator walking a 64 MiB window stops at the next 64 KiB refill.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ty := toyType("walker", "WALKHDR!", "test-walk")
	ty.MaxLen = 64 << 20
	fr = &fillReader{size: 64 << 20, prefix: []byte("WALKHDR!"), hook: func(n int) {
		if n == 3 {
			cancel2()
		}
	}}
	_, err = scan(ctx2, fr, []Run{{0, fr.size}}, toySet(ty), Options{Align: 4096, ValidateBudget: 1 << 30}, testValidators(), func(Hit) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("walk: err = %v, want context.Canceled", err)
	}
	if fr.reads != 3 {
		t.Errorf("walk: %d reads, want exactly 3 (scan chunk, two refills; the refill after the cancel reads nothing)", fr.reads)
	}
}

func TestValidateBudgetCeiling(t *testing.T) {
	const tib = int64(1) << 40
	if got := resolveBudget(0, 16*tib); got != MaxValidateBudget {
		t.Errorf("resolveBudget(0, 16 TiB) = %d, want %d", got, MaxValidateBudget)
	}
	if got := resolveBudget(0, math.MaxInt64); got != MaxValidateBudget {
		t.Errorf("resolveBudget(0, MaxInt64) = %d, want %d", got, MaxValidateBudget)
	}
	if got, want := resolveBudget(0, 1<<20), 8*(int64(1)<<20)+64<<20; got != want {
		t.Errorf("resolveBudget(0, 1 MiB) = %d, want %d", got, want)
	}
	if got := resolveBudget(12345, 16*tib); got != 12345 {
		t.Errorf("explicit budget = %d, want 12345", got)
	}

	// End to end: the budget a scan uses is clamped, for a claimed scope of
	// 16 TiB (the context is cancelled on the first read).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr := &fillReader{size: 16 * tib, hook: func(int) { cancel() }}
	st, err := scan(ctx, fr, []Run{{0, fr.size}}, toySet(), Options{Align: 4096}, testValidators(), func(Hit) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st.budgetLimit != MaxValidateBudget {
		t.Errorf("scan budget = %d, want %d", st.budgetLimit, MaxValidateBudget)
	}
	st, _ = scan(context.Background(), fr, []Run{{0, 1}}, toySet(), Options{ValidateBudget: math.MaxInt64}, testValidators(), func(Hit) error { return nil })
	if st.budgetLimit != MaxValidateBudget {
		t.Errorf("explicit huge budget = %d, want %d", st.budgetLimit, MaxValidateBudget)
	}
}

func TestScanPanicIsError(t *testing.T) {
	set := toySet(toyType("toy", toyHdr, "test"), toyType("boom", "BOOMHDR!", "test-panic"))
	img := carvetest.Image(16*kib, fillPattern)
	carvetest.Place(img, 0, toyObj(toyHdr, 64))
	carvetest.Place(img, 8192, []byte("BOOMHDR!"))
	hits, _, err := runScan(t, bytes.NewReader(img), whole(img), set, Options{Align: 4096})
	var pe *PanicError
	if !errors.As(err, &pe) || pe.Value != "validator bug" {
		t.Fatalf("err = %v, want *PanicError carrying the value", err)
	}
	if len(hits) != 1 || hits[0].Offset != 0 {
		t.Errorf("hits before the panic = %+v, want the one at 0", hits)
	}
	if pe.Error() == "" {
		t.Error("PanicError has no message")
	}
}

// flakyReader fails any read that overlaps [lo, hi) and returns the readable
// bytes before lo with the error.
type flakyReader struct {
	data   []byte
	lo, hi int64
}

var errFlaky = errors.New("flaky: bad sectors")

func (f *flakyReader) ReadAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > f.lo && off < f.hi {
		n := int(max(0, f.lo-off))
		copy(p[:n], f.data[off:])
		return n, errFlaky
	}
	return bytes.NewReader(f.data).ReadAt(p, off)
}

func TestScanReadErrorSplitsRun(t *testing.T) {
	img := carvetest.Image(3<<20, fillPattern)
	const bad = 1 << 20
	carvetest.Place(img, 0, toyObj(toyHdr, 100))
	carvetest.Place(img, 2<<20, toyObj(toyHdr, 100))
	// Straddles the bad range: the header is readable, the trailer is not.
	carvetest.Place(img, bad-4096, toyObj(toyHdr, 8192))
	fr := &flakyReader{data: img, lo: bad, hi: bad + 4096}

	var reported int64
	hits, st, err := runScan(t, fr, whole(img), toySet(), Options{Align: 4096, OnReadError: func(off, n int64, err error) error {
		if !errors.Is(err, errFlaky) {
			t.Errorf("OnReadError err = %v", err)
		}
		if off < bad || off+n > bad+4096 {
			t.Errorf("reported range %d+%d outside the failing bytes", off, n)
		}
		reported += n
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var offs []int64
	for _, h := range hits {
		offs = append(offs, h.Offset)
	}
	if !slices.Equal(offs, []int64{0, 2 << 20}) {
		t.Errorf("hits at %v, want 0 and 2 MiB (the straddling object is not found)", offs)
	}
	if st.UnreadableBytes != 4096 || reported != 4096 {
		t.Errorf("UnreadableBytes %d, reported %d, want 4096", st.UnreadableBytes, reported)
	}
	if st.Scanned+st.UnreadableBytes != int64(len(img)) {
		t.Errorf("Scanned %d + Unreadable %d != %d", st.Scanned, st.UnreadableBytes, len(img))
	}

	// Without OnReadError the error ends the scan.
	_, _, err = runScan(t, fr, whole(img), toySet(), Options{Align: 4096})
	if !errors.Is(err, errFlaky) {
		t.Errorf("nil OnReadError: err = %v, want the read error", err)
	}

	// OnReadError may itself end the scan.
	stop := errors.New("stop")
	_, _, err = runScan(t, fr, whole(img), toySet(), Options{Align: 4096, OnReadError: func(int64, int64, error) error { return stop }})
	if !errors.Is(err, stop) {
		t.Errorf("OnReadError error: err = %v, want stop", err)
	}
}

func TestScanShortReaderIsUnreadable(t *testing.T) {
	// A run that claims more than the reader holds: the missing bytes are
	// unreadable, not scanned and not silently dropped.
	img := carvetest.Image(8*kib, fillPattern)
	carvetest.Place(img, 0, toyObj(toyHdr, 64))
	var n int64
	_, st, err := runScan(t, bytes.NewReader(img), []Run{{0, 16 * kib}}, toySet(), Options{Align: 4096, OnReadError: func(_, c int64, _ error) error {
		n += c
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if st.UnreadableBytes != 8*kib || n != 8*kib {
		t.Errorf("UnreadableBytes %d reported %d, want 8192", st.UnreadableBytes, n)
	}
}

func TestScanEmitErrorStops(t *testing.T) {
	img := carvetest.Image(16*kib, fillPattern)
	for i := range 3 {
		carvetest.Place(img, i*4096, toyObj(toyHdr, 64))
	}
	stop := errors.New("emit refused")
	calls := 0
	_, err := scan(context.Background(), bytes.NewReader(img), whole(img), toySet(), Options{Align: 4096}, testValidators(), func(Hit) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("err = %v after %d emits, want the emit error after 1", err, calls)
	}
}

func TestScanProgress(t *testing.T) {
	img := carvetest.Image(3*DefaultBufSize, fillPattern)
	var seen []int64
	_, st, err := runScan(t, bytes.NewReader(img), whole(img), toySet(), Options{Align: 4096, Progress: func(n int64) { seen = append(seen, n) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 3 || !slices.IsSorted(seen) || seen[len(seen)-1] > st.Scanned || st.Scanned != int64(len(img)) {
		t.Errorf("progress %v, scanned %d", seen, st.Scanned)
	}
}

func TestScanRefusesBadInput(t *testing.T) {
	img := carvetest.Image(4*kib, fillPattern)
	rd := bytes.NewReader(img)
	emit := func(Hit) error { return nil }
	if _, err := Scan(context.Background(), rd, whole(img), &Set{ID: "x"}, Options{}, emit); !errors.Is(err, ErrBadSignature) {
		t.Errorf("empty set: %v, want ErrBadSignature", err)
	}
	unknown := toySet(toyType("toy", toyHdr, "test")) // "test" is no built-in validator
	if _, err := Scan(context.Background(), rd, whole(img), unknown, Options{}, emit); !errors.Is(err, ErrBadSignature) {
		t.Errorf("unknown validator: %v, want ErrBadSignature", err)
	}
	for _, runs := range [][]Run{{{-1, 10}}, {{0, -1}}, {{math.MaxInt64, 2}}} {
		if _, err := Scan(context.Background(), rd, runs, Builtin(), Options{}, emit); !errors.Is(err, ErrBadRun) {
			t.Errorf("runs %v: %v, want ErrBadRun", runs, err)
		}
	}
	// A zero-length run is legal and scans nothing.
	if st, err := Scan(context.Background(), rd, []Run{{0, 0}}, Builtin(), Options{}, emit); err != nil || st.Scanned != 0 {
		t.Errorf("empty run: %+v, %v", st, err)
	}
}

func TestScanStatsMapsNeverNil(t *testing.T) {
	_, st, err := runScan(t, bytes.NewReader(nil), nil, toySet(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Accepted == nil || st.Rejected == nil {
		t.Errorf("nil stats maps: %+v", st)
	}
}

// badReader fails any read that overlaps a range for which bad(lo, hi) is
// true, returning no bytes (a reader that cannot say where it failed).
type badReader struct {
	data []byte
	bad  func(lo, hi int64) bool
}

func (b *badReader) ReadAt(p []byte, off int64) (int, error) {
	if b.bad(off, off+int64(len(p))) {
		return 0, errFlaky
	}
	return bytes.NewReader(b.data).ReadAt(p, off)
}

func TestScanSwallowedReadErrorRejectsUnreadable(t *testing.T) {
	// The header is in the loaded chunk; the bad range lies past it, so the
	// window is not clipped. The validator swallows the read error and says
	// validated: the scan still refuses the hit.
	img := carvetest.Image(2<<20, fillPattern)
	at := DefaultBufSize - 64
	carvetest.Place(img, at, toyObj("SWALLOWH", 1024))
	br := &badReader{data: img, bad: func(lo, hi int64) bool { return hi > DefaultBufSize+512 && lo < DefaultBufSize+1024 }}
	set := toySet(toyType("swal", "SWALLOWH", "test-swallow"))
	hits, st, err := runScan(t, br, whole(img), set, Options{Align: 1, OnReadError: func(int64, int64, error) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || st.Rejected["unreadable"] != 1 {
		t.Errorf("hits %+v rejected %v, want none and unreadable 1", hits, st.Rejected)
	}
	if st.UnreadableBytes != 512 {
		t.Errorf("UnreadableBytes = %d, want 512 (the later chunk load finds the range)", st.UnreadableBytes)
	}
}

func TestScanReadErrorBeatsTruncated(t *testing.T) {
	// A validator that hits an unreadable byte and then reads outside the
	// window: the reason is unreadable, not truncated.
	img := carvetest.Image(2<<20, fillPattern)
	carvetest.Place(img, DefaultBufSize-64, toyObj(toyHdr, 1024))
	br := &badReader{data: img, bad: func(lo, hi int64) bool { return hi > DefaultBufSize+900 && lo < DefaultBufSize+1024 }}
	hits, st, err := runScan(t, br, whole(img), toySet(toyType("both", toyHdr, "test-both")), Options{Align: 1, OnReadError: func(int64, int64, error) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || st.Rejected["unreadable"] != 1 || st.Rejected["truncated"] != 0 {
		t.Errorf("hits %+v rejected %v, want unreadable 1 and no truncated", hits, st.Rejected)
	}
}

func TestScanBlindValidatorLimitation(t *testing.T) {
	// 3E-C1 (c): a validator that never reads the body accepts an object
	// that spans an unreadable range lying past the loaded chunk. The part of
	// the range past the hit is found and counted when the scan resumes;
	// the writer (Task 11) sees the rest as a read failure.
	img := carvetest.Image(2<<20, fillPattern)
	carvetest.Place(img, DefaultBufSize-64, toyObj("BLINDHDR", 1024))
	lo := int64(DefaultBufSize + 512)
	br := &badReader{data: img, bad: func(a, b int64) bool { return b > lo && a < lo+512 }}
	hits, st, err := runScan(t, br, whole(img), toySet(toyType("blind", "BLINDHDR", "test-blind")), Options{Align: 1, OnReadError: func(int64, int64, error) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Length != 1024 {
		t.Fatalf("hits = %+v, want the one blind hit", hits)
	}
	// The hit ends at 1 MiB-64+1024 = 1 MiB+960, inside the bad range
	// [1 MiB+512, 1 MiB+1024): the scan resumes there and finds 64 bytes.
	if st.UnreadableBytes != 64 {
		t.Errorf("UnreadableBytes = %d, want 64", st.UnreadableBytes)
	}
}

func TestScanAlternatingBadSectorsBoundsMemory(t *testing.T) {
	const size = 32 << 20
	// Every other 512-byte sector is unreadable: 32,768 separate ranges.
	fr := &altReader{size: size}
	var n int64
	_, st, err := runScan(t, fr, []Run{{0, size}}, toySet(), Options{Align: 4096, OnReadError: func(_, c int64, _ error) error {
		n += c
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(size / 2); st.UnreadableBytes != want || n != want {
		t.Errorf("UnreadableBytes %d reported %d, want %d", st.UnreadableBytes, n, want)
	}
	if st.maxReported == 0 || st.maxReported > 2200 {
		t.Errorf("peak counted-range list = %d, want 1..2200 (about two chunks of ranges, not %d)", st.maxReported, size/1024)
	}
}

// altReader fails every read that overlaps an odd 512-byte sector.
type altReader struct{ size int64 }

func (a *altReader) ReadAt(p []byte, off int64) (int, error) {
	for s := off &^ 511; s < off+int64(len(p)); s += 512 {
		if (s/512)%2 == 1 {
			return 0, errFlaky
		}
	}
	if off >= a.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), a.size-off)
	for i := range n {
		p[i] = fillPattern[(off+i)%int64(len(fillPattern))]
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func TestScanExhaustedBudgetRejectsFooterlessType(t *testing.T) {
	greedy := toyType("greedy", "GREEDYHD", "test-greedy")
	greedy.MaxLen = 8 << 20
	capt := toyType("capt", "CAPHDR!!", "")
	capt.MaxLen = 512
	img := carvetest.Image(2<<20, fillPattern)
	carvetest.Place(img, 0, []byte("GREEDYHD"))
	carvetest.Place(img, 4096, []byte("CAPHDR!!"))
	hits, st, err := runScan(t, bytes.NewReader(img), whole(img), toySet(greedy, capt), Options{Align: 4096, ValidateBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || st.Rejected["validate-budget"] != 2 {
		t.Errorf("hits %+v rejected %v, want none and validate-budget 2 (the footer-less type too)", hits, st.Rejected)
	}
}

func TestScanHugeAlignDoesNotOverflow(t *testing.T) {
	img := carvetest.Image(1<<20, fillPattern)
	carvetest.Place(img, 1000, toyObj(toyHdr, 64))
	carvetest.Place(img, 1000+4096, toyObj(toyHdr, 64))
	run := []Run{{1000, int64(len(img)) - 1000}}
	hits, st, err := runScan(t, bytes.NewReader(img), run, toySet(), Options{Align: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != 1000 || st.Scanned != run[0].Length {
		t.Errorf("hits %+v scanned %d, want only offset 0 and all scanned", hits, st.Scanned)
	}
}
