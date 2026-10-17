package utf8fold

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// update rewrites tables_gen.go from the committed Unicode data:
//
//	go test ./internal/filesys/utf8fold -run TestGenerateTables -update
var update = flag.Bool("update", false, "rewrite tables_gen.go from testdata/ucd-12.1.0")

const ucdDir = "testdata/ucd-12.1.0"

// ucd is the part of the Unicode 12.1.0 data the tables are made from.
type ucd struct {
	assigned  []runeRange       // every assigned code point, surrogates excluded (First/Last ranges included)
	ccc       map[rune]uint8    // non-zero canonical combining classes
	decomp    map[rune][]rune   // canonical decompositions, one level (compatibility mappings left out)
	fold      map[rune][]rune   // CaseFolding.txt status C and F
	ignorable []runeRange       // Default_Ignorable_Code_Point, sorted
	sums      map[string]string // file name -> SHA-256 of the uncompressed file, as read
}

// readGz returns the uncompressed bytes of dir/name.gz.
func readGz(dir, name string) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, name+".gz"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

// readSources parses SOURCES: lines "<sha256>  <file>".
func readSources(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SOURCES"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				out[f[1]] = f[0]
			}
		}
	}
	return out, nil
}

func parseHex(s string) (rune, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 32)
	return rune(v), err
}

// loadUCD reads and parses the committed data. It refuses an input whose SHA-256 differs from SOURCES: the
// tables are tied to Unicode 12.1.0 and to nothing else.
func loadUCD(dir string) (*ucd, error) {
	want, err := readSources(dir)
	if err != nil {
		return nil, err
	}
	u := &ucd{ccc: map[rune]uint8{}, decomp: map[rune][]rune{}, fold: map[rune][]rune{}, sums: map[string]string{}}
	files := map[string][]byte{}
	for _, name := range []string{"UnicodeData.txt", "CaseFolding.txt", "DerivedCoreProperties.txt"} {
		b, err := readGz(dir, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		got := hex.EncodeToString(sum[:])
		if want[name] != got {
			return nil, fmt.Errorf("%s: sha256 %s differs from SOURCES (%s): the tables are for Unicode 12.1.0 only", name, got, want[name])
		}
		u.sums[name] = got
		files[name] = b
	}
	if err := u.parseUnicodeData(files["UnicodeData.txt"]); err != nil {
		return nil, err
	}
	if err := u.parseFolding(files["CaseFolding.txt"]); err != nil {
		return nil, err
	}
	return u, u.parseIgnorable(files["DerivedCoreProperties.txt"])
}

func (u *ucd) parseUnicodeData(b []byte) error {
	var first rune = -1
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ";")
		if len(f) < 6 {
			continue
		}
		cp, err := parseHex(f[0])
		if err != nil {
			return err
		}
		lo := cp
		switch {
		case strings.HasSuffix(f[1], ", First>"):
			first = cp
			continue
		case strings.HasSuffix(f[1], ", Last>"):
			lo = first
		}
		if f[2] != "Cs" {
			u.assigned = append(u.assigned, runeRange{lo, cp})
		}
		if f[3] != "0" {
			c, err := strconv.ParseUint(f[3], 10, 8)
			if err != nil {
				return err
			}
			u.ccc[cp] = uint8(c)
		}
		if f[5] != "" && !strings.HasPrefix(f[5], "<") {
			for _, h := range strings.Fields(f[5]) {
				r, err := parseHex(h)
				if err != nil {
					return err
				}
				u.decomp[cp] = append(u.decomp[cp], r)
			}
		}
	}
	return sc.Err()
}

func (u *ucd) parseFolding(b []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Split(line, ";")
		if len(f) < 3 || (strings.TrimSpace(f[1]) != "C" && strings.TrimSpace(f[1]) != "F") {
			continue
		}
		cp, err := parseHex(f[0])
		if err != nil {
			return err
		}
		for _, h := range strings.Fields(f[2]) {
			r, err := parseHex(h)
			if err != nil {
				return err
			}
			u.fold[cp] = append(u.fold[cp], r)
		}
	}
	return sc.Err()
}

func (u *ucd) parseIgnorable(b []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		rng, prop, ok := strings.Cut(line, ";")
		if !ok || strings.TrimSpace(prop) != "Default_Ignorable_Code_Point" {
			continue
		}
		loS, hiS, isRange := strings.Cut(strings.TrimSpace(rng), "..")
		lo, err := parseHex(loS)
		if err != nil {
			return err
		}
		hi := lo
		if isRange {
			if hi, err = parseHex(hiS); err != nil {
				return err
			}
		}
		u.ignorable = append(u.ignorable, runeRange{lo, hi})
	}
	sort.Slice(u.ignorable, func(i, j int) bool { return u.ignorable[i].lo < u.ignorable[j].lo })
	return sc.Err()
}

// expandCanon fully expands cp by the canonical decompositions of the data (Hangul syllables are computed).
func (u *ucd) expandCanon(dst []rune, cp rune) []rune {
	if cp >= hangulS && cp < hangulS+hangulCount {
		return appendHangul(dst, cp)
	}
	d, ok := u.decomp[cp]
	if !ok {
		return append(dst, cp)
	}
	for _, r := range d {
		dst = u.expandCanon(dst, r)
	}
	return dst
}

// perRune is NFD(fold(NFD(cp))) as plain rune strings, the unit the tables must reproduce.
func (u *ucd) perRune(cp rune) []rune {
	var out []rune
	for _, r := range u.expandCanon(nil, cp) {
		f, ok := u.fold[r]
		if !ok {
			f = []rune{r}
		}
		for _, g := range f {
			out = u.expandCanon(out, g)
		}
	}
	return out
}

// tablesSource renders tables_gen.go.
func (u *ucd) tablesSource() ([]byte, error) {
	var b bytes.Buffer
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	p("// Code generated by TestGenerateTables; DO NOT EDIT.\n//\n")
	p("// Inputs (Unicode 12.1.0, sha256 of the uncompressed files):\n")
	for _, n := range []string{"UnicodeData.txt", "CaseFolding.txt", "DerivedCoreProperties.txt"} {
		p("//\t%s  %s\n", u.sums[n], n)
	}
	p("\npackage utf8fold\n\n")

	// canonical combining classes, coalesced into ranges
	cps := make([]rune, 0, len(u.ccc))
	for r := range u.ccc {
		cps = append(cps, r)
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i] < cps[j] })
	p("// cccTable holds the non-zero canonical combining classes by range.\nvar cccTable = []cccRange{\n")
	for i := 0; i < len(cps); {
		j := i
		for j+1 < len(cps) && cps[j+1] == cps[j]+1 && u.ccc[cps[j+1]] == u.ccc[cps[i]] {
			j++
		}
		p("\t{0x%X, 0x%X, %d},\n", cps[i], cps[j], u.ccc[cps[i]])
		i = j + 1
	}
	p("}\n\n")

	emitMap := func(name, doc string, keys []rune, val func(rune) []rune) int {
		p("// %sIndex is %s, sorted by code point; each entry names its run in %sRunes.\nvar %sIndex = []mapEntry{\n", name, doc, name, name)
		var data []rune
		max := 0
		for _, k := range keys {
			v := val(k)
			p("\t{0x%X, %d, %d},\n", k, len(data), len(v))
			data = append(data, v...)
			if len(v) > max {
				max = len(v)
			}
		}
		p("}\n\nvar %sRunes = []rune{", name)
		for i, r := range data {
			if i%12 == 0 {
				p("\n\t")
			} else {
				p(" ")
			}
			p("0x%X,", r)
		}
		p("\n}\n\n")
		return max
	}
	sortedKeys := func(m map[rune][]rune) []rune {
		ks := make([]rune, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
		return ks
	}
	emitMap("decomp", "the full canonical decomposition of every code point that has one (Hangul syllables are computed)", sortedKeys(u.decomp),
		func(r rune) []rune { return u.expandCanon(nil, r) })
	emitMap("fold", "the C and F case foldings", sortedKeys(u.fold), func(r rune) []rune { return u.fold[r] })

	p("// ignorableTable holds the Default_Ignorable_Code_Point ranges.\nvar ignorableTable = []runeRange{\n")
	for _, r := range u.ignorable {
		p("\t{0x%X, 0x%X},\n", r.lo, r.hi)
	}
	p("}\n\n")

	// the largest number of runes one code point contributes to a key
	max := 1
	consider := func(cp rune) {
		if n := len(u.perRune(cp)); n > max {
			max = n
		}
	}
	for k := range u.decomp {
		consider(k)
	}
	for k := range u.fold {
		consider(k)
	}
	consider(hangulS + 1) // a LVT syllable: 3 jamo
	p("// maxKeyRunesPerRune is the most runes one code point adds to a key, len(NFD(fold(NFD(cp)))) over the\n// canonical decomposition and full-folding lengths of the data above (not compatibility figures).\nconst maxKeyRunesPerRune = %d\n", max)
	return format.Source(b.Bytes())
}

func TestGenerateTables(t *testing.T) {
	u, err := loadUCD(ucdDir)
	if err != nil {
		t.Fatal(err)
	}
	src, err := u.tablesSource()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("tables_gen.go", src, 0o644); err != nil { //nolint:gosec // a source file of this repository
			t.Fatal(err)
		}
		t.Logf("tables_gen.go rewritten (%d bytes)", len(src))
		return
	}
	have, err := os.ReadFile("tables_gen.go")
	if err != nil {
		t.Fatalf("tables_gen.go missing: %v (go test -run TestGenerateTables -update)", err)
	}
	if !bytes.Equal(have, src) {
		t.Fatalf("tables_gen.go differs from the tables generated from %s (go test -run TestGenerateTables -update)", ucdDir)
	}
}

// TestTablesAreUpToDate is the byte-for-byte comparison of the committed tables with a fresh generation.
func TestTablesAreUpToDate(t *testing.T) {
	if *update {
		t.Skip("-update rewrites the file in TestGenerateTables")
	}
	TestGenerateTables(t)
}

func TestNoUnicodeVersionDrift(t *testing.T) {
	if _, err := loadUCD(ucdDir); err != nil {
		t.Fatalf("committed data does not match SOURCES: %v", err)
	}
	// a copy whose CaseFolding.txt is another text must be refused by name
	tmp := t.TempDir()
	for _, n := range []string{"SOURCES", "UnicodeData.txt.gz", "CaseFolding.txt.gz", "DerivedCoreProperties.txt.gz"} {
		b, err := os.ReadFile(filepath.Join(ucdDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if n == "CaseFolding.txt.gz" {
			var z bytes.Buffer
			zw := gzip.NewWriter(&z)
			_, _ = zw.Write([]byte("# CaseFolding-99.0.0.txt\n0041; C; 0061;\n"))
			_ = zw.Close()
			b = z.Bytes()
		}
		if err := os.WriteFile(filepath.Join(tmp, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := loadUCD(tmp)
	if err == nil || !strings.Contains(err.Error(), "CaseFolding.txt") {
		t.Fatalf("a changed CaseFolding.txt was accepted: %v", err)
	}
}

func TestTablesAreSortedAndDisjoint(t *testing.T) {
	for i, r := range cccTable {
		if r.lo > r.hi || r.class == 0 || (i > 0 && r.lo <= cccTable[i-1].hi) {
			t.Fatalf("cccTable[%d] = %+v is empty, class 0, unsorted or overlapping", i, r)
		}
	}
	for i, r := range ignorableTable {
		if r.lo > r.hi || (i > 0 && r.lo <= ignorableTable[i-1].hi) {
			t.Fatalf("ignorableTable[%d] = %+v is unsorted or overlapping", i, r)
		}
	}
	for name, idx := range map[string][]mapEntry{"decomp": decompIndex, "fold": foldIndex} {
		data := decompRunes
		if name == "fold" {
			data = foldRunes
		}
		for i, e := range idx {
			if i > 0 && e.r <= idx[i-1].r {
				t.Fatalf("%sIndex[%d] = %+v is not after its predecessor", name, i, e)
			}
			if e.n == 0 || int(e.off)+int(e.n) > len(data) {
				t.Fatalf("%sIndex[%d] = %+v points outside its runes", name, i, e)
			}
		}
	}
}

// TestExpansionBound recomputes the maximum from the generated tables, by a route that shares no code with
// the generator or with Key, and compares it with the constant.
func TestExpansionBound(t *testing.T) {
	var nfd func(r rune) []rune
	nfd = func(r rune) []rune {
		if r >= hangulS && r < hangulS+hangulCount {
			return appendHangul(nil, r)
		}
		for _, e := range decompIndex {
			if e.r == r {
				return decompRunes[e.off : e.off+uint32(e.n)]
			}
		}
		return []rune{r}
	}
	foldOf := func(r rune) []rune {
		for _, e := range foldIndex {
			if e.r == r {
				return foldRunes[e.off : e.off+uint32(e.n)]
			}
		}
		return []rune{r}
	}
	max := 1
	for _, e := range append(append([]mapEntry(nil), decompIndex...), foldIndex...) {
		n := 0
		for _, a := range nfd(e.r) {
			for _, f := range foldOf(a) {
				n += len(nfd(f))
			}
		}
		if n > max {
			max = n
		}
	}
	if max != maxKeyRunesPerRune {
		t.Fatalf("recomputed maximum %d, generated constant %d", max, maxKeyRunesPerRune)
	}
	t.Logf("maxKeyRunesPerRune = %d", max)
}
