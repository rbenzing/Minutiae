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
	const golden = "src1:sha256:b88af6ee10f393ebf33e9c33160d4b187044d54ca2d1e9c08a28fc1e9075198c"
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
		Packages:    []ScopePackage{{ImportPath: "m/p", Dir: "d", Files: []string{"p.go"}}},
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
	s := Scope{Packages: []ScopePackage{{ImportPath: "m/p", Dir: "d", Files: []string{"p.go"}}}}
	_, err := HashScope(s, func(string, string) ([]byte, error) { return nil, fmt.Errorf("boom") })
	if err == nil {
		t.Error("read error must fail")
	}
	_, err = HashScope(s, func(string, string) ([]byte, error) { return []byte("package ("), nil })
	if err == nil {
		t.Error("syntax error in a hashed file must fail")
	}
}
