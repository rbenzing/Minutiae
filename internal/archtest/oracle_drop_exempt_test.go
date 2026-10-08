package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The engine-oracle tests of internal/sqlitefile and internal/decode build
// throwaway fixture databases with an independent engine, and drop tables in
// them (a dropped table is exactly the evidence they need). The single-writer
// rule stays strict for the record tables and for any non-constant table name;
// only a DROP TABLE of a plainly named, non-record table in a _test.go file of those
// two packages is exempt (ruling for plan 3I).

var oracleDropRE = regexp.MustCompile(`(?is)^drop\s+table\s+(?:if\s+exists\s+)?([a-z_][a-z_0-9]*)$`)

var oracleExemptDirs = []string{"internal/sqlitefile/", "internal/decode/"}

// oracleDropExempt reports whether the violation text at repository-relative
// path rel is such a DROP.
func oracleDropExempt(rel, text string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.HasSuffix(rel, "_test.go") {
		return false
	}
	in := false
	for _, d := range oracleExemptDirs {
		if strings.HasPrefix(rel, d) {
			in = true
		}
	}
	if !in {
		return false
	}
	m := oracleDropRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return false
	}
	return !regexp.MustCompile(`(?i)^` + recordTables + `$`).MatchString(m[1])
}

// TestOracleDropExemptionIsNarrow proves the exemption covers exactly a plain
// DROP of a non-record table in a _test.go file of the two oracle packages. The
// cases live in testdata/oracle_drop_cases.txt (not compiled, not scanned).
func TestOracleDropExemptionIsNarrow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "oracle_drop_cases.txt"))
	if err != nil {
		t.Fatal(err)
	}
	n, exempt := 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("bad case line %q", line)
		}
		want := f[2] == "true"
		n++
		if want {
			exempt++
		}
		if got := oracleDropExempt(f[0], f[1]); got != want {
			t.Errorf("oracleDropExempt(%q, %q) = %v, want %v", f[0], f[1], got, want)
		}
	}
	if n < 10 || exempt == 0 || exempt == n {
		t.Errorf("%d cases, %d exempt: the table does not test both sides", n, exempt)
	}
}
