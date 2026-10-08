package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dbRowFree are the trees whose non-test files must not name the db_row record type: it belongs to
// internal/dbrecover (recovered raw rows), never to a parser or a record-type package.
var dbRowFree = []string{"internal/parsers", "internal/recordtypes"}

// mentionsDBRow reports whether src holds the quoted string "db_row".
func mentionsDBRow(src string) bool {
	return strings.Contains(src, "\"db_row\"")
}

func TestDBRowTypeStaysOutOfParsers(t *testing.T) {
	root := repoRoot(t)
	total := 0
	for _, d := range dbRowFree {
		files := nonTestGoFiles(t, root, filepath.FromSlash(d))
		total += len(files)
		for _, p := range files {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if mentionsDBRow(string(src)) {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s names the \"db_row\" record type; only internal/dbrecover may", filepath.ToSlash(rel))
			}
		}
	}
	if total < 10 {
		t.Fatalf("scanned only %d files: the walk is broken", total)
	}
}

func TestDBRowScannerSelfTest(t *testing.T) {
	for src, want := range map[string]bool{
		"package p\nconst T = \"db_row\"\n":                true,
		"package p\nvar x = []string{\"a\", \"db_row\"}\n": true,
		"package p\nconst T = \"db_rows\"\n":               false,
		"package p\nconst T = \"message\"\n":               false,
		"package p\n":                                      false,
	} {
		if got := mentionsDBRow(src); got != want {
			t.Errorf("mentionsDBRow(%q) = %v, want %v", src, got, want)
		}
	}
	dir := t.TempDir()
	planted := filepath.Join(dir, "x.go")
	if err := os.WriteFile(planted, []byte("package x\nvar _ = \"db_row\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(planted)
	if !mentionsDBRow(string(b)) || !strings.Contains(string(b), "db_row") {
		t.Error("planted snippet not detected")
	}
}
