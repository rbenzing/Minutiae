package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two registry entries with one name@version are refused up front by -write and -check, before any
// lock line is written.
func TestDuplicateRegistryEntriesAreRefusedUpFront(t *testing.T) {
	dup := []Entry{
		{Name: "p", Version: "1.0.0", Package: testModule + "/internal/parsers/p"},
		{Name: "p", Version: "1.0.0", Package: testModule + "/internal/parsers/p"},
	}
	for _, flag := range []string{"-write", "-check"} {
		root := parserTree(t, pSrc)
		code, out, e := runCmd(t, root, dup, flag)
		if code == 0 || !strings.Contains(e, "p@1.0.0") || !strings.Contains(e, "more than once") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", flag, code, out, e)
		}
		if lock := readTreeFile(t, root, lockRel); strings.Contains(lock, "p 1.0.0") {
			t.Errorf("%s wrote a lock line for a duplicate entry:\n%s", flag, lock)
		}
	}
}

// A legacy "// +build" line changes which files the compiler uses where the go command still
// honours it, so it is hashed like a //go: directive.
func TestLegacyBuildConstraintIsHashed(t *testing.T) {
	a, err := NormalizeGo([]byte("// +build ignore\n\npackage p\n\nfunc F() {}\n"), "a.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeGo([]byte("package p\n\nfunc F() {}\n"), "b.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Error("a // +build line does not change the dump")
	}
}

// An embedded data file whose name ends in .go is data: it is hashed as raw bytes, not normalised as
// Go; only the files the package compiles are normalised.
func TestEmbeddedGoNamedDataIsHashedRaw(t *testing.T) {
	files := map[string]string{
		"internal/parsers/p/p.go":     "package p\n\nimport _ \"embed\"\n\n//go:embed table.go\nvar t string\n",
		"internal/parsers/p/table.go": "//go:build ignore\n\npackage data\n\nvar X = 1\n",
	}
	h1 := hashTree(t, writeTree(t, files), "internal/parsers/p")
	files["internal/parsers/p/table.go"] = "//go:build ignore\n\npackage data\n\nvar  X  =  1\n"
	if h2 := hashTree(t, writeTree(t, files), "internal/parsers/p"); h1 == h2 {
		t.Error("a whitespace change of an embedded data file is invisible to the hash (it was normalised as Go)")
	}
}

func readTreeFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // test temp tree
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
