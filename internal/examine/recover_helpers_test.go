package examine_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
)

// recoverAll runs a whole-volume recovery of the first partition that has a recoverable filesystem and
// fails the test on error.
func recoverAll(t *testing.T, s *examine.Session) examine.RecoverSummary {
	t.Helper()
	sum, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return sum
}

// only returns the single element of xs and fails the test unless there is exactly one.
func only[T any](t *testing.T, xs []T) T {
	t.Helper()
	if len(xs) != 1 {
		t.Fatalf("%d elements, want exactly 1: %+v", len(xs), xs)
	}
	return xs[0]
}

// assertEqualsRuns is the "never invented bytes" assertion: the artifact holds exactly the concatenation of
// the image bytes named by its recorded runs, and the runs add up to its size. The runs must be inline in
// the record (a sidecar is not followed here).
func assertEqualsRuns(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord, image []byte) {
	t.Helper()
	d := rec.Source.Derived
	if d == nil || d.RunsArtifact != "" {
		t.Fatalf("artifact %s: runs are not inline (derived %+v)", rec.ID, d)
	}
	var want []byte
	var sum int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Length < 0 || r.Offset+r.Length > int64(len(image)) {
			t.Fatalf("artifact %s: run %+v lies outside the %d-byte image", rec.ID, r, len(image))
		}
		want = append(want, image[r.Offset:r.Offset+r.Length]...)
		sum += r.Length
	}
	if sum != rec.Size {
		t.Errorf("artifact %s: runs add up to %d bytes, Size is %d", rec.ID, sum, rec.Size)
	}
	if got := readArtifact(t, c, rec); !bytes.Equal(got, want) {
		t.Errorf("artifact %s: %d bytes that are not the concatenation of its runs (%d bytes)", rec.ID, len(got), len(want))
	}
}
