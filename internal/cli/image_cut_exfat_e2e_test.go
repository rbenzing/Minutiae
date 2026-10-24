package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// exfatImageEnv imports img into a fresh case and returns the evidence ref and
// a runner for `image <verb> --case <case> ...`.
func exfatImageEnv(t *testing.T, img []byte) (cdir, ref string, do func(args ...string) (int, string, string)) {
	t.Helper()
	cdir = newCLICase(t)
	code, out := run(t, Deps{}, "image", "import", "--case", cdir, "--json", imgFile(t, img))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	return cdir, recs[0].ID, func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		dd := Deps{Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
		full := append([]string{"image", args[0], "--case", cdir}, args[1:]...)
		return Run(full, dd), stdout.String(), stderr.String()
	}
}

// exfatLoopImage is an exFAT image whose directory /D has three clusters (120
// files) and whose chain loops back after the second one: the end marker in
// the third cluster is never reached, so a missing name cannot be decided.
func exfatLoopImage(t *testing.T) []byte {
	t.Helper()
	files := []exfattest.File{{Path: "/D", Dir: true, FatChain: true}}
	for i := range 120 {
		files = append(files, exfattest.File{Path: fmt.Sprintf("/D/f%03d", i), Data: []byte("x")})
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{ClusterCount: 512}, files)
	dc := lay.Find("/D", false).Clusters
	if len(dc) != 3 {
		t.Fatalf("directory has %d clusters, want 3", len(dc))
	}
	off := int(lay.FatEntryOffset(dc[1]))
	img[off], img[off+1], img[off+2], img[off+3] = byte(dc[0]), byte(dc[0]>>8), byte(dc[0]>>16), byte(dc[0]>>24)
	return img
}

// An exFAT directory whose chain is damaged, driven through the command line:
// a name that is not there exits non-zero with the incomplete-scan message
// (never "not found", never 0), the readable part still lists, and the extract
// audits the "incomplete" reason before it returns.
func TestImageCutExfatDirectoryExitsNonZeroAndIsAudited(t *testing.T) {
	cdir, ref, do := exfatImageEnv(t, exfatLoopImage(t))

	if code, stdout, stderr := do("ls", ref, "/D/f000"); code != 0 {
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

// A volume without an up-case table folds nothing: the case variant of a stored
// name cannot be decided (exit non-zero, never found, never not found), the
// stored name itself resolves.
func TestImageExfatUndecidedLookupExitsNonZero(t *testing.T) {
	img := exfattest.Build(exfattest.Options{NoUpcase: true}, []exfattest.File{{Path: "/zebra", Data: []byte("z")}})
	_, ref, do := exfatImageEnv(t, img)

	code, stdout, stderr := do("ls", ref, "/ZEBRA")
	if code != ExitError {
		t.Errorf("ls /ZEBRA: exit %d, want %d\nstdout %q\nstderr %q", code, ExitError, stdout, stderr)
	}
	if !strings.Contains(stderr, "cannot be decided") || strings.Contains(stderr, "not found") && !strings.Contains(stderr, "neither found nor absent") {
		t.Errorf("stderr must say the lookup cannot be decided, never not found:\n%s", stderr)
	}
	if code, stdout, stderr := do("ls", ref, "/zebra"); code != 0 {
		t.Errorf("ls /zebra: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
}
