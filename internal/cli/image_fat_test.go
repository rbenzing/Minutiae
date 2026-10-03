package cli

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// A warning the FAT reader raises while extracting (here a cluster chain that
// ends before the file size) is counted on the summary line ("filesystem
// warnings: N"), apart from the one skip (the partial read) the short chain also
// causes, and carried by --json as fs_warnings. The default driver registry recognizes the
// FAT volume.
func TestImageExtractReportsFATFilesystemWarnings(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := fattest.Build(o, []fattest.File{
		{Path: "SHORT.BIN", Data: make([]byte, 4*g.SectorSize*g.SecPerClus)},
		{Path: "FINE.BIN", Data: []byte("fine")},
	})
	// SHORT.BIN owns clusters 2..5; end its chain after two clusters.
	for n := range g.NumFATs {
		binary.LittleEndian.PutUint16(img[int(g.FATStart(n))*g.SectorSize+3*2:], 0xFFFF)
	}

	c := newCLICase(t)
	code, out := run(t, Deps{}, "image", "import", "--case", c, "--json", imgFile(t, img))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	ref := recs[0].ID

	code, out = run(t, Deps{}, "image", "extract", "--case", c, "-r", ref, "/")
	if code != ExitOK {
		t.Errorf("extract exit = %d, want %d (skips and filesystem warnings do not fail the run):\n%s", code, ExitOK, out)
	}
	if !strings.Contains(out, "filesystem warnings: 1 (see analysis.warning entries in audit.jsonl)") {
		t.Errorf("extract output (exit %d) lacks the filesystem warning count:\n%s", code, out)
	}
	if !strings.Contains(out, "skipped 1") {
		t.Errorf("the examine-side skip is not counted apart from the filesystem warning:\n%s", out)
	}

	code, out = run(t, Deps{}, "image", "extract", "--case", c, "-r", "--json", ref, "/")
	if code != ExitOK {
		t.Errorf("extract --json exit = %d, want %d:\n%s", code, ExitOK, out)
	}
	var sum map[string]any
	if err := json.Unmarshal(jsonPart(out), &sum); err != nil || sum["fs_warnings"] != float64(1) {
		t.Errorf("extract --json (exit %d, %v): %s", code, err, out)
	}
}
