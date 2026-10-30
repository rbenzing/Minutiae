package archtest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	iosVerdictRe = regexp.MustCompile(`\(verdict ([^)]*)\)`)
	iosCreateRe  = regexp.MustCompile(`(?m)^CREATE TABLE (\w+)`)
	iosVerdicts  = map[string]bool{"verified": true, "historical": true, "no-public-source": true}
)

// iosDDLProblems checks every *.sql snapshot of dir against the CROSSCHECK.md text and the
// limitations text, and returns one problem per violation (plan 4E, Task 4). A snapshot states
// its provenance in a three-line header, exactly one `(verdict V)`; CROSSCHECK.md holds a row
// `| file | table | V |` for each of its tables with the same verdict; a snapshot without a
// public source says it is from memory and is listed by name in docs/limitations.md.
func iosDDLProblems(dir, crosscheck, limitations string) []string {
	var problems []string
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return []string{err.Error()}
	}
	for _, f := range files {
		base := filepath.Base(f)
		raw, err := os.ReadFile(f)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		src := string(raw)
		lines := strings.Split(src, "\n")
		if len(lines) < 4 || !strings.HasPrefix(lines[0], "-- ") ||
			!strings.HasPrefix(lines[1], "-- Source: ") || !strings.HasPrefix(lines[2], "-- Per-table verdict: ") {
			problems = append(problems, base+": the header is not description / '-- Source:' / '-- Per-table verdict:' lines")
			continue
		}
		vs := iosVerdictRe.FindAllStringSubmatch(src, -1)
		if len(vs) != 1 || !strings.Contains(lines[1], vs[0][0]) {
			problems = append(problems, fmt.Sprintf("%s: want exactly one '(verdict V)', on the '-- Source:' line; found %d", base, len(vs)))
			continue
		}
		verdict := vs[0][1]
		if !iosVerdicts[verdict] {
			problems = append(problems, fmt.Sprintf("%s: verdict %q is not verified, historical or no-public-source", base, verdict))
			continue
		}
		memory := strings.HasPrefix(lines[1], "-- Source: memory")
		if memory != (verdict == "no-public-source") {
			problems = append(problems, fmt.Sprintf("%s: verdict %s does not agree with the source line (memory iff no-public-source)", base, verdict))
		}
		for _, m := range iosCreateRe.FindAllStringSubmatch(src, -1) {
			row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(base) + ` \| ` + regexp.QuoteMeta(m[1]) + ` \| ([a-z-]+) \|`)
			rv := row.FindStringSubmatch(crosscheck)
			switch {
			case rv == nil:
				problems = append(problems, fmt.Sprintf("CROSSCHECK.md has no row for %s table %s", base, m[1]))
			case rv[1] != verdict:
				problems = append(problems, fmt.Sprintf("%s says %s but CROSSCHECK.md says %s for table %s", base, verdict, rv[1], m[1]))
			}
		}
		if verdict == "no-public-source" && !strings.Contains(limitations, base) {
			problems = append(problems, base+": no-public-source but not named in docs/limitations.md")
		}
	}
	if strings.Contains(crosscheck, "| MISMATCH |") || strings.Contains(crosscheck, "| MANUAL |") {
		problems = append(problems, "CROSSCHECK.md lists a MISMATCH or MANUAL row")
	}
	return problems
}

func TestIOSDDLSnapshotsStateTheirProvenance(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "tools", "fixtures", "schemas", "ios")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) != 9 {
		t.Fatalf("%d snapshots in %s (want 9): %v", len(files), dir, err)
	}
	cc, err := os.ReadFile(filepath.Join(dir, "CROSSCHECK.md"))
	if err != nil {
		t.Fatal(err)
	}
	lim, err := os.ReadFile(filepath.Join(root, "docs", "limitations.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range iosDDLProblems(dir, string(cc), string(lim)) {
		t.Error(p)
	}
}

func TestIOSDDLRuleSelfTest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, verdictLine, body string) {
		t.Helper()
		src := "-- description\n" + verdictLine + "\n-- Per-table verdict: CROSSCHECK.md\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mem := "-- Source: memory (no public schema source) (verdict no-public-source)."
	write("nov.sql", "-- Source: memory (no public schema source).", "CREATE TABLE a (x);")
	write("guess.sql", "-- Source: memory (verdict guessed).", "CREATE TABLE b (x);")
	write("missing.sql", mem, "CREATE TABLE c (x);\nCREATE TABLE d (x);")
	write("unlisted.sql", mem, "CREATE TABLE e (x);")
	cc := "| missing.sql | c | no-public-source |\n| unlisted.sql | e | no-public-source |\n"
	lim := "missing.sql nov.sql guess.sql"
	got := iosDDLProblems(dir, cc, lim)
	if len(got) != 4 {
		t.Fatalf("want 4 problems, got %d: %q", len(got), got)
	}
	for i, want := range []string{"guess.sql", "missing.sql", "nov.sql", "unlisted.sql"} {
		if !strings.HasPrefix(got[i], want) && !strings.Contains(got[i], want) {
			t.Errorf("problem %d = %q, want it to name %s", i, got[i], want)
		}
	}
	// The clean case: the same files fixed give no problem.
	write("nov.sql", mem, "CREATE TABLE a (x);")
	write("guess.sql", mem, "CREATE TABLE b (x);")
	cc += "| nov.sql | a | no-public-source |\n| guess.sql | b | no-public-source |\n| missing.sql | d | no-public-source |\n"
	if got := iosDDLProblems(dir, cc, lim+" unlisted.sql"); len(got) != 0 {
		t.Errorf("clean set reports %q", got)
	}
}
