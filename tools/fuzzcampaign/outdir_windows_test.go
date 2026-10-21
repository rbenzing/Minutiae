//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Task 3d: a junction into a subdirectory of the source (not the root, not the work copy) is
// refused too: the final path of the deepest existing ancestor is compared by containment.
func TestOutDirThroughAJunctionIntoASourceSubdirIsRefused(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	toSub := filepath.Join(base, "to-sub")
	mkJunction(t, toSub, sub)
	for _, out := range []string{toSub, filepath.Join(toSub, "new"), filepath.Join(toSub, "deep"), filepath.Join(toSub, "new", "deeper")} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q reaches a subdirectory of the source through a junction and was accepted", out)
		}
	}
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	toOther := filepath.Join(base, "to-other")
	mkJunction(t, toOther, other)
	if err := CheckOutDir(filepath.Join(toOther, "new"), src, work); err != nil {
		t.Errorf("a junction to an unrelated directory was refused: %v", err)
	}
}

func TestStripExtendedPrefix(t *testing.T) {
	sep := string(filepath.Separator)
	ext := sep + sep + "?" + sep
	for in, want := range map[string]string{
		ext + "C:" + sep + "a":                             "C:" + sep + "a",
		ext + "UNC" + sep + "srv" + sep + "sh" + sep + "a": sep + sep + "srv" + sep + "sh" + sep + "a",
		"C:" + sep + "plain":                               "C:" + sep + "plain",
	} {
		if got := stripExtendedPrefix(in); got != want {
			t.Errorf("stripExtendedPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// V75: a UNC -out is refused whether or not the share can be reached (the check cannot compare
// identity across a share), and so is an -out whose drive does not exist (no ancestor resolves).
func TestOutDirOnAUNCPathOrAnUnresolvableDriveIsRefused(t *testing.T) {
	base := t.TempDir()
	src, work := filepath.Join(base, "src"), filepath.Join(base, "work")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		"\\\\localhost\\C$\\fuzz-out", "\\\\localhost\\C$", "\\\\?\\UNC\\localhost\\C$\\fuzz-out", "\\\\no-such-host-lx8\\share\\out",
	} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q is a UNC path and was accepted", out)
		}
	}
	for letter := 'Z'; letter >= 'D'; letter-- {
		drive := string(letter) + `:\`
		if _, err := os.Stat(drive); err == nil {
			continue
		}
		if err := CheckOutDir(drive+`fuzz-out`, src, work); err == nil {
			t.Errorf("-out on the missing drive %s was accepted", drive)
		}
		return
	}
	t.Skip("every drive letter is in use")
}

func TestIsUNC(t *testing.T) {
	for in, want := range map[string]bool{
		"//localhost/C$/x":    true,
		"//srv/share":         true,
		"//?/UNC/srv/share/x": true,
		"//?/C:/x":            false,
		"//./C:/x":            false,
		"C:/x":                false,
		"relative/x":          false,
	} {
		p := strings.ReplaceAll(in, "/", string(filepath.Separator))
		if got := isUNC(p); got != want {
			t.Errorf("isUNC(%q) = %v, want %v", p, got, want)
		}
	}
}
