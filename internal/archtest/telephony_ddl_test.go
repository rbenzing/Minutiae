package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every telephony DDL snapshot cites its public source (tag and file) or says it has none,
// and CROSSCHECK.md holds a verdict for each of its tables (ruling T13).
func TestTelephonyDDLSnapshotsCiteTheirSourceAndHaveAVerdict(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "tools", "fixtures", "schemas", "android-telephony")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) != 8 {
		t.Fatalf("%d snapshots in %s (want 8): %v", len(files), dir, err)
	}
	report, err := os.ReadFile(filepath.Join(dir, "CROSSCHECK.md"))
	if err != nil {
		t.Fatal(err)
	}
	cite := regexp.MustCompile(`(?m)^-- Source: .*tag android-[0-9.]+_r[0-9]+, \S+\.java`)
	create := regexp.MustCompile(`(?m)^CREATE TABLE (\w+) `)
	header := regexp.MustCompile(`\(?verdict (verified|historical)\b`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(f)
		if !cite.Match(src) {
			t.Errorf("%s does not cite a public source (tag and Java file) in a '-- Source:' line", base)
		}
		if strings.Contains(string(src), "from memory") && !strings.Contains(string(src), "no-public-source") {
			t.Errorf("%s still says 'from memory' for a table that has a public source", base)
		}
		hv := header.FindAllStringSubmatch(string(src), -1)
		agree := len(hv) > 0
		for _, h := range hv {
			agree = agree && h[1] == hv[0][1]
		}
		if !agree {
			t.Errorf("%s must state 'verdict verified' or 'verdict historical' at least once, and never both", base)
			continue
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			// The row is anchored at the start of its line and its verdict is the third cell, so
			// neither the Evidence column nor a longer row can satisfy it.
			row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(base) + ` \| ` + regexp.QuoteMeta(m[1]) + ` \| (verified|historical|no-public-source) \|`)
			rv := row.FindStringSubmatch(string(report))
			switch {
			case rv == nil:
				t.Errorf("CROSSCHECK.md has no verdict row for %s table %s", base, m[1])
			case rv[1] != hv[0][1]:
				t.Errorf("%s says verdict %s but CROSSCHECK.md says %s for table %s", base, hv[0][1], rv[1], m[1])
			}
		}
	}
	if strings.Contains(string(report), "| MISMATCH |") || strings.Contains(string(report), "| MANUAL |") {
		t.Error("CROSSCHECK.md still lists a MISMATCH or a MANUAL row: fix the snapshot or read the source and record the verdict")
	}
}
