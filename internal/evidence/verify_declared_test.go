package evidence_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// addExtract writes one extract artifact of parent (incomplete when incomplete)
// that names declared as its full run list.
func addExtract(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, rel string, incomplete bool, declared string) evidence.ManifestRecord {
	t.Helper()
	return addExtractWith(t, c, parent, rel, incomplete, "extract", declared, nil)
}

// addExtractWith is addExtract with the artifact kind and a hook that edits the derivation.
func addExtractWith(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, rel string, incomplete bool, kind, declared string, mod func(*evidence.Derivation)) evidence.ManifestRecord {
	t.Helper()
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/" + rel,
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, DeclaredRunsArtifact: declared,
	}
	if mod != nil {
		mod(d)
	}
	src := evidence.Source{Kind: kind, DeviceID: parent.Source.DeviceID, Derived: d}
	rec, _ := c.Capture(parent.Source.DeviceID, "dec1", "p1-mtfs/"+rel, src, func(w io.Writer) error {
		if _, err := w.Write(bytes.Repeat([]byte{7}, 512)); err != nil {
			return err
		}
		if incomplete {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	return rec
}

// addSidecar writes a runs sidecar derived from parent, under device dev (the
// directory of the extract artifacts), so a test can derive it from ANOTHER
// parent while keeping it in the same directory as the extract.
func addSidecar(t *testing.T, c *evidence.Case, dev string, parent evidence.ManifestRecord, rel string) evidence.ManifestRecord {
	t.Helper()
	return addSidecarRuns(t, c, dev, parent, rel, "{\"offset\":0,\"length\":512}\n")
}

// addSidecarRuns is addSidecar with the sidecar content given.
func addSidecarRuns(t *testing.T, c *evidence.Case, dev string, parent evidence.ManifestRecord, rel, content string) evidence.ManifestRecord {
	t.Helper()
	sd := &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs"}
	rec, err := c.Capture(dev, "dec1", "p1-mtfs/"+rel+".runs.jsonl", evidence.Source{Kind: "runs", DeviceID: dev, Derived: sd},
		func(w io.Writer) error {
			_, err := w.Write([]byte(content))
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVerifyExtractDeclaredRuns(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 4096)
	t.Run("a partial extract naming its sidecar is clean", func(t *testing.T) {
		c := evidencetest.NewCase(t)
		parent := evidencetest.AddImage(t, c, data)
		sc := addSidecar(t, c, parent.Source.DeviceID, parent, "a.bin")
		addExtract(t, c, parent, "a.bin", true, sc.ID)
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		evidencetest.RequireNoProblem(t, rep, "declared runs")
	})
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord)
		want  string
	}{
		{"id not in the manifest", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			addExtract(t, c, p, "a.bin", true, "no-such-id")
		}, "is not in the manifest"},
		{"a complete artifact declares a list", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			sc := addSidecar(t, c, p.Source.DeviceID, p, "a.bin")
			addExtract(t, c, p, "a.bin", false, sc.ID)
		}, "only an incomplete"},
		{"the declared artifact is not a runs sidecar", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			addExtract(t, c, p, "a.bin", true, p.ID)
		}, "not a runs sidecar"},
		{"the sidecar belongs to another parent", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			// AddImage uses fixed ids and cannot be called twice: import the second
			// parent through Capture under its own device and acquisition ids.
			otherData := bytes.Repeat([]byte{9}, 4096)
			other, err := c.Capture("dev2", "imp2", "other.img",
				evidence.Source{Kind: "import", DeviceID: "dev2", OriginalPath: "other.img", Segment: 1, Segments: 1},
				func(w io.Writer) error { _, err := w.Write(otherData); return err })
			if err != nil {
				t.Fatal(err)
			}
			// the sidecar lives in the extract's directory (device of p) but derives from `other`
			sc := addSidecar(t, c, p.Source.DeviceID, other, "a.bin")
			addExtract(t, c, p, "a.bin", true, sc.ID)
		}, "not a runs sidecar of the artifact's parent"},
		{"the declared list is the artifact's own captured sidecar", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			sc := addSidecar(t, c, p.Source.DeviceID, p, "a.bin")
			addExtractWith(t, c, p, "a.bin", true, "extract", sc.ID, func(d *evidence.Derivation) {
				d.Runs, d.RunsArtifact = nil, sc.ID
			})
		}, "also the captured runs sidecar"},
		{"the sidecar lies in another directory", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			// same parent, kind runs, but under another device directory
			sc := addSidecar(t, c, "dev2", p, "a.bin")
			addExtract(t, c, p, "a.bin", true, sc.ID)
		}, "not a runs sidecar in the same directory"},
		{"the declared artifact is a same-directory non-runs artifact", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			other := addExtract(t, c, p, "b.bin", true, "")
			addExtract(t, c, p, "a.bin", true, other.ID)
		}, "not a runs sidecar in the same directory"},
		{"a kind other than extract carries the field", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			sc := addSidecar(t, c, p.Source.DeviceID, p, "a.bin")
			addExtractWith(t, c, p, "a.bin", true, "unallocated", sc.ID, nil)
		}, "only an extract takes one"},
		{"two artifacts name one sidecar", func(t *testing.T, c *evidence.Case, p evidence.ManifestRecord) {
			sc := addSidecar(t, c, p.Source.DeviceID, p, "a.bin")
			addExtract(t, c, p, "a.bin", true, sc.ID)
			addExtract(t, c, p, "b.bin", true, sc.ID)
		}, "referenced by more than one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := evidencetest.NewCase(t)
			parent := evidencetest.AddImage(t, c, data)
			tc.setup(t, c, parent)
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

const fullRuns = "{\"offset\":0,\"length\":512}\n{\"offset\":512,\"length\":512}\n"

// X23: case verify recomputes that the captured runs are an in-order prefix of the declared list.
func TestVerifyExtractCapturedRunsArePrefixOfDeclared(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 4096)
	r := func(off, n int64) evidence.Run { return evidence.Run{Offset: off, Length: n} }
	for _, tc := range []struct {
		name     string
		captured []evidence.Run
		ok       bool
	}{
		{"the whole list", []evidence.Run{r(0, 512), r(512, 512)}, true},
		{"a leading run only", []evidence.Run{r(0, 512)}, true},
		{"the last run cut shorter", []evidence.Run{r(0, 512), r(512, 100)}, true},
		{"a first run cut shorter and last", []evidence.Run{r(0, 100)}, true},
		{"a run at another offset", []evidence.Run{r(0, 512), r(1024, 512)}, false},
		{"the last run longer than declared", []evidence.Run{r(0, 512), r(512, 600)}, false},
		{"a middle run cut shorter", []evidence.Run{r(0, 100), r(512, 512)}, false},
		{"more runs than declared", []evidence.Run{r(0, 512), r(512, 512), r(1024, 512)}, false},
		{"out of order", []evidence.Run{r(512, 512), r(0, 512)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := evidencetest.NewCase(t)
			parent := evidencetest.AddImage(t, c, data)
			sc := addSidecarRuns(t, c, parent.Source.DeviceID, parent, "a.bin", fullRuns)
			addExtractWith(t, c, parent, "a.bin", true, "extract", sc.ID, func(d *evidence.Derivation) { d.Runs = tc.captured })
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			if tc.ok {
				evidencetest.RequireNoProblem(t, rep, "not a prefix of")
			} else {
				evidencetest.RequireProblem(t, rep, "not a prefix of")
			}
		})
	}
}

// Editing one run of the declared sidecar after the fact (same size) is seen.
func TestVerifyDeclaredSidecarTamperBreaksPrefix(t *testing.T) {
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, bytes.Repeat([]byte{7}, 4096))
	sc := addSidecarRuns(t, c, parent.Source.DeviceID, parent, "a.bin", fullRuns)
	addExtractWith(t, c, parent, "a.bin", true, "extract", sc.ID, func(d *evidence.Derivation) {
		d.Runs = []evidence.Run{{Offset: 0, Length: 512}, {Offset: 512, Length: 100}}
	})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "not a prefix of")
	p := filepath.Join(c.Dir, filepath.FromSlash(sc.Path))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), `"offset":512`, `"offset":513`, 1))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "not a prefix of")
}
