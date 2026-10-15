package records_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	}
	for _, tc := range cases {
		if got := pathWithin(root, tc.p); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", root, tc.p, got, tc.want)
		}
	}
}
