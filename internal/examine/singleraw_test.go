package examine_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/image"
)

// evidence cannot import image, so verify's R4 bound ("a run lies beyond the parent") applies to a
// parent only when evidence's own isSingleRaw says its file size is the media size. This test pins
// that decision to image container detection on the committed raw, split-raw and E01 fixtures.
func TestIsSingleRawAgreesWithImageContainerDetection(t *testing.T) {
	disk := ewfTestdata(t, "ewf-disk.img.gz")
	cases := []struct {
		name  string
		segs  [][]byte
		names []string
	}{
		{"raw", [][]byte{disk}, []string{"disk.img"}},
		{"split-raw", [][]byte{disk[:len(disk)/2], disk[len(disk)/2:]}, []string{"disk.001", "disk.002"}},
		{"e01-single-none", [][]byte{ewfTestdata(t, "ewf-single-none.E01.gz")}, []string{"a.E01"}},
		{"e01-single-best", [][]byte{ewfTestdata(t, "ewf-single-best.E01.gz")}, []string{"a.E01"}},
		{"e01-seed-small", [][]byte{ewfTestdata(t, "ewf-seed-small.E01.gz")}, []string{"a.E01"}},
		{"e01-multi-none", func() [][]byte {
			var s [][]byte
			for _, n := range []string{"ewf-multi-none.E01.gz", "ewf-multi-none.E02.gz", "ewf-multi-none.E03.gz", "ewf-multi-none.E04.gz", "ewf-multi-none.E05.gz"} {
				s = append(s, ewfTestdata(t, n))
			}
			return s
		}(), e01Names(5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			recs, paths := importFiles(t, c, tc.names, tc.segs)
			img, err := image.Open(paths)
			if err != nil {
				t.Fatal(err)
			}
			format := img.Format()
			_ = img.Close()
			wantSingleRaw := format == "raw" && len(paths) == 1
			parent := recs[0]
			// A run just past the parent file: beyond a single raw parent, inside the media of an E01 one.
			evidencetest.AddRecovered(t, c, parent, nil, evidencetest.RecoveredSpec{
				Runs: []evidence.Run{{Offset: parent.Size, Length: 512}}, Data: make([]byte, 512),
			})
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			got := false
			for _, p := range rep.Problems {
				if strings.Contains(p, "lies beyond the parent's") {
					got = true
				}
			}
			if got != wantSingleRaw {
				t.Errorf("format %q, %d file(s): image detection says single raw = %v, evidence's R4 bound applied = %v\n%s",
					format, len(paths), wantSingleRaw, got, strings.Join(rep.Problems, "\n"))
			}
		})
	}
}
