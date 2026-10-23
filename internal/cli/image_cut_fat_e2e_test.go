package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// fatCutImage is a FAT16 image whose directory /D (ALPHA.TXT and 29 more
// files: two 512-byte clusters, no end marker) has its chain broken after the
// first cluster, so the scan of /D cannot read it to the end. The chain is
// found from the root entry and the FAT with plain byte reads, independently of
// the reader.
func fatCutImage(t *testing.T) []byte {
	t.Helper()
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	files := []fattest.File{{Path: "D", Dir: true}, {Path: "D/ALPHA.TXT", Data: []byte("first file")}}
	for i := range 29 {
		files = append(files, fattest.File{Path: fmt.Sprintf("D/F%02d.TXT", i), Data: []byte{1}})
	}
	img := fattest.Build(o, files)
	root := int(g.FATStart(g.NumFATs)) * g.SectorSize
	i := bytes.Index(img[root:root+16*32], []byte("D          "))
	if i < 0 {
		t.Fatal("the entry of /D is not in the root directory")
	}
	first := int(binary.LittleEndian.Uint16(img[root+i+26:]))
	for n := range g.NumFATs {
		off := int(g.FATStart(n))*g.SectorSize + first*2
		if next := binary.LittleEndian.Uint16(img[off:]); next < 2 || next >= 0xFFF8 {
			t.Fatalf("setup: /D does not continue after its first cluster (%#x)", next)
		}
		binary.LittleEndian.PutUint16(img[off:], 0xF000) // the chain leaves the volume
	}
	return img
}

// A FAT directory whose scan was cut, driven through the command line: `image
// ls` and `image extract` of a name that is not there exit non-zero with the
// incomplete-scan message (never "not found", never 0), the extract writes the
// audited "incomplete" reason, and `image ls -r` prints the entries that were
// read before it fails.
func TestImageCutFATDirectoryExitsNonZeroAndIsAudited(t *testing.T) {
	d := Deps{}
	cdir := newCLICase(t)
	code, out := run(t, d, "image", "import", "--case", cdir, "--json", imgFile(t, fatCutImage(t)))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	ref := recs[0].ID

	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		dd := Deps{Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
		full := append([]string{"image", args[0], "--case", cdir}, args[1:]...)
		return Run(full, dd), stdout.String(), stderr.String()
	}

	if code, stdout, stderr := do("ls", ref, "/D/alpha.txt"); code != 0 {
		t.Fatalf("ls of a readable name: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	for _, args := range [][]string{
		{"ls", ref, "/D/missing"},
		{"extract", "-r", ref, "/D/missing"},
	} {
		code, stdout, stderr := do(args...)
		if code != ExitError {
			t.Errorf("%v: exit %d, want %d\nstdout %q\nstderr %q", args, code, ExitError, stdout, stderr)
		}
		if !strings.Contains(stderr, "cannot be decided") {
			t.Errorf("%v: stderr does not say the lookup cannot be decided:\n%s", args, stderr)
		}
		if strings.Contains(stderr, "not found") {
			t.Errorf("%v: the lookup in a cut directory is reported as not found:\n%s", args, stderr)
		}
	}

	code, stdout, stderr := do("ls", "-r", ref, "/D")
	if code == 0 {
		t.Errorf("ls -r of a cut directory exits 0\nstdout %q\nstderr %q", stdout, stderr)
	}
	if !strings.Contains(stdout, "ALPHA.TXT") {
		t.Errorf("ls -r does not print the entries read before the cut:\nstdout %q\nstderr %q", stdout, stderr)
	}

	entries, err := evidence.ReadAuditEntries(filepath.Join(cdir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action != "analysis.error" {
			continue
		}
		n++
		if got, _ := e.Details["reason"].(string); got != "incomplete" {
			t.Errorf("analysis.error reason = %q, want \"incomplete\" (%v)", got, e.Details)
		}
		if got, _ := e.Details["path"].(string); got != "/D/missing" {
			t.Errorf("analysis.error path = %q, want /D/missing", got)
		}
	}
	if n != 1 {
		t.Errorf("%d analysis.error entries, want 1 (the extract; ls writes none)", n)
	}
}
