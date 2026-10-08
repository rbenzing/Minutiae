package examine_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// The analysis.end entry of an extraction keeps exactly the keys it had before the extra-details seam.
func TestExtractAnalysisEndUnchanged(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, fstest.Node{Path: "/a.txt", Data: []byte("aaa")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	ends := auditByAction(t, c, "analysis.end")
	if len(ends) != 1 {
		t.Fatalf("%d analysis.end entries", len(ends))
	}
	var keys []string
	for k := range ends[0].Details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"analysis_id", "bytes", "files", "skipped"}; !slices.Equal(keys, want) {
		t.Errorf("analysis.end keys = %v, want %v", keys, want)
	}
	if ends[0].Details["analysis_id"] != sum.AnalysisID || detailNum(ends[0].Details["files"]) != 1 || detailNum(ends[0].Details["bytes"]) != 3 {
		t.Errorf("analysis.end = %v", ends[0].Details)
	}
}
