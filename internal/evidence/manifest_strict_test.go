package evidence_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// replaceManifestLine replaces line n (1-based) of the case's manifest.jsonl by raw bytes.
func replaceManifestLine(t testing.TB, caseDir string, n int, line []byte) {
	t.Helper()
	p := filepath.Join(caseDir, "manifest.jsonl")
	b, err := os.ReadFile(p) //nolint:gosec // test helper on a case directory the test created
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if n < 1 || n > len(lines) {
		t.Fatalf("manifest has %d lines, not %d", len(lines), n)
	}
	lines[n-1] = line
	if err := os.WriteFile(p, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
}

// strictCase returns a case with two artifacts (an image and a derived extract with a time map) and
// the lines of its manifest, which the rows below edit as text.
func strictCase(t *testing.T) (*evidence.Case, []string) {
	t.Helper()
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, bytes.Repeat([]byte{7}, 4096))
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a.bin",
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, Times: map[string]string{"modified": "2024-01-02T03:04:05Z"},
	}
	src := evidence.Source{Kind: "extract", DeviceID: parent.Source.DeviceID, Derived: d}
	if _, err := c.Capture(parent.Source.DeviceID, "x1", "p1-mtfs/a.bin", src, func(w io.Writer) error {
		_, err := w.Write(bytes.Repeat([]byte{7}, 512))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Dir, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return c, strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestVerifyManifestStrict(t *testing.T) {
	for _, tc := range []struct {
		name string
		line int // 1-based manifest line edited
		edit func(l string) string
		want string // problem substring, "" = no strict problem
	}{
		{"clean", 2, func(l string) string { return l }, ""},
		{"unknown top-level", 2, func(l string) string { return strings.Replace(l, `{"id"`, `{"note":"x","id"`, 1) }, "manifest line 2: note"},
		{"unknown nested", 2, func(l string) string { return strings.Replace(l, `"fs_type"`, `"fs_tipe"`, 1) }, "source.derived.fs_tipe: not a field of this build"},
		{"case variant", 1, func(l string) string { return strings.Replace(l, `"sha256"`, `"SHA256":"00","sha256"`, 1) }, `SHA256: key spelled differently from "sha256"`},
		{"duplicate", 1, func(l string) string { return strings.Replace(l, `"size"`, `"size":1,"size"`, 1) }, "size: duplicate key"},
		{"omitted by this build", 1, func(l string) string { return strings.Replace(l, `"started"`, `"error":"","started"`, 1) }, "error: written empty, this build omits it"},
		{"omitted nested", 1, func(l string) string {
			return strings.Replace(l, `"device_id"`, `"remote_path":"","device_id"`, 1)
		}, "source.remote_path: written empty, this build omits it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, lines := strictCase(t)
			replaceManifestLine(t, c.Dir, tc.line, []byte(tc.edit(lines[tc.line-1])))
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "manifest line")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

func TestVerifyManifestStrictListsAtMostFifty(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := bytes.Repeat([]byte{7}, 64)
	for i := range 60 {
		src := evidence.Source{Kind: "import", DeviceID: evidencetest.ImageDevice, OriginalPath: fmt.Sprintf("d%d.img", i), Segment: 1, Segments: 1}
		if _, err := c.Capture(evidencetest.ImageDevice, evidencetest.ImageAcq, fmt.Sprintf("d%d.img", i), src, func(w io.Writer) error {
			_, err := w.Write(img)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(c.Dir, "manifest.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte(`{"id"`), []byte(`{"note":"x","id"`))
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // test file in a case directory the test created
		t.Fatal(err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, pr := range rep.Problems {
		if strings.HasPrefix(pr, "manifest line ") {
			listed++
		}
	}
	if listed != 50 {
		t.Errorf("listed %d manifest line problems, want 50", listed)
	}
	evidencetest.RequireProblem(t, rep, "10 further manifest decode problems are not listed")
}

// TestManifestOpenStaysLenient (PASSES ON ARRIVAL: the open path is unchanged; mutation: make readManifest strict).
func TestManifestOpenStaysLenient(t *testing.T) {
	c, lines := strictCase(t)
	replaceManifestLine(t, c.Dir, 1, []byte(strings.Replace(lines[0], `{"id"`, `{"note":"x","id"`, 1)))
	recs, err := c.Manifest()
	if err != nil || len(recs) != 2 {
		t.Fatalf("Manifest() = %d records, %v; want 2 records and no error", len(recs), err)
	}
}

func TestVerifyStrictManifestCleanOnRealCases(t *testing.T) {
	c := evidencetest.NewCase(t)
	data := bytes.Repeat([]byte{9}, 8192)
	parent := evidencetest.AddImage(t, c, data)
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 512}}})
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000002-b.bin", Runs: []evidence.Run{{Offset: 512, Length: 512}}, Sidecar: true,
	})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "manifest line")

	v2, err := evidence.Open(recordstest.CopyV2Case(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()
	rep, err = v2.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "manifest line")
}

func TestVerifyStrictManifestToleratesMissingZeroKey(t *testing.T) {
	c, lines := strictCase(t)
	if !strings.Contains(lines[0], `,"incomplete":false`) {
		t.Fatalf("test premise: no incomplete key in %s", lines[0])
	}
	replaceManifestLine(t, c.Dir, 1, []byte(strings.Replace(lines[0], `,"incomplete":false`, "", 1)))
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "manifest line")

	// a zero-valued nested key (partition 0 of the image-level derivation) is tolerated too
	replaceManifestLine(t, c.Dir, 1, []byte(lines[0]))
	if !strings.Contains(lines[1], `"partition_offset":0`) {
		t.Skip("no zero partition_offset to remove")
	}
	replaceManifestLine(t, c.Dir, 2, []byte(strings.Replace(lines[1], `"partition_offset":0,`, "", 1)))
	rep, err = c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "manifest line")

	replaceManifestLine(t, c.Dir, 2, []byte(lines[1]))
	replaceManifestLine(t, c.Dir, 1, []byte(strings.Replace(lines[0], `{"id"`, `{"future_field":1,"id"`, 1)))
	rep, err = c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "future_field")
}

func TestVerifyStrictManifestCleanOnCommittedCases(t *testing.T) {
	dirs := []string{recordstest.CopyV2Case(t), recordstest.NewV1Case(t)}
	var found []string
	for _, pat := range []string{"../*/testdata/*/manifest.jsonl", "../*/*/testdata/*/manifest.jsonl"} {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, m...)
	}
	if len(found) == 0 {
		t.Fatal("found no committed case directory")
	}
	for _, m := range found {
		dst := filepath.Join(t.TempDir(), "case")
		copyTree(t, filepath.Dir(m), dst)
		dirs = append(dirs, dst)
	}
	for _, d := range dirs {
		c, err := evidence.Open(d)
		if err != nil {
			t.Fatalf("open %s: %v", d, err)
		}
		rep, err := c.Verify()
		_ = c.Close()
		if err != nil {
			t.Fatal(err)
		}
		evidencetest.RequireNoProblem(t, rep, "manifest line")
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // committed testdata
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600) //nolint:gosec // test copy of committed testdata
	})
	if err != nil {
		t.Fatal(err)
	}
}
