//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mkJunction creates a directory junction (no privilege needed, unlike a symlink) and fails the
// test when it cannot: there is no skip, the junction case is the point.
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target) //nolint:gosec // test-created paths
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
	t.Cleanup(func() { _ = os.Remove(link) }) // removes the junction only, never its target
}

// V71: EvalSymlinks does not resolve a junction on Windows, so -out is compared by identity.
func TestOutDirThroughAJunctionIsRefused(t *testing.T) {
	base := t.TempDir()
	src, cases, other := filepath.Join(base, "src"), filepath.Join(base, "cases"), filepath.Join(base, "other")
	for _, d := range []string{src, cases, other, filepath.Join(src, "inner")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	toSrc, toCases, toWork, toOther := filepath.Join(base, "to-src"), filepath.Join(base, "to-cases"), filepath.Join(base, "to-work"), filepath.Join(base, "to-other")
	mkJunction(t, toSrc, src)
	mkJunction(t, toCases, cases)
	mkJunction(t, toWork, work)
	mkJunction(t, toOther, other)
	for _, out := range []string{
		toSrc, filepath.Join(toSrc, "new", "deeper"), filepath.Join(toSrc, "inner"),
		toCases, filepath.Join(toCases, "x"), toWork, filepath.Join(toWork, "x"),
	} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q reaches the source, the work copy or a cases directory through a junction and was accepted", out)
		}
	}
	// A junction to an unrelated directory, and a path under it, are fine and get no spurious error.
	for _, out := range []string{toOther, filepath.Join(toOther, "new"), filepath.Join(toOther, "new", "deeper")} {
		if err := CheckOutDir(out, src, work); err != nil {
			t.Errorf("-out %q is under a junction to an unrelated directory and was refused: %v", out, err)
		}
	}
}
