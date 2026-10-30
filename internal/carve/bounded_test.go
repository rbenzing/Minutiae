package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

// hostileObj is one input of the hostile-input tests: bytes that the validator
// named by id is meant to judge.
type hostileObj struct {
	name, id string
	data     []byte
}

func hostileOggPages() []carvetest.OggPage {
	return []carvetest.OggPage{
		{Flags: 2, Serial: 1, Seq: 0, Segments: seg(30)},
		{Flags: 2, Serial: 2, Seq: 0, Segments: seg(20)},
		{Serial: 1, Seq: 1, Segments: [][]byte{bytes.Repeat([]byte{0xA5}, 255), seg(40)[0]}},
		{Flags: 4, Serial: 1, Seq: 2, Segments: seg(10)},
		{Flags: 4, Serial: 2, Seq: 1, Segments: seg(10)},
	}
}

// builderCorpus is every builder variant of the twelve validators, valid and
// hostile (a bad variant is a seed for the corrupt path). Map iteration is
// avoided: the order of the corpus is fixed.
func builderCorpus() []hostileObj {
	var c []hostileObj
	add := func(id, name string, data []byte) { c = append(c, hostileObj{id + "/" + name, id, data}) }

	jpegs := []struct {
		n string
		o carvetest.JPEGOpts
	}{
		{"baseline", carvetest.JPEGOpts{}},
		{"progressive", carvetest.JPEGOpts{Progressive: true}},
		{"restart", carvetest.JPEGOpts{Restart: true}},
		{"fill", carvetest.JPEGOpts{FillBytes: true}},
		{"thumbnail", carvetest.JPEGOpts{Thumbnail: bytes.Repeat([]byte{0xFF, 0xD8, 0xFF, 0xD9}, 30)}},
		{"no-eoi", carvetest.JPEGOpts{OmitEOI: true}},
		{"zero-width", carvetest.JPEGOpts{ZeroWidth: true}},
		{"long-entropy", carvetest.JPEGOpts{EntropyLen: 3000}},
	}
	for _, j := range jpegs {
		add("jpeg", j.n, carvetest.JPEG(j.o))
	}
	pngs := []struct {
		n string
		o carvetest.PNGOpts
	}{
		{"rgb", carvetest.PNGOpts{}},
		{"palette", carvetest.PNGOpts{ColorType: 3, BitDepth: 8}},
		{"multi-idat", carvetest.PNGOpts{IDATs: 3}},
		{"ancillary", carvetest.PNGOpts{Ancillary: []string{"gAMA", "tEXt"}}},
		{"apng", carvetest.PNGOpts{APNG: true}},
		{"bad-crc", carvetest.PNGOpts{BadCRC: true}},
		{"no-idat", carvetest.PNGOpts{NoIDAT: true}},
		{"ihdr-late", carvetest.PNGOpts{IHDRNotFirst: true}},
		{"zero-width", carvetest.PNGOpts{ZeroWidth: true}},
		{"no-iend", carvetest.PNGOpts{OmitIEND: true}},
		{"iend-payload", carvetest.PNGOpts{IENDPayload: true}},
	}
	for _, p := range pngs {
		add("png", p.n, carvetest.PNG(p.o))
	}
	gifs := []struct {
		n string
		o carvetest.GIFOpts
	}{
		{"89a", carvetest.GIFOpts{}},
		{"87a", carvetest.GIFOpts{Version: "GIF87a"}},
		{"tables", carvetest.GIFOpts{GCT: true, LCT: true}},
		{"frames", carvetest.GIFOpts{Frames: 3}},
		{"no-frames", carvetest.GIFOpts{NoFrames: true}},
		{"no-trailer", carvetest.GIFOpts{NoTrailer: true}},
		{"zero-dims", carvetest.GIFOpts{ZeroDims: true}},
		{"big-frame", carvetest.GIFOpts{DataLen: 2000}},
	}
	for _, g := range gifs {
		add("gif", g.n, carvetest.GIF(g.o))
	}
	webps := []struct {
		n string
		o carvetest.WebPOpts
	}{
		{"lossy", carvetest.WebPOpts{}},
		{"lossless", carvetest.WebPOpts{Kind: "VP8L"}},
		{"extended", carvetest.WebPOpts{Kind: "VP8X", Extra: []string{"ICCP", "ALPH", "EXIF", "XMP "}}},
		{"odd", carvetest.WebPOpts{OddChunk: true}},
		{"bad-start", carvetest.WebPOpts{BadStartCode: true}},
		{"zero-dims", carvetest.WebPOpts{ZeroDims: true}},
		{"size+2", carvetest.WebPOpts{SizeDelta: 2}},
	}
	for _, w := range webps {
		add("riff-webp", w.n, carvetest.WebP(w.o))
	}
	bmffs := []struct {
		n string
		o carvetest.BMFFOpts
	}{
		{"mp4", carvetest.BMFFOpts{}},
		{"mdat-first", carvetest.BMFFOpts{MdatFirst: true}},
		{"large-mdat", carvetest.BMFFOpts{LargeMdat: true}},
		{"size-zero", carvetest.BMFFOpts{SizeZeroLast: true}},
		{"heic", carvetest.BMFFOpts{HEIF: true}},
		{"heic-size-zero", carvetest.BMFFOpts{HEIF: true, SizeZeroLast: true}},
		{"no-moov", carvetest.BMFFOpts{NoMoov: true}},
		{"no-meta", carvetest.BMFFOpts{HEIF: true, NoMeta: true}},
		{"bad-children", carvetest.BMFFOpts{BadChildren: true}},
		{"qt", carvetest.BMFFOpts{Brand: "qt  "}},
		{"trailer", carvetest.BMFFOpts{Trailer: carvetest.Box("free", make([]byte, 30))}},
	}
	for _, b := range bmffs {
		add("isobmff", b.n, carvetest.MP4(b.o))
	}
	sqls := []struct {
		n string
		o carvetest.SQLiteOpts
	}{
		{"512x4", carvetest.SQLiteOpts{PageSize: 512, Pages: 4, ValidFor: true}},
		{"4096x2", carvetest.SQLiteOpts{PageSize: 4096, Pages: 2, ValidFor: true}},
		{"65536x1", carvetest.SQLiteOpts{PageSize: 65536, Pages: 1, ValidFor: true}},
		{"bad-fractions", carvetest.SQLiteOpts{PageSize: 512, Pages: 4, BadFractions: true}},
		{"leaf-page", carvetest.SQLiteOpts{PageSize: 1024, Pages: 3, ValidFor: true, Page1Type: 0x0D}},
	}
	for _, s := range sqls {
		add("sqlite", s.n, carvetest.SQLiteHeader(s.o))
	}
	bps := []struct {
		n      string
		objs   [][]byte
		top    int
		os, rs byte
	}{
		{"1-byte", bpObjects(), 0, 1, 1},
		{"2-byte", bpObjects(), 0, 2, 1},
		{"8-byte", bpObjects(), 0, 8, 2},
		{"scalar", [][]byte{carvetest.BPlistInt1(7)}, 0, 1, 1},
		{"bad-top", bpObjects(), 9, 1, 1},
	}
	for _, b := range bps {
		add("bplist", b.n, carvetest.BPlist(b.objs, b.top, b.os, b.rs))
	}
	zipEntries := []carvetest.ZIPEntry{{Name: "a.txt", Data: bytes.Repeat([]byte("a"), 50)}, {Name: "b/c", Data: bytes.Repeat([]byte("c"), 70)}}
	inner := carvetest.ZIP(zipEntries, "")
	add("zip", "two-entries", inner)
	add("zip", "comment", carvetest.ZIP(zipEntries, "a comment"))
	add("zip", "empty-data", carvetest.ZIP([]carvetest.ZIPEntry{{Name: "e"}}, ""))
	add("zip", "nested", carvetest.ZIP([]carvetest.ZIPEntry{{Name: "inner.zip", Data: inner}}, ""))
	add("zip", "no-eocd", inner[:len(inner)-22])
	pdfs := []struct {
		n string
		o carvetest.PDFOpts
	}{
		{"single", carvetest.PDFOpts{}},
		{"incremental", carvetest.PDFOpts{Increments: 2}},
		{"bad-startxref", carvetest.PDFOpts{BadStartxref: true}},
		{"no-eof", carvetest.PDFOpts{NoEOF: true}},
		{"v1.4", carvetest.PDFOpts{Version: "1.4"}},
	}
	for _, p := range pdfs {
		add("pdf", p.n, carvetest.PDF(p.o))
	}
	for flags := 0; flags < 32; flags += 3 {
		add("gzip", "flags-"+strconv.Itoa(flags), carvetest.GzipHeader(byte(flags), "name.bin", "a comment"))
	}
	add("gzip", "reserved-bit", carvetest.GzipHeader(0x20, "", ""))
	wavs := []struct {
		n string
		o carvetest.WAVOpts
	}{
		{"pcm16", carvetest.WAVOpts{}},
		{"pcm8", carvetest.WAVOpts{Bits: 8, Channels: 2, Rate: 44100}},
		{"list", carvetest.WAVOpts{ListBefore: true}},
		{"odd-chunk", carvetest.WAVOpts{OddChunk: true}},
		{"bad-fmt", carvetest.WAVOpts{BadFmt: true}},
		{"no-data", carvetest.WAVOpts{NoData: true}},
		{"size+2", carvetest.WAVOpts{SizeDelta: 2}},
		{"size-2", carvetest.WAVOpts{SizeDelta: -2}},
	}
	for _, w := range wavs {
		add("riff-wave", w.n, carvetest.WAV(w.o))
	}
	oggs := []struct {
		n     string
		pages []carvetest.OggPage
	}{
		{"one-stream", oneStream()},
		{"multiplexed", hostileOggPages()},
		{"bad-crc", []carvetest.OggPage{{Flags: 2, Serial: 1, Segments: seg(30), BadCRC: true}, {Flags: 4, Serial: 1, Seq: 1, Segments: seg(5)}}},
		{"no-bos", []carvetest.OggPage{{Serial: 1, Segments: seg(30)}, {Flags: 4, Serial: 1, Seq: 1, Segments: seg(5)}}},
		{"no-eos", []carvetest.OggPage{{Flags: 2, Serial: 1, Segments: seg(30)}, {Serial: 1, Seq: 1, Segments: seg(5)}}},
	}
	for _, o := range oggs {
		add("ogg", o.n, carvetest.Ogg(o.pages))
	}
	c = append(c, tiffCorpus()...)
	return c
}

// realCorpus is the real encoder objects of testdata/objects.
func realCorpus(t testing.TB) []hostileObj {
	t.Helper()
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct{ File, Family string }
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, ty := range Builtin().Types {
		ids[ty.Name] = ty.Validate
	}
	var c []hostileObj
	for _, o := range exp.Objects {
		id, ok := ids[o.Family]
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, o.File))
		if err != nil {
			t.Fatal(err)
		}
		c = append(c, hostileObj{"real/" + o.File, id, data})
	}
	return c
}

// guardReader fails the test on any read outside [lo, hi) of its image: the
// window of the hit. Everything around the hit is poisoned.
type guardReader struct {
	t      *testing.T
	img    []byte
	lo, hi int64
	name   string
}

func (g *guardReader) ReadAt(p []byte, off int64) (int, error) {
	if off < g.lo || off+int64(len(p)) > g.hi {
		g.t.Errorf("%s: read of %d bytes at %d outside the hit [%d,%d)", g.name, len(p), off, g.lo, g.hi)
		return 0, io.ErrUnexpectedEOF
	}
	return copy(p, g.img[off:]), nil
}

// hostileVariants is data truncated at every 1/16 of its length and with a
// byte flipped at 32 positions (the first 16 bytes and 16 spread over the rest).
func hostileVariants(data []byte) map[string][]byte {
	v := map[string][]byte{"whole": data}
	for k := 1; k <= 16; k++ {
		v["trunc-"+strconv.Itoa(k)] = data[:len(data)*k/16]
	}
	for i := range 32 {
		pos := i
		if i >= 16 {
			pos = 16 + (i-16)*(len(data)-16)/16
		}
		if pos >= len(data) {
			continue
		}
		f := slices.Clone(data)
		f[pos] ^= 0xFF
		v["flip-"+strconv.Itoa(pos)] = f
	}
	return v
}

// minLenOf is the smallest MinLen of the built-in types that name validator id.
func minLenOf(id string) int64 {
	m := int64(-1)
	for _, ty := range Builtin().Types {
		if ty.Validate == id && (m < 0 || ty.MinLen < m) {
			m = ty.MinLen
		}
	}
	return m
}

// TestCarveValidatorsBounded: every built-in validator, on every builder
// variant and real object whole, truncated at each 1/16 and with a byte
// flipped at 32 positions, stays inside its window (3E-R29): the reader is
// poisoned outside [hit, hit+window), the window counts no refused read
// (OutsideReads == 0), nothing panics, Length <= the window and a validated
// length is at least the type's MinLen.
func TestCarveValidatorsBounded(t *testing.T) {
	validators := builtinValidators()
	corpus := slices.Concat(builderCorpus(), realCorpus(t))
	seen := map[string]bool{}
	const pad = 777
	for _, obj := range corpus {
		seen[obj.id] = true
		fn, ok := validators[obj.id]
		if !ok {
			t.Fatalf("%s: no validator %q", obj.name, obj.id)
		}
		minLen := minLenOf(obj.id)
		for vn, data := range hostileVariants(obj.data) {
			if int64(len(data)) < minLen {
				continue // the scanner never opens a window below MinLen
			}
			img := slices.Concat(bytes.Repeat([]byte{0xEE}, pad), data, bytes.Repeat([]byte{0xDD}, pad))
			name := obj.name + "/" + vn
			g := &guardReader{t: t, img: img, lo: pad, hi: pad + int64(len(data)), name: name}
			w := newWindow(context.Background(), g, pad, int64(len(data)), newBudget(1<<30))
			v := fn(w)
			if w.outsideReads() != 0 {
				t.Errorf("%s: %d reads past the window", name, w.outsideReads())
			}
			if v.Outcome == Rejected {
				continue
			}
			if v.Length < 1 || v.Length > w.Len() {
				t.Errorf("%s: %s length %d outside 1..%d", name, v.Outcome, v.Length, w.Len())
			}
			if v.Outcome == Validated && v.Length < minLen {
				t.Errorf("%s: validated length %d below MinLen %d", name, v.Length, minLen)
			}
		}
	}
	for id := range validators {
		if !implementedValidators[id] {
			continue // a stub until its format task lands (3E2 tasks 3-6); Task 7 requires the corpus of all fifteen
		}
		if !seen[id] {
			t.Errorf("validator %q has no corpus object", id)
		}
	}
}

// TestCarveGzipBodyIsNotInflated: a gzip member is carved from its header and
// the window alone (3E-R2): its body is never decoded, whatever it would
// inflate to, so a hit costs the header and one cache fill of validator reads.
func TestCarveGzipBodyIsNotInflated(t *testing.T) {
	body := bytes.Repeat([]byte{0x00, 0xFF, 0xFF, 0x00, 0x00}, 12000)
	obj := slices.Concat(carvetest.GzipHeader(0x08, "bomb.bin", ""), body)
	img := slices.Concat(make([]byte, 512), obj, make([]byte, 512))
	var hits []Hit
	st, err := Scan(context.Background(), bytes.NewReader(img), []Run{{0, int64(len(img))}}, Builtin(), Options{Align: 1}, func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Family != "gzip" || hits[0].Outcome != Bounded || hits[0].Offset != 512 {
		t.Fatalf("hits = %+v, want one bounded gzip hit at 512", hits)
	}
	if st.ValidateBytes > 128+64<<10 {
		t.Errorf("validate bytes = %d, want the header and one cache fill at most", st.ValidateBytes)
	}
}

// TestTruncatedObjectsAskForNothingOutside: a validator learns that an object
// is cut by testing its position against the window, not by asking the window
// for bytes past its end: for a builder object cut at every length the window
// refuses nothing (a refusal is counted in Stats.OutsideReads, which is zero
// for a clean scan). Found by TestCarveValidatorsBounded: the JPEG, GIF and
// PNG validators tried the read and relied on the refusal.
func TestTruncatedObjectsAskForNothingOutside(t *testing.T) {
	validators := builtinValidators()
	for _, obj := range builderCorpus() {
		if obj.id != "jpeg" && obj.id != "gif" && obj.id != "png" {
			continue
		}
		for n := 1; n <= len(obj.data); n++ {
			w := newWindow(context.Background(), bytes.NewReader(obj.data[:n]), 0, int64(n), newBudget(1<<30))
			v := validators[obj.id](w)
			if w.outsideReads() != 0 {
				t.Fatalf("%s cut at %d of %d: %d reads past the window (verdict %+v)", obj.name, n, len(obj.data), w.outsideReads(), v)
			}
		}
	}
}

// TestJPEGWindowEndsAfterEntropyFF: the window ends right after an FF byte in
// the entropy data, so the byte that would say what the FF is lies past it. The
// validator must find the object cut by its own bound, asking for nothing.
func TestJPEGWindowEndsAfterEntropyFF(t *testing.T) {
	obj := carvetest.JPEG(carvetest.JPEGOpts{})
	sos := bytes.Index(obj, []byte{0xFF, 0xDA})
	if sos < 0 {
		t.Fatal("no SOS in the builder object")
	}
	start := sos + 2 + int(obj[sos+2])<<8 + int(obj[sos+3])
	cut := append(slices.Clone(obj[:start]), 0x41, 0xFF)
	w := newWindow(context.Background(), bytes.NewReader(cut), 0, int64(len(cut)), newBudget(1<<30))
	v := validateJPEG(w)
	if v.Outcome != Rejected || v.Reason != "truncated" || w.outsideReads() != 0 {
		t.Fatalf("verdict = %+v, outside reads = %d, want rejected truncated and none", v, w.outsideReads())
	}
}

// TestReadCheckedEmptyIsNotAnOutsideRead pins the zero-length window of a
// first-hand reader: asking for no bytes asks for nothing, so it is not
// counted as a read outside the window.
func TestReadCheckedEmptyIsNotAnOutsideRead(t *testing.T) {
	w := newWindow(context.Background(), bytes.NewReader(nil), 0, 0, newBudget(1<<20))
	if err := w.readChecked(nil, 0); err != nil {
		t.Fatalf("readChecked(empty): %v", err)
	}
	if n := w.outsideReads(); n != 0 {
		t.Fatalf("outside reads = %d, want 0", n)
	}
	if err := w.readChecked(make([]byte, 1), 0); !errors.Is(err, ErrOutsideWindow) {
		t.Fatalf("readChecked(1 byte, empty window) = %v, want ErrOutsideWindow", err)
	}
}
