package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLocalPathsCollision(t *testing.T) {
	l := NewLocalPaths("files")
	dir, err := l.Dir("/sdcard/DCIM")
	if err != nil || dir != "files/sdcard/DCIM" {
		t.Fatalf("Dir = %q, %v", dir, err)
	}
	// Case-insensitive collision between directories.
	if d2, _ := l.Dir("/sdcard/dcim"); d2 != "files/sdcard/dcim~2" {
		t.Fatalf("case-folded dir = %q", d2)
	}
	// Repeating a directory returns the same assignment.
	if again, _ := l.Dir("/sdcard/DCIM/"); again != dir {
		t.Fatalf("repeat Dir = %q, want %q", again, dir)
	}
	steps := []struct{ parent, name, want string }{
		{dir, "File.txt", dir + "/File.txt"},
		{dir, "file.txt", dir + "/file~2.txt"},
		{dir, "FILE.TXT", dir + "/FILE~3.TXT"},
		{dir, ".profile", dir + "/.profile"},
		{dir, ".PROFILE", dir + "/.PROFILE~2"},
		{dir, "a:b", dir + "/a_b"},
		{dir, "a_b", dir + "/a_b~2"},
	}
	for _, s := range steps {
		got, err := l.File(s.parent, s.name)
		if err != nil || got != s.want {
			t.Errorf("File(%q, %q) = %q, %v; want %q", s.parent, s.name, got, err, s.want)
		}
	}
	// A directory takes a "~N" suffix after the whole name, even with a dot.
	if _, err := l.File("files", "x.d"); err != nil {
		t.Fatal(err)
	}
	if d, _ := l.Dir("/X.D"); d != "files/X.D~2" {
		t.Fatalf("dir with dot = %q", d)
	}
	if _, err := l.File(dir, ".."); err == nil {
		t.Error("escaping name accepted")
	}
}

func TestNewLocalPathsRoot(t *testing.T) {
	l := NewLocalPaths("x")
	if d, err := l.Dir("/"); err != nil || d != "x" {
		t.Fatalf("root Dir = %q, %v", d, err)
	}
	if d, err := l.Dir("/a/b"); err != nil || d != "x/a/b" {
		t.Fatalf("Dir = %q, %v", d, err)
	}
	if f, err := l.File("x", "f"); err != nil || f != "x/f" {
		t.Fatalf("File = %q, %v", f, err)
	}
}

func TestWithSuffixAndFoldCase(t *testing.T) {
	for in, want := range map[string]string{"a.txt": "a~2.txt", ".profile": ".profile~2", "noext": "noext~2", "x.tar.gz": "x.tar~2.gz"} {
		if got := withSuffix(in, 2, false); got != want {
			t.Errorf("withSuffix(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withSuffix("a.d", 3, true); got != "a.d~3" {
		t.Errorf("directory suffix = %q", got)
	}
	for _, pair := range [][2]string{{"DCIM/File.TXT", "dcim/file.txt"}, {"Σ", "ς"}, {"K", "K"}} {
		if FoldCase(pair[0]) != FoldCase(pair[1]) {
			t.Errorf("FoldCase(%q) != FoldCase(%q)", pair[0], pair[1])
		}
	}
	if FoldCase("a.txt") == FoldCase("b.txt") {
		t.Error("distinct names fold equal")
	}
}

func TestLocalPathsCapsLongComponents(t *testing.T) {
	const prefix = "~raw~"
	long1 := prefix + strings.Repeat("A", 340) // 345 bytes
	long2 := prefix + strings.Repeat("A", 339) + "B"
	cjk := strings.Repeat("日", 85) // 255 bytes
	l := NewLocalPaths("files")
	dir, err := l.Dir("/d")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, name := range []string{long1, long2, cjk, cjk + ".txt"} {
		p, err := l.File(dir, name)
		if err != nil {
			t.Fatalf("File(%d bytes) = %v", len(name), err)
		}
		comp := path.Base(p)
		if len(comp) > maxComponentBytes {
			t.Errorf("component of %d bytes exceeds %d: %q", len(comp), maxComponentBytes, comp)
		}
		if !utf8.ValidString(comp) {
			t.Errorf("truncation split a rune: %q", comp)
		}
		got[name] = p
	}
	if got[long1] == got[long2] {
		t.Errorf("two long names sharing a 200-byte prefix got the same local name %q", got[long1])
	}
	if !strings.HasPrefix(path.Base(got[long1]), prefix+"AAAA") || !strings.Contains(path.Base(got[long1]), "~") {
		t.Errorf("long name lost its prefix or hash suffix: %q", got[long1])
	}
	sum := sha256.Sum256([]byte(long1))
	if want := "~" + hex.EncodeToString(sum[:4]); !strings.HasSuffix(got[long1], want) {
		t.Errorf("%q does not end with the hash suffix %q", got[long1], want)
	}
	// Deterministic: the same name in a fresh assigner maps to the same component.
	l2 := NewLocalPaths("files")
	d2, _ := l2.Dir("/d")
	if again, _ := l2.File(d2, long1); again != got[long1] {
		t.Errorf("not deterministic: %q vs %q", again, got[long1])
	}
	// Long directory names are capped too; short names are untouched.
	if d, err := l.Dir("/" + long1 + "/sub"); err != nil || len(path.Base(path.Dir(d))) > maxComponentBytes {
		t.Errorf("long directory = %q, %v", d, err)
	}
	if p, _ := l.File(dir, strings.Repeat("s", maxComponentBytes)); path.Base(p) != strings.Repeat("s", maxComponentBytes) {
		t.Errorf("a name of exactly %d bytes was changed: %q", maxComponentBytes, p)
	}
}
