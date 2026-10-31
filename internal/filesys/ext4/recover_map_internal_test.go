package ext4

import "testing"

// Blocks that continue the previous segment on disk extend it: the segment list is bounded by the run cap, not by the
// number of blocks the map names (3C Task 6 review carry-over).
func TestMapWalkerMergesAdjacentSegments(t *testing.T) {
	w := &mapWalker{f: &FS{mapRunsCap: 8}, nb: 1000}
	for i := range int64(500) {
		if err := w.addSeg(100+i, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.addSeg(5000, 3); err != nil {
		t.Fatal(err)
	}
	want := []mapSeg{{0, 100, 500}, {500, 5000, 3}}
	if len(w.segs) != len(want) || w.segs[0] != want[0] || w.segs[1] != want[1] {
		t.Fatalf("%d segments, first %v, want %v", len(w.segs), w.segs[:min(2, len(w.segs))], want)
	}
	if w.next != 503 || w.nruns != 2 {
		t.Fatalf("next %d, nruns %d, want 503 and 2", w.next, w.nruns)
	}
}

// An overlap inside a merged segment still cuts at the exact block, as one segment per block would.
func TestMapWalkerOverlapInsideMergedSegmentCutsAtTheBlock(t *testing.T) {
	w := &mapWalker{f: &FS{mapRunsCap: 8}, nb: 1000, bs: 1024, limit: 1000 * 1024, seen: map[uint64]struct{}{}}
	for i := range int64(10) {
		if err := w.addSeg(100+i, 1); err != nil {
			t.Fatal(err)
		}
	}
	w.tree = []uint64{104} // a map block that is also named as data block 4
	m := w.finish()
	if m.covered != 4*1024 {
		t.Fatalf("covered %d, want %d (the cut is at logical block 4)", m.covered, 4*1024)
	}
	if m.stop != "map-overlap@4" {
		t.Fatalf("stop %q, want map-overlap@4", m.stop)
	}
}
