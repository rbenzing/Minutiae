package examine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
)

// TestVerifyStrictManifestCleanOnExamineProducers: the manifest lines of a real extract (inline runs
// and a runs sidecar) and an unallocated export of the committed ext4 fixture are exactly what the
// strict rule of case verify accepts (K25: no false positive on a real producer).
func TestVerifyStrictManifestCleanOnExamineProducers(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts a 16 MiB image")
	}
	img, _ := loadExt4Fixture(t, "ext4-4k-csum")
	c := newCase(t)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1}); err != nil {
		t.Fatalf("ExportUnallocated: %v", err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNoProblem(t, rep, "manifest line")
	for _, p := range rep.Problems {
		if strings.Contains(p, "manifest") {
			t.Errorf("unexpected manifest problem: %s", p)
		}
	}
}
