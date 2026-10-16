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
	verdict := regexp.MustCompile(`\| (verified|historical|no-public-source) \|`)
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
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			row := "| " + base + " | " + m[1] + " |"
			i := strings.Index(string(report), row)
			if i < 0 || !verdict.MatchString(string(report)[i:min(len(report), i+len(row)+40)]) {
				t.Errorf("CROSSCHECK.md has no verdict row for %s table %s", base, m[1])
			}
		}
	}
	if strings.Contains(string(report), "| MISMATCH |") || strings.Contains(string(report), "| MANUAL |") {
		t.Error("CROSSCHECK.md still lists a MISMATCH or a MANUAL row: fix the snapshot or read the source and record the verdict")
	}
}
