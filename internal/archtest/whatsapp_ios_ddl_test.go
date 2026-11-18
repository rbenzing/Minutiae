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
// CROSSCHECK.md row `| <file> | <table> | from-memory |` per CREATE TABLE.
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
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src, base := string(raw), filepath.Base(f)
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
		for _, m := range tables {
			row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(base) + ` \| ` + regexp.QuoteMeta(m[1]) + ` \| from-memory \|`)
			if !row.Match(report) {
				t.Errorf("CROSSCHECK.md has no '| %s | %s | from-memory |' row", base, m[1])
			}
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
