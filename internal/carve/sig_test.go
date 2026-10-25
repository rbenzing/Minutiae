package carve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// wantBuiltinDigest pins the built-in signature table. A change to the table
// MUST change BuiltinID too: audit records name the set by its id and digest.
const wantBuiltinDigest = "82aaea02765ca014555438e04e3fb74ba136739449e3eb361417827a33021ab1"

type wantType struct {
	name, ext, mime, validate string
	headers                   []Pattern
	min, max                  int64
}

func bs(s string) []byte { return []byte(s) }

var wantTable = []wantType{
	{"jpeg", "jpg", "image/jpeg", "jpeg", []Pattern{{0, []byte{0xFF, 0xD8, 0xFF}, nil}}, 64, 128 << 20},
	{"png", "png", "image/png", "png", []Pattern{{0, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, nil}}, 57, 64 << 20},
	{"gif", "gif", "image/gif", "gif", []Pattern{{0, bs("GIF8"), nil}}, 26, 32 << 20},
	{"webp", "webp", "image/webp", "riff-webp", []Pattern{{0, bs("RIFF"), nil}, {8, bs("WEBP"), nil}}, 26, 64 << 20},
	{"isobmff", "bmff", "video/mp4", "isobmff", []Pattern{{4, bs("ftyp"), nil}}, 24, 4 << 30},
	{"sqlite", "sqlite", "application/vnd.sqlite3", "sqlite", []Pattern{{0, bs("SQLite format 3\x00"), nil}}, 512, 1 << 30},
	{"bplist", "plist", "application/x-bplist", "bplist", []Pattern{{0, bs("bplist00"), nil}}, 42, 16 << 20},
	{"zip", "zip", "application/zip", "zip", []Pattern{{0, bs("PK\x03\x04"), nil}}, 64, 1 << 30},
	{"pdf", "pdf", "application/pdf", "pdf", []Pattern{{0, bs("%PDF-"), nil}}, 32, 512 << 20},
	{"gzip", "gz", "application/gzip", "gzip", []Pattern{{0, []byte{0x1F, 0x8B, 0x08}, nil}}, 18, 64 << 20},
	{"wav", "wav", "audio/wav", "riff-wave", []Pattern{{0, bs("RIFF"), nil}, {8, bs("WAVE"), nil}}, 44, 1 << 30},
	{"ogg", "ogg", "audio/ogg", "ogg", []Pattern{{0, bs("OggS\x00"), nil}}, 28, 256 << 20},
}

func TestBuiltinSetShape(t *testing.T) {
	s := Builtin()
	if len(s.Types) != len(wantTable) {
		t.Fatalf("%d types, want %d", len(s.Types), len(wantTable))
	}
	for i, w := range wantTable {
		got := s.Types[i]
		if got.Name != w.name || got.Ext != w.ext || got.MIME != w.mime || got.Validate != w.validate {
			t.Errorf("type %d = %q %q %q %q, want %q %q %q %q", i, got.Name, got.Ext, got.MIME, got.Validate, w.name, w.ext, w.mime, w.validate)
		}
		if got.MinLen != w.min || got.MaxLen != w.max {
			t.Errorf("%s: min/max = %d/%d, want %d/%d", w.name, got.MinLen, got.MaxLen, w.min, w.max)
		}
		if !reflect.DeepEqual(got.Headers, w.headers) {
			t.Errorf("%s: headers = %+v, want %+v", w.name, got.Headers, w.headers)
		}
		if got.Align != "block" || got.Footer != nil {
			t.Errorf("%s: align %q footer %x, want block and no footer", w.name, got.Align, got.Footer)
		}
	}
	if err := s.Check(); err != nil {
		t.Errorf("Builtin().Check() = %v", err)
	}
	for _, ty := range s.Types {
		if _, ok := builtinValidators()[ty.Validate]; !ok {
			t.Errorf("%s names validator %q that the registry lacks", ty.Name, ty.Validate)
		}
	}
	if len(builtinValidators()) != 12 {
		t.Errorf("registry has %d validators, want 12", len(builtinValidators()))
	}
}

func TestBuiltinSigsetDigestPinned(t *testing.T) {
	s := Builtin()
	if s.ID != BuiltinID {
		t.Errorf("ID = %q, want %q", s.ID, BuiltinID)
	}
	if got := s.Digest(); got != wantBuiltinDigest {
		t.Errorf("Digest() = %s, want %s (a table change must also change BuiltinID)", got, wantBuiltinDigest)
	}
	if len(s.Digest()) != 64 || strings.ToLower(s.Digest()) != s.Digest() {
		t.Errorf("digest %q is not lowercase hex SHA-256", s.Digest())
	}
}

func TestCanonicalIsOrderIndependent(t *testing.T) {
	s := Builtin()
	sh := Builtin()
	slices.Reverse(sh.Types)
	if !bytes.Equal(s.Canonical(), sh.Canonical()) || s.Digest() != sh.Digest() {
		t.Error("canonical form or digest depends on the order of the types")
	}
	if !json.Valid(s.Canonical()) {
		t.Error("canonical form is not JSON")
	}
	// bytes are lowercase hex, and a change anywhere changes the digest
	if !bytes.Contains(s.Canonical(), bs(`"ffd8ff"`)) {
		t.Errorf("jpeg header is not lowercase hex in %s", s.Canonical())
	}
	mod := Builtin()
	mod.Types[0].MaxLen++
	if mod.Digest() == s.Digest() {
		t.Error("a changed MaxLen leaves the digest unchanged")
	}
	mod = Builtin()
	mod.Types[3].Headers[1].Mask = []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if mod.Digest() == s.Digest() {
		t.Error("a changed mask leaves the digest unchanged")
	}
}

func TestSetCheckRefuses(t *testing.T) {
	good := func() Type {
		return Type{Name: "toy", Ext: "toy", MIME: "x/toy", Headers: []Pattern{{0, bs("TOY"), nil}}, MinLen: 1, MaxLen: 100, Align: "block"}
	}
	cases := map[string]func(*Type){
		"MaxLen 0":               func(ty *Type) { ty.MaxLen = 0 },
		"MaxLen over MaxTypeLen": func(ty *Type) { ty.MaxLen = MaxTypeLen + 1 },
		"MinLen above MaxLen":    func(ty *Type) { ty.MinLen = 101 },
		"MinLen negative":        func(ty *Type) { ty.MinLen = -1 },
		"no headers":             func(ty *Type) { ty.Headers = nil },
		"five headers": func(ty *Type) {
			ty.Headers = slices.Repeat([]Pattern{{0, bs("T"), nil}}, MaxHeaders+1)
		},
		"pattern of 33 bytes": func(ty *Type) { ty.Headers = []Pattern{{0, bytes.Repeat(bs("T"), MaxPatternLen+1), nil}} },
		"empty pattern":       func(ty *Type) { ty.Headers = []Pattern{{0, nil, nil}} },
		"offset 513":          func(ty *Type) { ty.Headers = []Pattern{{MaxPatternOffset + 1, bs("T"), nil}} },
		"offset negative":     func(ty *Type) { ty.Headers = []Pattern{{-1, bs("T"), nil}} },
		"mask length differs": func(ty *Type) { ty.Headers = []Pattern{{0, bs("TOY"), []byte{0xFF}}} },
		"unknown validator":   func(ty *Type) { ty.Validate = "tar" },
		"uppercase name":      func(ty *Type) { ty.Name = "Bad" },
		"empty name":          func(ty *Type) { ty.Name = "" },
		"name of 33 chars":    func(ty *Type) { ty.Name = strings.Repeat("a", 33) },
		"ext with a dot":      func(ty *Type) { ty.Ext = "j.pg" },
		"empty ext":           func(ty *Type) { ty.Ext = "" },
		"empty MIME":          func(ty *Type) { ty.MIME = "" },
		"footer too long":     func(ty *Type) { ty.Footer = bytes.Repeat(bs("F"), MaxPatternLen+1) },
		"align page":          func(ty *Type) { ty.Align = "page" },
		"align empty":         func(ty *Type) { ty.Align = "" },
	}
	if err := (&Set{ID: "x", Types: []Type{good()}}).Check(); err != nil {
		t.Fatalf("the good type is refused: %v", err)
	}
	for name, mut := range cases {
		ty := good()
		mut(&ty)
		if err := (&Set{ID: "x", Types: []Type{ty}}).Check(); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Check() = %v, want ErrBadSignature", name, err)
		}
	}
	if err := (&Set{ID: "x", Types: []Type{good(), good()}}).Check(); !errors.Is(err, ErrBadSignature) {
		t.Errorf("duplicate name: Check() = %v, want ErrBadSignature", err)
	}
	if err := (&Set{Types: []Type{good()}}).Check(); !errors.Is(err, ErrBadSignature) {
		t.Errorf("empty set id: Check() = %v, want ErrBadSignature", err)
	}
}

func TestSelectTypes(t *testing.T) {
	names := func(s *Set) []string {
		var out []string
		for _, ty := range s.Types {
			out = append(out, ty.Name)
		}
		return out
	}
	s := Builtin()
	got, filter, err := s.Select([]string{"mp4"})
	if err != nil || !slices.Equal(names(got), []string{"isobmff"}) || !reflect.DeepEqual(filter, map[string][]string{"isobmff": {"mp4"}}) {
		t.Errorf("Select(mp4) = %v, %v, %v", names(got), filter, err)
	}
	got, filter, err = s.Select([]string{"jpeg", "heic"})
	if err != nil || !slices.Equal(names(got), []string{"jpeg", "isobmff"}) || !reflect.DeepEqual(filter, map[string][]string{"isobmff": {"heic"}}) {
		t.Errorf("Select(jpeg,heic) = %v, %v, %v", names(got), filter, err)
	}
	if _, ok := filter["jpeg"]; ok {
		t.Error("an unrefined family has a filter entry")
	}
	got, filter, err = s.Select(nil)
	if err != nil || len(got.Types) != len(s.Types) || len(filter) != 0 {
		t.Errorf("Select(nil) = %d types, %v, %v", len(got.Types), filter, err)
	}
	// a family name keeps the whole family, even beside one of its refined names
	got, filter, err = s.Select([]string{"zip", "docx"})
	if err != nil || !slices.Equal(names(got), []string{"zip"}) || len(filter) != 0 {
		t.Errorf("Select(zip,docx) = %v, %v, %v", names(got), filter, err)
	}
	got, filter, err = s.Select([]string{"docx", "apk"})
	if err != nil || !slices.Equal(names(got), []string{"zip"}) || !reflect.DeepEqual(filter, map[string][]string{"zip": {"apk", "docx"}}) {
		t.Errorf("Select(docx,apk) = %v, %v, %v", names(got), filter, err)
	}
	for _, bad := range [][]string{{"tar"}, {"jpeg", "tar"}, {""}} {
		if _, _, err := s.Select(bad); !errors.Is(err, ErrUnknownType) {
			t.Errorf("Select(%v) err = %v, want ErrUnknownType", bad, err)
		}
	}
	// Select leaves its receiver alone
	if len(s.Types) != len(wantTable) {
		t.Error("Select changed the set")
	}
}

func TestRefinedTypes(t *testing.T) {
	want := map[string][]string{
		"isobmff": {"mp4", "mov", "3gp", "heic", "heif", "isobmff"},
		"zip":     {"zip", "apk", "jar", "docx", "xlsx", "pptx"},
	}
	if got := RefinedTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("RefinedTypes() = %v, want %v", got, want)
	}
	RefinedTypes()["zip"][0] = "x"
	if RefinedTypes()["zip"][0] != "zip" {
		t.Error("RefinedTypes returns shared state")
	}
}

func TestBuiltinReturnsFreshCopy(t *testing.T) {
	a := Builtin()
	a.Types[0].Headers[0].Bytes[0] = 0x00
	a.Types[0].Name = "changed"
	c := Builtin()
	if c.Types[0].Headers[0].Bytes[0] != 0xFF || c.Types[0].Name != "jpeg" {
		t.Error("Builtin() shares state between calls")
	}
}

func TestBuiltinValidatorsAreStubs(t *testing.T) {
	// Task 2 ships only the registry; each format task replaces its stub.
	w := newWindow(context.Background(), bytes.NewReader(make([]byte, 64)), 0, 64, unlimitedBudget())
	for id, v := range builtinValidators() {
		if id == "jpeg" { // implemented (Task 4)
			continue
		}
		if got := v(w); got.Outcome != Rejected || got.Reason != "no-validator" {
			t.Errorf("%s: %+v, want rejected no-validator", id, got)
		}
	}
}

func TestWindowRefusesOutside(t *testing.T) {
	w := newWindow(context.Background(), bytes.NewReader(make([]byte, 100)), 0, 10, unlimitedBudget())
	if w.Len() != 10 {
		t.Fatalf("Len() = %d", w.Len())
	}
	p := make([]byte, 4)
	n, err := w.ReadAt(p, 8)
	if n != 2 || !errors.Is(err, ErrOutsideWindow) {
		t.Errorf("ReadAt(8) = %d, %v, want 2, ErrOutsideWindow", n, err)
	}
	if w.outsideReads() != 1 {
		t.Errorf("outsideReads = %d, want 1", w.outsideReads())
	}
	if n, err := w.ReadAt(p, 10); n != 0 || !errors.Is(err, ErrOutsideWindow) {
		t.Errorf("ReadAt(10) = %d, %v", n, err)
	}
	if n, err := w.ReadAt(p, -1); n != 0 || !errors.Is(err, ErrOutsideWindow) {
		t.Errorf("ReadAt(-1) = %d, %v", n, err)
	}
	if _, err := w.Byte(10); !errors.Is(err, ErrOutsideWindow) {
		t.Errorf("Byte(10) err = %v", err)
	}
	if w.outsideReads() != 4 {
		t.Errorf("outsideReads = %d, want 4", w.outsideReads())
	}
}

func TestWindowReadsRelativeToTheHit(t *testing.T) {
	data := make([]byte, 300)
	for i := range data {
		data[i] = byte(i)
	}
	w := newWindow(context.Background(), bytes.NewReader(data), 100, 150, unlimitedBudget())
	p := make([]byte, 3)
	if n, err := w.ReadAt(p, 5); n != 3 || err != nil || p[0] != 105 || p[2] != 107 {
		t.Errorf("ReadAt = %d, %v, %v", n, err, p)
	}
	for _, off := range []int64{0, 149, 70} {
		if v, err := w.Byte(off); err != nil || v != byte(100+off) {
			t.Errorf("Byte(%d) = %d, %v", off, v, err)
		}
	}
	if w.outsideReads() != 0 {
		t.Errorf("outsideReads = %d, want 0", w.outsideReads())
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("boom") }

func TestWindowReadError(t *testing.T) {
	w := newWindow(context.Background(), failingReader{}, 0, 100, unlimitedBudget())
	if _, err := w.Byte(3); err == nil || errors.Is(err, ErrOutsideWindow) {
		t.Errorf("Byte over a failing reader: %v", err)
	}
	if _, err := w.ReadAt(make([]byte, 4), 0); err == nil || errors.Is(err, ErrOutsideWindow) {
		t.Errorf("ReadAt over a failing reader: %v", err)
	}
}

type countingReader struct {
	r     *bytes.Reader
	reads int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.r.ReadAt(p, off)
}

func TestWindowByteCachesAndChecksContext(t *testing.T) {
	cr := &countingReader{r: bytes.NewReader(make([]byte, 1<<20))}
	ctx, cancel := context.WithCancel(context.Background())
	w := newWindow(ctx, cr, 0, 1<<20, unlimitedBudget())
	for i := range int64(windowCacheSize) {
		if _, err := w.Byte(i); err != nil {
			t.Fatal(err)
		}
	}
	if cr.reads != 1 {
		t.Errorf("%d reads for %d sequential bytes, want 1 (a fixed %d-byte cache)", cr.reads, windowCacheSize, windowCacheSize)
	}
	cancel()
	// a cached byte needs no refill; the next refill must notice the cancel
	if _, err := w.Byte(0); err != nil {
		t.Errorf("cached byte after cancel: %v", err)
	}
	if _, err := w.Byte(windowCacheSize); !errors.Is(err, context.Canceled) {
		t.Errorf("refill after cancel: %v, want context.Canceled", err)
	}
	if cr.reads != 1 {
		t.Errorf("a cancelled refill still read: %d reads", cr.reads)
	}
}

func TestWindowBudgetCharged(t *testing.T) {
	bud := &budget{limit: 100}
	w := newWindow(context.Background(), bytes.NewReader(make([]byte, 1000)), 0, 1000, bud)
	if _, err := w.ReadAt(make([]byte, 60), 0); err != nil {
		t.Fatal(err)
	}
	if bud.spent != 60 {
		t.Errorf("spent = %d, want 60", bud.spent)
	}
	n, err := w.ReadAt(make([]byte, 60), 60)
	if n != 0 || !errors.Is(err, ErrValidateBudget) {
		t.Errorf("over budget: %d, %v, want 0, ErrValidateBudget", n, err)
	}
	if bud.spent != 60 || !bud.exhausted {
		t.Errorf("spent %d exhausted %v after a refused read, want 60 true", bud.spent, bud.exhausted)
	}
	if _, err := w.Byte(500); !errors.Is(err, ErrValidateBudget) {
		t.Errorf("Byte refill over budget: %v", err)
	}
}

// unlimitedBudget is the explicit test-only way to a window with no read cap.
func unlimitedBudget() *budget { return &budget{limit: math.MaxInt64} }

func TestNilBudgetFailsClosed(t *testing.T) {
	w := newWindow(context.Background(), bytes.NewReader(make([]byte, 100)), 0, 100, nil)
	if n, err := w.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, ErrValidateBudget) {
		t.Errorf("ReadAt with a nil budget = %d, %v, want 0, ErrValidateBudget", n, err)
	}
	if _, err := w.Byte(0); !errors.Is(err, ErrValidateBudget) {
		t.Errorf("Byte with a nil budget: %v, want ErrValidateBudget", err)
	}
}

func TestNewBudgetClamps(t *testing.T) {
	if b := newBudget(MaxValidateBudget + 1); b.limit != MaxValidateBudget {
		t.Errorf("limit above the ceiling = %d, want %d", b.limit, MaxValidateBudget)
	}
	if b := newBudget(math.MaxInt64); b.limit != MaxValidateBudget {
		t.Errorf("huge limit = %d, want %d", b.limit, MaxValidateBudget)
	}
	if b := newBudget(-5); b.limit != 0 {
		t.Errorf("negative limit = %d, want 0", b.limit)
	}
	if b := newBudget(1000); b.limit != 1000 || b.spent != 0 {
		t.Errorf("newBudget(1000) = %+v", b)
	}
}
