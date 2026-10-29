package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// slackPattern is non-uniform filler: a tail holding it is exported as slack.
func slackPattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%7)
	}
	return b
}

// slackFATImage is a FAT16 image (1024-byte clusters) with two files whose tails hold a pattern, an empty file
// (skipped: no runs) and 8192 pattern bytes appended past the filesystem (volume slack).
func slackFATImage(t *testing.T) []byte {
	t.Helper()
	files := []fattest.File{
		{Path: "A.TXT", Data: bytes.Repeat([]byte("x"), 10)},
		{Path: "B.TXT", Data: bytes.Repeat([]byte("y"), 600)},
		{Path: "E.TXT"},
	}
	img, built := fattest.BuildLayout(fattest.Options{Type: 16, SecPerClus: 2}, files)
	cs := int64(built.Geometry.SecPerClus * built.Geometry.SectorSize)
	for path, size := range map[string]int64{"A.TXT": 10, "B.TXT": 600} {
		e := built.Find(path, false)
		off := built.Geometry.ClusterOffset(e.Clusters[len(e.Clusters)-1]) + size%cs
		copy(img[off:], slackPattern(int(cs-size%cs), 0xA0))
	}
	return append(img, slackPattern(8192, 0x30)...)
}

// slackCutImage has a directory /D whose chain leaves the volume, so its walk is cut; A.TXT keeps its slack.
func slackCutImage(t *testing.T) []byte {
	t.Helper()
	files := []fattest.File{{Path: "A.TXT", Data: []byte("ten bytes!")}, {Path: "D", Dir: true}, {Path: "D/ALPHA.TXT", Data: []byte("first file")}}
	for i := range 29 {
		files = append(files, fattest.File{Path: fmt.Sprintf("D/F%02d.TXT", i), Data: []byte{1}})
	}
	img, built := fattest.BuildLayout(fattest.Options{Type: 16}, files)
	g := built.Geometry
	d := built.Find("D", false)
	if len(d.Clusters) < 2 {
		t.Fatalf("setup: /D has %d clusters", len(d.Clusters))
	}
	for n := range g.NumFATs {
		o := int(g.FATStart(n))*g.SectorSize + int(d.Clusters[0])*2
		binary.LittleEndian.PutUint16(img[o:], 0xF000) // the chain leaves the volume
	}
	a := built.Find("A.TXT", false)
	copy(img[g.ClusterOffset(a.Clusters[0])+10:], slackPattern(g.SecPerClus*g.SectorSize-10, 0xA0))
	return img
}

type slackEnv struct {
	c, ref string
}

func newSlackEnv(t *testing.T, img []byte) slackEnv {
	t.Helper()
	c := newCLICase(t)
	code, out := run(t, Deps{}, "image", "import", "--case", c, "--json", imgFile(t, img))
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	return slackEnv{c: c, ref: recs[0].ID}
}

// slack runs `image slack` with separate stdout and stderr.
func (e slackEnv) slack(args ...string) (code int, stdout, stderr string) {
	var o, er bytes.Buffer
	d := Deps{Out: &o, Err: &er, In: strings.NewReader(""), Registry: device.NewRegistry()}
	full := append([]string{"image", "slack", e.ref, "--case", e.c}, args...)
	return Run(full, d), o.String(), er.String()
}

func TestImageSlackRun(t *testing.T) {
	e := newSlackEnv(t, slackFATImage(t))
	code, out, errOut := e.slack()
	if code != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	for _, want := range []string{
		"slack of partition 0 (fat16, unit 1024 bytes): 3 live files examined",
		"file slack: 2 segment(s) exported (",
		"uniform segment(s) counted, not exported (",
		"skipped: no-runs 1",
		"volume slack: ",
		"[recovered:slack-file conf 10 low]",
		"[recovered:slack-volume conf 10 low]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "[incomplete]") {
		t.Errorf("a complete run prints no incomplete mark:\n%s", out)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d %s", code, out)
	}
}

func TestImageSlackJSON(t *testing.T) {
	e := newSlackEnv(t, slackFATImage(t))
	code, out, errOut := e.slack("--json")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	var j jsonSlack
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&j); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout holds more than the JSON document: %q", out)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"analysis_id", "partition", "fs_type", "unit", "files_examined", "file_segments", "file_bytes", "uniform_segments", "uniform_bytes",
		"volume_segments", "volume_bytes", "volume_uniform_chunks", "volume_uniform_bytes", "skipped_by", "walk_errors", "incomplete", "fs_warnings", "artifacts",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("JSON lacks key %q", k)
		}
	}
	if _, ok := raw["limit_reached"]; ok {
		t.Error("limit_reached must be omitted when empty")
	}
	if j.FSType != "fat16" || j.Unit != 1024 || j.FilesExamined != 3 || j.FileSegments != 2 || j.FileBytes != 1014+424 ||
		j.VolumeSegments != 1 || j.VolumeBytes != 8192 || j.SkippedBy["no-runs"] != 1 || j.Incomplete {
		t.Errorf("summary = %+v", j)
	}
	if len(j.Artifacts) != 4 {
		t.Fatalf("%d artifacts, want 4 manifest records", len(j.Artifacts))
	}
	methods := map[string]int{}
	for _, a := range j.Artifacts {
		if r := a.Source.Derived.Recovery; r != nil {
			methods[r.Method]++
		}
	}
	if methods["slack-file"] != 1 || methods["slack-volume"] != 1 {
		t.Errorf("methods = %v", methods)
	}

	// A second, text-mode run on a copy of the case reports the same counts.
	e2 := newSlackEnv(t, slackFATImage(t))
	_, text, _ := e2.slack()
	for _, want := range []string{
		"2 segment(s) exported (" + humanBytes(j.FileBytes) + ")",
		"1 segment(s) exported (" + humanBytes(j.VolumeBytes) + ")",
		strconv.Itoa(j.FilesExamined) + " live files examined",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text run lacks %q:\n%s", want, text)
		}
	}
}

func TestImageSlackFlags(t *testing.T) {
	e := newSlackEnv(t, slackFATImage(t))
	if code, _, errOut := e.slack("--files", "--volume"); code != 2 {
		t.Errorf("--files --volume: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := e.slack("--max-bytes", "-1"); code != 2 {
		t.Errorf("--max-bytes -1: exit %d, stderr %q", code, errOut)
	}
	paths := func(flag string) []string {
		e := newSlackEnv(t, slackFATImage(t))
		code, out, errOut := e.slack(flag, "--json")
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", flag, code, out, errOut)
		}
		var j jsonSlack
		if err := json.Unmarshal([]byte(out), &j); err != nil {
			t.Fatal(err)
		}
		var ps []string
		for _, a := range j.Artifacts {
			ps = append(ps, filepath.Base(a.Path))
		}
		return ps
	}
	if got := paths("--files"); !reflect.DeepEqual(got, []string{"slack.runs.jsonl", "slack.bin"}) {
		t.Errorf("--files wrote %v", got)
	}
	if got := paths("--volume"); !reflect.DeepEqual(got, []string{"volume-slack.runs.jsonl", "volume-slack.bin"}) {
		t.Errorf("--volume wrote %v", got)
	}
}

func TestImageSlackUnsupportedFilesystem(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "filesys", "hfsplus", "testdata", "hfsplus-empty.img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	e := newSlackEnv(t, img)
	audit := func() int {
		b, err := os.ReadFile(filepath.Join(e.c, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(b), "\n")
	}
	before := audit()
	code, out, errOut := e.slack()
	if code != 1 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "hfsplus") || !strings.Contains(errOut, "does not report allocation units") {
		t.Errorf("stderr = %q", errOut)
	}
	if got := audit() - before; got != 1 {
		t.Errorf("%d audit entries written, want only case.open", got)
	}
}

func TestImageSlackPrintsIncomplete(t *testing.T) {
	e := newSlackEnv(t, slackCutImage(t))
	code, out, errOut := e.slack("--files")
	if code != 0 {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if !strings.Contains(out, "INCOMPLETE: 1 directory not read to the end; slack of the files in them is unknown") {
		t.Errorf("no INCOMPLETE line:\n%s", out)
	}
	var fileLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[recovered:slack-file") {
			fileLine = l
		}
	}
	if !strings.HasSuffix(fileLine, " [incomplete]") {
		t.Errorf("file-slack line = %q", fileLine)
	}
	_, jout, _ := e.slack("--files", "--json")
	var j jsonSlack
	if err := json.Unmarshal([]byte(jout), &j); err != nil || !j.Incomplete || j.WalkErrors != 1 {
		t.Errorf("json = %+v, %v", j, err)
	}
}
