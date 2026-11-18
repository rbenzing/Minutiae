package archtest

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)
)

func whatsappIOSSchemaDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "tools", "fixtures", "schemas", "whatsapp-ios")
}

// TestWhatsAppIOSDDLSnapshotsCiteTheirSource pins ruling 4I2-R13: the three iOS WhatsApp Core Data
// snapshots name their sources by ID only (never a URL), state exactly one verdict, and have one
// CROSSCHECK.md row `| <file> | <table> | from-memory |` per CREATE TABLE and no row naming a table the file lacks, say
// that the column types are not checked against a public definition, and define Z_PRIMARYKEY.
func TestWhatsAppIOSDDLSnapshotsCiteTheirSource(t *testing.T) {
	dir := whatsappIOSSchemaDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) != 3 {
		t.Fatalf("%d snapshots in %s (want 3): %v", len(files), dir, err)
	}
	report, err := os.ReadFile(filepath.Join(dir, "CROSSCHECK.md"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(report), "\n") {
		if strings.Contains(line, "http") {
			t.Errorf("CROSSCHECK.md line %d names a URL; sources are cited by ID only", i+1)
		}
	}
	source := regexp.MustCompile(`(?m)^-- source S\d+( \(.*\))?$`)
	verdict := regexp.MustCompile(`(?m)^-- verdict from memory$`)
	anyVerdict := regexp.MustCompile(`(?m)^-- verdict `)
	create := regexp.MustCompile(`(?m)^CREATE TABLE (\w+) `)
	comment := regexp.MustCompile(`(?m)^-- ?`)
	space := regexp.MustCompile(`\s+`)
	rowOf := regexp.MustCompile(`(?m)^\| (\w+\.sql) \| (\w+) \| from-memory \|`)
	defined := map[string]map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src, base := string(raw), filepath.Base(f)
		// the sentence may wrap across comment lines
		if !strings.Contains(space.ReplaceAllString(comment.ReplaceAllString(src, ""), " "), "not checked against a public definition") {
			t.Errorf("%s does not say that its column types are not checked against a public definition", base)
		}
		if !source.MatchString(src) {
			t.Errorf("%s has no '-- source S<n>' line", base)
		}
		if len(verdict.FindAllString(src, -1)) != 1 || len(anyVerdict.FindAllString(src, -1)) != 1 {
			t.Errorf("%s must state exactly one '-- verdict from memory' line", base)
		}
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "http") {
				t.Errorf("%s line %d names a URL; sources are cited by ID only", base, i+1)
			}
		}
		tables := create.FindAllStringSubmatch(src, -1)
		if len(tables) == 0 {
			t.Errorf("%s has no CREATE TABLE", base)
		}
		defined[base] = map[string]bool{}
		for _, m := range tables {
			defined[base][m[1]] = true
		}
		if !defined[base]["Z_PRIMARYKEY"] {
			t.Errorf("%s does not define Z_PRIMARYKEY", base)
		}
		for _, m := range tables {
			row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(base) + ` \| ` + regexp.QuoteMeta(m[1]) + ` \| from-memory \|`)
			if !row.Match(report) {
				t.Errorf("CROSSCHECK.md has no '| %s | %s | from-memory |' row", base, m[1])
			}
		}
	}
	// the reverse direction: a verdict row names a CREATE TABLE of its own file
	for _, m := range rowOf.FindAllStringSubmatch(string(report), -1) {
		if !defined[m[1]][m[2]] {
			t.Errorf("CROSSCHECK.md row '%s | %s' names no CREATE TABLE of that file", m[1], m[2])
		}
	}
}

// TestWhatsAppIOSDDLSnapshotsLoadInTheEngine loads every snapshot into an engine database: a typo in a
// constructed statement fails here, not in a fixture later.
func TestWhatsAppIOSDDLSnapshotsLoadInTheEngine(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(whatsappIOSSchemaDir(t), "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no snapshots: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(string(raw)); err != nil {
				t.Fatalf("the snapshot does not load: %v", err)
			}
			var n int
			if err := db.QueryRow("select count(*) from sqlite_master where type = 'table'").Scan(&n); err != nil || n == 0 {
				t.Fatalf("no table was created (%d): %v", n, err)
			}
		})
	}
}
