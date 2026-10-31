package ext4

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"
)

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

// totalAlloc runs fn and returns the bytes it allocated.
func totalAlloc(fn func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// A hostile map that names millions of blocks in a few segments and overlaps near its end is cut at the exact block
// without allocating per block: the cost is bounded by the segments and the map blocks, never by the blocks named.
func TestMapWalkerOverlapCostIsBoundedBySegmentsNotBlocks(t *testing.T) {
	const nb = int64(1) << 22
	const ceiling = 64 << 20
	build := func() *mapWalker {
		w := &mapWalker{f: &FS{mapRunsCap: 8}, nb: nb, next: nb, bs: 1, limit: nb, seen: map[uint64]struct{}{}}
		w.segs = []mapSeg{{0, 1000, nb / 2}, {nb / 2, 10_000_000, nb / 2}}
		return w
	}
	t.Run("tree block", func(t *testing.T) {
		w := build()
		w.tree = []uint64{10_000_000 + uint64(nb/2) - 3}
		var m staleMap
		got := totalAlloc(func() { m = w.finish() })
		t.Logf("allocated %d bytes", got)
		if got > ceiling {
			t.Fatalf("allocated %d bytes, ceiling %d", got, ceiling)
		}
		if want := nb - 3; m.covered != want || m.stop != fmt.Sprintf("map-overlap@%d", want) {
			t.Fatalf("covered %d stop %q, want %d and map-overlap@%d", m.covered, m.stop, want, want)
		}
	})
	t.Run("earlier extent", func(t *testing.T) {
		w := build()
		w.segs = append(w.segs[:1:1], mapSeg{nb / 2, 1000 + nb/2 - 5, nb / 2}) // its first 5 blocks lie over the end of the first
		var m staleMap
		got := totalAlloc(func() { m = w.finish() })
		t.Logf("allocated %d bytes", got)
		if got > ceiling {
			t.Fatalf("allocated %d bytes, ceiling %d", got, ceiling)
		}
		if want := nb / 2; m.covered != want || m.stop != fmt.Sprintf("map-overlap@%d", want) {
			t.Fatalf("covered %d stop %q, want %d and map-overlap@%d", m.covered, m.stop, want, want)
		}
	})
}

// cutAtOverlap agrees with the definition it replaces: the cut is at the first block, in logical order, that an earlier
// block or a map block also names.
func TestCutAtOverlapMatchesPerBlockDefinition(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a fixed-seed generator for a reproducible test, not security
	for range 3000 {
		var segs []mapSeg
		var lblk int64
		for range 1 + rng.IntN(6) {
			n := int64(1 + rng.IntN(6))
			// pushSeg merges a run that continues the last one on disk, so merged segments are covered too; half the
			// time the run is made to continue the last one
			phys := int64(rng.IntN(30))
			if len(segs) > 0 && rng.IntN(2) == 0 {
				last := segs[len(segs)-1]
				phys = last.phys + last.n
			}
			segs = pushSeg(segs, lblk, phys, n)
			lblk += n
		}
		orig := slices.Clone(segs)
		var tree []uint64
		for range rng.IntN(3) {
			tree = append(tree, uint64(rng.IntN(36)))
		}
		seen := map[int64]bool{}
		for _, b := range tree {
			seen[int64(b)] = true
		}
		want, found := int64(0), false
	scan:
		for _, s := range segs {
			for i := range s.n {
				if seen[s.phys+i] {
					want, found = s.lblk+i, true
					break scan
				}
			}
			for i := range s.n {
				seen[s.phys+i] = true
			}
		}
		// the definition marks a block as seen only after the whole segment is checked, which a self-overlapping
		// segment cannot trigger (blocks of one segment are distinct)
		cut, got, ok := cutAtOverlap(segs, tree, nil)
		if ok != found || (ok && got != want) {
			t.Fatalf("segs %v tree %v: cut %d found %v, want %d %v", segs, tree, got, ok, want, found)
		}
		if ok {
			var kept int64
			for _, s := range cut {
				kept += s.n
			}
			if kept != want {
				t.Fatalf("segs %v tree %v: kept %d blocks, want %d", segs, tree, kept, want)
			}
			// shape: every segment but the last is unchanged, and the last is a prefix of the original segment at the
			// same index, ending exactly at the cut block (the whole segment when the cut is on its boundary)
			if idx := len(cut) - 1; idx >= 0 {
				if !slices.Equal(cut[:idx], orig[:idx]) {
					t.Fatalf("segs %v tree %v: cut %v changed the leading segments", orig, tree, cut)
				}
				last, o := cut[idx], orig[idx]
				if last.lblk != o.lblk || last.phys != o.phys || last.n < 1 || last.n > o.n || last.lblk+last.n != want {
					t.Fatalf("segs %v tree %v: last segment of cut %v, want a prefix of %v ending at lblk %d", orig, tree, cut, o, want)
				}
			}
		}
	}
}
