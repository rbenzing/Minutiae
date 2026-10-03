package evidence

import "testing"

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
		if foldCase(pair[0]) != foldCase(pair[1]) {
			t.Errorf("foldCase(%q) != foldCase(%q)", pair[0], pair[1])
		}
	}
	if foldCase("a.txt") == foldCase("b.txt") {
		t.Error("distinct names fold equal")
	}
}
