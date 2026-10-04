package hfsplus_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

// catKey encodes a catalog key without its keyLength prefix.
func catKey(parent uint32, name []uint16) []byte {
	b := make([]byte, 6+2*len(name))
	be.PutUint32(b, parent)
	be.PutUint16(b[4:], uint16(len(name)))
	for i, u := range name {
		be.PutUint16(b[6+2*i:], u)
	}
	return b
}

// putFork encodes an HFSPlusForkData by hand.
func putForkBytes(b []byte, logical uint64, clump, blocks uint32, exts ...[2]uint32) {
	be.PutUint64(b, logical)
	be.PutUint32(b[8:], clump)
	be.PutUint32(b[12:], blocks)
	for i, e := range exts {
		be.PutUint32(b[16+8*i:], e[0])
		be.PutUint32(b[20+8*i:], e[1])
	}
}

func TestCatalogRecordsDecode(t *testing.T) {
	t.Run("folder", func(t *testing.T) {
		b := make([]byte, 88)
		be.PutUint16(b[0:], 1)
		be.PutUint16(b[2:], 0x0102)    // flags
		be.PutUint32(b[4:], 7)         // valence
		be.PutUint32(b[8:], 0x1234)    // folderID
		be.PutUint32(b[12:], 1000)     // createDate
		be.PutUint32(b[16:], 2000)     // contentModDate
		be.PutUint32(b[20:], 3000)     // attributeModDate
		be.PutUint32(b[24:], 4000)     // accessDate
		be.PutUint32(b[28:], 5000)     // backupDate
		be.PutUint32(b[32:], 501)      // ownerID
		be.PutUint32(b[36:], 20)       // groupID
		b[40], b[41] = 0x11, 0x22      // adminFlags, ownerFlags
		be.PutUint16(b[42:], 0o40750)  // fileMode
		be.PutUint32(b[44:], 99)       // special
		be.PutUint32(b[80:], 0xABCD01) // textEncoding
		r, unknown, err := hfsplus.DecodeCatalogRecord(b)
		if err != nil || unknown {
			t.Fatalf("decode: %v unknown=%v", err, unknown)
		}
		want := hfsplus.CatRec{
			Type: 1, Flags: 0x0102, ID: 0x1234, Valence: 7, Create: 1000, ContentMod: 2000, AttrMod: 3000, Access: 4000, Backup: 5000,
			Owner: 501, Group: 20, AdminFlags: 0x11, OwnerFlags: 0x22, Mode: 0o40750, Special: 99, TextEnc: 0xABCD01,
		}
		if r.Name != nil {
			t.Errorf("a folder has no name field: %v", r.Name)
		}
		r.Name = nil
		if !reflect.DeepEqual(r, want) {
			t.Errorf("folder = %+v\nwant     %+v", r, want)
		}
	})
	t.Run("file", func(t *testing.T) {
		b := make([]byte, 248)
		be.PutUint16(b[0:], 2)
		be.PutUint16(b[2:], 0x0002)
		be.PutUint32(b[8:], 77) // fileID
		for i, off := range []int{12, 16, 20, 24, 28} {
			be.PutUint32(b[off:], uint32(100+i))
		}
		be.PutUint32(b[32:], 0xFFFFFFFE)
		be.PutUint32(b[36:], 80)
		b[40], b[41] = 0x01, 0x20
		be.PutUint16(b[42:], 0o100644)
		be.PutUint32(b[44:], 5)
		copy(b[48:], "TEXT")
		copy(b[52:], "ttxt")
		be.PutUint16(b[56:], 0x4001)
		be.PutUint32(b[80:], 3)
		putForkBytes(b[88:], 12345, 65536, 4, [2]uint32{10, 3}, [2]uint32{20, 1})
		putForkBytes(b[168:], 99, 4096, 1, [2]uint32{30, 1})
		r, _, err := hfsplus.DecodeCatalogRecord(b)
		if err != nil {
			t.Fatal(err)
		}
		want := hfsplus.CatRec{
			Type: 2, Flags: 2, ID: 77, Create: 100, ContentMod: 101, AttrMod: 102, Access: 103, Backup: 104,
			Owner: 0xFFFFFFFE, Group: 80, AdminFlags: 1, OwnerFlags: 0x20, Mode: 0o100644, Special: 5,
			FileType: 0x54455854, FileCreator: 0x74747874, FinderFlags: 0x4001, TextEnc: 3,
			DataLogical: 12345, DataClump: 65536, DataBlocks: 4, RsrcLogical: 99, RsrcClump: 4096, RsrcBlocks: 1,
		}
		want.DataExtents[0], want.DataExtents[1] = [2]uint32{10, 3}, [2]uint32{20, 1}
		want.RsrcExtents[0] = [2]uint32{30, 1}
		if !reflect.DeepEqual(r, want) {
			t.Errorf("file = %+v\nwant   %+v", r, want)
		}
	})
	t.Run("thread kinds", func(t *testing.T) {
		for _, typ := range []uint16{3, 4} {
			name := units("Café ☃ 😀")
			b := make([]byte, 10+2*len(name))
			be.PutUint16(b[0:], typ)
			be.PutUint16(b[2:], 0xFFFF) // reserved: ignored
			be.PutUint32(b[4:], 0x00C0FFEE)
			be.PutUint16(b[8:], uint16(len(name)))
			for i, u := range name {
				be.PutUint16(b[10+2*i:], u)
			}
			r, _, err := hfsplus.DecodeCatalogRecord(append(b, 0xEE, 0xEE)) // trailing bytes are ignored
			if err != nil || r.Type != int16(typ) || r.Parent != 0xC0FFEE || !slices.Equal(r.Name, name) {
				t.Errorf("thread %d = %+v, %v", typ, r, err)
			}
		}
	})
	t.Run("empty thread name", func(t *testing.T) {
		b := make([]byte, 10)
		be.PutUint16(b[0:], 3)
		if r, _, err := hfsplus.DecodeCatalogRecord(b); err != nil || len(r.Name) != 0 {
			t.Errorf("thread = %+v, %v", r, err)
		}
	})
	t.Run("key", func(t *testing.T) {
		name := units("Zoë")
		p, n, err := hfsplus.DecodeCatalogKey(catKey(0xDEADBEEF, name))
		if err != nil || p != 0xDEADBEEF || !slices.Equal(n, name) {
			t.Errorf("key = %d %v %v", p, n, err)
		}
		// Index keys may be padded to the tree's maximum: trailing bytes after the name are fine.
		k := append(catKey(5, units("x")), make([]byte, 40)...)
		if p, n, err := hfsplus.DecodeCatalogKey(k); err != nil || p != 5 || !slices.Equal(n, units("x")) {
			t.Errorf("padded key = %d %v %v", p, n, err)
		}
	})
}

func TestCatalogKeyHostile(t *testing.T) {
	name255 := make([]uint16, 255)
	for i := range name255 {
		name255[i] = 'a'
	}
	if _, n, err := hfsplus.DecodeCatalogKey(catKey(2, name255)); err != nil || len(n) != 255 {
		t.Errorf("255-unit name: %d units, %v", len(n), err)
	}
	keys := map[string][]byte{
		"empty":                   nil,
		"1 byte":                  {0},
		"5 bytes (keyLength < 6)": make([]byte, 5),
		"nameLen 256":             append(catKey(2, name255), 0, 0), // length patched below
		"nameLen 0xFFFF":          catKey(2, units("ab")),
		"name past the key":       catKey(2, units("abc"))[:10],
		"name one unit short":     catKey(2, units("abc"))[:11],
	}
	be.PutUint16(keys["nameLen 256"][4:], 256)
	be.PutUint16(keys["nameLen 0xFFFF"][4:], 0xFFFF)
	for name, k := range keys {
		if _, _, err := hfsplus.DecodeCatalogKey(k); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v, want a corrupt error", name, err)
		}
	}

	rec := func(typ uint16, n int) []byte {
		b := make([]byte, n)
		if n >= 2 {
			be.PutUint16(b, typ)
		}
		return b
	}
	short := map[string][]byte{
		"no type":                  rec(0, 1),
		"empty":                    nil,
		"folder of 87 bytes":       rec(1, 87),
		"file of 247 bytes":        rec(2, 247),
		"folder thread of 9 bytes": rec(3, 9),
		"file thread of 2 bytes":   rec(4, 2),
	}
	th := rec(3, 20)
	be.PutUint16(th[8:], 6) // 6 units need 22 bytes
	short["thread name past the record"] = th
	th2 := rec(3, 10+2*256)
	be.PutUint16(th2[8:], 256)
	short["thread name of 256 units"] = th2
	for name, b := range short {
		if _, _, err := hfsplus.DecodeCatalogRecord(b); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v, want a corrupt error", name, err)
		}
	}
	for _, typ := range []uint16{0, 5, 9, 0x100, 0xFFFF} {
		if r, unknown, err := hfsplus.DecodeCatalogRecord(rec(typ, 300)); err != nil || !unknown {
			t.Errorf("type %d: %+v unknown=%v err=%v, want an unknown record", typ, r, unknown, err)
		}
	}

	// In a tree: a damaged key stops the scan with a corrupt error.
	files := []hfsplustest.File{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}
	img, lay := buildFiles(t, hfsplustest.Options{}, files)
	off := recOffsetOf(t, img, lay, 1, 2, "b")
	be.PutUint16(img[catOff(lay, 1, off+2+4):], 300) // nameLen of record "b"
	f := open(t, img)
	err := f.CatalogScan(func(uint32, string, hfsplus.CatRec) bool { return true })
	wantCorrupt(t, err)
}

// recOffsetOf finds the node-relative offset of the record keyed (parent, name)
// in a single-leaf node by reading its offset table and keys.
func recOffsetOf(t testing.TB, img []byte, lay *hfsplustest.Layout, node uint32, parent uint32, name string) int {
	t.Helper()
	nrec := int(be.Uint16(img[catOff(lay, node, 10):]))
	for i := range nrec {
		off := recOffset(img, lay, node, i)
		key := img[catOff(lay, node, off):]
		p := be.Uint32(key[2:])
		n := int(be.Uint16(key[6:]))
		u := make([]uint16, n)
		for j := range u {
			u[j] = be.Uint16(key[8+2*j:])
		}
		if p == parent && string(utf16.Decode(u)) == name {
			return off
		}
	}
	t.Fatalf("no record (%d, %q) in node %d", parent, name, node)
	return 0
}

func TestUnknownRecordTypeWarns(t *testing.T) {
	files := []hfsplustest.File{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}
	img, lay := buildFiles(t, hfsplustest.Options{}, files)
	off := recOffsetOf(t, img, lay, 1, 2, "b")
	kl := int(be.Uint16(img[catOff(lay, 1, off):]))
	putCat16(img, lay, 1, off+(2+kl+1)&^1, 9) // the data of record "b": type 9
	f := open(t, img)
	var names []string
	if err := f.CatalogScan(func(p uint32, name string, _ hfsplus.CatRec) bool {
		if p == 2 {
			names = append(names, name)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "a", "c"}; !slices.Equal(names, want) {
		t.Errorf("records under the root = %q, want %q (the unknown record is skipped, the scan goes on)", names, want)
	}
	w := f.Info().Warnings
	if len(w) != 1 || !strings.Contains(w[0], "unknown type 9") {
		t.Errorf("Warnings = %q", w)
	}
	if _, _, err := f.FindRecord(2, "c"); err != nil {
		t.Errorf("FindRecord(c) = %v", err)
	}
	if _, _, err := f.FindRecord(2, "b"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("FindRecord(b) = %v, want ErrNotFound", err)
	}
	// The thread of the damaged file is intact.
	if th, err := f.CatalogThread(17); err != nil || th.Type != 4 {
		t.Errorf("thread 17 = %+v, %v", th, err)
	}
}

func TestHFSTimeConversion(t *testing.T) {
	if ts := hfsplus.HFSTime(0); !ts.T.IsZero() || ts.ZoneKnown {
		t.Errorf("0 = %+v, want absent", ts)
	}
	ts := hfsplus.HFSTime(2082844800)
	if ts.T.Unix() != 0 || !ts.ZoneKnown || ts.T.Location() != time.UTC {
		t.Errorf("2082844800 = %+v, want Unix 0 in UTC", ts)
	}
	if got := hfsplus.HFSTime(2082844800 + 1700000000).T.Unix(); got != 1700000000 {
		t.Errorf("fixed date = %d", got)
	}
	if got := hfsplus.HFSTime(1).T; got.Unix() != -2082844799 || got.Year() != 1904 {
		t.Errorf("1 = %v", got)
	}
	latest := hfsplus.HFSTime(0xFFFFFFFF)
	if latest.T.Unix() != 0xFFFFFFFF-2082844800 || latest.T.Year() != 2040 || !latest.ZoneKnown {
		t.Errorf("0xFFFFFFFF = %v (Unix %d)", latest.T, latest.T.Unix())
	}
}

func TestFoldAndCompare(t *testing.T) {
	// Ignorable code units, built from numbers so the source stays readable.
	var (
		zwnj = string(rune(0x200c)) // zero-width non-joiner
		zwj  = string(rune(0x200d)) // zero-width joiner
		lre  = string(rune(0x202a)) // left-to-right embedding
		iss  = string(rune(0x206f)) // nominal digit shapes
		bom  = string(rune(0xfeff)) // zero-width no-break space
	)
	fold := func(s string) string { return string(utf16.Decode(hfsplus.Fold(units(s)))) }
	for in, want := range map[string]string{
		"ABC xyz 09":                "abc xyz 09",
		"ÀÉÎÕÜ ß ÿ":                 "àéîõü ß ÿ", // Latin-1
		"ΑΒΓ Σ Ω σ":                 "αβγ σ ω σ", // Greek
		"ДОМ Ёж":                    "дом ёж",    // Cyrillic
		"a" + zwnj + "b" + zwj:      "ab",        // zero-width joiners are ignorable
		"x" + lre + "y" + iss + "z": "xyz",
		bom + "name":                "name",
		"a\x00b":                    "a￿b", // U+0000 folds to 0xFFFF: not ignorable (unverified against a real image)
		"😀A":                        "😀a",  // surrogates pass through
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	if got := hfsplus.Fold([]uint16{0xD83D}); !slices.Equal(got, []uint16{0xD83D}) { // a lone surrogate
		t.Errorf("fold(lone surrogate) = %x", got)
	}
	in := units("ABC")
	hfsplus.Fold(in)
	if !slices.Equal(in, units("ABC")) {
		t.Error("Fold modified its input")
	}

	cmp := func(a, b string, binary bool) int { return hfsplus.CompareNames(units(a), units(b), binary) }
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, c := range []struct {
		a, b   string
		binary bool
		want   int
	}{
		{"Readme", "README", false, 0},
		{"readme", "README", true, 1}, // binary: 'r' (0x72) > 'R' (0x52)
		{"a", "B", false, -1},
		{"a", "B", true, 1}, // binary: 'a' (0x61) > 'B' (0x42)
		{"É", "é", false, 0},
		{"É", "é", true, -1},
		{"Σ", "σ", false, 0},
		{"ДОМ", "дом", false, 0},
		{"ab", "abc", false, -1}, // a prefix sorts first
		{"abc", "ab", true, 1},
		{"", "a", false, -1},
		{"", "", true, 0},
		{"a" + zwj + "b", "AB", false, 0}, // ignorable units do not count
		{"a" + zwj + "b", "ab", true, 1},  // ... but binary compares them
		{"z", "a" + zwj, false, 1},
		{"\x00", "z", false, 1},    // U+0000 sorts after every other unit ...
		{"a\x00b", "ab", false, 1}, // ... and is not skipped
		{"a\x00b", "ab", true, -1}, // binary: 0 < b
		// HFSX binary order is by UTF-16 unit value, not by code point: the
		// surrogate pair of U+1F600 (D83D DE00) sorts before U+FF21 (FF21).
		{"😀", "Ａ", true, -1},
		{"😀", "Ａ", false, -1},
		{"ﬁ", "z", true, 1},
	} {
		if got := sign(cmp(c.a, c.b, c.binary)); got != c.want {
			t.Errorf("compare(%q, %q, binary=%v) = %d, want %d", c.a, c.b, c.binary, got, c.want)
		}
		if got := sign(cmp(c.b, c.a, c.binary)); got != -c.want {
			t.Errorf("compare(%q, %q, binary=%v) = %d, want %d (antisymmetry)", c.b, c.a, c.binary, got, -c.want)
		}
	}
	// Keys order by parent first.
	if hfsplus.CompareKeys(1, units("z"), 2, units("a"), false) >= 0 || hfsplus.CompareKeys(3, nil, 2, units("zzz"), true) <= 0 {
		t.Error("keys must order by parent id before name")
	}
	if hfsplus.CompareKeys(2, units("A"), 2, units("a"), false) != 0 || hfsplus.CompareKeys(2, units("A"), 2, units("a"), true) >= 0 {
		t.Error("equal parents compare by name in the volume's mode")
	}
}

func TestVolumeNameFromRootThread(t *testing.T) {
	for _, label := range []string{"My Volume", "Ünïcode ☃ 😀", "", strings.Repeat("v", 255)} {
		for _, o := range []hfsplustest.Options{{Label: label}, {Label: label, HFSX: true, CaseSensitive: true}, {Label: label, Wrapper: true}} {
			img, _ := buildFiles(t, o, nil)
			info := open(t, img).Info()
			if info.Label != label || len(info.Warnings) != 0 {
				t.Errorf("%+v: Label = %q, Warnings = %q", o, info.Label, info.Warnings)
			}
		}
	}
	t.Run("no root thread", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{Label: "Lost"}, nil)
		off := recOffsetOf(t, img, lay, 1, 2, "")
		be.PutUint32(img[catOff(lay, 1, off+2):], 3) // the thread is keyed (3, ""): not the root's
		info := open(t, img).Info()
		if info.Label != "" || !hasWarning(info, "no thread record") {
			t.Errorf("Label = %q, Warnings = %q", info.Label, info.Warnings)
		}
	})
	t.Run("unreadable catalog", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{Label: "Lost"}, nil)
		img[catOff(lay, 1, 8)] = 9 // the only leaf has an unknown kind
		info := open(t, img).Info()
		if info.Label != "" || !hasWarning(info, "volume name") {
			t.Errorf("Label = %q, Warnings = %q", info.Label, info.Warnings)
		}
	})
}
