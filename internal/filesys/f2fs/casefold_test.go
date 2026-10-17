package f2fs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/filesys/utf8fold"
)

// Names are built from code points by value, so no source line holds an invisible character.
var (
	sharpS = string(rune(0x00DF)) // LATIN SMALL LETTER SHARP S, folds to "ss"
	zwsp   = string(rune(0x200B)) // default-ignorable
	shy    = string(rune(0x00AD)) // default-ignorable
	cgj    = string(rune(0x034F)) // default-ignorable
)

// cfFS builds an image whose /cf is a casefold directory holding the named empty files in the given order,
// with encoding id enc (1 is utf8-12.1.0).
func cfFS(t testing.TB, enc uint16, names ...string) *f2fs.FS {
	t.Helper()
	return cfFSOpts(t, f2fstest.Options{Segments: 2, Encoding: enc}, names...)
}

func cfFSOpts(t testing.TB, o f2fstest.Options, names ...string) *f2fs.FS {
	t.Helper()
	files := []f2fstest.File{{Path: "/cf", Dir: true, Casefold: true}}
	for _, n := range names {
		files = append(files, f2fstest.File{Path: "/cf/" + n, Inline: true})
	}
	img, _ := f2fstest.BuildTree(o, files)
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func rawName(e filesys.Entry) []byte {
	if e.RawName != nil {
		return e.RawName
	}
	return []byte(e.Name)
}

// The offsets of s_encoding (2758) and s_encoding_flags (2760) inside the superblock were measured in Task 2
// by diffing mkfs.f2fs images with and without -O casefold -C utf8[:strict]; here they are read from the
// committed kernel-made image and from the builder.
func TestSuperblockEncodingFields(t *testing.T) {
	for _, c := range []struct{ enc, flags uint16 }{{0, 0}, {1, 0}, {1, 1}, {2, 6}} {
		img, _ := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: c.enc, EncodingFlags: c.flags}, nil)
		f := mustOpen(t, img)
		if enc, flags := f.Encoding(); enc != c.enc || flags != c.flags {
			t.Errorf("Encoding() = %d, %d; built with %d, %d", enc, flags, c.enc, c.flags)
		}
	}
	img := gunzipFixture(t, filepath.Join("testdata", "f2fs-casefold.img.gz"))
	const sbBase = 1024 // the first superblock copy
	if got := binary.LittleEndian.Uint16(img[sbBase+2758:]); got != 1 {
		t.Errorf("the committed image holds %d at superblock offset 2758, want s_encoding 1 (utf8-12.1)", got)
	}
	if got := binary.LittleEndian.Uint16(img[sbBase+2760:]); got != 0 {
		t.Errorf("the committed image holds %d at superblock offset 2760, want s_encoding_flags 0", got)
	}
	if feat := binary.LittleEndian.Uint32(img[sbBase+2180:]); feat&0x1000 == 0 {
		t.Errorf("the committed image's feature word %#x lacks the casefold bit 0x1000", feat)
	}
	f := mustOpen(t, img)
	if enc, flags := f.Encoding(); enc != 1 || flags != 0 {
		t.Errorf("f2fs-casefold: Encoding() = %d, %d, want 1, 0", enc, flags)
	}
	// an image made without casefold holds zero there
	for _, p := range fixturePaths(t) {
		plain := gunzipFixture(t, p)
		if binary.LittleEndian.Uint16(plain[sbBase+2758:]) != 0 || binary.LittleEndian.Uint16(plain[sbBase+2760:]) != 0 {
			t.Errorf("%s holds a non-zero s_encoding at 2758/2760", filepath.Base(p))
		}
	}
}

type kernelVectors struct {
	Generator struct {
		ImageSHA map[string]string `json:"image_sha256"`
	} `json:"generator"`
	Vectors []struct {
		ID       int    `json:"id"`
		Category string `json:"category"`
		Bucket   string `json:"bucket"`
		Stored   string `json:"stored_hex"`
		Query    string `json:"query_hex"`
		F2FS     bool   `json:"f2fs"`
	} `json:"vectors"`
}

func loadKernelVectors(t testing.TB) kernelVectors {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "utf8fold", "testdata", "kernel-vectors.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	var v kernelVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestCasefoldLookupMatchesKernel replays every kernel vector against the real F2FS casefold image: a true
// verdict means Lookup("/cf/<bucket>/<query>") is the entry whose raw name is the stored name; a false
// verdict means it is NOT that entry (another entry, or an error such as not found or undecided).
func TestCasefoldLookupMatchesKernel(t *testing.T) {
	v := loadKernelVectors(t)
	img := gunzipFixture(t, filepath.Join("testdata", "f2fs-casefold.img.gz"))
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != v.Generator.ImageSHA["f2fs"] {
		t.Fatalf("image sha256 %s, kernel-vectors.json.gz pins %s", got, v.Generator.ImageSHA["f2fs"])
	}
	// the reader spends a per-filesystem budget (1 GiB) on directory reads, and the 5000-name bucket
	// directory costs about 150 KiB a lookup: open the image again every 2000 rows so the budget is
	// never what a verdict reflects
	f := mustOpen(t, img)
	bad := 0
	for i, r := range v.Vectors {
		if i%2000 == 1999 {
			f = mustOpen(t, img)
		}
		stored, _ := hex.DecodeString(r.Stored)
		query, _ := hex.DecodeString(r.Query)
		e, err := f.Lookup("/cf/" + r.Bucket + "/" + string(query))
		got := err == nil && bytes.Equal(rawName(e), stored)
		if !r.F2FS && errors.Is(err, filesys.ErrCorrupt) {
			// a kernel-false row passes on any error, so a budget (or other corrupt) failure would hide here
			t.Fatalf("row %d (%s): Lookup(%q) failed with a corruption error, not an answer: %v", r.ID, r.Category, query, err)
		}
		if got != r.F2FS {
			bad++
			if bad <= 10 {
				t.Errorf("row %d (%s): Lookup(%q) = (%q, %v); the kernel says it resolves to %q: %v", r.ID, r.Category, query, rawName(e), err, stored, r.F2FS)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d rows disagree with the kernel", bad, len(v.Vectors))
	}
}

type casefoldExpect struct {
	Directories []struct {
		Path     string   `json:"path"`
		Casefold bool     `json:"casefold"`
		Count    int      `json:"count"`
		SHA256   string   `json:"names_sha256"`
		NamesHex []string `json:"names_hex"`
	} `json:"directories"`
}

func digestOf(hexNames []string) string {
	s := slices.Clone(hexNames)
	sort.Strings(s)
	s = slices.Compact(s)
	h := sha256.New()
	for _, n := range s {
		_, _ = io.WriteString(h, n+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestCasefoldFixtureTreeEqualsOracle: ReadDir of every directory of the real image lists exactly the names
// the host tool read from it (count and digest of the raw names; the full list for /plain).
func TestCasefoldFixtureTreeEqualsOracle(t *testing.T) {
	path := filepath.Join("testdata", "f2fs-casefold.img.gz")
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".img.gz") + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex casefoldExpect
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	f := mustOpen(t, gunzipFixture(t, path))
	for _, d := range ex.Directories {
		es := readDirPath(t, f, d.Path)
		var got []string
		for _, e := range es {
			if e.Deleted {
				continue
			}
			got = append(got, hex.EncodeToString(rawName(e)))
		}
		if len(got) != d.Count || digestOf(got) != d.SHA256 {
			t.Errorf("%s: %d names, digest %s; the oracle has %d, %s", d.Path, len(got), digestOf(got), d.Count, d.SHA256)
		}
		if len(d.NamesHex) > 0 {
			sort.Strings(got)
			if !slices.Equal(got, d.NamesHex) {
				t.Errorf("%s: names differ from the oracle's list", d.Path)
			}
		}
	}
}

// /plain is not a casefold directory: a name that differs only in case is not found there.
func TestCasefoldPlainDirectoryIsExact(t *testing.T) {
	f := mustOpen(t, gunzipFixture(t, filepath.Join("testdata", "f2fs-casefold.img.gz")))
	checked := 0
	for _, e := range readDirPath(t, f, "/plain") {
		up := strings.ToUpper(e.Name)
		if e.RawName != nil || up == e.Name || strings.ToLower(up) != e.Name {
			continue
		}
		if _, err := f.Lookup("/plain/" + up); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(/plain/%q) = %v, want not found (exact directory)", up, err)
		}
		if checked++; checked >= 20 {
			break
		}
	}
	if checked == 0 {
		t.Fatal("no name of /plain to check")
	}
}

func TestCasefoldExactBeatsFold(t *testing.T) {
	f := cfFS(t, 1, "Stra"+sharpS+"e", "Strasse", "STRASSE")
	for _, n := range []string{"STRASSE", "Strasse"} {
		e, err := f.Lookup("/cf/" + n)
		if err != nil || e.Name != n {
			t.Fatalf("Lookup(%s) = %+v, %v; want the exact name", n, e, err)
		}
	}
}

func TestCasefoldAliasBeatsFold(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	f := cfFS(t, 1, strings.ToUpper(alias[:5])+alias[5:], strings.ToUpper(alias), bad)
	e, err := f.Lookup("/cf/" + alias)
	if err != nil || string(e.RawName) != bad {
		t.Fatalf("Lookup(alias) = %+v, %v; want the stored non-UTF-8 name", e, err)
	}
}

// One fold hit is not enough to beat the alias. Mutation: take a single fold hit before the alias.
func TestCasefoldAliasBeatsASingleFoldHit(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	f := cfFS(t, 1, "~RAW~"+alias[5:], bad)
	e, err := f.Lookup("/cf/" + alias)
	if err != nil || string(e.RawName) != bad {
		t.Fatalf("Lookup(alias) = %+v, %v; want the alias target", e, err)
	}
}

func TestCasefoldAmbiguousIsAnError(t *testing.T) {
	f := cfFS(t, 1, "Stra"+sharpS+"e", "STRASSE")
	_, err := f.Lookup("/cf/strasse")
	if !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup(strasse) = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{fmt.Sprintf("%q", "Stra"+sharpS+"e"), `"STRASSE"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if e, err := f.Lookup("/cf/STRASSE"); err != nil || e.Name != "STRASSE" {
		t.Errorf("Lookup(STRASSE) = %+v, %v; the exact name still resolves", e, err)
	}
}

// foldVariants returns n distinct names that all fold equal to "strasse" (default-ignorable characters
// inserted at the gaps of the word, a different mix for each name).
func foldVariants(n int) []string {
	ign := []string{"", zwsp, shy, cgj, string(rune(0x200D)), string(rune(0x2060)), string(rune(0xFEFF))}
	out := make([]string, 0, n)
	for i := 0; len(out) < n; i++ {
		var b strings.Builder
		x := i
		for _, r := range "strasse" {
			b.WriteString(ign[x%len(ign)])
			x /= len(ign)
			b.WriteRune(r)
		}
		b.WriteString(ign[x%len(ign)])
		out = append(out, b.String())
	}
	return out
}

// An exact match that comes AFTER many fold hits still wins; mutation: stop the scan at the second fold hit.
func TestCasefoldExactAfterManyFoldHitsWins(t *testing.T) {
	f := cfFS(t, 1, "Stra"+sharpS+"e", "Strasse", "STRASSE")
	if e, err := f.Lookup("/cf/STRASSE"); err != nil || e.Name != "STRASSE" {
		t.Fatalf("three names: Lookup(STRASSE) = %+v, %v", e, err)
	}
	names := append(foldVariants(1500)[1:], "STRASSE")
	f = cfFSOpts(t, f2fstest.Options{Segments: 8, Encoding: 1}, names...)
	e, err := f.Lookup("/cf/STRASSE")
	if err != nil || e.Name != "STRASSE" {
		t.Fatalf("1499 fold hits then the exact name: Lookup(STRASSE) = %+v, %v", e, err)
	}
}

// The alias is the LAST name, after two fold hits; mutation: stop the scan at the second fold hit.
func TestCasefoldAliasLastStillBeatsFold(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	f := cfFS(t, 1, strings.ToUpper(alias), "~RAW~"+alias[5:], bad)
	e, err := f.Lookup("/cf/" + alias)
	if err != nil || string(e.RawName) != bad {
		t.Fatalf("Lookup(alias) = %+v, %v; want the alias target, not ErrAmbiguous", e, err)
	}
}

// Ruling Z47: two records whose bytes are the alias, with no exact match, are two alias matches: ErrAmbiguous.
func TestCasefoldTwoAliasRecordsAreAmbiguous(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	img, _ := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: 1}, []f2fstest.File{
		{Path: "/cf", Dir: true, Casefold: true},
		{Path: "/cf/d1", RawName: []byte(bad), Inline: true},
		{Path: "/cf/d2", RawName: []byte(bad), Inline: true},
		{Path: "/cf/other", Inline: true},
	})
	f := mustOpen(t, img)
	if _, err := f.Lookup("/cf/" + alias); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup(alias) with two matching records = %v, want ErrAmbiguous", err)
	}
	if _, err := f.Lookup("/cf/" + bad); err != nil {
		t.Errorf("Lookup(exact) = %v", err)
	}
}

// The scan reads the whole directory and keeps at most two records: the allocations of a lookup that meets
// many fold hits do not exceed those of a lookup in a directory of the same size that meets two.
func TestCasefoldAmbiguityScanIsBounded(t *testing.T) {
	const n = 1500
	opts := f2fstest.Options{Segments: 8, Encoding: 1}
	many := foldVariants(n)[1:]
	few := foldVariants(2)[1:]
	for i := len(few); i < n-1; i++ { // the same number of names, only two of which fold to the query
		few = append(few, fmt.Sprintf("other%05d-xxxxxxxxxxxxxxxxxxxxxxx", i))
	}
	few = append(few, "STRASSE")
	many = append(many, "zzzzz")
	fm := cfFSOpts(t, opts, many...)
	ff := cfFSOpts(t, opts, few...)
	_, err := fm.Lookup("/cf/strasse")
	if !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup with %d hits = %v, want ErrAmbiguous", n-1, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(n-1-2)) {
		t.Errorf("the error does not report the %d other names: %v", n-1-2, err)
	}
	am := testing.AllocsPerRun(3, func() { _, _ = fm.Lookup("/cf/strasse") })
	af := testing.AllocsPerRun(3, func() { _, _ = ff.Lookup("/cf/strasse") })
	if am > af+60 {
		t.Errorf("a lookup with %d fold hits made %v allocations, with 2 hits %v: it grows with the hits", n-1, am, af)
	}
}

func TestCasefoldUnknownEncodingIsUnsupported(t *testing.T) {
	for _, o := range []f2fstest.Options{
		{Segments: 2},                                // no encoding at all
		{Segments: 2, Encoding: 2},                   // an encoding id this reader does not implement
		{Segments: 2, Encoding: 1, EncodingFlags: 2}, // a flags value this reader does not know
	} {
		f := cfFSOpts(t, o, "File.TXT")
		if e, err := f.Lookup("/cf/File.TXT"); err != nil || e.Name != "File.TXT" {
			t.Errorf("%+v: an exact hit = %+v, %v", o, e, err)
		}
		_, err := f.Lookup("/cf/file.txt")
		if !errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%+v: a miss in a casefold directory = %v, want ErrUnsupported and not not-found", o, err)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("s_encoding %d", o.Encoding)) {
			t.Errorf("%+v: the error does not name the encoding id: %v", o, err)
		}
		if !hasWarning(f.Info(), "casefold") {
			t.Errorf("%+v: no casefold warning in %v", o, f.Info().Warnings)
		}
		if len(readDirPath(t, f, "/cf")) == 0 {
			t.Errorf("%+v: ReadDir lists nothing", o)
		}
		for _, dot := range []string{"/cf/.", "/cf/.."} {
			if _, err := f.Lookup(dot); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("%+v: Lookup(%s) = %v, want not found", o, dot, err)
			}
		}
	}
	// strict mode (flag bit 0) is read and not applied: names are still folded
	f := cfFSOpts(t, f2fstest.Options{Segments: 2, Encoding: 1, EncodingFlags: 1}, "File.TXT")
	if e, err := f.Lookup("/cf/file.txt"); err != nil || e.Name != "File.TXT" {
		t.Errorf("strict volume: Lookup = %+v, %v", e, err)
	}
}

func TestCasefoldNeverFoldsEncryptedOrDots(t *testing.T) {
	img, _ := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encrypt: true, Encoding: 1}, []f2fstest.File{
		{Path: "/enc", Dir: true, Casefold: true, Encrypted: true},
		{Path: "/enc/cipher", RawName: []byte("cipher"), Inline: true, Encrypted: true},
	})
	f := mustOpen(t, img)
	if _, err := f.Lookup("/enc/CIPHER"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("an encrypted directory folded a name: %v", err)
	}
	g := cfFS(t, 1, "a")
	for _, dot := range []string{"/cf/.", "/cf/.."} {
		if _, err := g.Lookup(dot); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%s) = %v, want not found", dot, err)
		}
	}
}

// A stored name that is not valid UTF-8 is found only by its exact bytes or its ~raw~ alias, never folded.
func TestCasefoldInvalidUTF8ComparesBytes(t *testing.T) {
	stored := "A" + string([]byte{0xFF})
	f := cfFS(t, 1, stored, "other")
	if e, err := f.Lookup("/cf/" + stored); err != nil || string(e.RawName) != stored {
		t.Errorf("exact = %+v, %v", e, err)
	}
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(stored))
	if e, err := f.Lookup("/cf/" + alias); err != nil || string(e.RawName) != stored {
		t.Errorf("alias = %+v, %v", e, err)
	}
	for _, q := range []string{"a" + string([]byte{0xFF}), "A", "a"} {
		if e, err := f.Lookup("/cf/" + q); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %+v, %v; want not found (opaque name, no fold)", q, e, err)
		}
	}
}

// A comparison the kernel was not measured for is Unknown, and a lookup that finds nothing else is an
// undecided error, never "not found" (ruling Z39).
func TestCasefoldUnknownShapeIsUndecidedNotNotFound(t *testing.T) {
	odd := "x" + string([]byte{0xFF}) + "y"
	f := cfFS(t, 1, "plain", odd)
	_, err := f.Lookup("/cf/missing")
	if !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("Lookup(missing) = %v, want ErrUndecided and not not-found", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", odd)) {
		t.Errorf("the error does not name the undecidable name: %v", err)
	}
	if e, err := f.Lookup("/cf/" + odd); err != nil || string(e.RawName) != odd {
		t.Errorf("exact = %+v, %v", e, err)
	}
	if e, err := f.Lookup("/cf/PLAIN"); err != nil || e.Name != "plain" {
		t.Errorf("a single fold hit decides the answer = %+v, %v", e, err)
	}
	g := cfFS(t, 1, "Stra"+sharpS+"e", "STRASSE", odd)
	if _, err := g.Lookup("/cf/strasse"); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("Lookup(strasse) = %v, want ErrAmbiguous", err)
	}
	h := cfFS(t, 1, "plain")
	if _, err := h.Lookup("/cf/" + odd); !errors.Is(err, filesys.ErrUndecided) {
		t.Errorf("Lookup of an unmeasured query = %v, want ErrUndecided", err)
	}
	if _, err := cfFS(t, 1, "plain").Lookup("/cf/missing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a clean miss = %v, want not found", err)
	}
	if _, err := f.Lookup("/cf/" + strings.Repeat("a", 256)); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a 256-byte component = %v, want not found", err)
	}
}

// 600 names of 255 bytes made of the code points whose keys are longest: the lookup finishes and allocates
// within a fixed bound.
func TestCasefoldLookupIsBounded(t *testing.T) {
	type cand struct{ r rune }
	var best []cand
	most := 0
	for r := rune(0x80); r < 0x20000; r++ {
		if r >= 0xD800 && r < 0xE000 {
			continue
		}
		k, ok := utf8fold.Key(nil, []byte(string(r)))
		if !ok {
			continue
		}
		n := len([]rune(string(k)))
		switch {
		case n > most:
			most, best = n, []cand{{r}}
		case n == most:
			best = append(best, cand{r})
		}
	}
	if most < 3 || len(best) == 0 {
		t.Fatalf("no code point expands to several runes: %d", most)
	}
	var names []string
	for i := 0; i < 600; i++ {
		var b strings.Builder
		for j := 0; b.Len()+4 <= 245; j++ {
			b.WriteRune(best[(i+j)%len(best)].r)
		}
		fmt.Fprintf(&b, "%04d", i) // distinct names
		names = append(names, b.String())
	}
	f := cfFSOpts(t, f2fstest.Options{Segments: 10, Encoding: 1}, names...)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_, err := f.Lookup("/cf/" + strings.ToUpper(names[300]))
	runtime.ReadMemStats(&m1)
	if err != nil && !errors.Is(err, filesys.ErrNotFound) && !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup: %v", err)
	}
	if grew := m1.TotalAlloc - m0.TotalAlloc; grew > 64<<20 {
		t.Errorf("one lookup in a directory of 600 maximal names allocated %d bytes", grew)
	}
}
