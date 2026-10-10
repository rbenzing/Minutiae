package evidence_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// addExtract writes one extract artifact of parent (incomplete when incomplete)
// that names declared as its full run list.
func addExtract(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, rel string, incomplete bool, declared string) evidence.ManifestRecord {
	t.Helper()
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/" + rel,
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, DeclaredRunsArtifact: declared,
	}
	src := evidence.Source{Kind: "extract", DeviceID: parent.Source.DeviceID, Derived: d}
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
	sd := &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs"}
	rec, err := c.Capture(dev, "dec1", "p1-mtfs/"+rel+".runs.jsonl", evidence.Source{Kind: "runs", DeviceID: dev, Derived: sd},
		func(w io.Writer) error {
			_, err := w.Write([]byte("{\"offset\":0,\"length\":512}\n"))
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
