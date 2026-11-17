package archtest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recoveryFixtureNames returns the base names (without .img.gz), sorted and unique, of every recovery fixture
// image: internal/filesys/*/testdata/{recover,slack,rollforward,journal}/*.img.gz and
// internal/examine/testdata/carve/*.img.gz.
func recoveryFixtureNames(root string) ([]string, error) {
	var files []string
	for _, kind := range []string{"recover", "slack", "rollforward", "journal"} {
		m, err := filepath.Glob(filepath.Join(root, "internal", "filesys", "*", "testdata", kind, "*.img.gz"))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	m, err := filepath.Glob(filepath.Join(root, "internal", "examine", "testdata", "carve", "*.img.gz"))
	if err != nil {
		return nil, err
	}
	files = append(files, m...)
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".img.gz"))
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// fixtureTable is the text of tools/fixtures/README.md from the line "## Fixtures" to the next "## " heading.
func fixtureTable(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "tools", "fixtures", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var sec []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "## Fixtures":
			in = true
		case in && strings.HasPrefix(l, "## "):
			return strings.Join(sec, "\n")
		case in:
			sec = append(sec, l)
		}
	}
	if !in {
		t.Fatal("tools/fixtures/README.md has no `## Fixtures` heading")
	}
	return strings.Join(sec, "\n")
}

// Every recovery, slack, roll-forward, journal and carving fixture image is named in the README's Fixtures table, so
// an examiner finds the script that regenerates it and the source of its oracle (3K-R3). A missing name fails with
// the list of every missing name.
func TestFixtureTableNamesEveryRecoveryFixture(t *testing.T) {
	root := repoRoot(t)
	names, err := recoveryFixtureNames(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 20 {
		t.Fatalf("found only %d recovery fixtures %v: the glob is wrong or the fixtures moved", len(names), names)
	}
	table := fixtureTable(t, root)
	var missing []string
	for _, n := range names {
		if !strings.Contains(table, "`"+n+"`") {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the Fixtures table of tools/fixtures/README.md names no row for %d fixtures: %s", len(missing), strings.Join(missing, ", "))
	}
}
