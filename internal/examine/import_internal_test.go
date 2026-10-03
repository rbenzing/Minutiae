package examine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrimExtendedPrefix(t *testing.T) {
	for in, want := range map[string]string{
		`\?\C:\case\x`:         `C:\case\x`,
		`\?\UNC\srv\share\x`:   `\srv\share\x`,
		`C:\plain`:             `C:\plain`,
		`\srv\share\x`:         `\srv\share\x`,
		`/unix/path`:           `/unix/path`,
		`\?\`:                  ``,
		`\?\UNC\`:              `\`,
		`\.\C:\device-style\x`: `\.\C:\device-style\x`,
	} {
		if got := trimExtendedPrefix(in); got != want {
			t.Errorf("trimExtendedPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasAncestorSameFile(t *testing.T) {
	base := t.TempDir()
	caseDir := filepath.Join(base, "case")
	sub := filepath.Join(caseDir, "a", "b")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "f.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.bin")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasAncestorSameFile(caseDir, file) || !hasAncestorSameFile(caseDir, caseDir) {
		t.Error("a file below the case, and the case itself, must match")
	}
	if hasAncestorSameFile(caseDir, outside) {
		t.Error("a file outside the case matched")
	}
	// An alias spelled differently from the case path (here a symlinked
	// directory) is the same file as the case directory.
	link := filepath.Join(base, "alias")
	if err := os.Symlink(caseDir, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	aliased := filepath.Join(link, "a", "b", "f.bin")
	if !hasAncestorSameFile(caseDir, aliased) {
		t.Error("a path through an alias of the case directory did not match")
	}
	if !insideCase(caseDir, aliased) {
		t.Error("insideCase missed the aliased path")
	}
}
