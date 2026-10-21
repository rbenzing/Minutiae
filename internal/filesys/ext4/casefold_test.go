package ext4_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/utf8fold"
)

// Names are built from code points by value, so no source line holds an invisible character.
var (
	sharpS = string(rune(0x00DF)) // LATIN SMALL LETTER SHARP S, folds to "ss"
	zwsp   = string(rune(0x200B)) // default-ignorable
	shy    = string(rune(0x00AD)) // default-ignorable
	cgj    = string(rune(0x034F)) // default-ignorable
)

const casefoldFlag = 0x40000000

// cfFS builds an image whose /cf is a casefold directory holding the named empty files in the given order,
// with encoding id enc (1 is utf8-12.1.0).
func cfFS(t testing.TB, enc uint16, names ...string) *ext4.FS {
	t.Helper()
	return cfFSOpts(t, ext4test.Options{Extents: true, Encoding: enc}, names...)
}

func cfFSOpts(t testing.TB, o ext4test.Options, names ...string) *ext4.FS {
	t.Helper()
	files := []ext4test.File{{Path: "/cf", Dir: true, Flags: casefoldFlag}}
	for _, n := range names {
		files = append(files, ext4test.File{Path: "/cf/" + n})
	}
	img := ext4test.Build(o, files)
	f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
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

func TestSuperblockEncodingFields(t *testing.T) {
	for _, c := range []struct{ enc, flags uint16 }{{0, 0}, {1, 0}, {1, 1}, {2, 6}} {
		img := ext4test.Build(ext4test.Options{Extents: true, Encoding: c.enc, EncodingFlags: c.flags}, nil)
		f := mustOpen(t, img)
		if enc, flags := f.Encoding(); enc != c.enc || flags != c.flags {
			t.Errorf("Encoding() = %d, %d; built with %d, %d", enc, flags, c.enc, c.flags)
		}
	}
	// the committed image made by mke2fs -O casefold -E encoding=utf8: s_encoding 1, no flags (the offsets
	// 0x27C and 0x27E were confirmed by diffing mke2fs superblocks, see the task report)
	img := gunzipFixture(t, filepath.Join("testdata", "ext4-casefold.img.gz"))
	f := mustOpen(t, img)
	if enc, flags := f.Encoding(); enc != 1 || flags != 0 {
		t.Errorf("ext4-casefold: Encoding() = %d, %d, want 1, 0", enc, flags)
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
		Ext4     bool   `json:"ext4"`
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

// TestCasefoldLookupMatchesKernel replays every kernel vector against the real casefold image: a true
// verdict means Lookup("/cf/<bucket>/<query>") is the entry whose raw name is the stored name; a false
// verdict means it is NOT that entry (another entry, or an error such as not found or undecided).
func TestCasefoldLookupMatchesKernel(t *testing.T) {
	v := loadKernelVectors(t)
	img := gunzipFixture(t, filepath.Join("testdata", "ext4-casefold.img.gz"))
	sum := sha256.Sum256(img)
	if got := hex.EncodeToString(sum[:]); got != v.Generator.ImageSHA["ext4"] {
		t.Fatalf("image sha256 %s, kernel-vectors.json.gz pins %s", got, v.Generator.ImageSHA["ext4"])
	}
	f := mustOpen(t, img)
	bad := 0
	for _, r := range v.Vectors {
		stored, _ := hex.DecodeString(r.Stored)
		query, _ := hex.DecodeString(r.Query)
		e, err := f.Lookup("/cf/" + r.Bucket + "/" + string(query))
		got := err == nil && bytes.Equal(rawName(e), stored)
		if !r.Ext4 && errors.Is(err, filesys.ErrCorrupt) {
			// a kernel-false row passes on any error, so a corruption failure would hide here
			t.Fatalf("row %d (%s): Lookup(%q) failed with a corruption error, not an answer: %v", r.ID, r.Category, query, err)
		}
		if got != r.Ext4 {
			bad++
			if bad <= 10 {
				t.Errorf("row %d (%s): Lookup(%q) = (%q, %v); the kernel says it resolves to %q: %v", r.ID, r.Category, query, rawName(e), err, stored, r.Ext4)
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
// the host tool read from it (count and digest of the raw names; the full list for /plain and /ambig).
func TestCasefoldFixtureTreeEqualsOracle(t *testing.T) {
	path := filepath.Join("testdata", "ext4-casefold.img.gz")
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
		es := readDir(t, f, d.Path)
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
	f := mustOpen(t, gunzipFixture(t, filepath.Join("testdata", "ext4-casefold.img.gz")))
	checked := 0
	for _, e := range readDir(t, f, "/plain") {
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
	e, err := f.Lookup("/cf/STRASSE")
	if err != nil || e.Name != "STRASSE" {
		t.Fatalf("Lookup(STRASSE) = %+v, %v; want the exact name", e, err)
	}
	e, err = f.Lookup("/cf/Strasse")
	if err != nil || e.Name != "Strasse" {
		t.Fatalf("Lookup(Strasse) = %+v, %v; want the exact name", e, err)
	}
}

func TestCasefoldAliasBeatsFold(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	// two names that fold equal to the alias text come first; the stored non-UTF-8 name is last
	f := cfFS(t, 1, strings.ToUpper(alias[:5])+alias[5:], strings.ToUpper(alias), bad)
	e, err := f.Lookup("/cf/" + alias)
	if err != nil || string(e.RawName) != bad {
		t.Fatalf("Lookup(alias) = %+v, %v; want the stored non-UTF-8 name", e, err)
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
	if e, err := f.Lookup("/cf/Stra" + sharpS + "e"); err != nil || e.Name != "Stra"+sharpS+"e" {
		t.Errorf("Lookup(exact sharp s) = %+v, %v", e, err)
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
		for gap, r := range "strasse" {
			b.WriteString(ign[x%len(ign)])
			x /= len(ign)
			_ = gap
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
	names := append(foldVariants(5000)[1:], "STRASSE") // variant 0 is "strasse" itself; none equals "STRASSE"
	f = cfFSOpts(t, ext4test.Options{Extents: true, Encoding: 1, BlockSize: 4096, BlocksPerGroup: 8192, InodesPerGroup: 8192, InodeSize: 128}, names...)
	e, err := f.Lookup("/cf/STRASSE")
	if err != nil || e.Name != "STRASSE" {
		t.Fatalf("5000 fold hits then the exact name: Lookup(STRASSE) = %+v, %v", e, err)
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

// The scan reads the whole directory and keeps at most two records: the allocations of a lookup that meets
// 10000 fold hits do not exceed those of a lookup in a directory of the same size that meets two.
func TestCasefoldAmbiguityScanIsBounded(t *testing.T) {
	const n = 10000
	opts := ext4test.Options{Extents: true, Encoding: 1, BlockSize: 4096, BlocksPerGroup: 8192, InodesPerGroup: 12288, InodeSize: 128}
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
	for _, o := range []ext4test.Options{
		{Extents: true},                                // no encoding at all
		{Extents: true, Encoding: 2},                   // an encoding id this reader does not implement
		{Extents: true, Encoding: 1, EncodingFlags: 2}, // a flags value this reader does not know
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
		if len(readDir(t, f, "/cf")) == 0 {
			t.Errorf("%+v: ReadDir lists nothing", o)
		}
		for _, dot := range []string{"/cf/.", "/cf/.."} {
			if _, err := f.Lookup(dot); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("%+v: Lookup(%s) = %v, want not found", o, dot, err)
			}
		}
	}
	// strict mode (flag bit 0) is read and not applied: names are still folded
	f := cfFSOpts(t, ext4test.Options{Extents: true, Encoding: 1, EncodingFlags: 1}, "File.TXT")
	if e, err := f.Lookup("/cf/file.txt"); err != nil || e.Name != "File.TXT" {
		t.Errorf("strict volume: Lookup = %+v, %v", e, err)
	}
}

func TestCasefoldNeverFoldsEncryptedOrDots(t *testing.T) {
	files := []ext4test.File{
		{Path: "/enc", Dir: true, Flags: casefoldFlag},
		{Path: "/enc/cipher", Data: []byte("c")},
	}
	// "/enc" gets the ENCRYPT flag from the builder (Options.Encrypt); the casefold flag is also set
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, Encrypt: true, Encoding: 1}, files))
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

// Ruling Z39/Z33: a comparison the kernel was not measured for is Unknown, and a lookup that finds nothing
// else is an undecided error, never "not found". The name below (an invalid byte followed by text) has no
// measured shape.
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
	// the exact name still resolves, and so does a single fold hit elsewhere
	if e, err := f.Lookup("/cf/" + odd); err != nil || string(e.RawName) != odd {
		t.Errorf("exact = %+v, %v", e, err)
	}
	if e, err := f.Lookup("/cf/PLAIN"); err != nil || e.Name != "plain" {
		t.Errorf("a single fold hit decides the answer = %+v, %v", e, err)
	}
	// two fold hits are ambiguous, even next to an undecidable name
	g := cfFS(t, 1, "Stra"+sharpS+"e", "STRASSE", odd)
	if _, err := g.Lookup("/cf/strasse"); !errors.Is(err, filesys.ErrAmbiguous) {
		t.Errorf("Lookup(strasse) = %v, want ErrAmbiguous", err)
	}
	// a query of an unmeasured shape cannot be compared with anything
	h := cfFS(t, 1, "plain")
	if _, err := h.Lookup("/cf/" + odd); !errors.Is(err, filesys.ErrUndecided) {
		t.Errorf("Lookup of an unmeasured query = %v, want ErrUndecided", err)
	}
	// without any undecidable name the same miss is a plain not-found
	if _, err := cfFS(t, 1, "plain").Lookup("/cf/missing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a clean miss = %v, want not found", err)
	}
	// a name of 256 bytes cannot exist: provably absent, not undecided
	long := strings.Repeat("a", 256)
	if _, err := f.Lookup("/cf/" + long); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a 256-byte component = %v, want not found", err)
	}
}

// Ruling Z20 (existing behaviour pinned): a name found stands even though an EARLIER directory block is
// damaged. Block 0 has a forged rec_len (the rest of that block is not read); the exact name, and
// separately the alias name, lie in the next block.
func TestCasefoldExactAndAliasStandOnADamagedEarlierBlock(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	for _, c := range []struct {
		name, query, want string
	}{
		{"exact", "Exact.TXT", "Exact.TXT"},
		{"alias", alias, bad},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := []ext4test.File{
				{Path: "/cf", Dir: true, Flags: casefoldFlag},
				{Path: "/cf/first", Data: []byte("1")},
				{Path: "/cf/second", Data: []byte("2")},
				{Path: "/cf/" + c.want, Data: []byte("t"), NewBlock: true},
			}
			img := ext4test.Build(ext4test.Options{Extents: true, Encoding: 1}, files)
			f := mustOpen(t, img)
			d, err := f.Lookup("/cf")
			if err != nil {
				t.Fatal(err)
			}
			blk := int(dirRuns(t, f, d)[0].Offset)
			put16(img, blk+24+4, 0) // rec_len of the first file record of block 0: the rest of block 0 is cut
			f = mustOpen(t, img)
			e, err := f.Lookup("/cf/" + c.query)
			if err != nil || !bytes.Equal(rawName(e), []byte(c.want)) {
				t.Fatalf("Lookup(%q) = %+v, %v; want %q after a damaged earlier block", c.query, e, err, c.want)
			}
			if !hasWarning(f.Info(), "rec_len") {
				t.Errorf("the forged rec_len was not noticed: %v", f.Info().Warnings)
			}
		})
	}
}

// 1000 names of 255 bytes made of the code points whose keys are longest: the lookup finishes and allocates
// within a fixed bound.
func TestCasefoldLookupIsBounded(t *testing.T) {
	type cand struct {
		r rune
		n int
	}
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
			most, best = n, []cand{{r, n}}
		case n == most:
			best = append(best, cand{r, n})
		}
	}
	if most < 3 || len(best) == 0 {
		t.Fatalf("no code point expands to several runes: %d", most)
	}
	var names []string
	for i := 0; i < 1000; i++ {
		var b strings.Builder
		for j := 0; b.Len()+4 <= 255; j++ {
			b.WriteRune(best[(i+j)%len(best)].r)
		}
		names = append(names, b.String())
	}
	f := cfFSOpts(t, ext4test.Options{Extents: true, Encoding: 1, BlockSize: 4096, BlocksPerGroup: 8192, InodesPerGroup: 2048, InodeSize: 128}, names...)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_, err := f.Lookup("/cf/" + strings.ToUpper(names[500]))
	runtime.ReadMemStats(&m1)
	if err != nil && !errors.Is(err, filesys.ErrNotFound) && !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup: %v", err)
	}
	if grew := m1.TotalAlloc - m0.TotalAlloc; grew > 64<<20 {
		t.Errorf("one lookup in a directory of 1000 maximal names allocated %d bytes", grew)
	}
}

// One fold hit is not enough to beat the alias: the alias target decides first (the fold hit comes first on
// disk and is the only one). Mutation: take a single fold hit before the alias.
func TestCasefoldAliasBeatsASingleFoldHit(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	f := cfFS(t, 1, "~RAW~"+alias[5:], bad)
	e, err := f.Lookup("/cf/" + alias)
	if err != nil || string(e.RawName) != bad {
		t.Fatalf("Lookup(alias) = %+v, %v; want the alias target", e, err)
	}
}

// Ruling Z47 (fail closed): two records whose bytes are the alias, with no exact match, are two alias
// matches: ErrAmbiguous, never one of them and never a fold hit.
func TestCasefoldTwoAliasRecordsAreAmbiguous(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	files := []ext4test.File{
		{Path: "/cf", Dir: true, Flags: casefoldFlag},
		{Path: "/cf/" + bad},
		{Path: "/cf/" + bad},
		{Path: "/cf/other"},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, Encoding: 1}, files)
	f := mustOpen(t, img)
	_, err := f.Lookup("/cf/" + alias)
	if !errors.Is(err, filesys.ErrAmbiguous) {
		t.Fatalf("Lookup(alias) with two matching records = %v, want ErrAmbiguous", err)
	}
	// the exact bytes still resolve (the exact step has its own order: the first record)
	if _, err := f.Lookup("/cf/" + bad); err != nil {
		t.Errorf("Lookup(exact) = %v", err)
	}
}
