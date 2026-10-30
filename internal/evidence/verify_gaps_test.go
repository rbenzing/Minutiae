package evidence_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func TestVerifyAcquisitionGapNoteShape(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 4096)
	for _, tc := range []struct {
		name string
		kind string
		note *evidence.AcquisitionGapNote
		want string // problem substring, "" = clean
	}{
		{"exact overlap", "extract", &evidence.AcquisitionGapNote{Bytes: 512, Ranges: 1, First: []evidence.Run{{Offset: 0, Length: 512}}}, ""},
		{"unknown overlap", "extract", &evidence.AcquisitionGapNote{Unknown: true}, ""},
		{"empty note", "extract", &evidence.AcquisitionGapNote{}, "acquisition gap note is empty"},
		{"negative bytes", "extract", &evidence.AcquisitionGapNote{Bytes: -1, Ranges: 1}, "negative"},
		{"bytes without ranges", "extract", &evidence.AcquisitionGapNote{Bytes: 512}, "bytes but no ranges"},
		{"ranges without bytes", "extract", &evidence.AcquisitionGapNote{Ranges: 1, Unknown: true}, "ranges but no bytes"},
		{"unknown with bytes and ranges", "extract", &evidence.AcquisitionGapNote{Bytes: 512, Ranges: 1, Unknown: true}, ""},
		{"too many first runs", "extract", &evidence.AcquisitionGapNote{Bytes: 9, Ranges: 9, First: make([]evidence.Run, 9)}, "more than 8"},
		{"bad first run", "extract", &evidence.AcquisitionGapNote{Bytes: 1, Ranges: 1, First: []evidence.Run{{Offset: -1, Length: 1}}}, "invalid first run"},
		{"more than the artifact holds", "extract", &evidence.AcquisitionGapNote{Bytes: 1 << 20, Ranges: 1}, "more gap bytes than the artifact"},
		{"wrong kind", "runs", &evidence.AcquisitionGapNote{Bytes: 512, Ranges: 1}, "only extract, unallocated and carve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := evidencetest.NewCase(t)
			parent := evidencetest.AddImage(t, c, data)
			d := &evidence.Derivation{
				ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a.bin",
				Runs: []evidence.Run{{Offset: 0, Length: 512}}, AcquisitionGaps: tc.note,
			}
			src := evidence.Source{Kind: tc.kind, DeviceID: parent.Source.DeviceID, Derived: d}
			if _, err := c.Capture(parent.Source.DeviceID, "gap1", "p1-mtfs/a.bin", src, func(w io.Writer) error {
				_, err := w.Write(data[:512])
				return err
			}); err != nil {
				t.Fatal(err)
			}
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "acquisition gap")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}
