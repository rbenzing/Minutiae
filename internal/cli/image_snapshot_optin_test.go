package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestImageLsRecursiveIncludeSnapshots(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	code, out := e.image(t, "ls", e.ref, "/Data", "-r", "--include-snapshots")
	if code != 0 {
		t.Fatalf("ls -r --include-snapshots: %d\n%s", code, out)
	}
	for _, want := range []string{"/Data/.snapshots/S/docs/old.txt", "/Data/.snapshots/S/docs/a.txt", "/Data/docs/a.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %s:\n%s", want, out)
		}
	}
	if n, _ := examineNotes(t, e.c); n != 0 {
		t.Errorf("an opted-in listing skips nothing and must write no note, wrote %d", n)
	}
	// the default is unchanged
	e2 := apfsEnv(t, snapshotVolume("Data"))
	_, out = e2.image(t, "ls", e2.ref, "/Data", "-r")
	if strings.Contains(out, "old.txt") {
		t.Errorf("the default recursive listing must not descend into .snapshots:\n%s", out)
	}
	if strings.Contains(out, "[snapshot") {
		t.Errorf("the default listing carries a snapshot marker:\n%s", out)
	}
}

// Snapshot entries of an opted-in listing are marked as snapshot data in text and JSON; live
// entries and the default output carry no marker.
func TestImageLsIncludeSnapshotsMarksSnapshotData(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	_, out := e.image(t, "ls", e.ref, "/Data", "-r", "--include-snapshots")
	var marked, live int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "/Data/.snapshots/S/"):
			marked++
			if !strings.HasSuffix(line, " [snapshot S]") {
				t.Errorf("a snapshot line is not marked: %q", line)
			}
		case strings.Contains(line, "/Data/docs") || strings.Contains(line, "/Data/added.txt"):
			live++
			if strings.Contains(line, "[snapshot") {
				t.Errorf("a live line is marked as snapshot data: %q", line)
			}
		}
	}
	if marked != 4 || live != 3 {
		t.Errorf("%d snapshot and %d live lines, want 4 and 3:\n%s", marked, live, out)
	}

	_, out = e.image(t, "ls", e.ref, "/Data", "-r", "--include-snapshots", "--json")
	var entries []struct {
		Path     string `json:"path"`
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(jsonPart(out), &entries); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	for _, en := range entries {
		inSnap := en.Path == "/Data/.snapshots/S" || strings.HasPrefix(en.Path, "/Data/.snapshots/S/")
		if inSnap && en.Snapshot != "S" {
			t.Errorf("%s: snapshot = %q, want S", en.Path, en.Snapshot)
		}
		if !inSnap && en.Snapshot != "" {
			t.Errorf("%s: a live entry names snapshot %q", en.Path, en.Snapshot)
		}
	}

	_, out = e.image(t, "ls", e.ref, "/Data", "-r", "--json")
	if strings.Contains(out, `"snapshot"`) {
		t.Errorf("the default JSON listing carries a snapshot field:\n%s", out)
	}
}

func TestImageExtractRecursiveIncludeSnapshots(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	code, out := e.image(t, "extract", e.ref, "/Data", "-r", "--include-snapshots")
	if code != 0 {
		t.Fatalf("extract -r --include-snapshots: %d\n%s", code, out)
	}
	var live, snap int
	for _, r := range manifestRecords(t, e.c) {
		d := r.Source.Derived
		if r.Source.Kind != "extract" || d == nil {
			continue
		}
		if strings.HasPrefix(d.FSPath, "/Data/.snapshots/S/") {
			snap++
			if d.Snapshot == nil || d.Snapshot.Name != "S" {
				t.Errorf("%s: a snapshot artifact must record its snapshot, got %+v", d.FSPath, d.Snapshot)
			}
		} else {
			live++
			if d.Snapshot != nil {
				t.Errorf("%s: a live artifact records snapshot %+v", d.FSPath, d.Snapshot)
			}
		}
	}
	if live != 2 || snap != 2 {
		t.Errorf("%d live and %d snapshot artifacts, want 2 and 2", live, snap)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Errorf("case verify: %d\n%s", code, out)
	}
	// The choice is audited in analysis.start, before any artifact of the run.
	entries, err := evidence.ReadAuditEntries(filepath.Join(e.c, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	started := false
	for _, en := range entries {
		if en.Action == "analysis.start" && en.Details["include_snapshots"] == true {
			started = true
		}
	}
	if !started {
		t.Error("no analysis.start records include_snapshots=true")
	}
}

// Without the flag the extraction is unchanged: the snapshots are skipped and the audit says false.
func TestImageExtractRecursiveDefaultSkipsSnapshots(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	if code, out := e.image(t, "extract", e.ref, "/Data", "-r"); code != 0 {
		t.Fatalf("extract -r: %d\n%s", code, out)
	}
	for _, r := range manifestRecords(t, e.c) {
		if d := r.Source.Derived; r.Source.Kind == "extract" && d != nil && strings.Contains(d.FSPath, "/.snapshots/") {
			t.Errorf("the default extraction took snapshot data %s", d.FSPath)
		}
	}
	entries, err := evidence.ReadAuditEntries(filepath.Join(e.c, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Action == "analysis.start" && en.Details["include_snapshots"] == true {
			t.Error("a default run records include_snapshots=true")
		}
	}
}

func TestImageIncludeSnapshotsConflictsWithSnapshot(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	for _, args := range [][]string{
		{"ls", e.ref, "/Data", "-r", "--include-snapshots", "--snapshot", "S"},
		{"extract", e.ref, "/Data", "-r", "--include-snapshots", "--snapshot", "S"},
		{"ls", e.ref, "/Data", "--include-snapshots"},
		{"extract", e.ref, "/Data", "--include-snapshots"},
	} {
		if code, out := e.image(t, args[0], args[1:]...); code != ExitUsage || !strings.Contains(out, "include-snapshots") {
			t.Errorf("%v: code %d out %q; want a usage error naming the flag", args, code, out)
		}
	}
}

// snapshotListing lists every entry of `ls -r <args>` in text and in JSON and returns the path
// each is marked with (empty: unmarked), keyed by path, checking that both forms agree.
func snapshotListing(t *testing.T, e *imgEnv, args ...string) map[string]string {
	t.Helper()
	code, out := e.image(t, "ls", append([]string{e.ref}, args...)...)
	if code != 0 {
		t.Fatalf("ls %v: %d\n%s", args, code, out)
	}
	text := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		mark := ""
		if i := strings.LastIndex(line, " [snapshot "); i >= 0 && strings.HasSuffix(line, "]") {
			mark = strings.TrimSuffix(line[i+len(" [snapshot "):], "]")
			line = line[:i]
		}
		f := strings.Fields(line)
		text[strings.TrimSuffix(f[len(f)-1], "/")] = mark
	}
	_, out = e.image(t, "ls", append([]string{e.ref}, append(args, "--json")...)...)
	var entries []struct {
		Path     string `json:"path"`
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(jsonPart(out), &entries); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(entries) != len(text) {
		t.Fatalf("text lists %d entries, JSON %d", len(text), len(entries))
	}
	for _, en := range entries {
		if m, ok := text[en.Path]; !ok || m != en.Snapshot {
			t.Errorf("%s: text marker %q (listed %v), JSON %q", en.Path, m, ok, en.Snapshot)
		}
	}
	return text
}

// A listing that STARTS inside the snapshots still marks every entry as snapshot data, wherever it
// starts; the marker comes from the path, not from what the walk happened to enter (Y83).
func TestImageLsMarksSnapshotDataFromAnyStartPath(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	for _, start := range []string{"/Data/.snapshots", "/Data/.snapshots/S", "/Data/.snapshots/S/docs"} {
		got := snapshotListing(t, e, start, "-r", "--include-snapshots")
		var below int
		for p, m := range got {
			if p == start || p == "/Data/.snapshots" {
				continue // the start itself: .snapshots is synthetic; below it is checked
			}
			below++
			if m != "S" {
				t.Errorf("start %s: %s marked %q, want S", start, p, m)
			}
		}
		if below == 0 {
			t.Errorf("start %s listed nothing below it: %v", start, got)
		}
	}
}

// ls --snapshot S -r carries the same marker (Y84).
func TestImageLsSnapshotFlagMarksSnapshotData(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	got := snapshotListing(t, e, "/Data", "--snapshot", "S", "-r")
	if len(got) == 0 {
		t.Fatal("empty listing")
	}
	for p, m := range got {
		if m != "S" {
			t.Errorf("%s marked %q, want S", p, m)
		}
	}
}

// An opted-in listing appends no audit entry and creates no artifact (Y85).
func TestImageLsIncludeSnapshotsWritesNothing(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	audit := func() []evidence.AuditEntry {
		entries, err := evidence.ReadAuditEntries(filepath.Join(e.c, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	before, beforeArts := audit(), len(manifestRecords(t, e.c))
	if code, out := e.image(t, "ls", e.ref, "/Data", "-r", "--include-snapshots"); code != 0 {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	// Every command that opens the case appends its case.open; nothing else may be appended.
	for _, en := range audit()[len(before):] {
		if en.Action != "case.open" {
			t.Errorf("the listing appended a %s audit entry", en.Action)
		}
	}
	if n := len(manifestRecords(t, e.c)); n != beforeArts {
		t.Errorf("the listing created %d manifest records", n-beforeArts)
	}
}
