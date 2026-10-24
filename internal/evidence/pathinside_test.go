package evidence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathInside(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "case")
	other := filepath.Join(root, "other")
	for _, p := range []string{d, other} {
		if err := os.Mkdir(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]bool{
		d:                                true,
		filepath.Join(d, "a", "b"):       true, // not yet existing
		filepath.Join(d, "x", "..", "a"): true,
		filepath.Join(d, "..", "other"):  false,
		d + "-sibling":                   false,
		filepath.Join(other, "f"):        false,
	}
	// a symlink outside that points into the case, and a dangling one
	if err := os.Symlink(d, filepath.Join(other, "link")); err == nil {
		cases[filepath.Join(other, "link", "f")] = true
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(other, "dangling")); err == nil {
		cases[filepath.Join(other, "dangling", "f")] = false
	}
	for p, want := range cases {
		if got, err := PathInside(d, p); err != nil || got != want {
			t.Errorf("PathInside(%q) = %v, %v; want %v", p, got, err, want)
		}
	}
}
