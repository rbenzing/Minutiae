package archtest

import (
	"path/filepath"
	"regexp"
	"testing"
)

var (
	calIOSMemoryRE  = regexp.MustCompile(`(?m)^-- from memory$`)
	calIOSVerdictRE = regexp.MustCompile(`(?m)^-- verdict no-public-source$`)
	calIOSAnyVerd   = regexp.MustCompile(`(?m)^-- verdict `)
)

// TestCalendarIOSDDLSnapshotCitesItsSource pins the provenance of the iOS calendar snapshot (plan 4I3
// Task 7, spec 8 and 11, ruling 3, F04, F22): the directory holds exactly calendar.sql, written from
// memory, whose header says `-- from memory` and `-- verdict no-public-source` (once each);
// CROSSCHECK.md holds for every CREATE TABLE a row `| calendar.sql | <table> | no-public-source |`
// (the header's verdict, as ios_ddl_test.go requires of the iOS snapshots); neither file holds a
// URL; and the snapshot loads in the engine with exactly the tables it creates. It reads the
// snapshot through the shared helpers of calendar_ddl_helpers_test.go.
func TestCalendarIOSDDLSnapshotCitesItsSource(t *testing.T) {
	dir := calendarSchemaDir(t, "calendar-ios")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "calendar.sql" {
		t.Fatalf("snapshots %v, want exactly calendar.sql", files)
	}
	src := calendarReadFile(t, files[0])
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))
	if n := len(calIOSMemoryRE.FindAllString(src, -1)); n != 1 {
		t.Errorf("calendar.sql: %d '-- from memory' lines, want 1", n)
	}
	if len(calIOSVerdictRE.FindAllString(src, -1)) != 1 || len(calIOSAnyVerd.FindAllString(src, -1)) != 1 {
		t.Error("calendar.sql: want exactly one verdict line, '-- verdict no-public-source'")
	}
	for _, line := range calendarURLLines(src) {
		t.Errorf("calendar.sql cites a URL: %q", line)
	}
	for _, line := range calendarURLLines(report) {
		t.Errorf("CROSSCHECK.md cites a URL: %q", line)
	}
	tables := calendarCreateTables(src)
	if len(tables) == 0 {
		t.Error("calendar.sql has no CREATE TABLE")
	}
	for _, tb := range tables {
		switch got, ok := calendarCrosscheckVerdict(report, "calendar.sql", tb); {
		case !ok:
			t.Errorf("CROSSCHECK.md has no row for calendar.sql table %s", tb)
		case got != "no-public-source":
			t.Errorf("CROSSCHECK.md says %q for table %s, the header says no-public-source", got, tb)
		}
	}
	t.Run("LoadInTheEngine", func(t *testing.T) {
		if n, want := calendarLoadInEngine(t, src), len(tables); n != want {
			t.Errorf("the engine holds %d tables, the snapshot creates %d", n, want)
		}
	})
}
