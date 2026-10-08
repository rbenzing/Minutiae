package recordstest

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// AddDerivedWith stores data as a derived artifact with exactly the given Source (through Capture, so it
// is hashed, audited and in the manifest and artifacts.db). AddDerived is built on it.
func AddDerivedWith(t testing.TB, c *evidence.Case, name string, src evidence.Source, data []byte) evidence.ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq-derived", name, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// AddRunsSidecar stores runs as a runs sidecar of parent (kind runs, same parent, one JSON Run per line).
func AddRunsSidecar(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, name string, runs []evidence.Run) evidence.ManifestRecord {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range runs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return AddDerivedWith(t, c, name, sidecarSource(parent), buf.Bytes())
}

func sidecarSource(parent evidence.ManifestRecord) evidence.Source {
	return evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs",
	}}
}

// AddUnallocatedExport stores data as an unallocated export of parent together with its run map, whose
// lines are written in the real {offset, length, image_offset} format. The export references the run map
// as its runs sidecar. It returns the export and the sidecar.
func AddUnallocatedExport(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, lines []evidence.UnallocRun, data []byte) (bin, sidecar evidence.ManifestRecord) {
	t.Helper()
	type line struct {
		Offset      int64 `json:"offset"`
		Length      int64 `json:"length"`
		ImageOffset int64 `json:"image_offset"`
	}
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(line(l))
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	sidecar = AddDerivedWith(t, c, "unallocated.runs.jsonl", sidecarSource(parent), buf.Bytes())
	src := evidence.Source{Kind: "unallocated", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", RunsArtifact: sidecar.ID,
	}}
	bin = AddDerivedWith(t, c, "unallocated.bin", src, data)
	return bin, sidecar
}
