package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// undecidedFS answers every Lookup under /cf with filesys.ErrUndecided, as a casefold directory that holds
// a name of an unmeasured shape does when nothing else decides the lookup.
type undecidedFS struct{ filesys.FileSystem }

func (u undecidedFS) Lookup(p string) (filesys.Entry, error) {
	if strings.HasPrefix(p, "/cf") {
		return filesys.Entry{}, fmt.Errorf("fake: %w: casefold lookup of %q in directory inode 7: 1 name(s) have a shape the comparison was not measured for", filesys.ErrUndecided, p)
	}
	return u.FileSystem.Lookup(p)
}

func undecidedEnv(t *testing.T) (ref string, runSplit func(args ...string) (int, string, string)) {
	t.Helper()
	d := Deps{FSDrivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return undecidedFS{fsys}, nil
		},
	}}}
	c := newCLICase(t)
	code, out := run(t, d, "image", "import", "--case", c, "--json", imgFile(t, imgDisk(defaultNodes()...)))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	ref = recs[0].ID
	return ref, func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		dd := d
		dd.Out, dd.Err, dd.In, dd.Registry = &stdout, &stderr, strings.NewReader(""), device.NewRegistry()
		full := append([]string{"image", args[0], "--case", c}, args[1:]...)
		return Run(full, dd), stdout.String(), stderr.String()
	}
}

// Ruling Z45: a lookup that cannot be decided exits non-zero (never 0) and says so in words; it is never
// reported as "not found". The text names the situation and what the examiner can do.
func TestImageUndecidedLookupExitsNonZeroWithAClearMessage(t *testing.T) {
	ref, runSplit := undecidedEnv(t)
	for _, args := range [][]string{
		{"ls", ref, "/cf/x"},
		{"stat", ref, "/cf/x"},
		{"extract", "-r", ref, "/cf/x"},
	} {
		code, stdout, stderr := runSplit(args...)
		if code == 0 {
			t.Errorf("%v: exit 0 for an undecidable lookup\nstdout %q\nstderr %q", args, stdout, stderr)
		}
		if code != ExitError {
			t.Errorf("%v: exit %d, want %d", args, code, ExitError)
		}
		for _, want := range []string{"cannot be decided", "neither found nor absent"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%v: stderr lacks %q:\n%s", args, want, stderr)
			}
		}
		if strings.Contains(stderr, "not found") && !strings.Contains(stderr, "neither found nor absent") {
			t.Errorf("%v: the undecidable lookup is reported as not found:\n%s", args, stderr)
		}
		if strings.Contains(stdout, "x") && strings.Contains(stdout, "/cf") {
			t.Errorf("%v: stdout names the path as if it existed:\n%s", args, stdout)
		}
	}
}

func TestExitCodeOfAnUndecidedLookup(t *testing.T) {
	err := fmt.Errorf("/cf/x: %w", fmt.Errorf("ext4: %w: casefold lookup", filesys.ErrUndecided))
	if got := ExitCode(err); got != ExitError {
		t.Errorf("ExitCode = %d, want %d (a typed error of the image content, never 0)", got, ExitError)
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Error("an undecided error is also not-found")
	}
}
