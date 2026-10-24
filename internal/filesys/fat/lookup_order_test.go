package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// longEnts returns the raw entries of a file whose long name is units (up to 13
// units: one long-name entry) and whose 8.3 name is short.
func longEnts(units []uint16, short string, cluster uint32) []rawEnt {
	sum := lfnSum(short11(short))
	return []rawEnt{mkLFN(0x41, sum, units), mkShort(short, 0x20, cluster, 0)}
}

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

func rootWith(t *testing.T, groups ...[]rawEnt) *fat.FS {
	t.Helper()
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := build(o)
	var all []rawEnt
	for _, grp := range groups {
		all = append(all, grp...)
	}
	setRoot(img, g, all...)
	return openImg(t, img)
}

func wantAmbiguous(t *testing.T, what string, err error, contains ...string) {
	t.Helper()
	if !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("%s: %v, want filesys.ErrAmbiguous", what, err)
		return
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("%s: %q lacks %q", what, err, c)
		}
	}
}

// Pins existing behaviour (mutation: make filesys.RawNameAlias pad).
func TestRawNameDisplayIsTheSharedAlias(t *testing.T) {
	f := rootWith(t,
		longEnts([]uint16{'a', 0xD800, 'b'}, "ONE     TXT", 0),
		longEnts([]uint16{0xD800}, "TWO     TXT", 0))
	es := readDir(t, f, "/")
	if len(es) != 2 || es[0].Name != "~raw~YQAA2GIA" || es[1].Name != "~raw~ANg" {
		t.Fatalf("names %v, want the literal aliases", es)
	}
	for _, e := range es {
		if got, err := f.Lookup("/" + e.Name); err != nil || got.ID != e.ID {
			t.Errorf("Lookup(%q) = %v, %v", e.Name, got.ID, err)
		}
	}
}

func TestLookupAliasRejectsNonCanonicalSpelling(t *testing.T) {
	f := rootWith(t, longEnts([]uint16{0xD800}, "TWO     TXT", 0))
	if _, err := f.Lookup("/~raw~ANg"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Lookup("/~raw~ANh"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("non-canonical alias: %v, want ErrNotFound", err)
	}
}

func TestLookupAliasNeverStandsForAPlainName(t *testing.T) {
	raw := []byte("\x90T\x90     TXT")
	f := rootWith(t, []rawEnt{mkShort(string(raw), 0x20, 0, 0)})
	es := readDir(t, f, "/")
	if len(es) != 1 || es[0].Name != "ÉTÉ.TXT" {
		t.Fatalf("listing %v", es)
	}
	if _, err := f.Lookup("/" + filesys.RawNameAlias(raw)); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("alias of a plainly shown name: %v, want ErrNotFound", err)
	}
	if e, err := f.Lookup("/ÉTÉ.TXT"); err != nil || e.ID != es[0].ID {
		t.Errorf("Lookup(ÉTÉ.TXT) = %v, %v", e.ID, err)
	}
}

func TestLookupStoredNameThatIsAnotherRecordsAliasIsAmbiguous(t *testing.T) {
	stored := longEnts(u16("~raw~ANg"), "STORED  TXT", 0)
	aliased := longEnts([]uint16{0xD800}, "ALIASED TXT", 0)
	for name, groups := range map[string][][]rawEnt{"stored first": {stored, aliased}, "alias first": {aliased, stored}} {
		f := rootWith(t, groups...)
		_, err := f.Lookup("/~raw~ANg")
		wantAmbiguous(t, name, err, "~raw~ANg")
	}
}

func TestLookupTwoAliasRecordsAreAmbiguous(t *testing.T) {
	f := rootWith(t,
		longEnts([]uint16{0xD800}, "ONE     TXT", 0),
		longEnts([]uint16{0xD800}, "TWO     TXT", 0))
	_, err := f.Lookup("/~raw~ANg")
	wantAmbiguous(t, "two alias records", err, "2 entries")
}

func TestLookupTwoExactRecordsAreAmbiguous(t *testing.T) {
	f := rootWith(t,
		longEnts(u16("dup.txt"), "ONE     TXT", 0),
		longEnts(u16("dup.txt"), "TWO     TXT", 0))
	_, err := f.Lookup("/dup.txt")
	wantAmbiguous(t, "two exact records", err, "2 entries")
}

func TestLookupFoldAmbiguousIsAnError(t *testing.T) {
	f := rootWith(t,
		longEnts(u16("abc.txt"), "ABC     TXT", 0),
		longEnts(u16("ABC.txt"), "ABC~1   TXT", 0))
	es := readDir(t, f, "/")
	_, err := f.Lookup("/Abc.TXT")
	wantAmbiguous(t, "fold", err, `"abc.txt"`, `"ABC.txt"`)
	for _, e := range es {
		if got, err := f.Lookup("/" + e.Name); err != nil || got.ID != e.ID {
			t.Errorf("exact Lookup(%q) = %v, %v", e.Name, got.ID, err)
		}
	}
}

func TestLookupExactAfterFoldHitsWins(t *testing.T) {
	f := rootWith(t,
		longEnts(u16("abc.txt"), "ONE     TXT", 0),
		longEnts(u16("aBc.txt"), "TWO     TXT", 0),
		longEnts(u16("ABC.txt"), "THREE   TXT", 0))
	es := readDir(t, f, "/")
	if got, err := f.Lookup("/ABC.txt"); err != nil || got.ID != es[2].ID {
		t.Errorf("Lookup(ABC.txt) = %v, %v; want the third entry", got.ID, err)
	}
}

func TestLookupAliasAfterFoldHitsWins(t *testing.T) {
	f := rootWith(t,
		longEnts(u16("~RAW~anG"), "ONE     TXT", 0),
		longEnts(u16("~raw~Ang"), "TWO     TXT", 0),
		longEnts([]uint16{0xD800}, "THREE   TXT", 0))
	es := readDir(t, f, "/")
	if got, err := f.Lookup("/~raw~ANg"); err != nil || got.ID != es[2].ID {
		t.Errorf("Lookup(alias) = %v, %v; want the aliased entry", got.ID, err)
	}
}

// Pins existing behaviour (mutation: match only the long name).
func TestLookupShortNameIsAnExactName(t *testing.T) {
	f := rootWith(t, longEnts(u16("a long name.txt"), "ALONGN~1TXT", 0),
		longEnts(u16("alongn~1.txt"), "OTHER   TXT", 0)) // a case-variant of the 8.3 name
	es := readDir(t, f, "/")
	if got, err := f.Lookup("/ALONGN~1.TXT"); err != nil || got.ID != es[0].ID {
		t.Errorf("Lookup(8.3 name) = %v, %v", got.ID, err)
	}
}

func TestLookupExactStandsOnACutScan(t *testing.T) {
	img, _, _ := cutDirImage(t, 6)
	f := openImg(t, img)
	f.SetDirEntryCap(5) // F00, F01, F02 are read
	if _, err := f.Lookup("/D/F02.TXT"); err != nil {
		t.Errorf("exact name before the cut: %v", err)
	}
}

func TestLookupFoldHitOnACutScanIsIncomplete(t *testing.T) {
	img, _, _ := cutDirImage(t, 6)
	f := openImg(t, img)
	f.SetDirEntryCap(5)
	_, err := f.Lookup("/D/f00.txt")
	wantIncomplete(t, "fold hit on a cut scan", err, true)
}

// A correctness test, not an allocation claim.
func TestLookupManyFoldCollisionsIsAmbiguous(t *testing.T) {
	const n = 4000
	o := fattest.Options{Type: 32}
	g := fattest.Layout(o)
	img := build(o)
	variant := func(mask int) string {
		b := []byte("foldcollider")
		for i := range b {
			if mask&(1<<i) != 0 {
				b[i] -= 'a' - 'A'
			}
		}
		return string(b)
	}
	var ents []rawEnt
	for i := range n {
		ents = append(ents, longEnts(u16(variant(i)), fmt.Sprintf("V%04d   TXT", i), 0)...)
	}
	cs := g.SectorSize * g.SecPerClus
	clusters := (len(ents)*32 + cs - 1) / cs
	for c := range clusters {
		cl := uint32(c + 2)
		next := cl + 1
		if c == clusters-1 {
			next = 0x0FFFFFFF
		}
		setFATs(img, g, cl, next)
	}
	off := g.ClusterOffset(2)
	for i, e := range ents {
		copy(img[off+int64(i)*32:], e)
	}
	f := openImg(t, img)
	_, err := f.Lookup("/FOLDCOLLIDER")
	if err == nil || !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"4000", `"` + variant(0) + `"`, `"` + variant(1) + `"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	want := variant(1234)
	e, err := f.Lookup("/" + want)
	if err != nil || e.Name != want {
		t.Errorf("exact Lookup(%q) = %q, %v", want, e.Name, err)
	}
}

func TestFATLookupResolvesEveryOraclePath(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		if strings.Contains(name, "recover") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			img := gunzipFixture(t, path)
			fsys, err := fat.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			live, _ := walkAll(t, fsys)
			for p, want := range live {
				if p == "/" {
					continue
				}
				for _, q := range []string{p, strings.ToUpper(p)} {
					got, err := fsys.Lookup(q)
					if err != nil || got.ID != want.ID {
						t.Errorf("Lookup(%q) = %q, %v; want %q", q, got.ID, err, want.ID)
					}
				}
			}
		})
	}
}
