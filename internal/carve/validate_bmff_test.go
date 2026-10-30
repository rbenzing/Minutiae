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
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func init() { implementedValidators["isobmff"] = true }

func bmffWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// bmffVerdict validates obj followed by trail bytes of fill, the window being
// the whole buffer (what the scanner hands a validator at the run end).
func bmffVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateBMFF(bmffWindow(buf, newBudget(1<<30)))
}

func TestBMFFIdentityFromStructure(t *testing.T) {
	for _, c := range []struct {
		brand, typ, ext string
		heif            bool
	}{
		{"isom", "mp4", "mp4", false},
		{"iso2", "mp4", "mp4", false},
		{"iso9", "mp4", "mp4", false},
		{"mp42", "mp4", "mp4", false},
		{"M4V ", "mp4", "mp4", false},
		{"f4v ", "mp4", "mp4", false},
		{"qt  ", "mov", "mov", false},
		{"3gp5", "3gp", "3gp", false},
		{"3g2a", "3gp", "3gp", false},
		{"heic", "heic", "heic", true},
		{"hevx", "heic", "heic", true},
		{"mif1", "heif", "heif", true},
		{"msf1", "heif", "heif", true},
		{"XYZW", "isobmff", "bmff", false},
		{"iso1", "isobmff", "bmff", false},
		{"avif", "avif", "avif", true},
		{"avis", "avif", "avif", false},
		{"crx ", "cr3", "cr3", false},
	} {
		t.Run(c.brand, func(t *testing.T) {
			obj := carvetest.MP4(carvetest.BMFFOpts{Brand: c.brand, HEIF: c.heif, Compat: []string{"isom", "mp42"}})
			v := bmffVerdict(obj, 0)
			if v.Outcome != Validated || v.Length != int64(len(obj)) || v.Type != c.typ || v.Ext != c.ext {
				t.Fatalf("verdict = %+v, want validated %s/%s length %d", v, c.typ, c.ext, len(obj))
			}
			if !slices.Contains(v.Basis, "brand="+c.brand) {
				t.Errorf("basis = %v, want brand=%s", v.Basis, c.brand)
			}
		})
	}
	// The identity is the structure's: a HEIF brand over a moov-only file is
	// not a picture, and a non-printable brand is shown escaped, not raw.
	if v := bmffVerdict(carvetest.MP4(carvetest.BMFFOpts{Brand: "heic"}), 0); v.Outcome != Rejected || v.Reason != "missing-meta" {
		t.Fatalf("heic brand over moov = %+v, want rejected missing-meta", v)
	}
	for _, c := range []struct {
		name   string
		o      carvetest.BMFFOpts
		reason string
	}{
		{"avif without meta", carvetest.BMFFOpts{Brand: "avif", HEIF: true, NoMeta: true}, "missing-meta"},
		{"avis without moov", carvetest.BMFFOpts{Brand: "avis", NoMoov: true}, "missing-moov"},
		{"crx without moov", carvetest.BMFFOpts{Brand: "crx ", NoMoov: true}, "missing-moov"},
		{"avif over moov", carvetest.BMFFOpts{Brand: "avif"}, "missing-meta"},
	} {
		if v := bmffVerdict(carvetest.MP4(c.o), 0); v.Outcome != Rejected || v.Reason != c.reason {
			t.Errorf("%s = %+v, want rejected %s", c.name, v, c.reason)
		}
	}
	v := bmffVerdict(carvetest.MP4(carvetest.BMFFOpts{Brand: "A\x00\xffB"}), 0)
	if v.Outcome != Validated || !slices.Contains(v.Basis, `brand=A\x00\xffB`) {
		t.Fatalf("odd brand verdict = %+v", v)
	}
}

func TestBMFFWalkVariants(t *testing.T) {
	base := carvetest.MP4(carvetest.BMFFOpts{})
	free := carvetest.Box("free", make([]byte, 20))
	second := carvetest.MP4(carvetest.BMFFOpts{Brand: "qt  "})
	for _, c := range []struct {
		name  string
		obj   []byte // the object proper
		after []byte // what follows it in the window
		boxes string
	}{
		{"moov then mdat", base, nil, "moov,mdat"},
		{"mdat before moov", carvetest.MP4(carvetest.BMFFOpts{MdatFirst: true}), nil, "mdat,moov"},
		{"64-bit mdat", carvetest.MP4(carvetest.BMFFOpts{LargeMdat: true}), nil, "moov,mdat"},
		{"64-bit mdat first", carvetest.MP4(carvetest.BMFFOpts{LargeMdat: true, MdatFirst: true}), nil, "mdat,moov"},
		{"trailing free", slices.Concat(base, free), nil, "moov,mdat,free"},
		{"second ftyp stops the walk", base, second, "moov,mdat"},
		{"fill stops the walk", base, bytes.Repeat(fillPattern, 50), "moov,mdat"},
		{"short tail stops the walk", base, []byte{1, 2, 3}, "moov,mdat"},
		{"size 7 stops the walk", base, []byte{0, 0, 0, 7, 'f', 'r', 'e', 'e', 0, 0}, "moov,mdat"},
		{"control type byte stops the walk", base, []byte{0, 0, 0, 16, 'f', 'r', 0x01, 'e', 0, 0, 0, 0, 0, 0, 0, 0}, "moov,mdat"},
		{"unknown type stops the walk", base, carvetest.Box("zzzz", make([]byte, 8)), "moov,mdat"},
		{"heic", carvetest.MP4(carvetest.BMFFOpts{HEIF: true}), nil, "meta,mdat"},
	} {
		t.Run(c.name, func(t *testing.T) {
			buf := slices.Concat(c.obj, c.after)
			v := validateBMFF(bmffWindow(buf, newBudget(1<<30)))
			if v.Outcome != Validated || v.Length != int64(len(c.obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(c.obj))
			}
			if !slices.Contains(v.Basis, "boxes="+c.boxes) {
				t.Errorf("basis = %v, want boxes=%s", v.Basis, c.boxes)
			}
		})
	}
	// A box list longer than 16 is cut in the basis, not in the length.
	long := base
	for range 20 {
		long = slices.Concat(long, free)
	}
	v := bmffVerdict(long, 0)
	var boxes string
	for _, b := range v.Basis {
		if s, ok := strings.CutPrefix(b, "boxes="); ok {
			boxes = s
		}
	}
	if v.Outcome != Validated || v.Length != int64(len(long)) || len(strings.Split(boxes, ",")) != 16 {
		t.Fatalf("long walk = %+v (boxes %q), want validated %d with 16 listed", v, boxes, len(long))
	}
}

func TestBMFFSizeZeroLastBoxIsBounded(t *testing.T) {
	obj := carvetest.MP4(carvetest.BMFFOpts{SizeZeroLast: true})
	buf := slices.Concat(obj, bytes.Repeat(fillPattern, 10))
	v := validateBMFF(bmffWindow(buf, newBudget(1<<30)))
	if v.Outcome != Bounded || v.Length != int64(len(buf)) || !slices.Equal(v.Assumptions, []string{"box-extends-to-end"}) {
		t.Fatalf("verdict = %+v, want bounded length %d box-extends-to-end", v, len(buf))
	}
	if v.Type != "mp4" || !slices.Contains(v.Basis, "boxes=moov,mdat") {
		t.Errorf("type/basis = %s %v", v.Type, v.Basis)
	}
	before := carvetest.MP4(carvetest.BMFFOpts{SizeZeroLast: true, MdatFirst: true})
	if v := bmffVerdict(before, 0); v.Outcome != Rejected || v.Reason != "unbounded-box" {
		t.Fatalf("size 0 before moov = %+v, want rejected unbounded-box", v)
	}
	// A HEIF file needs its picture meta, not a moov, before an open box.
	heic := carvetest.MP4(carvetest.BMFFOpts{HEIF: true, SizeZeroLast: true})
	if v := bmffVerdict(heic, 0); v.Outcome != Bounded || v.Type != "heic" {
		t.Fatalf("heic size 0 last = %+v", v)
	}
	heicBefore := carvetest.MP4(carvetest.BMFFOpts{HEIF: true, SizeZeroLast: true, MdatFirst: true})
	if v := bmffVerdict(heicBefore, 0); v.Outcome != Rejected || v.Reason != "unbounded-box" {
		t.Fatalf("heic size 0 before meta = %+v", v)
	}
}

func TestBMFFRejects(t *testing.T) {
	box, full := carvetest.Box, carvetest.FullBox
	good := carvetest.MP4(carvetest.BMFFOpts{})
	ftyp := func(brand string) []byte { return box("ftyp", []byte(brand), make([]byte, 4)) }
	mvhd := full("mvhd", 0, 0, make([]byte, 96))
	moov := box("moov", mvhd)
	hdlr := func(handler string) []byte {
		return full("hdlr", 0, 0, make([]byte, 4), []byte(handler), make([]byte, 12), []byte{0})
	}
	heic := func(kids ...[]byte) []byte {
		return slices.Concat(ftyp("heic"), full("meta", 0, 0, kids...), box("mdat", make([]byte, 8)))
	}
	cutMdat := slices.Concat(ftyp("isom"), moov, box("mdat", make([]byte, 100)))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"no moov", carvetest.MP4(carvetest.BMFFOpts{NoMoov: true}), "missing-moov"},
		{"only ftyp", ftyp("isom"), "missing-moov"},
		{"heic without meta", carvetest.MP4(carvetest.BMFFOpts{HEIF: true, NoMeta: true}), "missing-meta"},
		{"heic with hdlr vide", heic(hdlr("vide")), "missing-meta"},
		{"heic meta without hdlr", heic(full("pitm", 0, 0, []byte{0, 1})), "missing-meta"},
		{"heic hdlr too short", heic(full("hdlr", 0, 0, make([]byte, 8))), "missing-meta"},
		{"heic first hdlr wins", heic(hdlr("vide"), hdlr("pict")), "missing-meta"},
		{"bad children", carvetest.MP4(carvetest.BMFFOpts{BadChildren: true}), "bad-children"},
		{"bad children in meta", carvetest.MP4(carvetest.BMFFOpts{HEIF: true, BadChildren: true}), "bad-children"},
		{"moov without mvhd", slices.Concat(ftyp("isom"), box("moov", box("trak", make([]byte, 8)))), "bad-children"},
		{"empty moov", slices.Concat(ftyp("isom"), box("moov")), "bad-children"},
		{"moov child size 0", slices.Concat(ftyp("isom"), box("moov", mvhd, []byte{0, 0, 0, 0, 't', 'r', 'a', 'k'})), "bad-children"},
		{"moov child size 1", slices.Concat(ftyp("isom"), box("moov", mvhd, []byte{0, 0, 0, 1, 't', 'r', 'a', 'k'})), "bad-children"},
		{"moov child overruns", slices.Concat(ftyp("isom"), box("moov", mvhd, []byte{0, 0, 0, 99, 't', 'r', 'a', 'k'})), "bad-children"},
		{"moov child control type", slices.Concat(ftyp("isom"), box("moov", mvhd, []byte{0, 0, 0, 8, 't', 'r', 0, 'k'})), "bad-children"},
		{"meta payload under 4 bytes", slices.Concat(ftyp("heic"), box("meta", []byte{0, 0, 0})), "bad-children"},
		{"ftyp size 8", slices.Concat(box("ftyp"), moov), "bad-ftyp"},
		{"ftyp size 12", slices.Concat(box("ftyp", []byte("isom")), moov), "bad-ftyp"},
		{"ftyp size not a multiple of 4", slices.Concat(box("ftyp", []byte("isom"), make([]byte, 5)), moov), "bad-ftyp"},
		{"ftyp size 4112", slices.Concat(box("ftyp", []byte("isom"), make([]byte, 4100)), moov), "bad-ftyp"},
		{"ftyp size 4096 but window short", box("ftyp", []byte("isom"), make([]byte, 4080))[:100], "truncated"},
		{"first box not ftyp", slices.Concat(box("free", make([]byte, 8)), moov), "bad-ftyp"},
		{"ftyp largesize", slices.Concat(carvetest.LargeBox("ftyp", make([]byte, 8)), moov), "bad-ftyp"},
		{"mdat cut short", cutMdat[:len(cutMdat)-10], "truncated"},
		{"moov cut short", slices.Concat(ftyp("isom"), moov)[:len(ftyp("isom"))+30], "truncated"},
		{"largesize 8", slices.Concat(ftyp("isom"), moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 8}), "bad-length"},
		{"largesize 15", slices.Concat(ftyp("isom"), moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 15}), "bad-length"},
		{"largesize 2^63", slices.Concat(ftyp("isom"), moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0x80, 0, 0, 0, 0, 0, 0, 0}), "truncated"},
		{"largesize past the window", slices.Concat(ftyp("isom"), moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0x10, 0, 0, 0}), "truncated"},
		{"largesize header cut", slices.Concat(ftyp("isom"), moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0}), "truncated"},
		{"size past the window", slices.Concat(ftyp("isom"), moov, []byte{0xFF, 0xFF, 0xFF, 0xF0, 'm', 'd', 'a', 't', 1, 2, 3, 4}), "truncated"},
		{"one byte", []byte{0}, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateBMFF(bmffWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	if v := bmffVerdict(good, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
	// The pict handler alone is accepted, with other children around it.
	ok := heic(hdlr("pict"), full("pitm", 0, 0, []byte{0, 1}))
	if v := bmffVerdict(ok, 0); v.Outcome != Validated || v.Length != int64(len(ok)) {
		t.Fatalf("heic control verdict = %+v", v)
	}
}

// TestBMFFWindowBoundsAndBudget: a declared size is never allocated from; a
// walk of thousands of small boxes advances 8 bytes per step and the budget
// stops it; a cancelled context is rejected "unreadable" (ruling 3E-C1).
func TestBMFFWindowBoundsAndBudget(t *testing.T) {
	ftyp := carvetest.Box("ftyp", []byte("isom"), make([]byte, 4))
	obj := ftyp
	for range 50000 {
		obj = slices.Concat(obj, carvetest.Box("free"))
	}
	b := newBudget(4 << 10)
	v := validateBMFF(bmffWindow(obj, b))
	if v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted || b.spent > b.limit {
		t.Fatalf("verdict = %+v exhausted = %v spent = %d, want rejected unreadable on an exhausted budget", v, b.exhausted, b.spent)
	}
	// With the budget, the same walk is complete and has no moov.
	if v := bmffVerdict(obj, 0); v.Outcome != Rejected || v.Reason != "missing-moov" {
		t.Fatalf("walk verdict = %+v, want missing-moov", v)
	}
	// A moov of thousands of 8-byte children is bounded by the budget too.
	kids := make([][]byte, 0, 20001)
	kids = append(kids, carvetest.FullBox("mvhd", 0, 0, make([]byte, 96)))
	for range 20000 {
		kids = append(kids, carvetest.Box("skip"))
	}
	moov := slices.Concat(ftyp, carvetest.Box("moov", kids...))
	b = newBudget(4 << 10)
	if v := validateBMFF(bmffWindow(moov, b)); v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted {
		t.Fatalf("moov verdict = %+v exhausted = %v", v, b.exhausted)
	}
	if v := bmffVerdict(moov, 0); v.Outcome != Validated || v.Length != int64(len(moov)) {
		t.Fatalf("moov control = %+v", v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(obj), 0, int64(len(obj)), newBudget(1<<30))
	if v := validateBMFF(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
	// A hit at the very end of a window: an 8-byte header and no ftyp room.
	if v := validateBMFF(bmffWindow(ftyp[:8], newBudget(1<<30))); v.Outcome != Rejected || v.Reason != "truncated" {
		t.Fatalf("ftyp header only = %+v", v)
	}
}

// TestBMFFReadErrorIsUnreadable (ruling 3E-C1): a media read error at the
// start, in the top-level walk, inside the moov children and inside the meta
// children is rejected "unreadable" (never "truncated") and is recorded in the
// window.
func TestBMFFReadErrorIsUnreadable(t *testing.T) {
	ftyp := carvetest.Box("ftyp", []byte("isom"), make([]byte, 4))
	kids := [][]byte{carvetest.FullBox("mvhd", 0, 0, make([]byte, 96))}
	for range 100 {
		kids = append(kids, carvetest.Box("skip", make([]byte, 24)))
	}
	moov := slices.Concat(ftyp, carvetest.Box("moov", kids...), carvetest.Box("mdat", make([]byte, 64)))
	hdlr := carvetest.FullBox("hdlr", 0, 0, make([]byte, 4), []byte("pict"), make([]byte, 12), []byte{0})
	heicKids := [][]byte{carvetest.FullBox("pitm", 0, 0, []byte{0, 1}), carvetest.Box("padd", make([]byte, 200)), hdlr}
	heic := slices.Concat(carvetest.Box("ftyp", []byte("heic"), make([]byte, 4)), carvetest.FullBox("meta", 0, 0, heicKids...), carvetest.Box("mdat", make([]byte, 64)))
	for _, c := range []struct {
		name string
		obj  []byte
		at   int64
	}{
		{"at ftyp", moov, 0},
		{"at the brand", moov, 8},
		{"top-level header", moov, int64(len(moov)) - 64 - 8},
		{"moov child header", moov, int64(len(ftyp)) + 8 + 100 + 30},
		{"meta child header", heic, 80},
		{"hdlr handler", heic, 268},
	} {
		t.Run(c.name, func(t *testing.T) {
			if int64(len(c.obj)) <= c.at {
				t.Fatalf("object of %d bytes does not reach the failure at %d", len(c.obj), c.at)
			}
			if v := validateBMFF(bmffWindow(c.obj, newBudget(1<<30))); v.Outcome != Validated {
				t.Fatalf("control verdict = %+v", v)
			}
			w := newWindow(context.Background(), failAt{c.obj, c.at}, 0, int64(len(c.obj)), newBudget(1<<30))
			v := validateBMFF(w)
			if v.Outcome != Rejected || v.Reason != "unreadable" {
				t.Fatalf("verdict = %+v, want rejected unreadable", v)
			}
			if !errors.Is(w.readErr, errMedia) {
				t.Fatalf("window read error = %v, want the media error", w.readErr)
			}
		})
	}
}

// TestBMFFBigEndianSizes pins the byte order of the size fields.
func TestBMFFBigEndianSizes(t *testing.T) {
	obj := carvetest.MP4(carvetest.BMFFOpts{})
	if binary.BigEndian.Uint32(obj) != uint32(len(carvetest.Box("ftyp", []byte("isom"), make([]byte, 4)))) {
		t.Fatal("builder ftyp size is not big-endian")
	}
	le := slices.Clone(obj)
	slices.Reverse(le[:4])
	if v := bmffVerdict(le, 0); v.Outcome != Rejected || v.Reason != "bad-ftyp" {
		t.Fatalf("little-endian ftyp size = %+v", v)
	}
}

func TestBMFFRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family, Type, Ext, Brand string
			Size                           int64
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "isobmff" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := bmffVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
				t.Fatalf("verdict = %+v, want validated length %d", v, o.Size)
			}
			if v.Type != o.Type || v.Ext != o.Ext {
				t.Errorf("identity = %s/%s, recorded %s/%s", v.Type, v.Ext, o.Type, o.Ext)
			}
			if !slices.Contains(v.Basis, "brand="+o.Brand) {
				t.Errorf("basis = %v, want brand=%q (the recorded major brand)", v.Basis, o.Brand)
			}
		})
	}
	if n < 4 {
		t.Fatalf("expected at least 4 isobmff objects, found %d", n)
	}
}

// TestBMFFHdlrUnderHandlerSize: an hdlr of 12-19 bytes (the short-hdlr guard)
// has no handler_type, so the picture meta is missing. The trap box after the
// size-12 hdlr puts the bytes "pict" exactly where a guard-less read of the
// handler would look.
func TestBMFFHdlrUnderHandlerSize(t *testing.T) {
	box, full := carvetest.Box, carvetest.FullBox
	ftyp := box("ftyp", []byte("heic"), make([]byte, 4))
	for size := 12; size <= 19; size++ {
		hdlr := box("hdlr", make([]byte, size-8))
		trap := box("pict", make([]byte, 8))
		obj := slices.Concat(ftyp, full("meta", 0, 0, hdlr, trap), box("mdat", make([]byte, 8)))
		if v := bmffVerdict(obj, 0); v.Outcome != Rejected || v.Reason != "missing-meta" {
			t.Errorf("hdlr of %d bytes: verdict = %+v, want rejected missing-meta", size, v)
		}
	}
	// Control: the full 20-byte hdlr with the same handler is accepted.
	hdlr := full("hdlr", 0, 0, make([]byte, 4), []byte("pict"))
	ok := slices.Concat(ftyp, full("meta", 0, 0, hdlr), box("mdat", make([]byte, 8)))
	if v := bmffVerdict(ok, 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestBMFFCopyrightTypeByte: the type byte 0xA9 (a (c) atom such as
// (c)xyz) is a valid type character in a moov child, in a udta below the moov
// and in a top-level udta; a control byte is not.
func TestBMFFCopyrightTypeByte(t *testing.T) {
	box, full := carvetest.Box, carvetest.FullBox
	ftyp := box("ftyp", []byte("isom"), make([]byte, 4))
	mvhd := full("mvhd", 0, 0, make([]byte, 96))
	cpr := box("\xa9xyz", make([]byte, 12))
	for name, moov := range map[string][]byte{
		"child of moov":      box("moov", mvhd, cpr),
		"inside udta":        box("moov", mvhd, box("udta", cpr)),
		"after udta in moov": box("moov", mvhd, box("udta", cpr), cpr),
	} {
		obj := slices.Concat(ftyp, moov, box("mdat", make([]byte, 8)))
		if v := bmffVerdict(obj, 0); v.Outcome != Validated || v.Length != int64(len(obj)) {
			t.Errorf("%s: verdict = %+v, want validated %d", name, v, len(obj))
		}
	}
	// A top-level udta holding the atom is walked over; the atom itself is not
	// a top-level type and stops the walk without failing the file.
	good := slices.Concat(ftyp, box("moov", mvhd), box("udta", cpr))
	if v := bmffVerdict(slices.Concat(good, cpr), 0); v.Outcome != Validated || v.Length != int64(len(good)) {
		t.Fatalf("top-level udta verdict = %+v, want validated %d", v, len(good))
	}
	bad := slices.Concat(ftyp, box("moov", mvhd, box("\x01xyz", make([]byte, 12))))
	if v := bmffVerdict(bad, 0); v.Outcome != Rejected || v.Reason != "bad-children" {
		t.Fatalf("control-byte child = %+v, want rejected bad-children", v)
	}
}

// TestBMFFNonHEIFMalformedMetaValidates pins the accepted HEIF-only rule: the
// layout of a meta box is judged only for a HEIF brand; for any other identity
// it is walked over by its size.
func TestBMFFNonHEIFMalformedMetaValidates(t *testing.T) {
	box, full := carvetest.Box, carvetest.FullBox
	ftyp := box("ftyp", []byte("isom"), make([]byte, 4))
	moov := box("moov", full("mvhd", 0, 0, make([]byte, 96)))
	for name, meta := range map[string][]byte{
		"payload under 4 bytes": box("meta", []byte{0, 0, 0}),
		"stray children":        full("meta", 0, 0, []byte{1, 2, 3}),
	} {
		obj := slices.Concat(ftyp, moov, meta, box("mdat", make([]byte, 8)))
		v := bmffVerdict(obj, 0)
		if v.Outcome != Validated || v.Length != int64(len(obj)) || !slices.Contains(v.Basis, "boxes=moov,meta,mdat") {
			t.Errorf("%s: verdict = %+v, want validated %d with meta listed", name, v, len(obj))
		}
	}
	// The same meta under a HEIF brand is judged.
	heic := slices.Concat(box("ftyp", []byte("heic"), make([]byte, 4)), box("meta", []byte{0, 0, 0}))
	if v := bmffVerdict(heic, 0); v.Outcome != Rejected || v.Reason != "bad-children" {
		t.Fatalf("heic malformed meta = %+v, want rejected bad-children", v)
	}
}
