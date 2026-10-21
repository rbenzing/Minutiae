package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// corpusFile writes a file of the given size and sets its modification time.
func corpusFile(t *testing.T, dir, name string, size int, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

var corpusBase = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestPruneCorpusRemovesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	// mtimes (seconds): a=4 b=0 c=2 d=1 e=3
	for name, sec := range map[string]int{"a": 4, "b": 0, "c": 2, "d": 1, "e": 3} {
		corpusFile(t, dir, name, 1, corpusBase.Add(time.Duration(sec)*time.Second))
	}
	removed, err := PruneCorpus(dir, 2, 1<<20)
	if err != nil || removed != 3 {
		t.Fatalf("PruneCorpus = %d, %v; want 3, nil", removed, err)
	}
	if got, want := listDir(t, dir), []string{"a", "e"}; !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
	// Equal mtimes: the name decides, the lower name goes first.
	dir = t.TempDir()
	for _, name := range []string{"m", "k", "l"} {
		corpusFile(t, dir, name, 1, corpusBase)
	}
	if removed, err = PruneCorpus(dir, 1, 1<<20); err != nil || removed != 2 {
		t.Fatalf("PruneCorpus = %d, %v; want 2, nil", removed, err)
	}
	if got, want := listDir(t, dir), []string{"m"}; !slices.Equal(got, want) {
		t.Errorf("tie left %v, want %v", got, want)
	}
}

func TestPruneCorpusRespectsBothCaps(t *testing.T) {
	fill := func() string {
		dir := t.TempDir()
		for i, name := range []string{"f1", "f2", "f3", "f4", "f5", "f6"} {
			corpusFile(t, dir, name, 10, corpusBase.Add(time.Duration(i)*time.Second))
		}
		return dir
	}
	// The byte cap binds: 25 bytes hold two of the 10 byte files.
	dir := fill()
	removed, err := PruneCorpus(dir, 5, 25)
	if err != nil || removed != 4 {
		t.Fatalf("bytes cap: %d, %v; want 4, nil", removed, err)
	}
	if got, want := listDir(t, dir), []string{"f5", "f6"}; !slices.Equal(got, want) {
		t.Errorf("bytes cap left %v, want %v", got, want)
	}
	// The count cap binds.
	dir = fill()
	if removed, err = PruneCorpus(dir, 3, 1<<20); err != nil || removed != 3 {
		t.Fatalf("count cap: %d, %v; want 3, nil", removed, err)
	}
	if got, want := listDir(t, dir), []string{"f4", "f5", "f6"}; !slices.Equal(got, want) {
		t.Errorf("count cap left %v, want %v", got, want)
	}
	// Exactly at both caps nothing goes.
	dir = fill()
	if removed, err = PruneCorpus(dir, 6, 60); err != nil || removed != 0 {
		t.Fatalf("at the caps: %d, %v; want 0, nil", removed, err)
	}
	// A zero count cap empties the directory.
	dir = fill()
	if removed, err = PruneCorpus(dir, 0, 1<<20); err != nil || removed != 6 {
		t.Fatalf("zero cap: %d, %v; want 6, nil", removed, err)
	}
	// A negative cap is an error and removes nothing.
	dir = fill()
	if _, err = PruneCorpus(dir, -1, 10); err == nil {
		t.Errorf("a negative count cap was accepted")
	}
	if _, err = PruneCorpus(dir, 1, -1); err == nil {
		t.Errorf("a negative byte cap was accepted")
	}
	if got := listDir(t, dir); len(got) != 6 {
		t.Errorf("a refused call removed files: %v", got)
	}
}

func TestPruneCorpusRefusesUnsafeDirs(t *testing.T) {
	base := t.TempDir()
	committed := filepath.Join(base, "pkg", "testdata", "fuzz", "FuzzX")
	if err := os.MkdirAll(committed, 0o750); err != nil {
		t.Fatal(err)
	}
	corpusFile(t, committed, "seed", 1, corpusBase)
	testdataItself := filepath.Join(base, "pkg", "testdata")
	for _, dir := range []string{"", "/", `C:\`, "x", "x/y", "/a/b", `\a\b`, ".", "..", committed, testdataItself, filepath.Join(base, "pkg", "TestData", "fuzz", "FuzzX")} {
		removed, err := PruneCorpus(dir, 0, 0)
		if !errors.Is(err, ErrUnsafeDir) || removed != 0 {
			t.Errorf("PruneCorpus(%q) = %d, %v; want 0, ErrUnsafeDir", dir, removed, err)
		}
	}
	if got := listDir(t, committed); !slices.Equal(got, []string{"seed"}) {
		t.Errorf("the committed corpus was touched: %v", got)
	}
}

func TestPruneCorpusLeavesSymlinksAndDirectories(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	corpusFile(t, outside, "target", 1, corpusBase)
	corpusFile(t, dir, "old", 1, corpusBase)
	corpusFile(t, dir, "new", 1, corpusBase.Add(time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	corpusFile(t, filepath.Join(dir, "sub"), "inner", 1, corpusBase)
	linked := false
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(dir, "linkfile")); err == nil {
		linked = true
	} else {
		t.Logf("file symlinks cannot be created here: %v", err)
	}
	removed, err := PruneCorpus(dir, 1, 1<<20)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	var sk *SkippedError
	if !errors.As(err, &sk) {
		t.Fatalf("err = %v, want a *SkippedError", err)
	}
	wantLinks := 0
	if linked {
		wantLinks = 1
	}
	if sk.Dirs != 1 || sk.Symlinks != wantLinks || sk.Unreadable != 0 {
		t.Errorf("SkippedError = %+v, want 1 dir, %d symlinks, 0 unreadable", sk, wantLinks)
	}
	got := listDir(t, dir)
	want := []string{"new", "sub"}
	if linked {
		want = []string{"linkfile", "new", "sub"}
	}
	if !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "inner")); err != nil {
		t.Errorf("a file inside a subdirectory was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "target")); err != nil {
		t.Errorf("a symlink target was touched: %v", err)
	}
}

func TestPruneCorpusOnAnEmptyDir(t *testing.T) {
	removed, err := PruneCorpus(t.TempDir(), 0, 0)
	if err != nil || removed != 0 {
		t.Errorf("PruneCorpus on an empty dir = %d, %v; want 0, nil", removed, err)
	}
	if _, err := PruneCorpus(filepath.Join(t.TempDir(), "missing"), 1, 1); err == nil {
		t.Errorf("a missing directory was not an error")
	}
}

func TestPruneCorpusRefusesRelativeAndDotDotPaths(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	corpusFile(t, deep, "keep", 1, corpusBase)
	sep := string(os.PathSeparator)
	// The same directory spelled with a .. element: it resolves, but is refused.
	dotted := base + sep + "a" + sep + "x" + sep + ".." + sep + "b" + sep + "c"
	if err := os.MkdirAll(filepath.Join(base, "a", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base) // so that a/b/c names the directory relatively, with no .. element
	rel := filepath.Join("a", "b", "c")
	for _, dir := range []string{dotted, rel} {
		if removed, err := PruneCorpus(dir, 0, 0); !errors.Is(err, ErrUnsafeDir) || removed != 0 {
			t.Errorf("PruneCorpus(%q) = %d, %v; want 0, ErrUnsafeDir", dir, removed, err)
		}
	}
	if got := listDir(t, deep); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("a refused call touched the directory: %v", got)
	}
}

func TestPruneCorpusRefusesASymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real", "corpus", "x")
	if err := os.MkdirAll(realDir, 0o750); err != nil {
		t.Fatal(err)
	}
	corpusFile(t, realDir, "keep", 1, corpusBase)
	link := filepath.Join(base, "link", "to", "corpus")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks cannot be created here: %v", err)
	}
	if removed, err := PruneCorpus(link, 0, 0); !errors.Is(err, ErrUnsafeDir) || removed != 0 {
		t.Errorf("PruneCorpus(symlink) = %d, %v; want 0, ErrUnsafeDir", removed, err)
	}
	if got := listDir(t, realDir); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("the symlink target was touched: %v", got)
	}
}

// V42: a symlink in the middle of the path is as dangerous as one at the leaf: the leaf is a
// plain directory, but the path leads somewhere else.
func TestPruneCorpusRefusesASymlinkedIntermediateDirectory(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real", "a", "b", "c")
	if err := os.MkdirAll(realDir, 0o750); err != nil {
		t.Fatal(err)
	}
	corpusFile(t, realDir, "keep", 1, corpusBase)
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Skipf("symlinks cannot be created here: %v", err)
	}
	via := filepath.Join(base, "link", "a", "b", "c")
	if removed, err := PruneCorpus(via, 0, 0); !errors.Is(err, ErrUnsafeDir) || removed != 0 {
		t.Errorf("PruneCorpus(%q) = %d, %v; want 0, ErrUnsafeDir", via, removed, err)
	}
	if got := listDir(t, realDir); !slices.Equal(got, []string{"keep"}) {
		t.Errorf("the target of the symlinked path was touched: %v", got)
	}
}
