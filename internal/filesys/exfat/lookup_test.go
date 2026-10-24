package exfat_test

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

// aliasOf is the display form of a name, computed with the shared helper.
func aliasOf(units []uint16) string {
	var raw []byte
	for _, u := range units {
		raw = binary.LittleEndian.AppendUint16(raw, u)
	}
	return filesys.RawNameAlias(raw)
}

func lookupErr(f *exfat.FS, p string) error {
	_, err := f.Lookup(p)
	return err
}

// Pins existing behaviour: the display form is the shared alias.
func TestExfatRawNameDisplayIsTheSharedAlias(t *testing.T) {
	for _, c := range []struct {
		units []uint16
		want  string
	}{
		{[]uint16{'a', 0xD800, 'b'}, "~raw~YQAA2GIA"},
		{u16("."), "~raw~LgA"},
		{[]uint16{0xD800}, "~raw~ANg"},
	} {
		img := exfattest.Build(exfattest.Options{}, []exfattest.File{{Path: "/q", RawName: c.units, Data: []byte("x")}})
		f := openImg(t, img)
		es, err := f.ReadDir(f.Root())
		if err != nil || len(es) != 1 || es[0].Name != c.want {
			t.Fatalf("ReadDir = %+v, %v; want the single name %q", es, err, c.want)
		}
		if got, err := f.Lookup("/" + c.want); err != nil || got.ID != es[0].ID {
			t.Errorf("Lookup(%q) = %+v, %v", c.want, got, err)
		}
	}
}

func TestExfatLookupAliasRejectsNonCanonicalSpelling(t *testing.T) {
	img := exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/p", RawName: []uint16{0xD800}, Data: []byte("x")},
		{Path: "/r", RawName: []uint16{'a'}, Data: []byte("y")},
	})
	f := openImg(t, img)
	for _, p := range []string{"/~raw~ANh", "/~raw~YQ"} { // trailing bits set; one byte (odd length)
		if err := lookupErr(f, p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if err := lookupErr(f, "/~raw~ANg"); err != nil {
		t.Errorf("canonical spelling: %v", err)
	}
}

func TestExfatLookupAliasNeverStandsForAPlainName(t *testing.T) {
	img := exfattest.Build(exfattest.Options{}, []exfattest.File{{Path: "/abc", Data: []byte("x")}})
	f := openImg(t, img)
	if err := lookupErr(f, "/abc"); err != nil {
		t.Fatal(err)
	}
	if err := lookupErr(f, "/"+aliasOf(u16("abc"))); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of the alias of a plain name = %v, want ErrNotFound", err)
	}
}

func TestExfatLookupStoredNameThatIsAnotherRecordsAliasIsAmbiguous(t *testing.T) {
	lone := []uint16{'a', 0xD800, 'b'}
	literal := aliasOf(lone)
	a := exfattest.File{Path: "/" + literal, Data: []byte("literal")}
	b := exfattest.File{Path: "/q", RawName: lone, Data: []byte("raw")}
	for name, files := range map[string][]exfattest.File{"literal first": {a, b}, "raw first": {b, a}} {
		t.Run(name, func(t *testing.T) {
			f := openImg(t, exfattest.Build(exfattest.Options{}, files))
			if err := lookupErr(f, "/"+literal); !errors.Is(err, filesys.ErrAmbiguous) {
				t.Errorf("Lookup(%q) = %v, want ErrAmbiguous", literal, err)
			}
		})
	}
}

func TestExfatLookupTwoAliasRecordsAreAmbiguous(t *testing.T) {
	lone := []uint16{'a', 0xD800, 'b'}
	f := openImg(t, exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/q1", RawName: lone, Data: []byte("1")}, {Path: "/q2", RawName: lone, Data: []byte("2")},
	}))
	if err := lookupErr(f, "/"+aliasOf(lone)); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("Lookup = %v, want ErrAmbiguous", err)
	}
}

func TestExfatLookupTwoExactRecordsAreAmbiguous(t *testing.T) {
	f := openImg(t, exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/dup", Data: []byte("1")}, {Path: "/x", RawName: u16("dup"), Data: []byte("2")},
	}))
	if err := lookupErr(f, "/dup"); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("Lookup = %v, want ErrAmbiguous", err)
	}
}

func TestExfatLookupFoldAmbiguousIsAnError(t *testing.T) {
	f := openImg(t, exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/abc", Data: []byte("lower")}, {Path: "/ABC", Data: []byte("upper")},
	}))
	if err := lookupErr(f, "/Abc"); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("Lookup(/Abc) = %v, want ErrAmbiguous", err)
	}
	for p, want := range map[string]string{"/abc": "lower", "/ABC": "upper"} {
		e, err := f.Lookup(p)
		if err != nil || string(readAll(t, f, e)) != want {
			t.Errorf("Lookup(%q) = %v, %v", p, e.Name, err)
		}
	}
}

func TestExfatLookupExactAfterFoldHitsWins(t *testing.T) {
	f := openImg(t, exfattest.Build(exfattest.Options{}, []exfattest.File{
		{Path: "/ABC", Data: []byte("fold1")}, {Path: "/aBc", Data: []byte("fold2")}, {Path: "/abc", Data: []byte("exact")},
	}))
	e, err := f.Lookup("/abc")
	if err != nil || string(readAll(t, f, e)) != "exact" {
		t.Errorf("Lookup(/abc) = %v, %v", e.Name, err)
	}
}

// N25: a correctness test, not an allocation claim.
func TestExfatLookupManyFoldCollisionsIsAmbiguous(t *testing.T) {
	const n = 4000
	spell := func(bits int) string {
		b := make([]byte, 12)
		for i := range b {
			b[i] = 'a'
			if bits>>i&1 != 0 {
				b[i] = 'A'
			}
		}
		return string(b)
	}
	files := make([]exfattest.File, n)
	for i := range files {
		files[i] = exfattest.File{Path: "/" + spell(i)}
	}
	f := openImg(t, exfattest.Build(exfattest.Options{ClusterCount: 512}, files))
	err := lookupErr(f, "/"+spell(4095)) // the one spelling the directory does not hold
	if !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"4000", spell(0), spell(1)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), spell(2)) {
		t.Errorf("error %q names a third candidate", err)
	}
}

func TestExfatLookupWithoutUpcaseTableIsUndecided(t *testing.T) {
	files := []exfattest.File{{Path: "/zebra", Data: []byte("z")}, {Path: "/Mixed.Name", Data: []byte("m")}}
	check := func(t *testing.T, f *exfat.FS) {
		t.Helper()
		if f.UpcaseLoaded() {
			t.Fatal("a table is in use")
		}
		if err := lookupErr(f, "/ZEBRA"); !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(/ZEBRA) = %v, want ErrUndecided", err)
		}
		if err := lookupErr(f, "/zebra"); err != nil {
			t.Errorf("exact: %v", err)
		}
		if err := lookupErr(f, "/zebras"); !errors.Is(err, filesys.ErrNotFound) { // no stored name of that length
			t.Errorf("Lookup(/zebras) = %v, want ErrNotFound", err)
		}
	}
	t.Run("absent", func(t *testing.T) {
		check(t, openImg(t, exfattest.Build(exfattest.Options{NoUpcase: true}, files)))
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
		root := lay.ClusterOffset(lay.RootCluster)
		patched := false
		for off := root; off < root+int64(lay.ClusterSize); off += 32 {
			if img[off] == 0x82 {
				img[off+4] ^= 0xFF
				patched = true
			}
		}
		if !patched {
			t.Fatal("no up-case entry in the root")
		}
		check(t, openImg(t, img))
	})
}

func TestExfatLookupTruncatedNameNeverMatches(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/aaaaaaaaaaaaaaa", Data: []byte("x")}})
	loc := lay.Find("/aaaaaaaaaaaaaaa", false)
	img[loc.Offset+32+3] = 20 // NameLength 20, one Name entry present (15 units)
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	if err := lookupErr(f, "/aaaaaaaaaaaaaaa"); !errors.Is(err, filesys.ErrUndecided) {
		t.Errorf("Lookup = %v, want ErrUndecided", err)
	}
}

func TestExfatLookupUnreadableNameMakesAMissUndecided(t *testing.T) {
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/lost", Data: []byte("x")}, {Path: "/kept", Data: []byte("y")},
	})
	loc := lay.Find("/lost", false)
	img[loc.Offset+32] = 0xC2 // not a Stream Extension any more
	exfattest.FixSetChecksum(img, loc.Offset)
	f := openImg(t, img)
	if err := lookupErr(f, "/missing"); !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/missing) = %v, want ErrUndecided", err)
	}
	if err := lookupErr(f, "/kept"); err != nil {
		t.Errorf("an exact name elsewhere: %v", err)
	}
}

// Pins existing behaviour (mutation: drop the fold step).
func TestExfatLookupResolvesEveryOraclePath(t *testing.T) {
	const path = "testdata/exfat.img.gz"
	want := loadOracle(t, path)
	f := openImg(t, gunzipFixture(t, path))
	folded := map[string]int{}
	for _, w := range want.Files {
		folded[strings.ToUpper(w.Path)]++
	}
	special := 0
	for _, w := range want.Files {
		e, err := f.Lookup(w.Path)
		if err != nil {
			t.Errorf("Lookup(%q): %v", w.Path, err)
			continue
		}
		up := strings.ToUpper(w.Path)
		if strings.ContainsAny(w.Path, "éö") {
			special++
		}
		got, err := f.Lookup(up)
		switch {
		case folded[up] > 1 && up != w.Path: // another oracle path folds to the same name
			if err == nil && got.ID != e.ID {
				t.Errorf("Lookup(%q) resolved to the wrong one of the names that fold together", up)
			}
		case err != nil:
			t.Errorf("Lookup(%q) (upper case of %q): %v", up, w.Path, err)
		case got.ID != e.ID:
			t.Errorf("Lookup(%q) = %q, want the entry of %q", up, got.ID, w.Path)
		}
	}
	if special == 0 {
		t.Error("the oracle has no path with an accented letter: the fixture lost its point")
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings: %v", f.Info().Warnings)
	}
}
