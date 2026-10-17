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
	"testing"
)

// The categories the probe script (tools/fixtures/casefold_probes.py) emits.
// Negative controls and identity controls live in the category they control,
// so every category has both a true and a false verdict.
var vectorCategories = []string{
	"casefold", "decomposition", "ignorable", "unmapped", "reorder",
	"special", "post-12.1", "invalid",
}

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
		Kernel   string            `json:"kernel"`
		Unicode  string            `json:"unicode"`
		ImageSHA map[string]string `json:"image_sha256"`
		Commands []string          `json:"commands"`
	} `json:"generator"`
	Vectors    []vectorRow `json:"vectors"`
	Exceptions []int       `json:"exceptions"`
}

func readVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "kernel-vectors.json"))
	if err != nil {
		t.Fatalf("kernel-vectors.json missing: %v (regenerate with tools/fixtures/gen.sh casefold)", err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("kernel-vectors.json: %v", err)
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
	}
	type seen struct{ yes, no bool }
	cats := map[string]*seen{}
	exc := map[int]bool{}
	for _, id := range v.Exceptions {
		exc[id] = true
	}
	for i, r := range v.Vectors {
		if r.ID != i+1 {
			t.Fatalf("row %d has id %d: ids must be unique and dense from 1", i, r.ID)
		}
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

type expectDir struct {
	Path     string   `json:"path"`
	Casefold bool     `json:"casefold"`
	NamesHex []string `json:"names_hex"`
	Source   string   `json:"source"`
}

type expectFile struct {
	Generator struct {
		ImageSHA string   `json:"image_sha256"`
		Commands []string `json:"commands"`
	} `json:"generator"`
	Directories   []expectDir `json:"directories"`
	AmbiguousPair *struct {
		Path     string   `json:"path"`
		NamesHex []string `json:"names_hex"`
	} `json:"ambiguous_pair"`
}

func gunzipSHA(t *testing.T, path string) string {
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
	h := sha256.New()
	// the committed images are 32 and 128 MiB: a larger stream is a defect, not a reason to read on
	n, err := io.Copy(h, io.LimitReader(zr, 1<<30+1))
	if err == nil && n > 1<<30 {
		err = io.ErrShortBuffer
	}
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestExpectJSONWellFormed(t *testing.T) {
	v := readVectors(t)
	buckets := map[string]bool{}
	for _, r := range v.Vectors {
		buckets[r.Bucket] = true
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
			for _, h := range d.NamesHex {
				n, err := hex.DecodeString(h)
				if err != nil || len(n) == 0 || len(n) > 255 || bytes.ContainsAny(n, "/\x00\n") {
					t.Errorf("%s %s: bad name %q", fs, d.Path, h)
				}
			}
			if !sort.StringsAreSorted(d.NamesHex) {
				t.Errorf("%s %s: names are not sorted", fs, d.Path)
			}
		}
		for b := range buckets {
			d, ok := dirs["/cf/"+b]
			switch {
			case !ok:
				t.Errorf("%s: directory /cf/%s missing", fs, b)
			case !d.Casefold || len(d.NamesHex) == 0:
				t.Errorf("%s: /cf/%s casefold=%v names=%d", fs, b, d.Casefold, len(d.NamesHex))
			}
		}
		if p, ok := dirs["/plain"]; !ok || p.Casefold || len(p.NamesHex) == 0 {
			t.Errorf("%s: /plain missing, casefold or empty: %+v", fs, p)
		}
		// every stored spelling of every row is present byte for byte in its bucket directory
		for _, r := range v.Vectors {
			d := dirs["/cf/"+r.Bucket]
			i := sort.SearchStrings(d.NamesHex, r.StoredHex)
			if i == len(d.NamesHex) || d.NamesHex[i] != r.StoredHex {
				t.Errorf("%s: vector %d stored name %s is not in /cf/%s", fs, r.ID, r.StoredHex, r.Bucket)
				break
			}
		}
		if p := e.AmbiguousPair; p != nil && (len(p.NamesHex) != 2 || p.Path == "" || p.NamesHex[0] == p.NamesHex[1]) {
			t.Errorf("%s: malformed ambiguous_pair %+v", fs, *p)
		}
	}
}

func TestKernelVectorsAreReproducibleFromTheProbeScript(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if out, err := exec.Command(py, "-I", "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)").CombinedOutput(); err != nil {
		t.Skipf("python3 is not usable: %v %s", err, out)
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
	out, err := exec.Command(py, "-I", script, "--list").Output()
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
