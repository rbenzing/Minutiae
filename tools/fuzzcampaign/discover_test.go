package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverFindsTargets(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/b/b_test.go": "package b\nimport \"testing\"\nfunc FuzzB(f *testing.F) {}\nfunc FuzzA(f *testing.F) {}\n",
		"internal/a/a_test.go": "package a\nimport \"testing\"\nfunc FuzzZ(f *testing.F) {}\n",
		"internal/a/x_test.go": "package a_test\nimport t \"testing\"\nfunc FuzzExternal(f *t.F) {}\n",
		"top_test.go":          "package top\nimport \"testing\"\nfunc FuzzTop(f *testing.F) {}\n",
	})
	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{".", "FuzzTop"},
		{"internal/a", "FuzzExternal"},
		{"internal/a", "FuzzZ"},
		{"internal/b", "FuzzA"},
		{"internal/b", "FuzzB"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

func TestDiscoverIgnoresHelpersThatAreNotTargets(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"p/p_test.go": `package p
import "testing"
type R struct{}
func FuzzHelper(f *testing.F, p int) {}
func (r R) FuzzM(f *testing.F) {}
func Fuzzy(f *testing.F) {}
func FuzzNotF(t *testing.T) {}
func Fuzz2(f *testing.F) {}
func FuzzGeneric[T any](f *testing.F) {}
func FuzzTwoNames(a, b *testing.F) {}
func FuzzValue(f testing.F) {}
func FuzzReal(f *testing.F) {}
`,
		"p/helper.go":                  "package p\nimport \"testing\"\nfunc FuzzParse(t *testing.T, p int, remotePath string, db, wal, journal []byte) {}\nfunc FuzzInNonTest(f *testing.F) {}\n",
		"p/testdata/q/q_test.go":       "package q\nimport \"testing\"\nfunc FuzzInTestdata(f *testing.F) {}\n",
		"p/noimport_test.go":           "package p\nfunc FuzzNoImport(f *F) {}\n",
		"p/otherpkg_test.go":           "package p\nimport testing \"example.com/other\"\nfunc FuzzOtherTesting(f *testing.F) {}\n",
		".git/x_test.go":               "package g\nimport \"testing\"\nfunc FuzzGit(f *testing.F) {}\n",
		"docs/x_test.go":               "package g\nimport \"testing\"\nfunc FuzzDocs(f *testing.F) {}\n",
		".superpowers/x_test.go":       "package g\nimport \"testing\"\nfunc FuzzSuper(f *testing.F) {}\n",
		"vendor/v/x_test.go":           "package g\nimport \"testing\"\nfunc FuzzVendor(f *testing.F) {}\n",
		"bin/x_test.go":                "package g\nimport \"testing\"\nfunc FuzzBin(f *testing.F) {}\n",
		"cases/c1/x_test.go":           "package g\nimport \"testing\"\nfunc FuzzCases(f *testing.F) {}\n",
		"internal/decode/d_test.go":    "package d\nimport \"testing\"\nfunc FuzzKept(f *testing.F) {}\n",
		"internal/cases_test.go":       "package d\nimport \"testing\"\nfunc FuzzAlsoKept(f *testing.F) {}\n",
		"internal/docsx/docsx_test.go": "package d\nimport \"testing\"\nfunc FuzzDocsPrefixOnly(f *testing.F) {}\n",
	})
	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{"internal", "FuzzAlsoKept"},
		{"internal/decode", "FuzzKept"},
		{"internal/docsx", "FuzzDocsPrefixOnly"},
		{"p", "Fuzz2"}, // a digit after the prefix is allowed: Fuzz[A-Z0-9_]*
		{"p", "FuzzReal"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

func TestDiscoverHonoursBuildConstraints(t *testing.T) {
	root := t.TempDir()
	fz := func(pkg, name, constraint string) string {
		return constraint + "package " + pkg + "\nimport \"testing\"\nfunc " + name + "(f *testing.F) {}\n"
	}
	writeTree(t, root, map[string]string{
		"c/win_test.go":       fz("c", "FuzzWinTag", "//go:build windows\n\n"),
		"c/hw_test.go":        fz("c", "FuzzHardware", "//go:build hardware\n\n"),
		"c/cgo_test.go":       fz("c", "FuzzCgo", "//go:build cgo\n\n"),
		"c/x_windows_test.go": fz("c", "FuzzWinName", ""),
		"c/x_darwin_test.go":  fz("c", "FuzzDarwinName", ""),
		"c/lin_test.go":       fz("c", "FuzzLinuxNoCgo", "//go:build linux && !cgo\n\n"),
		"c/nowin_test.go":     fz("c", "FuzzNotWindows", "//go:build !windows\n\n"),
		"c/plain_test.go":     fz("c", "FuzzPlain", ""),
	})
	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{{"c", "FuzzLinuxNoCgo"}, {"c", "FuzzNotWindows"}, {"c", "FuzzPlain"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

func TestDiscoverReportsAnUnparsableTestFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"ok/ok_test.go":   "package ok\nimport \"testing\"\nfunc FuzzOK(f *testing.F) {}\n",
		"bad/bad_test.go": "package bad\nfunc (((\n",
	})
	got, err := Discover(root)
	if err == nil || got != nil {
		t.Fatalf("Discover = %v, %v; want a nil result and an error", got, err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *ParseError", err, err)
	}
	if want := filepath.Join(root, "bad", "bad_test.go"); pe.Path != want {
		t.Fatalf("ParseError.Path = %q, want %q", pe.Path, want)
	}
}

func TestDiscoverDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{
		"o_test.go": "package o\nimport \"testing\"\nfunc FuzzOutside(f *testing.F) {}\n",
	})
	writeTree(t, root, map[string]string{
		"real/r_test.go": "package r\nimport \"testing\"\nfunc FuzzReal(f *testing.F) {}\n",
	})
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks cannot be created here: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "o_test.go"), filepath.Join(root, "real", "link_test.go")); err != nil {
		t.Skipf("symlinks cannot be created here: %v", err)
	}
	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Target{{"real", "FuzzReal"}}; !slices.Equal(got, want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
}

func TestDiscoverCapsTheFileCount(t *testing.T) {
	old := maxFiles
	maxFiles = 10
	t.Cleanup(func() { maxFiles = old })
	root := t.TempDir()
	files := map[string]string{}
	for i := range 11 {
		files["d/f"+string(rune('a'+i))+".txt"] = "x"
	}
	writeTree(t, root, files)
	if _, err := Discover(root); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("Discover over the cap: err = %v, want ErrTooManyFiles", err)
	}
	// Exactly at the cap is accepted.
	root2 := t.TempDir()
	files2 := map[string]string{}
	for i := range 10 {
		files2["d/f"+string(rune('a'+i))+".txt"] = "x"
	}
	writeTree(t, root2, files2)
	if _, err := Discover(root2); err != nil {
		t.Fatalf("Discover at the cap: %v", err)
	}
}

func TestDiscoverOnTheRepository(t *testing.T) {
	got, err := Discover(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []Target{
		{"internal/decode/plist", "FuzzPlist"},
		{"internal/decode/sqlitedb", "FuzzSQLiteDB"},
		{"internal/decode/ts", "FuzzTS"},
		{"internal/decode/typedstream", "FuzzTypedstream"},
	} {
		if !slices.Contains(got, w) {
			t.Errorf("Discover on the repository misses %v", w)
		}
	}
	if len(got) < 40 {
		t.Errorf("Discover on the repository found %d targets, want at least 40", len(got))
	}
	if !slices.IsSortedFunc(got, cmpTarget) {
		t.Errorf("the result is not sorted")
	}
}
