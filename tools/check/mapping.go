package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// nonCodePaths is the one table of changed files that need no package and no escalation: they
// contain no code and no test input. Everything else that belongs to no package escalates.
var nonCodePaths = []struct {
	what  string
	match func(f string) bool
}{
	{"docs/**", func(f string) bool { return strings.HasPrefix(f, "docs/") }},
	{"*.md at any depth", func(f string) bool { return path.Ext(f) == ".md" }},
	{".gitignore", func(f string) bool { return f == ".gitignore" }},
	{".claude/agents/**", func(f string) bool { return strings.HasPrefix(f, ".claude/agents/") }},
	{"LICENSE*", func(f string) bool { return strings.HasPrefix(path.Base(f), "LICENSE") }},
}

func allowedNonCode(f string) bool {
	for _, n := range nonCodePaths {
		if n.match(f) {
			return true
		}
	}
	return false
}

// owner returns the nearest ancestor directory of f that is a package.
func owner(f string, dirs map[string]bool) (string, bool) {
	for d := path.Dir(f); ; d = path.Dir(d) {
		if dirs[d] {
			return d, true
		}
		if d == "." || d == "/" {
			return "", false
		}
	}
}

// changedPackages maps every changed file to its nearest ancestor package directory (so fixtures
// and schemas under a package count for it) or, when a file embeds into a package, to that
// package. Files with no package are returned in unmapped, in input order.
func changedPackages(files []string, pkgs []pkg) (hit map[string]bool, unmapped []string) {
	dirs := map[string]bool{}
	embeds := map[string]string{}
	for _, p := range pkgs {
		dirs[p.Dir] = true
		for _, e := range p.Embeds {
			embeds[e] = p.Dir
		}
	}
	hit = map[string]bool{}
	for _, f := range files {
		if d, ok := embeds[f]; ok {
			hit[d] = true
			continue
		}
		if d, ok := owner(f, dirs); ok {
			hit[d] = true
			continue
		}
		unmapped = append(unmapped, f)
	}
	return hit, unmapped
}

// goFileReason returns why a .go file escalates, or "".
func goFileReason(f string, pkgDirs map[string]bool, exists func(string) bool, read, readBase func(string) ([]byte, error)) (string, error) {
	if !strings.HasSuffix(f, ".go") || strings.HasPrefix(f, "testdata/") || strings.Contains(f, "/testdata/") {
		return "", nil
	}
	if nameIsConstrained(strings.TrimSuffix(path.Base(f), ".go")) {
		return f + " is constrained to a GOOS or GOARCH by its name", nil
	}
	var data []byte
	var err error
	if exists(f) {
		data, err = read(f)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f, err)
		}
	} else {
		if !pkgDirs[path.Dir(f)] {
			return "package directory " + path.Dir(f) + " was deleted", nil
		}
		// A deleted file is judged by its build constraint at the base.
		if data, err = readBase(f); err != nil {
			return "", fmt.Errorf("reading deleted %s from the base: %w", f, err)
		}
	}
	tag, err := constraintTag(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f, err)
	}
	if tag != "" {
		return f + " has a build constraint naming " + tag, nil
	}
	return "", nil
}

// escalation returns why the change needs the full form, or "" when it does not.
func escalation(files []string, pkgs []pkg, exists func(string) bool, read, readBase func(string) ([]byte, error)) (string, error) {
	pkgDirs := map[string]bool{}
	for _, p := range pkgs {
		pkgDirs[p.Dir] = true
	}
	for _, f := range files {
		switch {
		case f == "go.mod" || f == "go.sum" || f == ".golangci.yml":
			return f + " changed", nil
		case strings.HasPrefix(f, "tools/check/"):
			return f + " changed (the check itself)", nil
		}
		reason, err := goFileReason(f, pkgDirs, exists, read, readBase)
		if err != nil || reason != "" {
			return reason, err
		}
		if _, ok := owner(f, pkgDirs); !ok && !allowedNonCode(f) {
			return f + " belongs to no package and is not an allow-listed non-code path", nil
		}
	}
	return "", nil
}

// changedFiles lists, sorted and slash separated, the files that differ from base (committed,
// staged and unstaged) plus the untracked files that are not ignored. A failing git call is an
// error, never an empty set.
func changedFiles(git func(args ...string) ([]byte, error), base string) ([]string, error) {
	diff, err := git("diff", "--name-only", "--no-renames", "-z", "--end-of-options", base, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}
	others, err := git("ls-files", "--others", "--exclude-standard", "--full-name", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	set := map[string]bool{}
	for _, out := range [][]byte{diff, others} {
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				set[path.Clean(strings.ReplaceAll(f, "\\", "/"))] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// resolveBase returns base, or the merge base of HEAD and main when base is empty. A base that
// starts with "-" is refused: it would be read as a git option.
func resolveBase(git func(args ...string) ([]byte, error), base string) (string, error) {
	if strings.HasPrefix(base, "-") {
		return "", usageError{fmt.Sprintf("-base %q starts with -", base)}
	}
	if base != "" {
		return base, nil
	}
	out, err := git("merge-base", "HEAD", "main")
	if err != nil {
		return "", fmt.Errorf("git merge-base HEAD main: %w", err)
	}
	b := strings.TrimSpace(string(out))
	if b == "" {
		return "", fmt.Errorf("git merge-base HEAD main printed nothing")
	}
	return b, nil
}

// testSrc is one _test.go file: its module-relative path and content.
type testSrc struct {
	Path string
	Src  []byte
}

// loadTestSources reads every _test.go file in the package directories (build tags do not
// matter: a file excluded from this build can still read another package's testdata).
func loadTestSources(pkgs []pkg) ([]testSrc, error) {
	var out []testSrc
	for _, p := range pkgs {
		ents, err := os.ReadDir(filepath.FromSlash(p.Dir))
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			rel := path.Join(p.Dir, e.Name())
			b, err := os.ReadFile(filepath.FromSlash(rel))
			if err != nil {
				return nil, err
			}
			out = append(out, testSrc{rel, b})
		}
	}
	return out, nil
}

// scanFile returns the directory patterns of the testdata directories that a test file in dir
// reaches through a ".."-relative path: a string literal such as "../x/testdata/f", a
// concatenation such as "../" + n + "/testdata", or a Join call such as Join("..", "x", name,
// "testdata"). A part that is not a literal becomes "*". A path that does not start with ".." is
// not relative to the package and is not resolved here.
func scanFile(dir string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	add := func(s string) {
		if t, ok := resolveTestdataRef(dir, s); ok {
			set[t] = true
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(x); ok {
				add(s)
			}
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				add(concatText(x))
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
				parts := make([]string, len(x.Args))
				for i, a := range x.Args {
					parts[i] = partText(a)
				}
				add(strings.Join(parts, "/"))
			}
		}
		return true
	})
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func stringLit(e ast.Expr) (string, bool) {
	l, ok := e.(*ast.BasicLit)
	if !ok || l.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(l.Value)
	return s, err == nil
}

func partText(e ast.Expr) string {
	if s, ok := stringLit(e); ok {
		return s
	}
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return concatText(b)
	}
	return "*"
}

func concatText(b *ast.BinaryExpr) string {
	return partText(b.X) + partText(b.Y)
}

// resolveTestdataRef resolves a path that starts with ".." and reaches a testdata element,
// relative to dir, to the directory that owns that testdata.
func resolveTestdataRef(dir, s string) (string, bool) {
	segs := strings.Split(filepath.ToSlash(s), "/")
	if segs[0] != ".." || !containsString(segs, "testdata") {
		return "", false
	}
	var stack []string
	if dir != "." {
		stack = strings.Split(dir, "/")
	}
	for _, seg := range segs {
		switch seg {
		case "testdata":
			if len(stack) == 0 {
				return "", false
			}
			return strings.Join(stack, "/"), true
		case "", ".":
		case "..":
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	return "", false
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// testdataReaders returns the packages whose test sources read the testdata of one of dirs: by a
// ".."-relative path (scanFile), or by naming "<dir>/testdata" anywhere in the text (a module
// path, a comment: over-inclusion is fine, omission is not). A source that does not parse is an
// error.
func testdataReaders(srcs []testSrc, dirs []string) (map[string]bool, error) {
	readers := map[string]bool{}
	for _, k := range knownTestdataReaders {
		for _, d := range dirs {
			if k.owner == d {
				readers[k.reader] = true
			}
		}
	}
	for _, s := range srcs {
		pkgDir := path.Dir(s.Path)
		refs, err := scanFile(pkgDir, s.Src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path, err)
		}
		text := string(s.Src)
		for _, d := range dirs {
			if strings.Contains(text, d+"/testdata") {
				readers[pkgDir] = true
			}
			for _, r := range refs {
				if ok, err := path.Match(r, d); ok || err != nil {
					readers[pkgDir] = true
				}
			}
		}
	}
	return readers, nil
}

// mention is a string literal in a test source that names a testdata directory.
type mention struct {
	Line int
	Text string
}

// knownTestdataReaders are the test sources that reach another package's testdata through a path
// the scan cannot resolve (a variable root, a computed segment). Each entry names the reader, the
// owner of the testdata and why; the owner's testdata changes make the reader affected
// (testdataReaders). TestEveryTestdataReferenceIsResolved fails on any other unresolved mention.
var knownTestdataReaders = []struct{ reader, owner, reason string }{
	{"internal/archtest", "none", "the testdata paths are inputs to path predicates (hand-written strings); no testdata file is read; archtest is in the global block anyway"},
	{"tools/parserhash", "none", "scope_test.go names testdata paths inside a virtual tree it writes to a temp directory; no repository testdata is read"},
}

// unresolvedTestdata returns the string literals of a test source in dir that name a testdata
// directory and are neither the package's own (a path starting with "testdata") nor resolved by
// scanFile's rules to another package's directory.
func unresolvedTestdata(dir string, src []byte) ([]mention, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	handled := map[*ast.BasicLit]bool{}
	var out []mention
	lits := func(n ast.Node) (ls []*ast.BasicLit) {
		ast.Inspect(n, func(m ast.Node) bool {
			if l, ok := m.(*ast.BasicLit); ok && l.Kind == token.STRING {
				ls = append(ls, l)
			}
			return true
		})
		return ls
	}
	classify := func(text string, ls []*ast.BasicLit, pos token.Pos) {
		for _, l := range ls {
			handled[l] = true
		}
		segs := strings.Split(strings.ReplaceAll(text, "\\", "/"), "/")
		if !containsString(segs, "testdata") || segs[0] == "testdata" {
			return
		}
		if _, ok := resolveTestdataRef(dir, text); ok {
			return
		}
		out = append(out, mention{Line: fset.Position(pos).Line, Text: text})
	}
	allHandled := func(ls []*ast.BasicLit) bool {
		for _, l := range ls {
			if !handled[l] {
				return false
			}
		}
		return true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
				parts := make([]string, len(x.Args))
				for i, a := range x.Args {
					parts[i] = partText(a)
				}
				classify(strings.Join(parts, "/"), lits(x), x.Pos())
			}
		case *ast.BinaryExpr:
			if ls := lits(x); x.Op == token.ADD && !allHandled(ls) {
				classify(concatText(x), ls, x.Pos())
			}
		case *ast.BasicLit:
			if s, ok := stringLit(x); ok && !handled[x] {
				classify(s, []*ast.BasicLit{x}, x.Pos())
			}
		}
		return true
	})
	return out, nil
}
