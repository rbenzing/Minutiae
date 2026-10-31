package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

var tiffDescription = []byte("a long description\x00") // 19 bytes: an out-of-line ASCII value

// tiffPage is one page with a 64-byte strip and an out-of-line description.
func tiffPage(be bool) carvetest.TIFFIFD {
	return carvetest.TIFFIFD{
		Entries: []carvetest.TIFFEntry{
			{Tag: 256, Type: 4, Value: carvetest.TIFFValue(be, 4, 64)},
			{Tag: 257, Type: 4, Value: carvetest.TIFFValue(be, 4, 48)},
			{Tag: 270, Type: 2, Value: tiffDescription},
		},
		Strips: [][]byte{bytes.Repeat([]byte{0xA5}, 64)},
	}
}

// tiffOpts: IFDs first, no late value.
func tiffOpts(pages ...carvetest.TIFFIFD) carvetest.TIFFOpts {
	return carvetest.TIFFOpts{Pages: pages, IFDsFirst: true, LateValueOf: -1}
}

func tiffWindow(buf []byte) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), newBudget(1<<30))
}

// tiffVerdict validates obj followed by trail bytes of fill, the window being the whole buffer.
func tiffVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateTIFF(tiffWindow(buf))
}

// tiffSet is the built-in set with the MaxLen of every tiff row replaced.
func tiffSet(maxLen int64) *Set {
	s := Builtin()
	for i := range s.Types {
		if s.Types[i].Name == "tiff" {
			s.Types[i].MaxLen = maxLen
		}
	}
	return s
}

func scanTIFF(t *testing.T, img []byte, runs []Run, set *Set, o Options) ([]Hit, Stats) {
	t.Helper()
	var hits []Hit
	st, err := Scan(context.Background(), bytes.NewReader(img), runs, set, o, func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits, st
}

func TestTIFFValidatesBuilderVariants(t *testing.T) {
	tiles := tiffPage(false)
	tiles.Strips, tiles.Tiles = nil, [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}
	short := tiffPage(false)
	short.ShortArrays = true
	thumb := carvetest.TIFFIFD{
		Entries:         []carvetest.TIFFEntry{{Tag: 259, Type: 3, Value: carvetest.TIFFValue(false, 3, 6)}},
		JPEGInterchange: bytes.Repeat([]byte{0xD8}, 40),
	}
	exif := tiffPage(false)
	exif.Exif = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 33434, Type: 5, Value: carvetest.TIFFValue(false, 5, 1, 100)}}}
	exif.GPS = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 1, Type: 2, Value: []byte("N\x00")}}}
	exif.Interop = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 1, Type: 2, Value: []byte("R98\x00")}}}
	subs := tiffPage(false)
	subs.SubIFDs = []carvetest.TIFFIFD{tiffPage(false), tiffPage(false)}
	mk := func(f func(*carvetest.TIFFOpts), pages ...carvetest.TIFFIFD) carvetest.TIFFOpts {
		o := tiffOpts(pages...)
		if f != nil {
			f(&o)
		}
		return o
	}
	for _, c := range []struct {
		name string
		o    carvetest.TIFFOpts
		ifds int
		refs int
		big  bool
		be   bool
	}{
		{"little-endian", mk(nil, tiffPage(false)), 1, 1, false, false},
		{"big-endian", mk(func(o *carvetest.TIFFOpts) { o.BigEndian = true }, tiffPage(true)), 1, 1, false, true},
		{"bigtiff little", mk(func(o *carvetest.TIFFOpts) { o.Big = true }, tiffPage(false)), 1, 1, true, false},
		{"bigtiff big-endian", mk(func(o *carvetest.TIFFOpts) { o.Big, o.BigEndian = true, true }, tiffPage(true)), 1, 1, true, true},
		{"three pages", mk(nil, tiffPage(false), tiffPage(false), tiffPage(false)), 3, 3, false, false},
		{"tiled", mk(nil, tiles), 1, 2, false, false},
		{"short arrays", mk(nil, short), 1, 1, false, false},
		{"jpeg thumbnail", mk(nil, tiffPage(false), thumb), 2, 2, false, false},
		{"exif gps interop", mk(nil, exif), 4, 1, false, false},
		{"two subifds", mk(nil, subs), 3, 3, false, false},
		{"data first", mk(func(o *carvetest.TIFFOpts) { o.IFDsFirst = false }, tiffPage(false), tiffPage(false)), 2, 2, false, false},
		{"ifds first", mk(nil, tiffPage(false), tiffPage(false)), 2, 2, false, false},
		{"bigtiff three pages, data first", mk(func(o *carvetest.TIFFOpts) { o.Big, o.IFDsFirst = true, false }, tiffPage(false), tiffPage(false), tiffPage(false)), 3, 3, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := carvetest.TIFF(c.o)
			v := tiffVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			order := "order=II"
			if c.be {
				order = "order=MM"
			}
			for _, want := range []string{order, "ifds=" + strconv.Itoa(c.ifds), "data-refs=" + strconv.Itoa(c.refs)} {
				if !slices.Contains(v.Basis, want) {
					t.Errorf("basis = %v, want %s", v.Basis, want)
				}
			}
			if slices.Contains(v.Basis, "bigtiff") != c.big {
				t.Errorf("basis = %v, bigtiff want %v", v.Basis, c.big)
			}
			if v.Type != "tiff" || v.Ext != "tif" {
				t.Errorf("type, ext = %q, %q, want tiff, tif", v.Type, v.Ext)
			}
		})
	}
}

func TestTIFFTrailingDataNotIncluded(t *testing.T) {
	obj := carvetest.TIFF(tiffOpts(tiffPage(false)))
	if v := tiffVerdict(obj, 4096); v.Outcome != Validated || v.Length != int64(len(obj)) {
		t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
	}
}

// TestTIFFLengthIsMaxEndOfAllReferences (Review Focus 4): the length is the
// maximum end over every reference, never the end of the last IFD.
func TestTIFFLengthIsMaxEndOfAllReferences(t *testing.T) {
	for _, ifdsFirst := range []bool{true, false} {
		t.Run("ifds first "+strconv.FormatBool(ifdsFirst), func(t *testing.T) {
			o := tiffOpts(tiffPage(false), tiffPage(false))
			o.IFDsFirst, o.LateValueOf = ifdsFirst, 0
			obj := carvetest.TIFF(o)
			if !bytes.HasSuffix(obj, tiffDescription) {
				t.Fatalf("builder did not write page 0's first out-of-line value last")
			}
			for _, trail := range []int{0, 4096} {
				if v := tiffVerdict(obj, trail); v.Outcome != Validated || v.Length != int64(len(obj)) {
					t.Errorf("trail %d: verdict = %+v, want validated length %d", trail, v, len(obj))
				}
			}
		})
	}
}

func TestTIFFIdentityFromStructure(t *testing.T) {
	dng := tiffPage(false)
	dng.DNGVersion = true
	dngBE := tiffPage(true)
	dngBE.DNGVersion = true
	sub := tiffPage(false)
	dngSub := tiffPage(false)
	dngSub.DNGVersion = true
	sub.SubIFDs = []carvetest.TIFFIFD{dngSub}
	mk := func(f func(*carvetest.TIFFOpts), p carvetest.TIFFIFD) carvetest.TIFFOpts {
		o := tiffOpts(p)
		f(&o)
		return o
	}
	for _, c := range []struct {
		name      string
		o         carvetest.TIFFOpts
		typ, ext  string
		bigtiff   bool
		firstByte string
	}{
		{"plain", mk(func(*carvetest.TIFFOpts) {}, tiffPage(false)), "tiff", "tif", false, "II*\x00"},
		{"DNGVersion", mk(func(*carvetest.TIFFOpts) {}, dng), "dng", "dng", false, "II*\x00"},
		{"DNGVersion big-endian", mk(func(o *carvetest.TIFFOpts) { o.BigEndian = true }, dngBE), "dng", "dng", false, "MM\x00*"},
		{"CR2", mk(func(o *carvetest.TIFFOpts) { o.CR2 = true }, tiffPage(false)), "cr2", "cr2", false, "II*\x00"},
		{"IIRO", mk(func(o *carvetest.TIFFOpts) { o.Magic = "IIRO" }, tiffPage(false)), "orf", "orf", false, "IIRO"},
		{"IIRS", mk(func(o *carvetest.TIFFOpts) { o.Magic = "IIRS" }, tiffPage(false)), "orf", "orf", false, "IIRS"},
		{"MMOR", mk(func(o *carvetest.TIFFOpts) { o.Magic, o.BigEndian = "MMOR", true }, tiffPage(true)), "orf", "orf", false, "MMOR"},
		{"IIU", mk(func(o *carvetest.TIFFOpts) { o.Magic = "IIU\x00" }, tiffPage(false)), "rw2", "rw2", false, "IIU\x00"},
		{"BigTIFF with DNGVersion", mk(func(o *carvetest.TIFFOpts) { o.Big = true }, dng), "tiff", "tif", true, "II+\x00"},
		{"CR bytes in a big-endian file", mk(func(o *carvetest.TIFFOpts) { o.CR2, o.BigEndian = true, true }, tiffPage(true)), "tiff", "tif", false, "MM\x00*"},
		{"DNGVersion only in a SubIFD", mk(func(*carvetest.TIFFOpts) {}, sub), "tiff", "tif", false, "II*\x00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := carvetest.TIFF(c.o)
			if string(obj[:4]) != c.firstByte {
				t.Fatalf("builder wrote magic %q, want %q", obj[:4], c.firstByte)
			}
			v := tiffVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if v.Type != c.typ || v.Ext != c.ext {
				t.Errorf("type, ext = %q, %q, want %q, %q", v.Type, v.Ext, c.typ, c.ext)
			}
			if slices.Contains(v.Basis, "bigtiff") != c.bigtiff {
				t.Errorf("basis = %v, bigtiff want %v", v.Basis, c.bigtiff)
			}
		})
	}
}

func TestTIFFMakerNoteAssumption(t *testing.T) {
	const assumption = "makernote-private-data-not-followed"
	p := tiffPage(false)
	p.MakerNote = bytes.Repeat([]byte{0x4D}, 300)
	o := tiffOpts(p)
	o.IFDsFirst = false
	obj := carvetest.TIFF(o)
	// the maker note is the last value of the file: the length covers it only if the walk counted it
	if !bytes.HasSuffix(obj, p.MakerNote) {
		t.Fatal("builder did not write the maker note last")
	}
	v := tiffVerdict(obj, 0)
	if v.Outcome != Validated || v.Length != int64(len(obj)) || !slices.Contains(v.Assumptions, assumption) {
		t.Fatalf("verdict = %+v, want validated length %d with %s", v, len(obj), assumption)
	}
	plain := carvetest.TIFF(tiffOpts(tiffPage(false)))
	if v := tiffVerdict(plain, 0); v.Outcome != Validated || slices.Contains(v.Assumptions, assumption) {
		t.Errorf("verdict without a maker note = %+v, want validated without the assumption", v)
	}
}

// tiffPatch helpers: a tiny independent reader of the built bytes.
func tiffOrder(obj []byte) binary.ByteOrder {
	if obj[0] == 'M' {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

func tiffIFD0(obj []byte) int {
	if obj[2] == 0x2B || obj[3] == 0x2B {
		return int(tiffOrder(obj).Uint64(obj[8:]))
	}
	return int(tiffOrder(obj).Uint32(obj[4:]))
}

// tiffEntryAt returns the position of the entry with tag in IFD0.
func tiffEntryAt(t *testing.T, obj []byte, tag uint16) int {
	t.Helper()
	big := obj[2] == 0x2B || obj[3] == 0x2B
	bo := tiffOrder(obj)
	p, n, sz := tiffIFD0(obj), 0, 12
	if big {
		n, sz = int(bo.Uint64(obj[p:])), 20
		p += 8
	} else {
		n = int(bo.Uint16(obj[p:]))
		p += 2
	}
	for i := range n {
		if bo.Uint16(obj[p+i*sz:]) == tag {
			return p + i*sz
		}
	}
	t.Fatalf("no entry with tag %d in IFD0", tag)
	return 0
}

func TestTIFFRejects(t *testing.T) {
	good := func(f func(*carvetest.TIFFOpts)) carvetest.TIFFOpts {
		o := tiffOpts(tiffPage(false))
		if f != nil {
			f(&o)
		}
		return o
	}
	bigGood := func(f func(*carvetest.TIFFOpts)) carvetest.TIFFOpts {
		return good(func(o *carvetest.TIFFOpts) {
			o.Big = true
			if f != nil {
				f(o)
			}
		})
	}
	patch := func(f func(obj []byte)) []byte {
		obj := carvetest.TIFF(good(nil))
		f(obj)
		return obj
	}
	noData := tiffPage(false)
	noData.Strips = nil
	zeroBytes := func(f func(*carvetest.TIFFOpts)) []byte {
		o := good(f)
		o.Pages = []carvetest.TIFFIFD{noData}
		return carvetest.TIFF(o)
	}
	longCount := carvetest.TIFFIFD{
		Entries: []carvetest.TIFFEntry{{Tag: 700, Type: 16, Count: 1, Value: carvetest.TIFFValue(false, 16, 5)}},
		Strips:  [][]byte{{1, 2, 3, 4}},
	}
	bigLong := func() []byte {
		o := tiffOpts(longCount)
		o.Big = true
		obj := carvetest.TIFF(o)
		at := tiffEntryAt(t, obj, 700)
		binary.LittleEndian.PutUint64(obj[at+4:], 1<<62)
		binary.LittleEndian.PutUint64(obj[at+12:], 100) // the value field is now an offset
		return obj
	}()
	bigPatch := func(f func(obj []byte)) []byte {
		obj := carvetest.TIFF(bigGood(nil))
		f(obj)
		return obj
	}
	shortWindow := carvetest.TIFF(good(nil))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"mixed order II 00 2A", patch(func(b []byte) { b[2], b[3] = 0x00, 0x2A }), "bad-header"},
		{"MM with the little-endian magic", patch(func(b []byte) { b[0], b[1] = 'M', 'M' }), "bad-header"},
		{"BigTIFF offset size 4", bigPatch(func(b []byte) { b[4] = 4 }), "bad-header"},
		{"BigTIFF reserved word", bigPatch(func(b []byte) { b[6] = 1 }), "bad-header"},
		{"IFD0 offset 4", patch(func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 4) }), "bad-offset"},
		{"IFD0 offset past the window", patch(func(b []byte) { binary.LittleEndian.PutUint32(b[4:], uint32(len(b)+100)) }), "truncated"},
		{"IFD0 offset at the window end", patch(func(b []byte) { binary.LittleEndian.PutUint32(b[4:], uint32(len(b))) }), "truncated"},
		{"IFD count 0", patch(func(b []byte) { binary.LittleEndian.PutUint16(b[tiffIFD0(b):], 0) }), "empty-ifd"},
		{"value offset inside the header", patch(func(b []byte) { binary.LittleEndian.PutUint32(b[tiffEntryAt(t, b, 270)+8:], 2) }), "bad-offset"},
		{"entry type 14 classic", carvetest.TIFF(good(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 14 })), "bad-entry"},
		{"entry type 16 classic", carvetest.TIFF(good(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 16 })), "bad-entry"},
		{"entry type 0xFFFF classic", carvetest.TIFF(good(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 0xFFFF })), "bad-entry"},
		{"entry type 14 BigTIFF", carvetest.TIFF(bigGood(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 14 })), "bad-entry"},
		{"entry type 15 BigTIFF", carvetest.TIFF(bigGood(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 15 })), "bad-entry"},
		{"entry type 19 BigTIFF", carvetest.TIFF(bigGood(func(o *carvetest.TIFFOpts) { o.Hostile.BadEntryType = 19 })), "bad-entry"},
		{"count mismatch", carvetest.TIFF(good(func(o *carvetest.TIFFOpts) { o.Hostile.CountMismatch = true })), "bad-strips"},
		{"StripOffsets typed ASCII", patch(func(b []byte) { binary.LittleEndian.PutUint16(b[tiffEntryAt(t, b, 273)+2:], 2) }), "bad-strips"},
		{"ExifIFD pointer typed ASCII", patch(func(b []byte) {
			// 270 becomes an Exif pointer typed ASCII
			binary.LittleEndian.PutUint16(b[tiffEntryAt(t, b, 270):], 34665)
		}), "bad-entry"},
		{"no image data", zeroBytes(nil), "no-image-data"},
		{"BigTIFF LONG8 count 1<<62", bigLong, "truncated"},
		{"a strip ending past the window", shortWindow[:len(shortWindow)-1], "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := c.obj
			v := tiffVerdict(obj, 0)
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %s", v, c.reason)
			}
		})
	}
}

// TestTIFFRejectsThroughTheScanner: a valid TIFF longer than the (lowered)
// MaxLen of its rows is rejected truncated and not carved.
func TestTIFFRejectsThroughTheScanner(t *testing.T) {
	p := tiffPage(false)
	p.Strips = [][]byte{bytes.Repeat([]byte{0xA5}, 80<<10)}
	obj := carvetest.TIFF(tiffOpts(p))
	img := carvetest.Image(256<<10, fillPattern)
	carvetest.Place(img, 0, obj)
	hits, st := scanTIFF(t, img, whole(img), tiffSet(64<<10), Options{Align: 512})
	if len(hits) != 0 || st.Rejected["truncated"] != 1 {
		t.Fatalf("hits = %+v rejected = %v, want one truncated and no hit", hits, st.Rejected)
	}
	// the same object under the real MaxLen is carved exactly
	hits, st = scanTIFF(t, img, whole(img), Builtin(), Options{Align: 512})
	if len(hits) != 1 || hits[0].Outcome != Validated || hits[0].Length != int64(len(obj)) || hits[0].Type != "tiff" || len(st.Rejected) != 0 {
		t.Fatalf("hits = %+v rejected = %v", hits, st.Rejected)
	}
}

func TestTIFFIFDCycleIsRejected(t *testing.T) {
	for name, h := range map[string]carvetest.TIFFHostile{
		"next loop":    {NextLoop: true},
		"self subifd":  {SelfSubIFD: true},
		"exif to ifd0": {ExifToIFD0: true},
	} {
		t.Run(name, func(t *testing.T) {
			o := tiffOpts(tiffPage(false))
			o.Hostile = h
			obj := carvetest.TIFF(o)
			w := tiffWindow(obj)
			v := validateTIFF(w)
			if v.Outcome != Rejected || v.Reason != "ifd-cycle" {
				t.Fatalf("verdict = %+v, want rejected ifd-cycle", v)
			}
			if w.bud.spent > int64(len(obj))+64<<10 {
				t.Errorf("validate bytes = %d, want at most %d", w.bud.spent, int64(len(obj))+64<<10)
			}
		})
	}
}

// tiffBoundedCase is one walk that cannot finish.
type tiffBoundedCase struct {
	name, reason, typ, ext string
	obj                    []byte
}

func tiffBoundedCases() []tiffBoundedCase {
	dng := func(p carvetest.TIFFIFD) carvetest.TIFFIFD { p.DNGVersion = true; return p }
	var chain []carvetest.TIFFIFD
	for i := range 65 {
		p := tiffPage(false)
		p.DNGVersion = i == 0
		chain = append(chain, p)
	}
	// tags above 50706 sort after DNGVersion, so it is among the first entries
	many := tiffPage(false)
	many.DNGVersion = true
	for i := range 1025 {
		many.Entries = append(many.Entries, carvetest.TIFFEntry{Tag: uint16(51000 + i), Type: 3, Value: carvetest.TIFFValue(false, 3, uint64(i))})
	}
	huge := dng(tiffPage(false))
	one := []byte{7}
	huge.Strips = make([][]byte, 1<<20+1) // tiffMaxValues + 1, one shared byte
	for i := range huge.Strips {
		huge.Strips[i] = one
	}
	zero := dng(tiffPage(false))
	zeroOpts := tiffOpts(zero)
	zeroOpts.Hostile.ZeroByteCount = true
	return []tiffBoundedCase{
		{"65 pages", "tiff-walk-capped=ifds", "dng", "dng", carvetest.TIFF(tiffOpts(chain...))},
		{"1025 entries", "tiff-walk-capped=entries", "dng", "dng", carvetest.TIFF(tiffOpts(many))},
		{"strip array over tiffMaxValues", "tiff-walk-capped=values", "dng", "dng", carvetest.TIFF(tiffOpts(huge))},
		{"zero byte count", "tiff-zero-bytecount", "dng", "dng", carvetest.TIFF(zeroOpts)},
		{"zero byte count, plain", "tiff-zero-bytecount", "tiff", "tif", func() []byte {
			o := tiffOpts(tiffPage(false))
			o.Hostile.ZeroByteCount = true
			return carvetest.TIFF(o)
		}()},
	}
}

// TestTIFFIncompleteWalkIsBounded (ruling Q1): a walk that cannot finish is
// carve-bounded at the window with a counted reason and the identity its
// structure showed, never validated.
func TestTIFFIncompleteWalkIsBounded(t *testing.T) {
	for _, c := range tiffBoundedCases() {
		t.Run(c.name, func(t *testing.T) {
			buf := append(slices.Clone(c.obj), bytes.Repeat(fillPattern, 4096/len(fillPattern)+1)...)
			w := tiffWindow(buf)
			v := validateTIFF(w)
			if v.Outcome != Bounded || v.Length != w.Len() || v.Reason != c.reason || v.Type != c.typ || v.Ext != c.ext {
				t.Fatalf("verdict = %+v, want bounded length %d reason %s type %s", v, w.Len(), c.reason, c.typ)
			}
			// through Scan over a run shorter than MaxLen
			img := carvetest.Image(512+len(buf), fillPattern)
			carvetest.Place(img, 512, buf)
			hits, st := scanTIFF(t, img, whole(img), Builtin(), Options{Align: 512})
			if len(hits) != 1 {
				t.Fatalf("hits = %+v, want one", hits)
			}
			h := hits[0]
			if h.Confidence != 25 || h.Method != "carve-bounded" || h.Outcome != Bounded || h.Offset != 512 || h.Length != int64(len(buf)) ||
				h.Type != c.typ || !slices.Contains(h.Assumptions, c.reason) || !slices.Contains(h.Assumptions, "cut-at-run-end") ||
				st.BoundedBy[c.reason] != 1 || len(st.BoundedBy) != 1 {
				t.Fatalf("hit = %+v BoundedBy = %v", h, st.BoundedBy)
			}
		})
	}
	t.Run("ifds over a longer run is length-capped", func(t *testing.T) {
		c := tiffBoundedCases()[0]
		img := carvetest.Image(128<<10, fillPattern)
		carvetest.Place(img, 0, c.obj)
		hits, st := scanTIFF(t, img, whole(img), tiffSet(64<<10), Options{Align: 512})
		if len(hits) != 1 || hits[0].Length != 64<<10 || !slices.Contains(hits[0].Assumptions, c.reason) ||
			!slices.Contains(hits[0].Assumptions, "length-capped") || st.BoundedBy[c.reason] != 1 {
			t.Fatalf("hits = %+v BoundedBy = %v", hits, st.BoundedBy)
		}
	})
}

func TestTIFFHostileCountsDoNotAllocate(t *testing.T) {
	const size = 1 << 20
	random := func(obj []byte) []byte {
		buf := make([]byte, size)
		x := uint32(2463534242) // xorshift32: seeded pseudo-random bytes
		for i := range buf {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			buf[i] = byte(x)
		}
		copy(buf, obj)
		return buf
	}
	p := tiffPage(false)
	o := tiffOpts(p)
	o.Hostile.EntryCount = 65535
	bigEntries := random(carvetest.TIFF(o))
	arr := carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{
		{Tag: 273, Type: 4, Count: 0xFFFFFFFF, Value: carvetest.TIFFValue(false, 4, 100)},
		{Tag: 279, Type: 4, Count: 0xFFFFFFFF, Value: carvetest.TIFFValue(false, 4, 100)},
	}}
	bigArray := random(carvetest.TIFF(tiffOpts(arr)))
	for _, c := range []struct {
		name    string
		buf     []byte
		outcome Outcome
		reason  string
	}{
		{"65535 entries over random bytes", bigEntries, Rejected, "bad-entry"}, // the random entries are read before the cap is reached
		{"strip array of 0xFFFFFFFF", bigArray, Rejected, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := tiffWindow(c.buf)
			v := validateTIFF(w)
			if v.Outcome != c.outcome || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want %s %s", v, c.outcome, c.reason)
			}
			if w.bud.spent > size+64<<10 {
				t.Errorf("validate bytes = %d, want at most %d", w.bud.spent, size+64<<10)
			}
			if a := testing.AllocsPerRun(5, func() { validateTIFF(tiffWindow(c.buf)) }); a > 16 {
				t.Errorf("allocs per run = %v, want at most 16", a)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			validateTIFF(tiffWindow(c.buf))
			runtime.ReadMemStats(&after)
			if d := after.TotalAlloc - before.TotalAlloc; d >= 1<<20 {
				t.Errorf("allocated %d bytes, want under 1 MiB", d)
			}
		})
	}
}

// TestTIFFInsideJPEGExifIsNestedOnly (Review Focus 1): every camera JPEG
// holds a complete TIFF in its Exif segment; it is one JPEG artifact, and the
// TIFF is found only with --nested, labelled as nested, never at the top level.
func TestTIFFInsideJPEGExifIsNestedOnly(t *testing.T) {
	p := tiffPage(false)
	p.Strips = [][]byte{bytes.Repeat([]byte{0xA5}, 64)}
	jpg := carvetest.JPEG(carvetest.JPEGOpts{W: 64, H: 48, Thumbnail: carvetest.TIFF(tiffOpts(p))}) // the builder adds Exif\0\0
	img := carvetest.Image(8*kib, fillPattern)
	carvetest.Place(img, 0, jpg)
	hits, st := scanTIFF(t, img, whole(img), Builtin(), Options{Align: 512})
	if len(hits) != 1 || hits[0].Family != "jpeg" || hits[0].Outcome != Validated {
		t.Fatalf("hits = %+v rejected = %v, want exactly the JPEG", hits, st.Rejected)
	}
	hits, _ = scanTIFF(t, img, whole(img), Builtin(), Options{Align: 512, Nested: true})
	if len(hits) != 2 {
		t.Fatalf("nested hits = %+v, want the JPEG and the TIFF", hits)
	}
	var tiff *Hit
	for i := range hits {
		if hits[i].Family == "tiff" {
			tiff = &hits[i]
		}
	}
	if tiff == nil || tiff.NestedIn != 0 || !slices.Contains(tiff.Assumptions, "nested-in=0000000000000000") || tiff.Offset == 0 {
		t.Fatalf("tiff hit = %+v, want nested in the JPEG at offset 0", tiff)
	}
}

func TestTIFFNegativeControls(t *testing.T) {
	t.Run("text quoting the magics", func(t *testing.T) {
		magics := []string{"II*\x00", "MM\x00*", "II+\x00", "MM\x00+", "IIRO", "IIRS", "MMOR", "II\x55\x00"}
		img := carvetest.Image(len(magics)*512+512, fillPattern)
		for i, m := range magics {
			carvetest.Place(img, i*512, []byte(m+" is how a TIFF begins; this prose only quotes it, and the bytes after it are text."))
		}
		hits, st := scanTIFF(t, img, whole(img), Builtin(), Options{Align: 512})
		if len(hits) != 0 {
			t.Fatalf("hits = %+v rejected = %v, want none", hits, st.Rejected)
		}
	})
	t.Run("first 60% of a valid file", func(t *testing.T) {
		for _, ifdsFirst := range []bool{true, false} {
			p := tiffPage(false)
			p.Strips = [][]byte{bytes.Repeat([]byte{1}, 3000), bytes.Repeat([]byte{2}, 3000)}
			o := tiffOpts(p, p)
			o.IFDsFirst = ifdsFirst
			obj := carvetest.TIFF(o)
			cut := obj[:len(obj)*6/10]
			if v := tiffVerdict(cut, 0); v.Outcome != Rejected || v.Reason != "truncated" {
				t.Errorf("ifds first %v: verdict = %+v, want rejected truncated", ifdsFirst, v)
			}
		}
	})
}

// TestTIFFReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// header, in an IFD and in an offset array is rejected "unreadable" (never
// "truncated") and recorded in the window.
func TestTIFFReadErrorIsUnreadable(t *testing.T) {
	many := tiffPage(false)
	many.Strips = make([][]byte, 5000)
	for i := range many.Strips {
		many.Strips[i] = []byte{byte(i)}
	}
	plain := carvetest.TIFF(tiffOpts(tiffPage(false)))
	arrays := carvetest.TIFF(tiffOpts(many))
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at header", plain, 0},
		{"in the directory", plain, int64(tiffIFD0(plain)) + 30},
		{"in an offset array", arrays, 30000},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateTIFF(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

func TestTIFFTypeSizes(t *testing.T) {
	for typ, want := range map[uint16][2]int64{
		0: {0, 0}, 1: {1, 1}, 2: {1, 1}, 3: {2, 2}, 4: {4, 4}, 5: {8, 8}, 6: {1, 1}, 7: {1, 1}, 8: {2, 2},
		9: {4, 4}, 10: {8, 8}, 11: {4, 4}, 12: {8, 8}, 13: {4, 4}, 14: {0, 0}, 15: {0, 0},
		16: {0, 8}, 17: {0, 8}, 18: {0, 8}, 19: {0, 0},
	} {
		if got := [2]int64{tiffTypeSize(typ, false), tiffTypeSize(typ, true)}; got != want {
			t.Errorf("type %d sizes (classic, big) = %v, want %v", typ, got, want)
		}
	}
}

// tiffCorpus is every builder variant of the TIFF validator, valid and
// hostile, for the bounded-window and fuzz-seed tests.
func tiffCorpus() []hostileObj {
	var c []hostileObj
	add := func(name string, o carvetest.TIFFOpts) {
		c = append(c, hostileObj{"tiff/" + name, "tiff", carvetest.TIFF(o)})
	}
	with := func(f func(*carvetest.TIFFOpts), pages ...carvetest.TIFFIFD) carvetest.TIFFOpts {
		o := tiffOpts(pages...)
		if f != nil {
			f(&o)
		}
		return o
	}
	dng := tiffPage(false)
	dng.DNGVersion = true
	tiles := tiffPage(false)
	tiles.Strips, tiles.Tiles = nil, [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}
	short := tiffPage(false)
	short.ShortArrays = true
	thumb := carvetest.TIFFIFD{JPEGInterchange: bytes.Repeat([]byte{0xD8}, 40)}
	exif := tiffPage(false)
	exif.Exif = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 33434, Type: 5, Value: carvetest.TIFFValue(false, 5, 1, 100)}}}
	exif.GPS = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 1, Type: 2, Value: []byte("N\x00")}}}
	exif.Interop = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 1, Type: 2, Value: []byte("R98\x00")}}}
	exif.MakerNote = bytes.Repeat([]byte{0x4D}, 100)
	subs := tiffPage(false)
	subs.SubIFDs = []carvetest.TIFFIFD{tiffPage(false), tiffPage(false)}
	hostile := func(h carvetest.TIFFHostile) func(*carvetest.TIFFOpts) {
		return func(o *carvetest.TIFFOpts) { o.Hostile = h }
	}
	add("little", with(nil, tiffPage(false)))
	add("big-endian", with(func(o *carvetest.TIFFOpts) { o.BigEndian = true }, tiffPage(true)))
	add("bigtiff", with(func(o *carvetest.TIFFOpts) { o.Big = true }, tiffPage(false)))
	add("bigtiff-big-endian", with(func(o *carvetest.TIFFOpts) { o.Big, o.BigEndian = true, true }, tiffPage(true)))
	add("pages", with(nil, tiffPage(false), tiffPage(false), tiffPage(false)))
	add("tiled", with(nil, tiles))
	add("short-arrays", with(nil, short))
	add("jpeg-thumbnail", with(nil, tiffPage(false), thumb))
	add("exif-gps-interop", with(nil, exif))
	add("subifds", with(nil, subs))
	add("data-first", with(func(o *carvetest.TIFFOpts) { o.IFDsFirst = false }, tiffPage(false), tiffPage(false)))
	add("late-value", with(func(o *carvetest.TIFFOpts) { o.LateValueOf = 0 }, tiffPage(false), tiffPage(false)))
	add("dng", with(nil, dng))
	add("cr2", with(func(o *carvetest.TIFFOpts) { o.CR2 = true }, tiffPage(false)))
	raw := tiffPage(false)
	raw.Strips = [][]byte{bytes.Repeat([]byte{0x5A}, 200)}
	add("cr2-raw-ifd", with(func(o *carvetest.TIFFOpts) { o.CR2, o.CR2Raw = true, &raw }, tiffPage(false)))
	manySubs := dng
	manySubs.SubIFDs = make([]carvetest.TIFFIFD, 65)
	for i := range manySubs.SubIFDs {
		manySubs.SubIFDs[i] = tiffPage(false)
	}
	add("subifd-cap", with(nil, manySubs))
	add("orf", with(func(o *carvetest.TIFFOpts) { o.Magic = "IIRO" }, tiffPage(false)))
	add("rw2", with(func(o *carvetest.TIFFOpts) { o.Magic = "IIU\x00" }, tiffPage(false)))
	add("next-loop", with(hostile(carvetest.TIFFHostile{NextLoop: true}), tiffPage(false)))
	add("self-subifd", with(hostile(carvetest.TIFFHostile{SelfSubIFD: true}), tiffPage(false)))
	add("exif-to-ifd0", with(hostile(carvetest.TIFFHostile{ExifToIFD0: true}), tiffPage(false)))
	add("zero-bytecount", with(hostile(carvetest.TIFFHostile{ZeroByteCount: true}), tiffPage(false)))
	add("count-mismatch", with(hostile(carvetest.TIFFHostile{CountMismatch: true}), tiffPage(false)))
	add("bad-entry-type", with(hostile(carvetest.TIFFHostile{BadEntryType: 14}), tiffPage(false)))
	add("entry-count", with(hostile(carvetest.TIFFHostile{EntryCount: 2000}), tiffPage(false)))
	return c
}

// TestTIFFCR2RawIFDPointer (T4-m1): the u32 at header byte 12 of a CR2 is a
// main IFD queued like every other pointer, so the strips of a raw IFD on no
// next-IFD chain count toward the length. 0 is absent, a pointer to an IFD
// already queued is the same IFD, and a bad one is the usual typed reject.
func TestTIFFCR2RawIFDPointer(t *testing.T) {
	raw := tiffPage(false)
	raw.Strips = [][]byte{bytes.Repeat([]byte{0x5A}, 500)}
	o := tiffOpts(tiffPage(false))
	o.CR2, o.CR2Raw = true, &raw
	obj := carvetest.TIFF(o)
	chained := carvetest.TIFF(func() carvetest.TIFFOpts { c := o; c.CR2Raw = nil; return c }())
	v := tiffVerdict(obj, 40)
	if v.Outcome != Validated || v.Type != "cr2" || v.Length != int64(len(obj)) {
		t.Fatalf("CR2 with an unchained raw IFD = %+v, want validated cr2 length %d", v, len(obj))
	}
	if v := tiffVerdict(chained, 40); v.Length >= int64(len(obj)) || v.Length != int64(len(chained)) {
		t.Fatalf("CR2 without a raw IFD = %+v, want length %d", v, len(chained))
	}
	if !slices.Contains(v.Basis, "ifds=2") {
		t.Errorf("basis = %v, want ifds=2", v.Basis)
	}
	patch := func(off uint32) []byte {
		b := slices.Clone(obj)
		binary.LittleEndian.PutUint32(b[12:], off)
		return b
	}
	if v := tiffVerdict(patch(16), 0); v.Outcome != Validated {
		t.Errorf("raw pointer to IFD0 = %+v, want validated (the same IFD, not a cycle)", v)
	}
	for _, c := range []struct {
		name   string
		off    uint32
		reason string
	}{
		{"inside the header", 8, "bad-offset"},
		{"past the window", uint32(len(obj)) + 100, "truncated"},
	} {
		if v := tiffVerdict(patch(c.off), 0); v.Outcome != Rejected || v.Reason != c.reason {
			t.Errorf("raw pointer %s = %+v, want rejected %s", c.name, v, c.reason)
		}
	}
	// The raw IFD is capped like any other: with 64 IFDs queued it is the 65th.
	many := make([]carvetest.TIFFIFD, 64)
	for i := range many {
		many[i] = tiffPage(false)
	}
	mo := tiffOpts(many...)
	mo.CR2, mo.CR2Raw = true, &raw
	if v := tiffVerdict(carvetest.TIFF(mo), 0); v.Outcome != Bounded || v.Reason != "tiff-walk-capped=ifds" {
		t.Errorf("64 chained IFDs plus a raw IFD = %+v, want bounded ifds", v)
	}
}

// TestTIFFCapInIFD0TypesTiffNotDNG (T4-m2, PASSES ON ARRIVAL: it pins the
// existing order of the walk): a cap hit inside IFD0 before tag 50706 (more
// than 64 SubIFD offsets in tag 330) is bounded and keeps the identity shown so
// far, so it is typed tiff although DNGVersion follows it in the same IFD.
func TestTIFFCapInIFD0TypesTiffNotDNG(t *testing.T) {
	page := tiffPage(false)
	page.DNGVersion = true
	page.SubIFDs = make([]carvetest.TIFFIFD, 65)
	for i := range page.SubIFDs {
		page.SubIFDs[i] = tiffPage(false)
	}
	v := tiffVerdict(carvetest.TIFF(tiffOpts(page)), 0)
	if v.Outcome != Bounded || v.Reason != "tiff-walk-capped=ifds" || v.Type != "tiff" || v.Ext != "tif" {
		t.Fatalf("verdict = %+v, want bounded tiff-walk-capped=ifds typed tiff/tif", v)
	}
}

// TestTIFFCR2RawPointerOnlyExemptsMainIFDs (review fix): the raw pointer may
// name an IFD the chain already holds only if that is a main IFD; at a queued
// Exif IFD it is ifd-cycle, and a new raw IFD whose next pointer loops back to
// IFD0 is ifd-cycle too.
func TestTIFFCR2RawPointerOnlyExemptsMainIFDs(t *testing.T) {
	page := tiffPage(false)
	page.Exif = &carvetest.TIFFIFD{Entries: []carvetest.TIFFEntry{{Tag: 33434, Type: 5, Value: carvetest.TIFFValue(false, 5, 1, 100)}}}
	o := tiffOpts(page)
	o.CR2 = true
	obj := carvetest.TIFF(o)
	count := int(binary.LittleEndian.Uint16(obj[16:]))
	var exifOff uint32
	for i := range count {
		e := obj[18+12*i:]
		if binary.LittleEndian.Uint16(e) == 34665 {
			exifOff = binary.LittleEndian.Uint32(e[8:])
		}
	}
	if exifOff == 0 {
		t.Fatal("no Exif pointer in the builder object")
	}
	patched := slices.Clone(obj)
	binary.LittleEndian.PutUint32(patched[12:], exifOff)
	if v := tiffVerdict(patched, 0); v.Outcome != Rejected || v.Reason != "ifd-cycle" {
		t.Errorf("raw pointer at the Exif IFD = %+v, want rejected ifd-cycle", v)
	}

	raw := tiffPage(false)
	ro := tiffOpts(tiffPage(false))
	ro.CR2, ro.CR2Raw = true, &raw
	robj := carvetest.TIFF(ro)
	rawOff := int(binary.LittleEndian.Uint32(robj[12:]))
	n := int(binary.LittleEndian.Uint16(robj[rawOff:]))
	binary.LittleEndian.PutUint32(robj[rawOff+2+12*n:], 16)
	if v := tiffVerdict(robj, 0); v.Outcome != Rejected || v.Reason != "ifd-cycle" {
		t.Errorf("raw IFD looping back to IFD0 = %+v, want rejected ifd-cycle", v)
	}
}
