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

func whatsappSchemaDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "tools", "fixtures", "schemas", "whatsapp-android")
}

// whatsappVerdicts maps the verdict a snapshot states about itself to the word CROSSCHECK.md uses.
var whatsappVerdicts = map[string]string{"quoted": "quoted", "retyped (no licence)": "retyped", "from memory": "from-memory"}

// TestWhatsAppDDLSnapshotsCiteTheirSource pins ruling 4I-R6: every WhatsApp DDL snapshot names its
// source by ID, states one verdict, and has one CROSSCHECK.md row per table with that verdict; a
// quoted snapshot carries its licence notice and a full commit; the sources of S4 to S7 and S10 to
// S13 are never given by anything but their ID; the retyped modern snapshot copies no view or trigger.
func TestWhatsAppDDLSnapshotsCiteTheirSource(t *testing.T) {
	dir := whatsappSchemaDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) != 5 {
		t.Fatalf("%d snapshots in %s (want 5): %v", len(files), dir, err)
	}
	report, err := os.ReadFile(filepath.Join(dir, "CROSSCHECK.md"))
	if err != nil {
		t.Fatal(err)
	}
	source := regexp.MustCompile(`(?m)^-- source S[0-9]+ `)
	verdictRE := regexp.MustCompile(`(?m)^-- verdict (quoted|retyped \(no licence\)|from memory)$`)
	anyVerdict := regexp.MustCompile(`(?m)^-- verdict `)
	create := regexp.MustCompile(`(?m)^CREATE TABLE (\w+) `)
	commit := regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	idOnly := regexp.MustCompile(`^-- source S(4|5|6|7|10|11|12|13)\b`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src, base := string(raw), filepath.Base(f)
		if !source.MatchString(src) {
			t.Errorf("%s has no '-- source S<n> ' line", base)
		}
		vs := verdictRE.FindAllStringSubmatch(src, -1)
		if len(vs) != 1 || len(anyVerdict.FindAllString(src, -1)) != 1 {
			t.Errorf("%s must state exactly one '-- verdict' line (quoted, retyped (no licence) or from memory)", base)
			continue
		}
		verdict := vs[0][1]
		if verdict == "quoted" && (!strings.Contains(src, "MIT License") || !commit.MatchString(src)) {
			t.Errorf("%s is quoted: it must carry the MIT licence notice and a 40-hex commit", base)
		}
		for _, line := range strings.Split(src, "\n") {
			if idOnly.MatchString(line) && strings.Contains(line, "http") {
				t.Errorf("%s: %q names a source that is cited by ID only", base, line)
			}
		}
		tables := create.FindAllStringSubmatch(src, -1)
		if len(tables) == 0 {
			t.Errorf("%s has no CREATE TABLE", base)
		}
		for _, m := range tables {
			row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(base) + ` \| ` + regexp.QuoteMeta(m[1]) + ` \| ([a-z-]+) \|`)
			rv := row.FindStringSubmatch(string(report))
			switch {
			case rv == nil:
				t.Errorf("CROSSCHECK.md has no verdict row for %s table %s", base, m[1])
			case rv[1] != whatsappVerdicts[verdict]:
				t.Errorf("%s says %q but CROSSCHECK.md says %s for table %s", base, verdict, rv[1], m[1])
			}
		}
		if base == "modern-2026.sql" && (strings.Contains(src, "CREATE VIEW") || strings.Contains(src, "CREATE TRIGGER")) {
			t.Errorf("%s is retyped from an unlicensed source: it must hold no view and no trigger", base)
		}
	}
}

// TestWhatsAppDDLSnapshotsLoadInTheEngine loads every snapshot into an engine database: a typo in a
// retyped statement fails here, not in a fixture later.
func TestWhatsAppDDLSnapshotsLoadInTheEngine(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(whatsappSchemaDir(t), "*.sql"))
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
