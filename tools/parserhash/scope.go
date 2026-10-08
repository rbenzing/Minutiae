package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config selects the module and the in-scope package prefixes.
type Config struct {
	ModuleRoot, ModulePath string   // directory holding go.mod, and its module path
	Prefixes               []string // default internal/parsers/, internal/recordtypes/, internal/decode/, internal/sqlitefile/
}

// ScopePackage is one package of a parser's hash scope.
type ScopePackage struct {
	ImportPath string
	Dir        string
	Files      []string // hashed file names relative to Dir (sorted; embedded files may contain "/")
	GoFiles    []string // the subset of Files the package compiles: normalised as Go; every other file is hashed raw
}

// ModuleRef is a third-party module in the scope.
type ModuleRef struct{ Path, Version, Sum string }

// Scope is everything a parser hash covers.
type Scope struct {
	Packages    []ScopePackage
	Modules     []ModuleRef
	GoDirective string
}

var defaultPrefixes = []string{"internal/parsers/", "internal/recordtypes/", "internal/decode/", "internal/sqlitefile/"}

// nonGoExts are source kinds that would change behaviour without being hashed.
var nonGoExts = map[string]bool{
	".s": true, ".S": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".m": true, ".f": true, ".syso": true, ".swig": true,
	".cxx": true, ".hh": true, ".hpp": true, ".hxx": true, ".F": true, ".for": true,
	".f90": true, ".sx": true, ".swigcxx": true,
}

// listModule names the module that provides importPath ("" for the standard
// library). It is a variable so tests need no network or module cache.
var listModule = func(root, importPath string) (string, string, error) {
	cmd := exec.Command("go", "list", "-f", "{{with .Module}}{{.Path}} {{.Version}}{{end}}", importPath)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("go list %s: %w: %s", importPath, err, strings.TrimSpace(stderr.String()))
	}
	f := strings.Fields(string(out))
	switch len(f) {
	case 0:
		return "", "", nil
	case 1:
		return f[0], "", nil
	default:
		return f[0], f[1], nil
	}
}

type resolver struct {
	cfg      Config
	prefixes []string
	pkgs     map[string]*ScopePackage
	third    map[string]bool
}

func (r *resolver) inScope(importPath string) bool {
	for _, p := range r.prefixes {
		if strings.HasPrefix(importPath, r.cfg.ModulePath+"/"+p) {
			return true
		}
	}
	return false
}

// ResolveScope computes the hash scope of the parser package rootImportPath.
func ResolveScope(cfg Config, rootImportPath string) (Scope, error) {
	if cfg.ModuleRoot == "" || cfg.ModulePath == "" {
		return Scope{}, errors.New("parserhash: module root and module path are required")
	}
	r := &resolver{cfg: cfg, prefixes: append([]string(nil), cfg.Prefixes...), pkgs: map[string]*ScopePackage{}, third: map[string]bool{}}
	if len(r.prefixes) == 0 {
		r.prefixes = append([]string(nil), defaultPrefixes...)
	}
	for i, p := range r.prefixes {
		if !strings.HasSuffix(p, "/") {
			r.prefixes[i] = p + "/"
		}
	}
	if !r.inScope(rootImportPath) {
		return Scope{}, fmt.Errorf("parserhash: %s is not under an in-scope prefix %v", rootImportPath, r.prefixes)
	}
	goDir, err := goDirective(filepath.Join(cfg.ModuleRoot, "go.mod"))
	if err != nil {
		return Scope{}, err
	}
	queue := []string{rootImportPath}
	for len(queue) > 0 {
		ip := queue[0]
		queue = queue[1:]
		if r.pkgs[ip] != nil {
			continue
		}
		pkg, follow, err := r.load(ip)
		if err != nil {
			return Scope{}, err
		}
		r.pkgs[ip] = pkg
		queue = append(queue, follow...)
	}
	s := Scope{GoDirective: goDir}
	for _, p := range r.pkgs {
		s.Packages = append(s.Packages, *p)
	}
	sort.Slice(s.Packages, func(i, j int) bool { return s.Packages[i].ImportPath < s.Packages[j].ImportPath })
	mods, err := r.modules()
	if err != nil {
		return Scope{}, err
	}
	s.Modules = mods
	return s, nil
}

// load reads one package directory and returns it with the in-scope imports to follow.
func (r *resolver) load(importPath string) (*ScopePackage, []string, error) {
	dir := filepath.Join(r.cfg.ModuleRoot, filepath.FromSlash(strings.TrimPrefix(importPath, r.cfg.ModulePath+"/")))
	if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("parserhash: package directory %s is a symlink", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("parserhash: package %s: %w", importPath, err)
	}
	fset := token.NewFileSet()
	var goFiles []string
	var follow []string
	var patterns []string
	names := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if nonGoExts[filepath.Ext(name)] {
			return nil, nil, fmt.Errorf("parserhash: non-Go source file %s in scope directory %s (it would change behaviour without being hashed)", name, dir)
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("parserhash: Go file %s in %s is a symlink", name, dir)
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parserhash: %w", err)
		}
		if isIgnoreTagged(f) {
			continue
		}
		goFiles = append(goFiles, name)
		names[f.Name.Name] = true
		for _, is := range f.Imports {
			p, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				return nil, nil, fmt.Errorf("parserhash: %s/%s: bad import %s", importPath, name, is.Path.Value)
			}
			switch {
			case p == "C":
				return nil, nil, fmt.Errorf("parserhash: %s imports \"C\" (cgo is not hashed): %s", name, dir)
			case p == r.cfg.ModulePath || strings.HasPrefix(p, r.cfg.ModulePath+"/"):
				if r.inScope(p) {
					follow = append(follow, p)
				}
			case strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
				r.third[p] = true
			}
		}
		pats, err := embedPatterns(f)
		if err != nil {
			return nil, nil, fmt.Errorf("parserhash: %s/%s: %w", importPath, name, err)
		}
		patterns = append(patterns, pats...)
	}
	if len(goFiles) == 0 {
		return nil, nil, fmt.Errorf("parserhash: package %s has no hashable Go files in %s", importPath, dir)
	}
	if len(names) > 1 {
		var ns []string
		for n := range names {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		return nil, nil, fmt.Errorf("parserhash: multiple package names %v in %s", ns, dir)
	}
	embedded, err := expandEmbed(dir, patterns)
	if err != nil {
		return nil, nil, fmt.Errorf("parserhash: package %s: %w", importPath, err)
	}
	set := map[string]bool{}
	for _, f := range append(goFiles, embedded...) {
		set[f] = true
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	sort.Strings(goFiles)
	return &ScopePackage{ImportPath: importPath, Dir: dir, Files: files, GoFiles: goFiles}, follow, nil
}

// modules resolves the third-party modules imported by the scope.
func (r *resolver) modules() ([]ModuleRef, error) {
	if len(r.third) == 0 {
		return nil, nil
	}
	sum, err := os.ReadFile(filepath.Join(r.cfg.ModuleRoot, "go.sum"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("parserhash: %w", err)
	}
	var imports []string
	for p := range r.third {
		imports = append(imports, p)
	}
	sort.Strings(imports)
	seen := map[string]bool{}
	var out []ModuleRef
	for _, ip := range imports {
		mp, ver, err := listModule(r.cfg.ModuleRoot, ip)
		if err != nil {
			return nil, fmt.Errorf("parserhash: %w", err)
		}
		if mp == "" || seen[mp] {
			continue
		}
		seen[mp] = true
		if ver == "" {
			return nil, fmt.Errorf("parserhash: module %s (imported as %s) has no version", mp, ip)
		}
		h1, err := findSum(sum, mp, ver)
		if err != nil {
			return nil, fmt.Errorf("parserhash: %w", err)
		}
		out = append(out, ModuleRef{Path: mp, Version: ver, Sum: h1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// isIgnoreTagged reports whether the file's constraint is exactly `ignore`.
func isIgnoreTagged(f *ast.File) bool {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if c.Pos() >= f.Package {
				return false
			}
			if constraint.IsGoBuild(c.Text) {
				if x, err := constraint.Parse(c.Text); err == nil && x.String() == "ignore" {
					return true
				}
			}
		}
	}
	return false
}

// embedPatterns returns the patterns of every //go:embed directive of f.
func embedPatterns(f *ast.File) ([]string, error) {
	var out []string
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			rest, ok := strings.CutPrefix(c.Text, "//go:embed")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
			pats, err := splitEmbed(rest)
			if err != nil {
				return nil, err
			}
			out = append(out, pats...)
		}
	}
	return out, nil
}

// splitEmbed splits the arguments of a //go:embed line: space-separated, with
// Go-quoted or backquoted patterns allowed.
func splitEmbed(s string) ([]string, error) {
	var out []string
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return out, nil
		}
		var end int
		switch s[0] {
		case '"':
			end = -1
			for i := 1; i < len(s); i++ {
				if s[i] == '\\' {
					i++
				} else if s[i] == '"' {
					end = i + 1
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("malformed //go:embed pattern %q", s)
			}
		case '`':
			i := strings.IndexByte(s[1:], '`')
			if i < 0 {
				return nil, fmt.Errorf("malformed //go:embed pattern %q", s)
			}
			end = i + 2
		default:
			end = strings.IndexAny(s, " \t")
			if end < 0 {
				end = len(s)
			}
		}
		tok := s[:end]
		if tok[0] == '"' || tok[0] == '`' {
			u, err := strconv.Unquote(tok)
			if err != nil {
				return nil, fmt.Errorf("malformed //go:embed pattern %s", tok)
			}
			tok = u
		}
		out = append(out, tok)
		s = s[end:]
	}
}

// expandEmbed returns the sorted, de-duplicated slash paths (relative to dir)
// of the files the //go:embed patterns select. Directories are recursive, names
// starting with . or _ are skipped inside them unless the pattern has the all:
// prefix; a pattern that is invalid, matches nothing, reaches an irregular file
// or a nested module is an error.
func expandEmbed(dir string, patterns []string) ([]string, error) {
	set := map[string]bool{}
	fsys := os.DirFS(dir)
	for _, pat := range patterns {
		all := false
		if p, ok := strings.CutPrefix(pat, "all:"); ok {
			pat, all = p, true
		}
		if !fs.ValidPath(pat) || pat == "." {
			return nil, fmt.Errorf("unsupported //go:embed pattern %q", pat)
		}
		if _, err := path.Match(pat, ""); err != nil {
			return nil, fmt.Errorf("unsupported //go:embed pattern %q: %w", pat, err)
		}
		matches, err := fs.Glob(fsys, pat)
		if err != nil {
			return nil, fmt.Errorf("unsupported //go:embed pattern %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("//go:embed pattern %q matches no files", pat)
		}
		for _, m := range matches {
			if err := addEmbedded(set, dir, m, all); err != nil {
				return nil, fmt.Errorf("//go:embed %q: %w", pat, err)
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

func addEmbedded(set map[string]bool, dir, match string, all bool) error {
	full := filepath.Join(dir, filepath.FromSlash(match))
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		set[match] = true
		return nil
	case !info.IsDir():
		return fmt.Errorf("%s is not a regular file or directory", match)
	}
	return filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if p != full && !all && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, err := os.Lstat(filepath.Join(p, "go.mod")); err == nil {
				return fmt.Errorf("%s holds another module", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		set[rel] = true
		return nil
	})
}

// goDirective returns the version of the go line of a go.mod file.
func goDirective(goMod string) (string, error) {
	b, err := os.ReadFile(goMod)
	if err != nil {
		return "", fmt.Errorf("parserhash: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "go" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("parserhash: no go directive in %s", goMod)
}

// findSum returns the zip h1: line of go.sum for path@version, never the
// "/go.mod" line.
func findSum(goSum []byte, path, version string) (string, error) {
	for _, line := range strings.Split(string(goSum), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == path && f[1] == version && strings.HasPrefix(f[2], "h1:") {
			return f[2], nil
		}
	}
	return "", fmt.Errorf("go.sum has no zip hash for %s %s", path, version)
}
