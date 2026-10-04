package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
)

// apfsDrivers opens raw APFS containers (the registry gets the driver with the
// unallocated-space task; the CLI tests inject it).
var apfsDrivers = []detect.Driver{{
	Name: "apfs", Probe: apfs.Probe,
	Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		f, err := apfs.Open(r, size)
		if err != nil {
			return nil, err
		}
		return f, nil
	},
}}

// apfsEnv imports an APFS container (the whole image, no partition table).
func apfsEnv(t *testing.T, vols ...apfstest.Volume) *imgEnv {
	t.Helper()
	e := &imgEnv{d: Deps{FSDrivers: apfsDrivers}, c: newCLICase(t)}
	img := apfstest.Build(apfstest.Options{Blocks: 2048, Xid: 20, Volumes: vols})
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, img))
	if code != 0 {
		t.Fatalf("image import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	return e
}

func snapshotVolume(name string) apfstest.Volume {
	return apfstest.Volume{
		Name: name, UUID: [16]byte{1}, CaseInsensitive: true, NormInsensitive: true,
		Files: []apfstest.File{
			{Path: "/docs/a.txt", Data: []byte("new a")},
			{Path: "/added.txt", Data: []byte("added")},
		},
		Snapshots: []apfstest.Snapshot{{Name: "S", Files: []apfstest.File{
			{Path: "/docs/a.txt", Data: []byte("old a")},
			{Path: "/docs/old.txt", Data: []byte("old")},
		}}},
	}
}

func TestImageLsSnapshot(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))

	// Default path: the only volume's root, mapped into the snapshot.
	code, out := e.image(t, "ls", e.ref, "--snapshot", "S")
	if code != 0 || !strings.Contains(out, "docs/") || strings.Contains(out, "added.txt") {
		t.Fatalf("ls --snapshot S: %d\n%s", code, out)
	}
	code, out = e.image(t, "ls", e.ref, "/Data/docs", "--snapshot", "S")
	if code != 0 || !strings.Contains(out, "a.txt") || !strings.Contains(out, "old.txt") {
		t.Fatalf("ls /Data/docs --snapshot S: %d\n%s", code, out)
	}

	// -r prints real paths, usable by stat and extract.
	code, out = e.image(t, "ls", e.ref, "/Data", "--snapshot", "S", "-r")
	if code != 0 {
		t.Fatalf("ls -r --snapshot: %d\n%s", code, out)
	}
	for _, want := range []string{"/Data/.snapshots/S/docs/a.txt", "/Data/.snapshots/S/docs/old.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls -r --snapshot lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "added.txt") || strings.Contains(out, "\n/Data/docs") {
		t.Errorf("the snapshot listing shows live files:\n%s", out)
	}
	code, out = e.image(t, "stat", e.ref, "/Data/.snapshots/S/docs/a.txt")
	if code != 0 || !strings.Contains(out, "/Data/.snapshots/S/docs/a.txt") {
		t.Errorf("stat of a printed path: %d\n%s", code, out)
	}

	// --json: the same real paths.
	code, out = e.image(t, "ls", e.ref, "/Data/docs", "--snapshot", "S", "-r", "--json")
	if code != 0 {
		t.Fatalf("ls --json --snapshot: %d\n%s", code, out)
	}
	var entries []struct{ Path string }
	if err := json.Unmarshal(jsonPart(out), &entries); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	var got []string
	for _, en := range entries {
		got = append(got, en.Path)
	}
	want := []string{"/Data/.snapshots/S/docs/a.txt", "/Data/.snapshots/S/docs/old.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("json paths = %v, want %v", got, want)
	}

	// The live tree is untouched by the flag's absence, and a recursive listing
	// of the volume shows the .snapshots directory but does not descend into it.
	code, out = e.image(t, "ls", e.ref, "/Data", "-r")
	if code != 0 || !strings.Contains(out, "/Data/docs/a.txt") || !strings.Contains(out, "/Data/added.txt") {
		t.Fatalf("ls -r: %d\n%s", code, out)
	}
	if !strings.Contains(out, "/Data/.snapshots/") || strings.Contains(out, "/Data/.snapshots/S") {
		t.Errorf("ls -r must list .snapshots without descending into it:\n%s", out)
	}
}

func TestImageLsSnapshotUnsupportedFilesystem(t *testing.T) {
	e := newImgEnv(t) // an MTFS image: its filesystem has no snapshots
	code, out := e.image(t, "ls", e.ref, "--snapshot", "S")
	if code != ExitUsage || !strings.Contains(out, "no snapshots") {
		t.Errorf("ls --snapshot on a filesystem without snapshots: %d, want %d with 'no snapshots':\n%s", code, ExitUsage, out)
	}
}

func TestImageLsSnapshotUnknownName(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	code, out := e.image(t, "ls", e.ref, "--snapshot", "nope\x1b[31m")
	if code != ExitError || !strings.Contains(out, `"S"`) || strings.Contains(out, "\x1b") {
		t.Errorf("ls --snapshot nope: %d, want %d naming the snapshots (escaped):\n%q", code, ExitError, out)
	}
}

func TestImageLsSnapshotNeedsVolume(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"), apfstest.Volume{Name: "Other", UUID: [16]byte{2}})
	code, out := e.image(t, "ls", e.ref, "--snapshot", "S")
	if code != ExitUsage || !strings.Contains(out, "volume") {
		t.Errorf("ls --snapshot S at the container root: %d, want %d (needs a volume path):\n%s", code, ExitUsage, out)
	}
	// With a volume it works; a path that is not a volume is not found.
	if code, out = e.image(t, "ls", e.ref, "/Data", "--snapshot", "S"); code != 0 {
		t.Errorf("ls /Data --snapshot S: %d\n%s", code, out)
	}
	if code, _ = e.image(t, "ls", e.ref, "id:n:0:0:2", "--snapshot", "S"); code != ExitUsage {
		t.Errorf("--snapshot with an id: path: %d, want %d", code, ExitUsage)
	}
}

// examineNotes counts the analysis.warning entries with source "examine" in the
// case's audit log.
func examineNotes(t *testing.T, caseDir string) (n int, first map[string]any) {
	t.Helper()
	entries, err := evidence.ReadAuditEntries(filepath.Join(caseDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "analysis.warning" && e.Details["source"] == "examine" {
			if n == 0 {
				first = e.Details
			}
			n++
		}
	}
	return n, first
}

// A recursive listing that skips .snapshots on a volume that HAS snapshots
// writes exactly one audit entry saying so; a volume without snapshots, a
// non-recursive listing and a listing of the snapshots write none.
func TestImageLsRecursiveNotesSkippedSnapshots(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	if code, out := e.image(t, "ls", e.ref, "/Data"); code != 0 {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	if n, _ := examineNotes(t, e.c); n != 0 {
		t.Errorf("a non-recursive ls wrote %d notes", n)
	}
	if code, out := e.image(t, "ls", e.ref, "/Data", "-r"); code != 0 {
		t.Fatalf("ls -r: %d\n%s", code, out)
	}
	n, d := examineNotes(t, e.c)
	if n != 1 || d["path"] != "/Data/.snapshots" || !strings.Contains(d["reason"].(string), "--snapshot") {
		t.Fatalf("after ls -r: %d notes, first %v; want exactly one for /Data/.snapshots", n, d)
	}
	if code, out := e.image(t, "ls", e.ref, "/Data", "--snapshot", "S", "-r"); code != 0 {
		t.Fatalf("ls --snapshot -r: %d\n%s", code, out)
	}
	if n, _ := examineNotes(t, e.c); n != 1 {
		t.Errorf("listing a snapshot wrote another note (%d in total)", n)
	}

	e = apfsEnv(t, apfstest.Volume{Name: "Plain", UUID: [16]byte{3}, Files: []apfstest.File{{Path: "/a.txt", Data: []byte("a")}}})
	if code, out := e.image(t, "ls", e.ref, "/Plain", "-r"); code != 0 {
		t.Fatalf("ls -r (no snapshots): %d\n%s", code, out)
	}
	if n, _ := examineNotes(t, e.c); n != 0 {
		t.Errorf("a volume without snapshots wrote %d notes", n)
	}
}

// manifestRecords reads the case manifest.
func manifestRecords(t *testing.T, caseDir string) []evidence.ManifestRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(caseDir, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.ManifestRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r evidence.ManifestRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("manifest line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// Bytes extracted from a snapshot view record the snapshot (name and xid) in
// the derivation; a live extraction leaves it empty; case verify is clean.
func TestImageExtractRecordsSnapshotProvenance(t *testing.T) {
	e := apfsEnv(t, snapshotVolume("Data"))
	if code, out := e.image(t, "extract", e.ref, "/Data/.snapshots/S/docs/a.txt"); code != 0 {
		t.Fatalf("extract snapshot file: %d\n%s", code, out)
	}
	if code, out := e.image(t, "extract", e.ref, "/Data/docs/a.txt"); code != 0 {
		t.Fatalf("extract live file: %d\n%s", code, out)
	}
	byPath := map[string]*evidence.Derivation{}
	for _, r := range manifestRecords(t, e.c) {
		if d := r.Source.Derived; d != nil {
			byPath[d.FSPath] = d
		}
	}
	snap, live := byPath["/Data/.snapshots/S/docs/a.txt"], byPath["/Data/docs/a.txt"]
	if snap == nil || live == nil {
		t.Fatalf("derivations by path: %v", byPath)
	}
	if snap.Snapshot == nil || snap.Snapshot.Name != "S" || snap.Snapshot.Xid != 2 {
		t.Errorf("snapshot extract: Snapshot = %+v, want {S 2}", snap.Snapshot)
	}
	if live.Snapshot != nil {
		t.Errorf("live extract: Snapshot = %+v, want none", live.Snapshot)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Errorf("case verify: %d\n%s", code, out)
	}
}
