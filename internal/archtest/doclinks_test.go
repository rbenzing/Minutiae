package archtest

import (
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	docLinkRe     = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	docCodeSpanRe = regexp.MustCompile("`[^`\n]*`")
)

// docSplitFolders maps each folder of the documentation set whose files an index must list to
// that index.
var docSplitFolders = [][2]string{
	{"docs/invariants", "docs/invariants.md"},
	{"docs/limitations", "docs/limitations.md"},
	{"docs/adr", "docs/adr/README.md"},
	{"docs/systems", "docs/systems/README.md"},
}

// docFiles lists the documentation set under root as slash-separated relative paths: the two
// root documents and every Markdown file under docs/ except the local-only docs/superpowers.
func docFiles(root string) ([]string, error) {
	var out []string
	for _, f := range []string{"CLAUDE.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			out = append(out, f)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && rel == "docs/superpowers" {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(rel, ".md") {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// docLinkTargets returns the relative link targets of a Markdown text, outside code, with any
// anchor removed and percent-escapes decoded. Web, mail and same-page links are left out.
func docLinkTargets(text string) []string {
	var prose strings.Builder
	fenced := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			prose.WriteString(docCodeSpanRe.ReplaceAllString(l, "") + "\n")
		}
	}
	var out []string
	for _, m := range docLinkRe.FindAllStringSubmatch(prose.String(), -1) {
		t := m[1]
		if strings.HasPrefix(t, "#") || strings.Contains(t, "://") || strings.HasPrefix(t, "mailto:") {
			continue
		}
		if i := strings.IndexByte(t, '#'); i >= 0 {
			t = t[:i]
		}
		if u, err := url.PathUnescape(t); err == nil {
			t = u
		}
		out = append(out, t)
	}
	return out
}

// docLinkProblems returns one problem per relative Markdown link, in the documentation set under
// root, whose target does not exist.
func docLinkProblems(root string) []string {
	files, err := docFiles(root)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(rel)))
		for _, t := range docLinkTargets(string(raw)) {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(t))); err != nil {
				problems = append(problems, rel+": "+t+" does not resolve")
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// docIndexProblems returns one problem per document of a split folder that its index does not
// link.
func docIndexProblems(root string) []string {
	var problems []string
	for _, f := range docSplitFolders {
		dir := filepath.Join(root, filepath.FromSlash(f[0]))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		index := filepath.Join(root, filepath.FromSlash(f[1]))
		raw, err := os.ReadFile(index)
		if err != nil {
			problems = append(problems, f[1]+": the index of "+f[0]+" cannot be read: "+err.Error())
			continue
		}
		linked := map[string]bool{}
		for _, t := range docLinkTargets(string(raw)) {
			linked[filepath.Clean(filepath.Join(filepath.Dir(index), filepath.FromSlash(t)))] = true
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".md") || n == "README.md" || n == "_template.md" || n == "0000-template.md" {
				continue
			}
			if !linked[filepath.Clean(filepath.Join(dir, n))] {
				problems = append(problems, f[0]+"/"+n+": not linked from "+f[1])
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func TestDocLinksResolve(t *testing.T) {
	for _, p := range docLinkProblems(repoRoot(t)) {
		t.Error(p)
	}
}

func TestDocSplitFoldersAreIndexed(t *testing.T) {
	for _, p := range docIndexProblems(repoRoot(t)) {
		t.Error(p)
	}
}

func TestDocRulesSelfTest(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("CLAUDE.md", "[map](docs/README.md) [gone](docs/missing.md)\n")
	write("README.md", "[web](https://example.com) [anchor](#usage) [mail](mailto:a@b.c)\n")
	write("docs/README.md", "[ok](invariants.md#areas) [space](My%20Guide/README.md) `[code](nowhere.md)`\n"+
		"```\n[fenced](nowhere.md)\n```\n")
	write("docs/My Guide/README.md", "x\n")
	write("docs/invariants.md", "[case](invariants/case.md)\n")
	write("docs/invariants/case.md", "[up](../invariants.md) [bad](recovery.md)\n")
	write("docs/invariants/apfs.md", "orphan\n")
	write("docs/limitations.md", "none\n")
	write("docs/adr/README.md", "[t](0000-template.md) [one](0001-x.md)\n")
	write("docs/adr/0000-template.md", "x\n")
	write("docs/adr/0001-x.md", "x\n")
	write("docs/adr/0002-y.md", "x\n")
	write("docs/systems/README.md", "[t](_template.md)\n")
	write("docs/systems/_template.md", "x\n")
	write("docs/superpowers/plans/p.md", "[local only](nowhere.md)\n")

	links := docLinkProblems(root)
	wantLinks := []string{"CLAUDE.md: docs/missing.md", "docs/invariants/case.md: recovery.md"}
	if len(links) != len(wantLinks) {
		t.Fatalf("link problems %q, want %d naming %q", links, len(wantLinks), wantLinks)
	}
	for i, w := range wantLinks {
		if !strings.Contains(links[i], w) {
			t.Errorf("link problem %d is %q, want it to name %q", i, links[i], w)
		}
	}

	index := docIndexProblems(root)
	wantIndex := []string{"docs/adr/0002-y.md", "docs/invariants/apfs.md"}
	if len(index) != len(wantIndex) {
		t.Fatalf("index problems %q, want %d naming %q", index, len(wantIndex), wantIndex)
	}
	for i, w := range wantIndex {
		if !strings.Contains(index[i], w) {
			t.Errorf("index problem %d is %q, want it to name %q", i, index[i], w)
		}
	}
}
