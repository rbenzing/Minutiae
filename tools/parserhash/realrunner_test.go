package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers"
)

// TestRecoveryHashMovesWithTheRealRunnerSource runs against the real repository: a registered
// parser's recover scope (its package plus the runner) holds the real runner package, a change to the
// runner's source moves the recover hash, and the live hash does not move.
func TestRecoveryHashMovesWithTheRealRunnerSource(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mp, err := modulePath(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{ModuleRoot: root, ModulePath: mp}
	runner := parsers.RecoveryRunnerPath()
	pkg := parsers.PackagePath(parsers.All()[0])
	live, err := ResolveScope(cfg, pkg)
	if err != nil {
		t.Fatal(err)
	}
	union, err := ResolveUnion(cfg, []string{pkg, runner})
	if err != nil {
		t.Fatal(err)
	}
	has := func(s Scope, path string) bool {
		for _, p := range s.Packages {
			if p.ImportPath == path {
				return true
			}
		}
		return false
	}
	if has(live, runner) || !has(union, runner) || !has(union, pkg) {
		t.Fatalf("live scope holds the runner: %v; recover scope holds the runner: %v and the parser: %v", has(live, runner), has(union, runner), has(union, pkg))
	}
	readWith := func(extra string) func(dir, file string) ([]byte, error) {
		return func(dir, file string) ([]byte, error) {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
			if err == nil && extra != "" && strings.HasSuffix(filepath.ToSlash(dir), "/decode/sqlitedb/recrun") && file == "run.go" {
				b = append(b, extra...)
			}
			return b, err
		}
	}
	hash := func(s Scope, extra string) string {
		h, err := HashScope(s, readWith(extra))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	const edit = "\nfunc injectedByTheTest() int { return 1 }\n"
	if hash(union, "") == hash(union, edit) {
		t.Error("a change to the runner source did not move the recover hash")
	}
	if hash(live, "") != hash(live, edit) {
		t.Error("a change to the runner source moved the live hash")
	}
}
