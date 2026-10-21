package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Target is one fuzz target: Pkg is the slash-separated package directory relative to the
// repository root ("internal/decode/plist"), Name the function ("FuzzPlist").
type Target struct{ Pkg, Name string }

// ErrTooManyFiles is returned when the tree under the root holds more than maxFiles files.
var ErrTooManyFiles = errors.New("fuzzcampaign: more than 200000 files under the root")

// maxFiles bounds the walk; a variable only so a test can lower it.
var maxFiles = 200000

// ParseError names the test file that could not be parsed.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("fuzzcampaign: cannot parse %s: %v", e.Path, e.Err)
}
func (e *ParseError) Unwrap() error { return e.Err }

// skippedDirs are directory names never entered below the root.
var skippedDirs = map[string]bool{
	".git": true, "bin": true, "cases": true, "testdata": true, "vendor": true, ".superpowers": true, "docs": true,
}

var fuzzName = regexp.MustCompile(`^Fuzz([A-Z0-9_].*)?$`)

func cmpTarget(a, b Target) int {
	if c := strings.Compare(a.Pkg, b.Pkg); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// Discover lists the fuzz targets under root, sorted by package then name. Only *_test.go
// files that match linux/amd64 with cgo off and no extra tags are read; the directories in
// skippedDirs are not entered and symlinks are not followed. A file that does not parse is a
// *ParseError, never skipped.
func Discover(root string) ([]Target, error) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled, ctx.BuildTags = "linux", "amd64", false, nil
	var out []Target
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if files++; files > maxFiles {
			return ErrTooManyFiles
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		match, err := ctx.MatchFile(dir, d.Name())
		if err != nil {
			return &ParseError{Path: path, Err: err}
		}
		if !match {
			return nil
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		names, err := fuzzNames(path)
		if err != nil {
			return err
		}
		for _, n := range names {
			out = append(out, Target{Pkg: filepath.ToSlash(rel), Name: n})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, cmpTarget)
	return slices.Compact(out), nil
}

// fuzzNames returns the names of the fuzz targets declared in one test file.
func fuzzNames(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	local := ""
	for _, imp := range f.Imports {
		if imp.Path.Value != `"testing"` {
			continue
		}
		local = "testing"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" || local == "." {
		return nil, nil
	}
	var names []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Type.TypeParams != nil || !fuzzName.MatchString(fd.Name.Name) {
			continue
		}
		if isTestingFParam(fd.Type.Params, local) {
			names = append(names, fd.Name.Name)
		}
	}
	return names, nil
}

// isTestingFParam reports whether the parameter list is exactly one parameter of type *<local>.F.
func isTestingFParam(params *ast.FieldList, local string) bool {
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == local
}
