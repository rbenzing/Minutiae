package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func relFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestCopyExcludesCasesAndEvidence(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	writeTree(t, src, map[string]string{
		"cases/x/manifest.jsonl":           "evidence",
		".git/config":                      "x",
		"bin/x.exe":                        "x",
		"tool.exe":                         "x",
		"docs/superpowers/plans/p.md":      "x",
		".superpowers/x":                   "x",
		"internal/a/a.go":                  "package a\n",
		"internal/a/cases/deep/secret.bin": "x",
	})
	if err := os.Symlink(filepath.Join(src, "internal"), filepath.Join(src, "link")); err != nil {
		t.Logf("symlinks cannot be created here (%v); the symlink count is not checked", err)
	}
	stats, err := CopyTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := relFiles(t, dst); !slices.Equal(got, []string{"internal/a/a.go"}) {
		t.Errorf("copied %v, want only internal/a/a.go", got)
	}
	if stats.Files != 1 || stats.Tracked {
		t.Errorf("stats %+v", stats)
	}
	if _, err := os.Lstat(filepath.Join(src, "link")); err == nil && stats.Symlinks != 1 {
		t.Errorf("Symlinks = %d, want 1", stats.Symlinks)
	}
}

func gitRepo(t *testing.T, files map[string]string, untracked map[string]string, ignore string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed: the tracked-files copy cannot be tested")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // test helper: fixed git arguments
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	writeTree(t, root, files)
	if ignore != "" {
		writeTree(t, root, map[string]string{".gitignore": ignore})
		files[".gitignore"] = ignore
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	run(append([]string{"add", "--"}, names...)...)
	run("commit", "-q", "-m", "x")
	writeTree(t, root, untracked)
	return root
}

func TestCopyUsesTrackedFilesWhenTheSourceIsAGitTree(t *testing.T) {
	root := gitRepo(t,
		map[string]string{"internal/a/a.go": "package a\n"},
		map[string]string{"cases/x/manifest.jsonl": "evidence", "notes/dump.bin": "device bytes"}, "")
	dst := filepath.Join(t.TempDir(), "copy")
	stats, err := CopyTree(root, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := relFiles(t, dst); !slices.Equal(got, []string{"internal/a/a.go"}) || !stats.Tracked {
		t.Errorf("copied %v (tracked %v), want only the tracked file", got, stats.Tracked)
	}
}

func TestUntrackedCasesNeverReachTheCopy(t *testing.T) {
	root := gitRepo(t,
		map[string]string{"internal/a/a.go": "package a\n"},
		map[string]string{"cases/x/manifest.jsonl": "evidence"}, "cases/\n")
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTree(root, dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range relFiles(t, dst) {
		if strings.Contains(f, "cases") {
			t.Errorf("%s reached the copy", f)
		}
	}
}

func TestCopyReportsATrackedButDeletedFileAsAnError(t *testing.T) {
	root := gitRepo(t, map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, nil, "")
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyTree(root, filepath.Join(t.TempDir(), "copy")); err == nil {
		t.Errorf("a tracked file that is gone was silently skipped")
	}
}

func TestCopyRefusesOver2GiB(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a.go": strings.Repeat("x", 100), "b.go": strings.Repeat("y", 100)})
	old := maxCopyBytes
	maxCopyBytes = 150
	t.Cleanup(func() { maxCopyBytes = old })
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy")); !errors.Is(err, errCopyTooBig) {
		t.Errorf("err = %v, want errCopyTooBig", err)
	}
	maxCopyBytes = 200
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy2")); err != nil {
		t.Errorf("a copy exactly at the cap: %v", err)
	}
}

func TestCopyKeepsFileContentExactly(t *testing.T) {
	src := t.TempDir()
	data := []byte{0, 1, 2, 0xff, '\r', '\n'}
	if err := os.WriteFile(filepath.Join(src, "f.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "f.bin"))
	if !bytes.Equal(got, data) {
		t.Errorf("content changed: %x", got)
	}
}

func TestOutDirInsideTheSourceIsRefused(t *testing.T) {
	src := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	for _, out := range []string{src, filepath.Join(src, "out"), filepath.Join(src, "a", "b")} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q inside the source %q was accepted", out, src)
		}
	}
	if err := CheckOutDir(filepath.Join(t.TempDir(), "out"), src, work); err != nil {
		t.Errorf("a sibling directory was refused: %v", err)
	}
}

func TestOutDirUnderCasesIsRefused(t *testing.T) {
	src := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	base := t.TempDir()
	for _, out := range []string{filepath.Join(base, "cases", "x"), filepath.Join(base, "Cases"), filepath.Join(base, "cases")} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q under a cases directory was accepted", out)
		}
	}
}

func TestOutDirEqualToWorkIsRefused(t *testing.T) {
	src, base := t.TempDir(), t.TempDir()
	if err := CheckOutDir(base, src, base); err == nil {
		t.Errorf("-out equal to the work copy was accepted")
	}
	if err := CheckOutDir("relative/out", src, filepath.Join(base, "w")); err != nil && !strings.Contains(err.Error(), "fuzzcampaign") {
		t.Errorf("unexpected error text %v", err)
	}
}

func TestHostModeIsRefusedOnWindows(t *testing.T) {
	for goos, wantErr := range map[string]bool{"windows": true, "linux": false, "darwin": false} {
		err := HostModeAllowed(goos)
		if (err != nil) != wantErr {
			t.Errorf("HostModeAllowed(%q) = %v, want error %v", goos, err, wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "use -mode docker; see docs/testing.md") {
			t.Errorf("the refusal %q does not carry the pointer", err)
		}
	}
	_ = runtime.GOOS
}

// The name exclusions are a second guard: even a file that is tracked under cases/ is not
// copied.
func TestTrackedFileUnderCasesIsStillExcluded(t *testing.T) {
	root := gitRepo(t,
		map[string]string{"internal/a/a.go": "package a\n", "cases/tracked.txt": "evidence", "x/bin/tool.exe": "x"}, nil, "")
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := CopyTree(root, dst); err != nil {
		t.Fatal(err)
	}
	if got := relFiles(t, dst); !slices.Equal(got, []string{"internal/a/a.go"}) {
		t.Errorf("copied %v, want only internal/a/a.go", got)
	}
}
