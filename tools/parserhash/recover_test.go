package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

const (
	runnerRel  = "internal/decode/sqlitedb/recrun"
	runnerPath = testModule + "/" + runnerRel
)

func recoverTree(t *testing.T) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"internal/parsers/p/p.go": pSrc,
		runnerRel + "/r.go":       "package recrun\n\nfunc Run() int { return 1 }\n",
		lockRel:                   "# test lock\n",
		genRel:                    string(GenerateIdentity(nil)),
	})
}

func recoverEntries(version string) []Entry {
	pkg := testModule + "/internal/parsers/p"
	return []Entry{
		{Name: "p", Version: version, Package: pkg},
		{Name: "p.recover", Version: version, Package: pkg, Extra: []string{runnerPath}},
	}
}

func hashesOf(t *testing.T, root string, entries []Entry) map[string]string {
	t.Helper()
	hs, probs, err := hashEntries(testConfig(root), entries)
	if err != nil || len(probs) != 0 {
		t.Fatalf("hashEntries: %v %v", err, probs)
	}
	out := map[string]string{}
	for _, h := range hs {
		out[h.Name] = h.Hash
	}
	return out
}

func TestRecoveryHashCoversTheRunner(t *testing.T) {
	root := recoverTree(t)
	before := hashesOf(t, root, recoverEntries("1.0.0"))
	if before["p"] == before["p.recover"] {
		t.Fatal("the live and recover hashes are equal")
	}
	setFile(t, root, runnerRel+"/r.go", "package recrun\n\nfunc Run() int { return 2 }\n")
	after := hashesOf(t, root, recoverEntries("1.0.0"))
	if after["p"] != before["p"] {
		t.Error("a runner change moved the live hash")
	}
	if after["p.recover"] == before["p.recover"] {
		t.Error("a runner change did not move the recover hash")
	}
	setFile(t, root, "internal/parsers/p/p.go", strings.Replace(pSrc, "0o777", "0o776", 1))
	both := hashesOf(t, root, recoverEntries("1.0.0"))
	if both["p"] == after["p"] || both["p.recover"] == after["p.recover"] {
		t.Error("a parser change must move both hashes")
	}
}

func TestResolveUnionDeduplicatesAndSorts(t *testing.T) {
	root := recoverTree(t)
	pkg := testModule + "/internal/parsers/p"
	s, err := ResolveUnion(testConfig(root), []string{runnerPath, pkg, pkg})
	if err != nil {
		t.Fatal(err)
	}
	got := importPaths(s)
	if len(got) != 2 || got[0] != runnerPath || got[1] != pkg {
		t.Fatalf("packages = %v", got)
	}
	if _, err := ResolveUnion(testConfig(root), nil); err == nil {
		t.Error("an empty union was accepted")
	}
	if _, err := ResolveUnion(testConfig(root), []string{testModule + "/cmd/x"}); err == nil {
		t.Error("an out-of-scope root was accepted")
	}
}

type fakeParser struct{ meta parse.Meta }

func (f fakeParser) Meta() parse.Meta { return f.meta }
func (fakeParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{}, nil
}
func (fakeParser) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func TestRecoveryEntryOnlyForClaimingParsers(t *testing.T) {
	plain := fakeParser{parse.Meta{Name: "plain", Version: "1.0.0"}}
	claim := fakeParser{parse.Meta{Name: "claimer", Version: "1.2.3", Claims: []parse.TableClaim{{Role: "db", Table: "t"}}}}
	pkg := testModule + "/internal/parsers/p"
	got := entriesFor([]parse.Parser{plain, claim}, func(parse.Parser) string { return pkg }, runnerPath)
	want := []Entry{
		{Name: "plain", Version: "1.0.0", Package: pkg},
		{Name: "claimer", Version: "1.2.3", Package: pkg},
		{Name: "claimer.recover", Version: "1.2.3", Package: pkg, Extra: []string{runnerPath}},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v", got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Version != want[i].Version || got[i].Package != want[i].Package ||
			len(got[i].Extra) != len(want[i].Extra) || (len(want[i].Extra) == 1 && got[i].Extra[0] != want[i].Extra[0]) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRecoveryHashLockedLikeLive(t *testing.T) {
	root := recoverTree(t)
	if code, _, e := runCmd(t, root, recoverEntries("1.0.0"), "-write"); code != 0 {
		t.Fatalf("write: %d %q", code, e)
	}
	if code, _, e := runCmd(t, root, recoverEntries("1.0.0"), "-check"); code != 0 {
		t.Fatalf("check after write: %d %q", code, e)
	}
	setFile(t, root, runnerRel+"/r.go", "package recrun\n\nfunc Run() int { return 2 }\n")
	code, _, e := runCmd(t, root, recoverEntries("1.0.0"), "-check")
	if code != 1 || !strings.Contains(e, "p.recover@1.0.0: source changed since version 1.0.0 was locked: bump Meta.Version") {
		t.Fatalf("runner change under the same version: %d %q", code, e)
	}
	if strings.Count(e, "bump Meta.Version") != 1 {
		t.Errorf("only the recover line may be reported: %q", e)
	}
	if code, _, e := runCmd(t, root, recoverEntries("1.0.0"), "-write"); code != 1 || !strings.Contains(e, "p.recover@1.0.0") {
		t.Errorf("write must refuse to rewrite the locked recover line: %d %q", code, e)
	}
	if code, out, e := runCmd(t, root, recoverEntries("1.0.1"), "-write"); code != 0 || !strings.Contains(out, "locked p.recover 1.0.1") {
		t.Errorf("bumped write: %d %q %q", code, out, e)
	}
}
