package archtest

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// calendarAndroidSnapshots are the five Android calendar.db snapshots of plan 4I3 (spec 7.4).
var calendarAndroidSnapshots = []string{"403-upgraded-601.sql", "403.sql", "502.sql", "600.sql", "601.sql"}

var (
	calTagRE      = regexp.MustCompile(`(?m)^-- tag (android-[0-9.]+_r[0-9]+)$`)
	calAnyTagRE   = regexp.MustCompile(`(?m)^-- tag `)
	calVerdictRE  = regexp.MustCompile(`(?m)^-- verdict (verified|historical)$`)
	calAnyVerdict = regexp.MustCompile(`(?m)^-- verdict `)
	calVersionRE  = regexp.MustCompile(`(?m)^-- user_version ([0-9]+)$`)
	calAnyVersion = regexp.MustCompile(`(?m)^-- user_version `)
	calFileNumRE  = regexp.MustCompile(`([0-9]+)\.sql$`)
)

// TestCalendarAndroidDDLSnapshotsCiteTheirSource pins the provenance of the Android calendar
// snapshots (plan 4I3 Task 2, spec 11, 4I3-P3, F06, F31). The header grammar is three one-line
// facts, `-- tag android-…`, `-- verdict verified|historical` and `-- user_version <n>`, each exactly
// once; it deliberately differs from the three-line `-- Source:` / `(verdict V)` header that
// ios_ddl_test.go checks, because one directory here holds the snapshots of four tags and each
// snapshot names the tag its run checked and the user version its fixtures carry (F22).
// CROSSCHECK.md holds, for every CREATE TABLE of every snapshot, a row with the snapshot's verdict
// (or "MANUAL, read by hand at <file>:<line>" for a verified table the script could not evaluate),
// and no MISMATCH; no snapshot or report line holds a URL. The user version is the snapshot's own
// number (the last number of its file name).
func TestCalendarAndroidDDLSnapshotsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "calendar-android")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	slices.Sort(names)
	if !slices.Equal(names, calendarAndroidSnapshots) {
		t.Fatalf("snapshots %v, want exactly %v", names, calendarAndroidSnapshots)
	}
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))
	if strings.Contains(report, "MISMATCH") {
		t.Error("CROSSCHECK.md lists a MISMATCH: fix the snapshot, never the report")
	}
	for _, line := range calendarURLLines(report) {
		t.Errorf("CROSSCHECK.md cites a URL: %q", line)
	}
	for _, base := range names {
		src := calendarReadFile(t, filepath.Join(dir, base))
		for _, line := range calendarURLLines(src) {
			t.Errorf("%s cites a URL: %q", base, line)
		}
		tag, vd, uv := calTagRE.FindAllStringSubmatch(src, -1), calVerdictRE.FindAllStringSubmatch(src, -1), calVersionRE.FindAllStringSubmatch(src, -1)
		if len(tag) != 1 || len(calAnyTagRE.FindAllString(src, -1)) != 1 {
			t.Errorf("%s: want exactly one '-- tag android-<version>_r<n>' line", base)
		}
		if len(vd) != 1 || len(calAnyVerdict.FindAllString(src, -1)) != 1 {
			t.Errorf("%s: want exactly one '-- verdict verified' or '-- verdict historical' line", base)
			continue
		}
		if len(uv) != 1 || len(calAnyVersion.FindAllString(src, -1)) != 1 {
			t.Errorf("%s: want exactly one '-- user_version <n>' line", base)
		} else if n := calFileNumRE.FindStringSubmatch(base); n == nil || n[1] != uv[0][1] {
			t.Errorf("%s: user_version %s is not the snapshot's number", base, uv[0][1])
		}
		if want := strings.Contains(base, "upgraded"); want != (vd[0][1] == "historical") {
			t.Errorf("%s: verdict %s (an upgraded database is historical, a fresh bootstrap verified)", base, vd[0][1])
		}
		tables := calendarCreateTables(src)
		if len(tables) == 0 {
			t.Errorf("%s has no CREATE TABLE", base)
		}
		for _, tb := range tables {
			got, ok := calendarCrosscheckVerdict(report, base, tb)
			switch {
			case !ok:
				t.Errorf("CROSSCHECK.md has no row for %s table %s", base, tb)
			case got == vd[0][1]:
			case vd[0][1] == "verified" && strings.HasPrefix(got, "MANUAL, read by hand at "):
			default:
				t.Errorf("%s says verdict %s but CROSSCHECK.md says %q for table %s", base, vd[0][1], got, tb)
			}
		}
	}
	t.Run("LoadInTheEngine", func(t *testing.T) {
		for _, base := range names {
			src := calendarReadFile(t, filepath.Join(dir, base))
			if n, want := calendarLoadInEngine(t, src), len(calendarCreateTables(src)); n != want {
				t.Errorf("%s: the engine holds %d tables, the snapshot creates %d", base, n, want)
			}
		}
	})
}

// TestCalendarCrosscheckVerdictReadsTheThirdCell: the helper's row is anchored at the start of a line
// and reads the third cell only, so the Evidence column cannot satisfy it (self-test of the helper).
func TestCalendarCrosscheckVerdictReadsTheThirdCell(t *testing.T) {
	report := "| a.sql | T | verified | evidence |\n| a.sql | TT | historical | x |\nprose | a.sql | U | verified |\n| b.sql | V | MANUAL, read by hand at H.java:12 | x |\n"
	for _, c := range []struct {
		file, table, want string
		ok                bool
	}{
		{"a.sql", "T", "verified", true},
		{"a.sql", "TT", "historical", true},
		{"a.sql", "U", "", false},
		{"b.sql", "V", "MANUAL, read by hand at H.java:12", true},
		{"a.sql", "X", "", false},
	} {
		got, ok := calendarCrosscheckVerdict(report, c.file, c.table)
		if got != c.want || ok != c.ok {
			t.Errorf("%s %s: %q %v, want %q %v", c.file, c.table, got, ok, c.want, c.ok)
		}
	}
	if got := calendarURLLines("a\nsee http x\nb"); len(got) != 1 || !strings.Contains(got[0], "http") {
		t.Errorf("URL lines %q", got)
	}
	if got := strconv.Itoa(len(calendarCreateTables("CREATE TABLE a (x);\n-- CREATE TABLE b (y);\nCREATE TABLE c (z);"))); got != "2" {
		t.Errorf("%s tables, want 2", got)
	}
}
