package utf8fold

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The categories the probe script (tools/fixtures/casefold_probes.py) emits.
// Negative controls and identity controls live in the category they control,
// so every category has both a true and a false verdict.
var vectorCategories = []string{
	"casefold", "decomposition", "ignorable", "unmapped", "reorder",
	"special", "post-12.1", "invalid",
}

// maxDecompressed bounds what a test reads from a committed gzip file (the largest is a 128 MiB image).
const maxDecompressed = 1 << 30

type vectorRow struct {
	ID            int    `json:"id"`
	Category      string `json:"category"`
	Bucket        string `json:"bucket"`
	StoredHex     string `json:"stored_hex"`
	QueryHex      string `json:"query_hex"`
	StoredPresent bool   `json:"stored_present"`
	Ext4          bool   `json:"ext4"`
	F2FS          bool   `json:"f2fs"`
}

type vectorFile struct {
	Generator struct {
		Kernel    string            `json:"kernel"`
		Unicode   string            `json:"unicode"`
		ImageSHA  map[string]string `json:"image_sha256"`
		FsckExit  map[string]int    `json:"fsck_exit"`
		Commands  []string          `json:"commands"`
		GuestSecs int               `json:"guest_seconds"`
	} `json:"generator"`
	Vectors    []vectorRow `json:"vectors"`
	Exceptions []int       `json:"exceptions"`
}

type vectorMeta struct {
	VectorsSHA256 string `json:"vectors_sha256"`
	Rows          int    `json:"rows"`
	Gzip          string `json:"gzip"`
}

// gunzipBounded returns the decompressed bytes of a committed gzip file.
func gunzipBounded(t testing.TB, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	b, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
	if err != nil || len(b) > maxDecompressed {
		t.Fatalf("%s: read %d bytes, err %v", path, len(b), err)
	}
	return b
}

func readVectors(t *testing.T) vectorFile {
	t.Helper()
	gz := filepath.Join("testdata", "kernel-vectors.json.gz")
	if _, err := os.Stat(gz); err != nil {
		t.Fatalf("kernel-vectors.json.gz missing: %v (regenerate with tools/fixtures/gen.sh casefold)", err)
	}
	raw := gunzipBounded(t, gz)
	mb, err := os.ReadFile(filepath.Join("testdata", "kernel-vectors.meta.json"))
	if err != nil {
		t.Fatalf("kernel-vectors.meta.json missing: %v", err)
	}
	var m vectorMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("kernel-vectors.meta.json: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != m.VectorsSHA256 {
		t.Fatalf("decompressed kernel-vectors.json sha256 %s, meta pins %s", got, m.VectorsSHA256)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("kernel-vectors.json: %v", err)
	}
	if m.Rows != len(v.Vectors) {
		t.Fatalf("meta says %d rows, file has %d", m.Rows, len(v.Vectors))
	}
	return v
}

func decodeName(t *testing.T, id int, what, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("vector %d %s: bad hex %q: %v", id, what, h, err)
	}
	return b
}

func TestKernelVectorsWellFormed(t *testing.T) {
	v := readVectors(t)
	if v.Generator.Unicode != "12.1.0" {
		t.Errorf("unicode = %q, want 12.1.0", v.Generator.Unicode)
	}
	if len(v.Vectors) < 5000 {
		t.Errorf("%d vectors, want at least 5000", len(v.Vectors))
	}
	if v.Generator.Kernel == "" || len(v.Generator.Commands) == 0 {
		t.Errorf("generator kernel/commands missing: %+v", v.Generator)
	}
	for _, fs := range []string{"ext4", "f2fs"} {
		if len(v.Generator.ImageSHA[fs]) != 64 {
			t.Errorf("generator.image_sha256[%s] = %q", fs, v.Generator.ImageSHA[fs])
		}
		// the fsck exit status gates the generator: a recorded non-zero (or missing) status is a failed run
		if st, ok := v.Generator.FsckExit[fs]; !ok || st != 0 {
			t.Errorf("generator.fsck_exit[%s] = %d (present %v), want a recorded 0", fs, st, ok)
		}
	}
	type seen struct{ yes, no bool }
	cats := map[string]*seen{}
	exc := map[int]bool{}
	for _, id := range v.Exceptions {
		exc[id] = true
	}
	pairs := map[[2]string]int{}
	for i, r := range v.Vectors {
		if r.ID != i+1 {
			t.Fatalf("row %d has id %d: ids must be unique and dense from 1", i, r.ID)
		}
		if first, dup := pairs[[2]string{r.StoredHex, r.QueryHex}]; dup {
			t.Errorf("vector %d repeats the (stored, query) pair of vector %d", r.ID, first)
		}
		pairs[[2]string{r.StoredHex, r.QueryHex}] = r.ID
		if !r.StoredPresent {
			t.Errorf("vector %d: stored_present is false (a refused stored name records a false negative)", r.ID)
		}
		for what, h := range map[string]string{"stored": r.StoredHex, "query": r.QueryHex} {
			n := decodeName(t, r.ID, what, h)
			if len(n) == 0 || len(n) > 255 {
				t.Errorf("vector %d %s: %d bytes", r.ID, what, len(n))
			}
			if bytes.ContainsAny(n, "/\x00\n") {
				t.Errorf("vector %d %s contains / NUL or newline", r.ID, what)
			}
		}
		if r.Bucket == "" {
			t.Errorf("vector %d has no bucket", r.ID)
		}
		if r.Ext4 != r.F2FS && !exc[r.ID] {
			t.Errorf("vector %d (%s): ext4=%v f2fs=%v disagree and the row is not in exceptions", r.ID, r.Category, r.Ext4, r.F2FS)
		}
		c := cats[r.Category]
		if c == nil {
			c = &seen{}
			cats[r.Category] = c
		}
		if r.Ext4 {
			c.yes = true
		} else {
			c.no = true
		}
	}
	known := map[string]bool{}
	for _, name := range vectorCategories {
		known[name] = true
		c := cats[name]
		switch {
		case c == nil:
			t.Errorf("category %q has no rows", name)
		case !c.yes || !c.no:
			t.Errorf("category %q lacks a true or a false verdict: %+v", name, *c)
		}
	}
	for name := range cats {
		if !known[name] {
			t.Errorf("unknown category %q", name)
		}
	}
	if len(v.Exceptions) > 0 {
		t.Logf("ext4 and f2fs disagree on rows %v (a finding, reported in the task report)", v.Exceptions)
	}
}

// The kernel's rule for invalid UTF-8 is measured from these probes (ruling Z24), so the set must be there:
// every lone continuation byte after x, 0x80 at the start and between letters, a doubled 0x80, 0x80 after a
// multi-byte character and after a combining mark, x\x80 against x\x81, every truncated lead byte.
func TestInvalidProbeSetIsComplete(t *testing.T) {
	v := readVectors(t)
	have := map[[2]string]bool{}
	for _, r := range v.Vectors {
		if r.Category == "invalid" {
			have[[2]string{r.StoredHex, r.QueryHex}] = true
		}
	}
	need := func(stored, query string) {
		t.Helper()
		if !have[[2]string{hex.EncodeToString([]byte(stored)), hex.EncodeToString([]byte(query))}] {
			t.Errorf("invalid probe missing: stored %q query %q", stored, query)
		}
	}
	for b := 0x80; b <= 0xBF; b++ {
		need("x"+string([]byte{byte(b)}), "X"+string([]byte{byte(b)}))
		need("x"+string([]byte{byte(b)}), "x")
		need("x\x80", "x"+string([]byte{byte(b)}))
	}
	for b := 0xC0; b <= 0xFF; b++ {
		need("x"+string([]byte{byte(b)}), "X"+string([]byte{byte(b)}))
		need("x"+string([]byte{byte(b)}), "x")
	}
	need("\x80", "\x80")
	need("\x80x", "\x80X")
	need("a\x80b", "A\x80B")
	need("x\x80\x80", "X\x80\x80")
	need("x\x80\x80", "x\x80")
	need("é\x80", "É\x80")
	need("ạ\x80", "Ạ\x80")
}

type expectDir struct {
	Path        string   `json:"path"`
	Casefold    bool     `json:"casefold"`
	Count       int      `json:"count"`
	NamesSHA256 string   `json:"names_sha256"`
	NamesHex    []string `json:"names_hex"`
	Source      string   `json:"source"`
}

type expectFile struct {
	Generator struct {
		ImageSHA string   `json:"image_sha256"`
		Commands []string `json:"commands"`
	} `json:"generator"`
	Directories   []expectDir `json:"directories"`
	AmbiguousPair *struct {
		Path           string   `json:"path"`
		NamesHex       []string `json:"names_hex"`
		Origin         string   `json:"origin"`
		KernelBehavior string   `json:"kernel_behavior"`
	} `json:"ambiguous_pair"`
}

// namesDigest is the digest the generator records for a directory: SHA-256 over the sorted, de-duplicated
// lower-case hex names, each followed by a newline.
func namesDigest(hexNames []string) string {
	s := append([]string(nil), hexNames...)
	sort.Strings(s)
	h := sha256.New()
	last := ""
	for i, n := range s {
		if i > 0 && n == last {
			continue
		}
		last = n
		_, _ = io.WriteString(h, n+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func gunzipSHA(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(gunzipBounded(t, path))
	return hex.EncodeToString(sum[:])
}

func TestExpectJSONWellFormed(t *testing.T) {
	v := readVectors(t)
	stored := map[string]map[string]bool{} // bucket -> distinct stored names
	for _, r := range v.Vectors {
		if stored[r.Bucket] == nil {
			stored[r.Bucket] = map[string]bool{}
		}
		stored[r.Bucket][r.StoredHex] = true
	}
	for _, fs := range []string{"ext4", "f2fs"} {
		base := filepath.Join("..", fs, "testdata", fs+"-casefold")
		var e expectFile
		b, err := os.ReadFile(base + ".expect.json")
		if err != nil {
			t.Fatalf("%s: %v", fs, err)
		}
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatalf("%s: %v", fs, err)
		}
		if got := gunzipSHA(t, base+".img.gz"); got != e.Generator.ImageSHA {
			t.Errorf("%s: image sha256 %s, expect.json says %s", fs, got, e.Generator.ImageSHA)
		}
		if e.Generator.ImageSHA != v.Generator.ImageSHA[fs] {
			t.Errorf("%s: expect.json image sha256 differs from kernel-vectors.json", fs)
		}
		if len(e.Generator.Commands) == 0 {
			t.Errorf("%s: no generator commands", fs)
		}
		dirs := map[string]expectDir{}
		for _, d := range e.Directories {
			dirs[d.Path] = d
			if d.Source == "" {
				t.Errorf("%s %s: no source", fs, d.Path)
			}
			if len(d.NamesSHA256) != 64 || d.Count <= 0 {
				t.Errorf("%s %s: count %d, names_sha256 %q", fs, d.Path, d.Count, d.NamesSHA256)
			}
			full := d.Path == "/plain" || d.Path == "/ambig"
			if full != (len(d.NamesHex) > 0) {
				t.Errorf("%s %s: full names_hex is for /plain and /ambig only (has %d)", fs, d.Path, len(d.NamesHex))
			}
			for _, h := range d.NamesHex {
				n, err := hex.DecodeString(h)
				if err != nil || len(n) == 0 || len(n) > 255 || bytes.ContainsAny(n, "/\x00\n") {
					t.Errorf("%s %s: bad name %q", fs, d.Path, h)
				}
			}
			if len(d.NamesHex) > 0 {
				if !sort.StringsAreSorted(d.NamesHex) || len(d.NamesHex) != d.Count || namesDigest(d.NamesHex) != d.NamesSHA256 {
					t.Errorf("%s %s: names_hex is not sorted, or disagrees with count/names_sha256", fs, d.Path)
				}
			}
		}
		// each bucket directory holds exactly the distinct stored names of its bucket, byte for byte
		for b, set := range stored {
			d, ok := dirs["/cf/"+b]
			if !ok {
				t.Errorf("%s: directory /cf/%s missing", fs, b)
				continue
			}
			var names []string
			for n := range set {
				names = append(names, n)
			}
			if !d.Casefold || d.Count != len(names) || d.NamesSHA256 != namesDigest(names) {
				t.Errorf("%s: /cf/%s casefold=%v count=%d (want %d) or its names digest differs from the vectors' stored names", fs, b, d.Casefold, d.Count, len(names))
			}
		}
		p, ok := dirs["/plain"]
		if !ok || p.Casefold {
			t.Errorf("%s: /plain missing or casefold: %+v", fs, p)
		}
		var first []string
		for n := range stored["00"] {
			first = append(first, n)
		}
		if ok && p.NamesSHA256 != namesDigest(first) {
			t.Errorf("%s: /plain does not hold the first bucket's names", fs)
		}
		if pr := e.AmbiguousPair; pr != nil {
			if len(pr.NamesHex) != 2 || pr.Path == "" || pr.NamesHex[0] == pr.NamesHex[1] {
				t.Errorf("%s: malformed ambiguous_pair %+v", fs, *pr)
			}
			// the pair is written by a host tool after the guest unmounted, and must say so
			if !strings.Contains(pr.Origin, "host-tool") || !strings.Contains(pr.Origin, "not kernel-written") || pr.KernelBehavior == "" {
				t.Errorf("%s: ambiguous_pair lacks an origin label or the measured kernel behaviour: %+v", fs, *pr)
			}
		}
	}
}

// pythonCommand finds an interpreter: python3, python or `py -3`, each accepted only if it runs and is
// Python 3.8 or newer (which rejects the Windows Store stub).
func pythonCommand() ([]string, bool) {
	for _, c := range [][]string{{"python3"}, {"python"}, {"py", "-3"}} {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		args := append(append([]string(nil), c[1:]...), "-I", "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)")
		if exec.Command(path, args...).Run() == nil {
			return append([]string{path}, c[1:]...), true
		}
	}
	return nil, false
}

func TestPythonCommandFindsAnInterpreter(t *testing.T) {
	// On a machine with `py` or `python3` the reproducibility test below must run, not skip.
	if _, ok := pythonCommand(); !ok {
		t.Skip("no usable Python 3 on this machine")
	}
}

func TestKernelVectorsAreReproducibleFromTheProbeScript(t *testing.T) {
	py, ok := pythonCommand()
	if !ok {
		t.Skip("no usable Python 3 (python3, python or py -3)")
	}
	root := filepath.Join("..", "..", "..")
	script := filepath.Join(root, "tools", "fixtures", "casefold_probes.py")
	// read the inputs through os so the test cache tracks them
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	ucd, _ := filepath.Glob(filepath.Join("testdata", "ucd-12.1.0", "*"))
	for _, f := range ucd {
		if _, err := os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	args := append(append([]string(nil), py[1:]...), "-I", script, "--list")
	out, err := exec.Command(py[0], args...).Output() //nolint:gosec // py comes from LookPath; the script is this repository's own
	if err != nil {
		t.Fatalf("probe script: %v", err)
	}
	var want []struct {
		ID        int    `json:"id"`
		Category  string `json:"category"`
		Bucket    string `json:"bucket"`
		StoredHex string `json:"stored_hex"`
		QueryHex  string `json:"query_hex"`
	}
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatalf("probe list: %v", err)
	}
	v := readVectors(t)
	if len(want) != len(v.Vectors) {
		t.Fatalf("probe script lists %d probes, kernel-vectors.json has %d rows", len(want), len(v.Vectors))
	}
	for i, w := range want {
		g := v.Vectors[i]
		if !reflect.DeepEqual([]any{w.ID, w.Category, w.Bucket, w.StoredHex, w.QueryHex}, []any{g.ID, g.Category, g.Bucket, g.StoredHex, g.QueryHex}) {
			t.Fatalf("row %d differs: script %+v, committed %+v", i, w, g)
		}
	}
}
