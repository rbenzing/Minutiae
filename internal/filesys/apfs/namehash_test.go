package apfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// Names in both normalizations, spelled with escapes so no editor can change
// them: NFC is the precomposed form, NFD the decomposed one.
const (
	cafeNFC    = "café.txt"
	cafeNFD    = "café.txt"
	hangulNFC  = "한국어.txt"
	hangulNFD  = "한국어.txt"
	angstNFD   = "Ångström.txt"
	angstNFC   = "Ångström.txt"
	mtavruli   = "Ა.txt" // Georgian Mtavruli, Unicode 11.0: newer than the tables APFS uses
	kelvinSign = "K.txt" // folds to "k"
)

// The stored name hash of a directory record is the one recipe on every name:
// CRC-32C of the UTF-32 of NFD(casefold(name)) without the NUL. The values are
// stored in the real image written by a kernel driver (the oracle's
// name_hash of apfs-populated), so they do not come from this reader.
func TestNameHashNonASCIIMatchesRealImage(t *testing.T) {
	for name, want := range map[string]uint32{
		cafeNFC:                2830574,
		cafeNFD:                2830574, // the same hash: NFD is applied first
		angstNFD:               2233291,
		"über.txt":            1260285,
		hangulNFC:              1815068,
		hangulNFD:              1815068,
		"Straße.txt":           2864421, // sharp s folds to "ss"
		"emoji-\U0001f600.txt": 751668,
		"snow-☃.txt":           2291827,
		"über-Ångström.txt":    2962100,
		"ünï-link":             609082,
		"İstanbul.txt":         2348171, // dotted capital I folds to "i" + U+0307
		"ǅ-title.txt":          4078858, // titlecase letter folds to its lower case
		"Ελληνικά.txt":         2754903,
		"документ.txt":         11979,
		"日本語":                  75645,
		"ファイル.txt":             639725,
	} {
		got, ok := apfs.NameHash([]byte(name), true)
		if !ok || got != want {
			t.Errorf("NameHash(%+q) = %d, %v; want %d, true", name, got, ok, want)
		}
	}
	// Case folding is Unicode's, not ASCII's: these fold to the same string.
	for _, pair := range [][2]string{
		{"ÉCOLE", "école"},
		{"Δ", "δ"},
		{kelvinSign, "k.txt"},
		{"STRASSE", "Straße"},
	} {
		a, aok := apfs.NameHash([]byte(pair[0]), true)
		b, bok := apfs.NameHash([]byte(pair[1]), true)
		if !aok || !bok || a != b {
			t.Errorf("hashes of %+q and %+q: %d,%v and %d,%v; want equal on a case-insensitive volume", pair[0], pair[1], a, aok, b, bok)
		}
	}
	// A case-sensitive volume does not fold, but still hashes NFD.
	a, _ := apfs.NameHash([]byte("É"), false)
	b, _ := apfs.NameHash([]byte("é"), false)
	if a == b {
		t.Error("a case-sensitive hash must not fold non-ASCII case")
	}
	c, _ := apfs.NameHash([]byte("é"), false)
	d, _ := apfs.NameHash([]byte("é"), false)
	if c != d {
		t.Errorf("NFC and NFD spellings hash to %d and %d on a case-sensitive volume, want equal", c, d)
	}
}

// A hash is only claimed for names the recipe is known to cover: valid UTF-8
// whose code points all existed in the Unicode version APFS froze its tables
// at. A newer character would be hashed by Apple's tables as itself, by this
// reader's as something else, so such a name is not verified (ok false).
func TestNameHashUnverifiableNames(t *testing.T) {
	for _, name := range []string{
		mtavruli,         // Unicode 11.0, has a case folding in newer tables
		"a" + mtavruli,   // one such rune is enough
		"\xff\xfe.txt",   // not UTF-8
		"café\xc3",       // a truncated sequence
		"\U0001fae0.txt", // melting face, Unicode 14.0
	} {
		if _, ok := apfs.NameHash([]byte(name), true); ok {
			t.Errorf("NameHash(%+q) is claimed verifiable, want ok false", name)
		}
	}
	// Adlam capital letter DELTA: new in Unicode 9.0 itself, so covered.
	if _, ok := apfs.NameHash([]byte("\U0001e900.txt"), true); !ok {
		t.Error("a Unicode 9.0 character must be verifiable")
	}
}

// Every name of every real fixture is listed with no name_hash=bad and no
// warning: the recipe reproduces all the hashes real kernels and mkapfs wrote.
func TestRealFixturesHaveNoNameHashFindings(t *testing.T) {
	for _, name := range []string{"apfs-ci", "apfs-cs", "apfs-multichunk", "apfs-populated"} {
		t.Run(name, func(t *testing.T) {
			orcSkipShort(t, name)
			img, _ := orcLoad(t, name)
			f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			names, bad := 0, 0
			var walk func(dir filesys.Entry)
			walk = func(dir filesys.Entry) {
				if seen[dir.ID] {
					return
				}
				seen[dir.ID] = true
				es, err := f.ReadDir(dir)
				if err != nil {
					t.Errorf("ReadDir(%s): %v", dir.Name, err)
					return
				}
				for _, e := range es {
					names++
					if v, ok := attr(e, "name_hash"); ok {
						bad++
						t.Errorf("%q: name_hash=%s", e.Name, v)
					}
					if e.Type == filesys.TypeDir {
						walk(e)
					}
				}
			}
			walk(f.Root())
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a real fixture (%d names, %d bad hashes): %q", names, bad, w)
			}
			if names == 0 {
				t.Error("no names were listed")
			}
		})
	}
}

// All 678 names of the populated fixture (a real kernel driver wrote them, NFC,
// NFD, Hangul, folds and 255-byte names included) have their stored hash
// recomputed by the reader's recipe; none may differ, and none may be skipped
// as unverifiable (the fixture holds no character newer than the tables).
func TestPopulatedFixtureNameHashesAllVerified(t *testing.T) {
	orcSkipShort(t, popFixture)
	_, exp := popLoad(t)
	checked, nonASCII := 0, 0
	for _, n := range exp.Live.Tree {
		if n.NameHash == nil {
			continue
		}
		base := n.Path[bytes.LastIndexByte([]byte(n.Path), '/')+1:]
		h, ok := apfs.NameHash([]byte(base), true)
		if !ok {
			t.Errorf("%+q: the reader cannot verify this name", n.Path)
			continue
		}
		checked++
		if utf8.RuneCountInString(base) != len(base) {
			nonASCII++
		}
		if h != *n.NameHash {
			t.Errorf("%+q: stored name hash %d, reader computes %d", n.Path, *n.NameHash, h)
		}
	}
	if checked != 678 || nonASCII < 15 {
		t.Errorf("%d names checked (%d non-ASCII), want 678 and at least 15", checked, nonASCII)
	}
}

// Lookup on the real normalization-insensitive volume finds a name whatever
// its normalization: an NFC query for an NFD-stored name and the reverse.
func TestLookupNormalizationInsensitiveRealImage(t *testing.T) {
	orcSkipShort(t, popFixture)
	img, exp := popLoad(t)
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	root := "/" + exp.Volume.Name
	stored := map[string]popNode{}
	for _, n := range exp.Live.Tree {
		stored[n.Path] = n
	}
	for _, tc := range []struct{ query, stored string }{
		{"/nfd/" + cafeNFC, "/nfd/" + cafeNFD},         // NFC query, NFD stored
		{"/unicode/" + cafeNFD, "/unicode/" + cafeNFC}, // NFD query, NFC stored
		{"/nfd/" + hangulNFC, "/nfd/" + hangulNFD},     // composed syllables, jamo stored
		{"/unicode/" + hangulNFD, "/unicode/" + hangulNFC},
		{"/nfd/" + angstNFC, "/nfd/" + angstNFD},
		{"/nfd/CAFÉ.TXT", "/nfd/" + cafeNFD}, // case and normalization together
		{"/NFD/CAFÉ.TXT", "/nfd/" + cafeNFD},
		{"/unicode/" + cafeNFC, "/unicode/" + cafeNFC}, // exact
		{"/nfd/" + cafeNFD, "/nfd/" + cafeNFD},         // exact
	} {
		want, ok := stored[tc.stored]
		if !ok {
			t.Fatalf("the oracle has no path %+q: the test table is wrong", tc.stored)
		}
		e, err := f.Lookup(root + tc.query)
		if err != nil {
			t.Errorf("Lookup(%+q): %v", tc.query, err)
			continue
		}
		if e.Name != want.Path[bytes.LastIndexByte([]byte(want.Path), '/')+1:] {
			t.Errorf("Lookup(%+q) = %+q, want the stored name of %+q", tc.query, e.Name, tc.stored)
		}
		if !strings.HasSuffix(e.ID, fmt.Sprintf(":%d", want.Inode)) {
			t.Errorf("Lookup(%+q) is %s, want inode %d", tc.query, e.ID, want.Inode)
		}
	}
	if _, err := f.Lookup(root + "/nfd/cafe.txt"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of an accent-less name = %v, want ErrNotFound (normalization is not accent stripping)", err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

// On a hashed volume the search of one name reads the records of one hash, not
// the whole directory.
func TestLookupUsesTheNameHashToSearch(t *testing.T) {
	orcSkipShort(t, popFixture)
	img, exp := popLoad(t)
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	root := "/" + exp.Volume.Name
	many := mustLookup(t, f, root+"/many")
	start := f.DirBudget()
	if _, err := f.ReadDir(many); err != nil {
		t.Fatal(err)
	}
	listing := start - f.DirBudget()
	if listing <= 0 {
		t.Fatalf("listing /many charged %d bytes of directory budget", listing)
	}
	for _, q := range []string{"entry-0300.txt", "ENTRY-0300.TXT"} {
		before := f.DirBudget()
		if e := mustLookup(t, f, root+"/many/"+q); e.Name != "entry-0300.txt" {
			t.Fatalf("Lookup(%s) = %q", q, e.Name)
		}
		// The directory is read through its own lookup too (root, many); what
		// matters is that the final component does not scan 600 records.
		if spent := before - f.DirBudget(); spent >= listing/2 {
			t.Errorf("Lookup(%s) charged %d bytes of directory budget, a full listing %d", q, spent, listing)
		}
	}
	if _, err := f.Lookup(root + "/many/entry-9999.txt"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of a missing name = %v, want ErrNotFound", err)
	}
}

func volWith(ci, ni, hashed bool, files ...apfstest.File) apfstest.Volume {
	v := dataVolume(files...)
	v.CaseInsensitive, v.NormInsensitive, v.HashedKeys = ci, ni, hashed
	return v
}

// A volume that is normalization-insensitive only (incompat 0x8: case
// sensitive) matches NFC and NFD spellings but never differs-in-case names.
func TestLookupNormalizationInsensitiveOnlyVolume(t *testing.T) {
	f, _ := openOpts(t, volOpts(volWith(false, true, true,
		apfstest.File{Path: "/" + cafeNFD, Data: []byte("1")},
		apfstest.File{Path: "/" + hangulNFC, Data: []byte("22")},
		apfstest.File{Path: "/Upper.txt", Data: []byte("333")},
	)))
	if e := mustLookup(t, f, "/Data/"+cafeNFC); e.Name != cafeNFD || e.Size != 1 {
		t.Errorf("NFC query = %+v", e)
	}
	if e := mustLookup(t, f, "/Data/"+hangulNFD); e.Name != hangulNFC || e.Size != 2 {
		t.Errorf("NFD query = %+v", e)
	}
	for _, q := range []string{"/Data/upper.txt", "/Data/CAFÉ.TXT"} {
		if _, err := f.Lookup(q); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%+q) on a case-sensitive volume = %v, want ErrNotFound", q, err)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
}

// A normalization-sensitive volume is unchanged: only the exact name (or its
// ~raw~ alias) is found.
func TestLookupNormalizationSensitiveVolumeIsExact(t *testing.T) {
	for _, hashed := range []bool{true, false} {
		f, _ := openOpts(t, volOpts(volWith(false, false, hashed,
			apfstest.File{Path: "/" + cafeNFD, Data: []byte("1")},
			apfstest.File{Path: "/" + cafeNFC, Data: []byte("22")},
		)))
		if e := mustLookup(t, f, "/Data/"+cafeNFD); e.Size != 1 {
			t.Errorf("hashed=%v: NFD query = %+v, want the NFD file", hashed, e)
		}
		if e := mustLookup(t, f, "/Data/"+cafeNFC); e.Size != 2 {
			t.Errorf("hashed=%v: NFC query = %+v, want the NFC file", hashed, e)
		}
		if _, err := f.Lookup("/Data/CAFÉ.TXT"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("hashed=%v: a case-different query = %v, want ErrNotFound", hashed, err)
		}
	}
	f, _ := openOpts(t, volOpts(volWith(false, false, true, apfstest.File{Path: "/" + cafeNFD, Data: []byte("1")})))
	if _, err := f.Lookup("/Data/" + cafeNFC); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("NFC query for an NFD name on a normalization-sensitive volume = %v, want ErrNotFound", err)
	}
}

// The order of preference stays exact, then display alias, then the
// normalization/case-insensitive match: a volume (hostile or damaged) holding
// both an NFD and an NFC spelling returns the one the query spells exactly.
func TestLookupPreferenceExactThenInsensitive(t *testing.T) {
	// Only the folded spelling is stored: found.
	f, _ := openOpts(t, volOpts(volWith(true, true, true,
		apfstest.File{Path: "/" + cafeNFD, Data: []byte("1")},
	)))
	if e := mustLookup(t, f, "/Data/"+cafeNFC); e.Name != cafeNFD {
		t.Errorf("= %+v", e)
	}
	// Both spellings stored (the stored hashes are equal, so the records sit
	// together): each query finds its own.
	f, _ = openOpts(t, volOpts(volWith(true, true, true,
		apfstest.File{Path: "/" + cafeNFD, Data: []byte("1")},
		apfstest.File{Path: "/" + cafeNFC, Data: []byte("22")},
	)))
	if e := mustLookup(t, f, "/Data/"+cafeNFD); e.Size != 1 {
		t.Errorf("NFD query = %+v, want the NFD file", e)
	}
	if e := mustLookup(t, f, "/Data/"+cafeNFC); e.Size != 2 {
		t.Errorf("NFC query = %+v, want the NFC file", e)
	}
	// A third spelling resolves to one of them, deterministically the first in key order.
	e := mustLookup(t, f, "/Data/CAFÉ.TXT")
	if e.Name != cafeNFC && e.Name != cafeNFD {
		t.Errorf("fold query = %+v", e)
	}
}

// A record whose stored hash is wrong is still found (as before: a bad hash
// never hides an entry), by the scan that follows the hash search.
func TestLookupFindsABadHashRecord(t *testing.T) {
	f, _ := openOpts(t, volOpts(volWith(true, true, true,
		apfstest.File{Path: "/" + cafeNFD, Data: []byte("1"), BadHash: true},
		apfstest.File{Path: "/plain.txt", Data: []byte("22")},
	)))
	for _, q := range []string{cafeNFD, cafeNFC, "CAFÉ.TXT"} {
		e := mustLookup(t, f, "/Data/"+q)
		if v, ok := attr(e, "name_hash"); !ok || v != "bad" || e.Name != cafeNFD {
			t.Errorf("Lookup(%+q) = %+v", q, e)
		}
	}
}

// A name with a character newer than the tables APFS froze is listed with no
// name_hash attribute and no warning even when its stored hash differs from
// what this reader's newer tables compute; a bad hash on an older name still
// warns.
func TestUnverifiableNameIsNeverFlagged(t *testing.T) {
	files := []apfstest.File{
		{Path: "/" + mtavruli, Data: []byte("1"), BadHash: true},
		{Path: "/\U0001fae0.txt", Data: []byte("22"), BadHash: true},
		{Path: "/" + cafeNFD, Data: []byte("333"), BadHash: true},
		{Path: "/good-é.txt", Data: []byte("4444")},
	}
	f, _ := openOpts(t, volOpts(volWith(true, true, true, files...)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
	for _, e := range es {
		_, flagged := attr(e, "name_hash")
		want := e.Name == cafeNFD
		if flagged != want {
			t.Errorf("%+q: name_hash flagged = %v, want %v", e.Name, flagged, want)
		}
	}
	if w := f.Info().Warnings; len(w) != 1 || !hasWarn(f, "hash") {
		t.Errorf("warnings %q, want exactly one (for the old name with a bad hash)", w)
	}
	// Both stay findable by name (a scan, since no hash can be trusted).
	for _, name := range []string{mtavruli, "\U0001fae0.txt"} {
		if e := mustLookup(t, f, "/Data/"+name); e.Name != name {
			t.Errorf("Lookup(%+q) = %+q", name, e.Name)
		}
	}
}
