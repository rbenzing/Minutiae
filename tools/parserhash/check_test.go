package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pSrc = "package p\n\nimport \"fmt\"\n\nfunc F(a string, b string) string {\n\tx := 0o777\n\treturn fmt.Sprint(a, b, x)\n}\n"

func pEntry(version string) []Entry {
	return []Entry{{Name: "p", Version: version, Package: testModule + "/internal/parsers/p"}}
}

// parserTree is a synthetic module with one parser package and empty generated files.
func parserTree(t *testing.T, src string) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"internal/parsers/p/p.go": src,
		lockRel:                   "# test lock\n",
		genRel:                    string(GenerateIdentity(nil)),
	})
}

func setFile(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil { //nolint:gosec // test temp tree
		t.Fatal(err)
	}
}

func runCmd(t *testing.T, root string, entries []Entry, flag string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{"-root", root, flag}, &out, &errb, entries)
	return code, out.String(), errb.String()
}

func TestParserLockRequiresVersionBump(t *testing.T) {
	root := parserTree(t, pSrc)
	if code, out, e := runCmd(t, root, pEntry("1.0.0"), "-write"); code != 0 || !strings.Contains(out, "locked p 1.0.0") {
		t.Fatalf("write: %d %q %q", code, out, e)
	}
	if code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 0 {
		t.Fatalf("check after write: %d %q", code, e)
	}
	// a semantic edit under the same version
	setFile(t, root, "internal/parsers/p/p.go", strings.Replace(pSrc, "0o777", "0o776", 1))
	code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check")
	if code != 1 || !strings.Contains(e, "bump Meta.Version") {
		t.Fatalf("semantic edit: %d %q", code, e)
	}
	// the version bumped, not yet locked
	code, _, e = runCmd(t, root, pEntry("1.0.1"), "-check")
	if code != 1 || !strings.Contains(e, "is not locked") {
		t.Fatalf("bump: %d %q", code, e)
	}
	if code, out, e := runCmd(t, root, pEntry("1.0.1"), "-write"); code != 0 || !strings.Contains(out, "locked p 1.0.1") {
		t.Fatalf("write 1.0.1: %d %q %q", code, out, e)
	}
	lock, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(lockRel)))
	if !strings.Contains(string(lock), "p 1.0.0 ") || !strings.Contains(string(lock), "p 1.0.1 ") {
		t.Errorf("lock lost a line:\n%s", lock)
	}
	if code, _, e := runCmd(t, root, pEntry("1.0.1"), "-check"); code != 0 {
		t.Errorf("check after bump: %d %q", code, e)
	}
}

func TestParserLockFormattingOnlyChangeNeedsNoBump(t *testing.T) {
	root := parserTree(t, pSrc)
	runCmd(t, root, pEntry("1.0.0"), "-write")
	reformatted := "package p\n\n\n// moved comment\nimport (\n\t\"fmt\"\n)\n\n\nfunc F(a string, b string) string {\n\n\tx := 0o777\n\n\treturn fmt.Sprint(a, b, x)\n}\n"
	setFile(t, root, "internal/parsers/p/p.go", reformatted)
	if code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 0 {
		t.Fatalf("formatting-only change failed check: %q", e)
	}
	if code, out, _ := runCmd(t, root, pEntry("1.0.0"), "-write"); code != 0 || !strings.Contains(out, "nothing to add") {
		t.Errorf("write after formatting change: %d %q", code, out)
	}
}

func TestParserLockFormatterRewriteNeedsBump(t *testing.T) {
	for name, rep := range map[string][2]string{
		"0777 respelling": {"0o777", "0777"},
		"param grouping":  {"a string, b string", "a, b string"},
	} {
		root := parserTree(t, pSrc)
		runCmd(t, root, pEntry("1.0.0"), "-write")
		setFile(t, root, "internal/parsers/p/p.go", strings.Replace(pSrc, rep[0], rep[1], 1))
		code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check")
		if code != 1 || !strings.Contains(e, "bump Meta.Version") || !strings.Contains(e, "formatter rewrite") {
			t.Errorf("%s: %d %q", name, code, e)
		}
	}
}

func TestWriteNeverRewritesALockedLine(t *testing.T) {
	root := parserTree(t, pSrc)
	runCmd(t, root, pEntry("1.0.0"), "-write")
	lockPath := filepath.Join(root, filepath.FromSlash(lockRel))
	b, _ := os.ReadFile(lockPath)
	s := string(b)
	i := strings.LastIndex(s, "src1:sha256:") + len("src1:sha256:")
	flip := "0"
	if s[i] == '0' {
		flip = "1"
	}
	setFile(t, root, lockRel, s[:i]+flip+s[i+1:])
	before, _ := os.ReadFile(lockPath)
	code, _, e := runCmd(t, root, pEntry("1.0.0"), "-write")
	if code != 1 || !strings.Contains(e, "bump Meta.Version") {
		t.Fatalf("write over tampered lock: %d %q", code, e)
	}
	_, _, ce := runCmd(t, root, pEntry("1.0.0"), "-check")
	if !strings.Contains(ce, "bump Meta.Version") {
		t.Errorf("check text differs: %q", ce)
	}
	after, _ := os.ReadFile(lockPath)
	if !bytes.Equal(before, after) {
		t.Error("the lock file changed")
	}
}

func TestCheckFlagsRevertedVersion(t *testing.T) {
	root := parserTree(t, pSrc)
	runCmd(t, root, pEntry("1.0.1"), "-write")
	code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check")
	if code != 1 || !strings.Contains(e, "below the highest locked version 1.0.1") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, _ := runCmd(t, root, pEntry("1.0.0"), "-write"); code != 1 {
		t.Error("write accepted a reverted version")
	}
}

func TestCheckFlagsMissingOrStaleIdentityGen(t *testing.T) {
	root := parserTree(t, pSrc)
	runCmd(t, root, pEntry("1.0.0"), "-write")
	genPath := filepath.Join(root, filepath.FromSlash(genRel))
	good, _ := os.ReadFile(genPath)
	// stale: the empty generated file with a locked parser
	setFile(t, root, genRel, string(GenerateIdentity(nil)))
	if code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 1 || !strings.Contains(e, "identity_gen.go") {
		t.Errorf("stale gen: %d %q", code, e)
	}
	// missing
	if err := os.Remove(genPath); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 1 || !strings.Contains(e, "identity_gen.go") {
		t.Errorf("missing gen: %d %q", code, e)
	}
	// -write repairs it
	runCmd(t, root, pEntry("1.0.0"), "-write")
	if now, _ := os.ReadFile(genPath); !bytes.Equal(now, good) {
		t.Error("write did not regenerate identity_gen.go")
	}
	// stale lock: the parser is registered but its line is gone
	setFile(t, root, lockRel, "# test lock\n")
	if code, _, e := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 1 || !strings.Contains(e, "is not locked") {
		t.Errorf("stale lock: %d %q", code, e)
	}
}

func TestCheckFlagsPackageOutsideParsers(t *testing.T) {
	root := parserTree(t, pSrc)
	e := []Entry{{Name: "p", Version: "1.0.0", Package: testModule + "/internal/other/p"}}
	code, _, msg := runCmd(t, root, e, "-check")
	if code != 1 || !strings.Contains(msg, "outside") {
		t.Errorf("%d %q", code, msg)
	}
}

func TestLockRetainsRemovedParsers(t *testing.T) {
	root := parserTree(t, pSrc)
	runCmd(t, root, pEntry("1.0.0"), "-write")
	if code, _, e := runCmd(t, root, nil, "-write"); code != 0 {
		t.Fatalf("write with an empty registry: %d %q", code, e)
	}
	b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(lockRel)))
	if !strings.Contains(string(b), "p 1.0.0 ") {
		t.Errorf("removed parser dropped from the lock:\n%s", b)
	}
	if code, _, e := runCmd(t, root, nil, "-check"); code != 0 {
		t.Errorf("check with an empty registry: %d %q", code, e)
	}
}

func TestRunExitCodes(t *testing.T) {
	root := parserTree(t, pSrc)
	if code, _, _ := runCmd(t, root, nil, "-check"); code != 0 {
		t.Errorf("clean: %d", code)
	}
	if code, _, _ := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 1 {
		t.Errorf("problems: %d", code)
	}
	var o, e bytes.Buffer
	for _, args := range [][]string{{"-bogus"}, {}, {"-check", "-write"}, {"-check", "extra"}} {
		if code := run(args, &o, &e, nil); code != 2 {
			t.Errorf("%v: %d, want 2", args, code)
		}
	}
	if code, _, _ := runCmd(t, root, pEntry("1.0.0"), "-write"); code != 0 {
		t.Error("write failed")
	}
	if code, _, _ := runCmd(t, root, pEntry("1.0.0"), "-check"); code != 0 {
		t.Error("check after write failed")
	}
}

func TestCheckStepIsWiredIntoToolsCheck(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "check", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	step := strings.Index(src, `"./tools/parserhash", "-check"`)
	lint := strings.Index(src, `"golangci-lint"`)
	vet := strings.Index(src, `"vet"`)
	if step < 0 || lint < 0 || vet < 0 || vet >= step || step >= lint {
		t.Errorf("parserhash -check step must sit between vet (%d) and lint (%d), found at %d", vet, lint, step)
	}
}

func TestRepositoryIdentitiesAreCurrent(t *testing.T) {
	var out, e bytes.Buffer
	if code := run([]string{"-root", filepath.Join("..", ".."), "-check"}, &out, &e, entriesFromRegistry()); code != 0 {
		t.Fatalf("identity_gen.go, parsers.lock or a parser source is out of date (run go run ./tools/parserhash -write): %s", e.String())
	}
}
