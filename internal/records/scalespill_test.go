package records_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The scale harness measures, per phase, what the process left in the OS temp directory and its peak
// memory (rulings D46, D50). The arithmetic and the platform reads are plain functions pinned here,
// so the harness (scale_test.go, build tag scale) needs no 10 million record case to be trusted.

// snapshotTempDir lists every file under dir with its size, keyed by its path relative to dir. A
// missing directory is an empty listing.
func snapshotTempDir(dir string) (map[string]int64, error) {
	out := map[string]int64{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = fi.Size()
		return nil
	})
	return out, err
}

// diffTempListings names what appeared in and what vanished from a directory between two listings.
// A file whose size changed counts as added (it was rewritten). Both results are sorted.
func diffTempListings(before, after map[string]int64) (added, removed []string) {
	for n, sz := range after {
		if old, ok := before[n]; !ok || old != sz {
			added = append(added, n)
		}
	}
	for n := range before {
		if _, ok := after[n]; !ok {
			removed = append(removed, n)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

func TestSnapshotTempDirListsFilesWithSizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"a": 3, "sub/b": 5} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := snapshotTempDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"] != 3 || got["sub/b"] != 5 {
		t.Fatalf("listing = %v, want a=3 and sub/b=5", got)
	}
	missing, err := snapshotTempDir(filepath.Join(dir, "nope"))
	if err != nil || len(missing) != 0 {
		t.Fatalf("a missing directory: %v, %v; want an empty listing", missing, err)
	}
}

func TestDiffTempListingsNamesWhatChanged(t *testing.T) {
	before := map[string]int64{"keep": 1, "gone": 2, "grown": 3}
	after := map[string]int64{"keep": 1, "grown": 9, "etilqs_x": 4}
	added, removed := diffTempListings(before, after)
	if !slices.Equal(added, []string{"etilqs_x", "grown"}) || !slices.Equal(removed, []string{"gone"}) {
		t.Fatalf("added %v removed %v", added, removed)
	}
	if a, r := diffTempListings(before, before); len(a) != 0 || len(r) != 0 {
		t.Fatalf("equal listings differ: %v %v", a, r)
	}
}

func TestPeakRSSIsReadOnThisPlatform(t *testing.T) {
	v, ok := peakRSS()
	if !ok {
		t.Skip("peak resident memory is not read on this platform; the harness records it as not measured")
	}
	if v < 4<<20 {
		t.Fatalf("peak RSS %d bytes is below 4 MiB: not a process high-water mark", v)
	}
	// a high-water mark never falls (the package's other tests may have raised it far above any block a
	// test could allocate here, so growth itself cannot be asserted)
	buf := make([]byte, 16<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	v2, ok := peakRSS()
	if !ok || v2 < v {
		t.Fatalf("peak RSS went from %d to %d (ok %v): a high-water mark never falls", v, v2, ok)
	}
	// the peak is a high-water mark of the process, so it is at least the heap the runtime holds in use
	// right now (read just before the peak): a constant or a wrong counter fails this (D64)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	v3, ok := peakRSS()
	if !ok || v3 < int64(ms.HeapInuse) {
		t.Fatalf("peak RSS %d (ok %v) is below runtime HeapInuse %d: not a process high-water mark", v3, ok, ms.HeapInuse)
	}
	runtimeKeepAlive(buf)
}

// pathWithin reports whether p is root or lies under it (after cleaning; no symlinks are resolved).
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestPathWithinRequiresTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scale")
	cases := []struct {
		p    string
		want bool
	}{
		{filepath.Join(root, "tmp"), true},
		{root, true},
		{filepath.Join(root, "a", "..", "tmp"), true},
		{root + "-other", false},
		{filepath.Dir(root), false},
		{filepath.Join(root, "..", "x"), false},
		{filepath.Join(root, "..tmp"), true}, // a name that merely starts with two dots is inside the root
	}
	for _, tc := range cases {
		if got := pathWithin(root, tc.p); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", root, tc.p, got, tc.want)
		}
	}
}

// scaleResult is one scale run's record (the harness in scale_test.go, build tag scale, fills it).
type scaleResult struct {
	Label   string             `json:"label"`
	Commit  string             `json:"commit"`
	Machine string             `json:"machine"`
	TempDir string             `json:"os_temp_dir"`
	Records int64              `json:"records"`
	Values  map[string]float64 `json:"values"`
	Plans   map[string]string  `json:"plans,omitempty"`
	Phases  map[string]float64 `json:"verify_phases_s,omitempty"`
}

// newScaleResult starts a run record. The commit is passed in; the measured OS temp directory is the
// one the ostemp_added_<phase> values were taken in (D61).
func newScaleResult(label, commit, machine string, n int64) *scaleResult {
	return &scaleResult{
		Label: label, Commit: commit, Machine: machine, TempDir: os.TempDir(), Records: n,
		Values: map[string]float64{}, Plans: map[string]string{}, Phases: map[string]float64{},
	}
}

func TestRunRecordNamesTheMeasuredOSTempDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMPDIR", dir)
	r := newScaleResult("l", "c", "m", 1)
	if r.TempDir != os.TempDir() || r.TempDir != dir {
		t.Fatalf("run record names os temp dir %q, want the measured %q", r.TempDir, dir)
	}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"os_temp_dir":`+strconv.Quote(dir)) {
		t.Fatalf("json %s (err %v) lacks os_temp_dir", b, err)
	}
}
