package examine_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func snapshotNodes() []fstest.Node {
	return []fstest.Node{
		{Path: "/live.txt", Data: []byte("live")},
		{Path: "/.snapshots/S/old.txt", Data: []byte("old")},
	}
}

func extractedPaths(sum examine.Summary) []string {
	var out []string
	for _, a := range sum.Artifacts {
		out = append(out, a.Source.RemotePath)
	}
	slices.Sort(out)
	return out
}

func TestExtractRecursiveIncludeSnapshots(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, snapshotsHook("1"), 0, snapshotNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true, IncludeSnapshots: true})
	if got, want := extractedPaths(sum), []string{"/.snapshots/S/old.txt", "/live.txt"}; !slices.Equal(got, want) {
		t.Errorf("extract -r with snapshots = %v, want %v", got, want)
	}
	if ws := examineWarnings(t, c); len(ws) != 0 {
		t.Errorf("an opted-in run skips nothing and must write no skip warning: %+v", ws)
	}
	var started bool
	for _, e := range auditByAction(t, c, "analysis.start") {
		if e.Details["include_snapshots"] == true {
			started = true
		}
	}
	if !started {
		t.Error("the opt-in must be recorded in the analysis.start details (include_snapshots)")
	}
}

func TestExtractRecursiveDefaultStillSkipsSnapshots(t *testing.T) {
	c := newCase(t)
	s, _ := sessionHook(t, c, snapshotsHook("1"), 0, snapshotNodes()...)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if got := extractedPaths(sum); !slices.Equal(got, []string{"/live.txt"}) {
		t.Errorf("default extract -r = %v, want only /live.txt", got)
	}
	if ws := examineWarnings(t, c); len(ws) != 1 {
		t.Errorf("%d skip warnings, want exactly 1", len(ws))
	}
	for _, e := range auditByAction(t, c, "analysis.start") {
		if e.Details["include_snapshots"] == true {
			t.Error("include_snapshots must be false (or absent) by default")
		}
	}
}
