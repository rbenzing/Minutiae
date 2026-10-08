package main

import (
	"fmt"
	"strings"
	"testing"
)

const baseSource = `package a

import (
	"fmt"
	"strings"
)

//go:embed data.txt
var d string

//go:linkname h runtime.nanotime
func h() int64

// F doc.
func F(a string, b string) string {
	// inside
	x := 0o777
	s := ` + "`line1\nline2`" + `
	return fmt.Sprint(strings.ToUpper(a), b, x, s)
}

func G() int { return 1 }
`

// fixture builds the parser package a from src, with embedded data files.
func fixture(src, data string) map[string]string {
	return map[string]string{
		"internal/parsers/a/a.go":      src,
		"internal/parsers/a/data.txt":  data,
		"internal/parsers/a/other.txt": "other",
	}
}

func fixtureHash(t *testing.T, src, data string) string {
	t.Helper()
	return hashTree(t, writeTree(t, fixture(src, data)), "internal/parsers/a")
}

func TestParserHashIgnoresFormatting(t *testing.T) {
	base := fixtureHash(t, baseSource, "data")
	variants := map[string]string{
		"compact layout": `package a
import ("fmt";"strings")
//go:embed data.txt
var d string
//go:linkname h runtime.nanotime
func h() int64
func F(a string,b string) string{x:=0o777;s:=` + "`line1\nline2`" + `;return fmt.Sprint(strings.ToUpper(a),b,x,s)}
func G() int{return 1}
`,
		"imports regrouped, re-sorted, blank-separated": strings.Replace(baseSource,
			"import (\n\t\"fmt\"\n\t\"strings\"\n)", "import \"strings\"\n\nimport \"fmt\"", 1),
		"imports in separate groups": strings.Replace(baseSource,
			"\t\"fmt\"\n\t\"strings\"\n", "\t\"strings\"\n\n\n\t\"fmt\"\n", 1),
		"comments edited, added, deleted": strings.NewReplacer(
			"// F doc.", "/* a different\n\tdoc */", "// inside", "", "x := 0o777", "x := 0o777 // trailing").Replace(baseSource),
		"doc comment moved": strings.Replace(strings.Replace(baseSource, "// F doc.\n", "", 1),
			"func G()", "// F doc.\nfunc G()", 1),
		"extra blank lines": strings.ReplaceAll(baseSource, "\n\n", "\n\n\n"),
		"CRLF":              strings.ReplaceAll(baseSource, "\n", "\r\n"),
	}
	for name, src := range variants {
		if got := fixtureHash(t, src, "data"); got != base {
			t.Errorf("%s: hash changed", name)
		}
	}
}

func TestParserHashIgnoresCRLF(t *testing.T) {
	lf := fixtureHash(t, baseSource, "data")
	crlf := fixtureHash(t, strings.ReplaceAll(baseSource, "\n", "\r\n"), "data")
	if lf != crlf {
		t.Error("CRLF Go source changed the hash")
	}
	// An embedded data file is hashed as its raw bytes (ruling for T10): a parser
	// reads those bytes, so CRLF and LF data must differ.
	if fixtureHash(t, baseSource, "x\ny\n") == fixtureHash(t, baseSource, "x\r\ny\r\n") {
		t.Error("embedded data with CRLF and LF must hash differently")
	}
}

func TestParserHashChangesOnSemanticEdit(t *testing.T) {
	rep := func(old, repl string) string {
		if !strings.Contains(baseSource, old) {
			t.Fatalf("base lacks %q", old)
		}
		return strings.Replace(baseSource, old, repl, 1)
	}
	rows := []struct{ name, src string }{
		{"base", baseSource},
		{"renamed identifier", rep("func F(", "func F2(")},
		{"changed string", rep("line1", "line9")},
		{"changed int literal", rep("0o777", "0o770")},
		{"0777 vs 0o777 (formatter rewrite; documented residual, ruling 11)", rep("0o777", "0777")},
		{"func(a, b string) (formatter rewrite; documented residual, ruling 11)", rep("(a string, b string)", "(a, b string)")},
		{"added statement", rep("x := 0o777", "x := 0o777\n\t_ = x")},
		{"swapped function order", rep("func G() int { return 1 }\n", "") + "\n"},
		{"changed embed pattern", rep("//go:embed data.txt", "//go:embed other.txt")},
		{"edited linkname", rep("runtime.nanotime", "runtime.walltime")},
	}
	// Swapped order: move G before F.
	rows[7].src = strings.Replace(strings.Replace(baseSource, "func G() int { return 1 }\n", "", 1),
		"// F doc.", "func G() int { return 1 }\n\n// F doc.", 1)
	seen := map[string]string{}
	for _, r := range rows {
		h := fixtureHash(t, r.src, "data")
		if prev, dup := seen[h]; dup {
			t.Errorf("%q hashes like %q", r.name, prev)
		}
		seen[h] = r.name
	}
	if len(seen) != len(rows) {
		t.Fatalf("%d distinct hashes for %d rows", len(seen), len(rows))
	}
}

func TestParserHashStability(t *testing.T) {
	const golden = "src1:sha256:fa35e3d63be20667fc4484c7bf62d038b2ebd8b1895af9a34b0cef346dedec89"
	got := fixtureHash(t, baseSource, "data")
	if got != golden {
		t.Errorf("golden hash changed: got %s want %s (a change of the dump format needs a deliberate hash-v bump)", got, golden)
	}
	if len(got) != len("src1:sha256:")+64 {
		t.Errorf("hash length %d, want 76", len(got))
	}
}

func TestGoDirectiveInHash(t *testing.T) {
	s := Scope{
		Packages:    []ScopePackage{{ImportPath: "m/p", Dir: "d", Files: []string{"p.go"}, GoFiles: []string{"p.go"}}},
		GoDirective: "1.26",
	}
	read := func(string, string) ([]byte, error) { return []byte("package p\n"), nil }
	h1, err := HashScope(s, read)
	if err != nil {
		t.Fatal(err)
	}
	s.GoDirective = "1.27"
	h2, _ := HashScope(s, read)
	if h1 == h2 || !strings.HasPrefix(h1, "src1:sha256:") {
		t.Errorf("go directive must change the hash: %s %s", h1, h2)
	}
}

func TestHashScopeReadError(t *testing.T) {
	s := Scope{Packages: []ScopePackage{{ImportPath: "m/p", Dir: "d", Files: []string{"p.go"}, GoFiles: []string{"p.go"}}}}
	_, err := HashScope(s, func(string, string) ([]byte, error) { return nil, fmt.Errorf("boom") })
	if err == nil {
		t.Error("read error must fail")
	}
	_, err = HashScope(s, func(string, string) ([]byte, error) { return []byte("package ("), nil })
	if err == nil {
		t.Error("syntax error in a hashed file must fail")
	}
}

func TestDirectiveIsHashedWithItsDeclaration(t *testing.T) {
	embed := func(first bool) string {
		a, b := "//go:embed data.txt\n", ""
		if !first {
			a, b = b, a
		}
		return "package a\n\nimport _ \"embed\"\n\n" + a + "var x string\n\n" + b + "var y string\n"
	}
	if fixtureHash(t, embed(true), "d") == fixtureHash(t, embed(false), "d") {
		t.Error("moving //go:embed between vars must change the hash")
	}
	noinline := func(first bool) string {
		a, b := "//go:noinline\n", ""
		if !first {
			a, b = b, a
		}
		return "package a\n\n" + a + "func f() {}\n\n" + b + "func g() {}\n"
	}
	if fixtureHash(t, noinline(true), "d") == fixtureHash(t, noinline(false), "d") {
		t.Error("moving //go:noinline between funcs must change the hash")
	}
}

func TestLineDirectivesAreHashed(t *testing.T) {
	src := func(d string) string { return "package a\n\nfunc f() int {\n\t" + d + "\n\treturn 1\n}\n" }
	seen := map[string]string{}
	for _, d := range []string{"//line a.go:10", "//line a.go:11", "/*line a.go:10*/", "/*line a.go:11*/", ""} {
		h := fixtureHash(t, src(d), "d")
		if prev, dup := seen[h]; dup {
			t.Errorf("%q hashes like %q", d, prev)
		}
		seen[h] = d
	}
}

func TestHashFramingIsUnambiguous(t *testing.T) {
	scope := func(files ...string) Scope {
		return Scope{Packages: []ScopePackage{{ImportPath: "m/p", Dir: "d", Files: files}}, GoDirective: "1.26"}
	}
	hash := func(s Scope, data map[string]string) string {
		h, err := HashScope(s, func(_, f string) ([]byte, error) { return []byte(data[f]), nil })
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// Plain concatenation of name and content is the same in both trees.
	one := hash(scope("x", "y"), map[string]string{"x": "1", "y": "2"})
	two := hash(scope("x"), map[string]string{"x": "1\x00m/p/y\x002"})
	if one == two {
		t.Error("an embedded NUL faked a file boundary")
	}
}

// A directive separated from its declaration by blank lines or comments still
// belongs to the next declaration: its position decides what it annotates.
func TestDetachedDirectiveIsHashedWithNextDeclaration(t *testing.T) {
	for name, tc := range map[string]struct{ head, mid, a, b string }{
		"embed between vars": {
			"package a\n\nimport _ \"embed\"\n\n", "var x string\n\n", "//go:embed data.txt\n\n// c\n", "var y string\n",
		},
		"noinline between funcs": {
			"package a\n\n", "func f() {}\n\n", "//go:noinline\n\n", "func g() {}\n",
		},
	} {
		first := tc.head + tc.a + tc.mid + tc.b
		second := tc.head + tc.mid + tc.a + tc.b
		if fixtureHash(t, first, "d") == fixtureHash(t, second, "d") {
			t.Errorf("%s: moving a detached directive must change the hash", name)
		}
	}
	// A directive after the last declaration attaches to the file end and still counts.
	if fixtureHash(t, "package a\n\nfunc f() {}\n", "d") == fixtureHash(t, "package a\n\nfunc f() {}\n\n//go:noinline\n", "d") {
		t.Error("a trailing directive must change the hash")
	}
	// Directive text is still significant, and so is its place among two directives.
	if fixtureHash(t, "package a\n\n//go:noinline\n\nfunc f() {}\n", "d") == fixtureHash(t, "package a\n\n//go:nosplit\n\nfunc f() {}\n", "d") {
		t.Error("directive text must change the hash")
	}
}

// A //line directive inside a function body is part of THAT declaration, at
// the ordinal of the statement that follows it.
func TestLineDirectiveInABodyIsHashedAtItsStatement(t *testing.T) {
	body := func(at int) string {
		stmts := []string{"a := 1", "b := 2", "c := 3"}
		var sb strings.Builder
		sb.WriteString("package a\n\nfunc f() int {\n")
		for i, s := range stmts {
			if i == at {
				sb.WriteString("//line x.go:10\n")
			}
			sb.WriteString("\t" + s + "\n")
		}
		if at == len(stmts) {
			sb.WriteString("//line x.go:10\n")
		}
		sb.WriteString("\treturn a + b + c\n}\n")
		return sb.String()
	}
	seen := map[string]int{}
	for at := 0; at <= 3; at++ {
		h := fixtureHash(t, body(at), "d")
		if prev, dup := seen[h]; dup {
			t.Errorf("a directive before statement %d and before statement %d hash alike", prev, at)
		}
		seen[h] = at
	}
	// a body without the directive hashes differently from every one of them
	plain := "package a\n\nfunc f() int {\n\ta := 1\n\tb := 2\n\tc := 3\n\treturn a + b + c\n}\n"
	if _, dup := seen[fixtureHash(t, plain, "d")]; dup {
		t.Error("a body without the directive hashes like one with it")
	}
	// it belongs to its own declaration, not to the next one
	two := func(g string) string {
		return "package a\n\nfunc f() {\n\t_ = 1\n" + g + "}\n\nfunc g() {}\n"
	}
	if fixtureHash(t, two("//line x.go:10\n"), "d") == fixtureHash(t, "package a\n\nfunc f() {\n\t_ = 1\n}\n\n//line x.go:10\nfunc g() {}\n", "d") {
		t.Error("a directive inside f hashes like one before g")
	}
	// nested blocks count their own statements
	nested := func(at int) string {
		in := []string{"x := 1", "y := 2"}
		var sb strings.Builder
		sb.WriteString("package a\n\nfunc f(ok bool) {\n\tif ok {\n")
		for i, s := range in {
			if i == at {
				sb.WriteString("//line x.go:10\n")
			}
			sb.WriteString("\t\t" + s + "\n\t\t_ = " + s[:1] + "\n")
		}
		sb.WriteString("\t}\n}\n")
		return sb.String()
	}
	if fixtureHash(t, nested(0), "d") == fixtureHash(t, nested(1), "d") {
		t.Error("moving a directive between the statements of a nested block must change the hash")
	}
	// formatting still does not matter
	if fixtureHash(t, body(1), "d") != fixtureHash(t, strings.ReplaceAll(body(1), "\t", "    "), "d") {
		t.Error("indentation changed the hash")
	}
}

// A directive inside a declaration but outside any block (here a composite
// literal) is still hashed with that declaration.
func TestLineDirectiveOutsideABlockIsHashedWithItsDeclaration(t *testing.T) {
	with := "package a\n\nvar x = []int{\n//line x.go:5\n\t1,\n}\n"
	without := "package a\n\nvar x = []int{\n\t1,\n}\n"
	if fixtureHash(t, with, "d") == fixtureHash(t, without, "d") {
		t.Error("a //line inside a composite literal was not hashed")
	}
	before := "package a\n\n//line x.go:5\nvar x = []int{\n\t1,\n}\n"
	if fixtureHash(t, with, "d") == fixtureHash(t, before, "d") {
		t.Error("a directive inside a declaration hashes like one before it")
	}
}
