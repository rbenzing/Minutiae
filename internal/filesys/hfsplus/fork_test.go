package hfsplus_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// vhFork reads a fork of the primary volume header as a ForkData.
func vhFork(img []byte, lay *hfsplustest.Layout, off int) hfsplus.ForkData {
	vh := img[lay.PrimaryVH:]
	var exts [][2]uint32
	for i := range 8 {
		start, count := be.Uint32(vh[off+fExtent0+8*i:]), be.Uint32(vh[off+fExtent0+8*i+4:])
		if count == 0 {
			break
		}
		exts = append(exts, [2]uint32{start, count})
	}
	return hfsplus.NewForkData(be.Uint64(vh[off+fLogical:]), be.Uint32(vh[off+fBlocks:]), exts...)
}

// forgedVolume builds an empty volume whose extents-overflow tree holds the
// given records.
func forgedVolume(t testing.TB, recs ...hfsplustest.OverflowRecord) (*hfsplus.FS, []byte, *hfsplustest.Layout) {
	t.Helper()
	img, lay := buildFiles(t, hfsplustest.Options{OverflowRecords: recs}, nil)
	return open(t, img), img, lay
}

func ext(s, c uint32) hfsplustest.Extent { return hfsplustest.Extent{Start: s, Count: c} }

// fork20 is a 20-block data fork of file 100 with 4 blocks inline.
var fork20 = hfsplus.NewForkData(20*4096, 20, [2]uint32{10, 4})

func TestExtentsOverflowResolves(t *testing.T) {
	f, _, _ := forgedVolume(t,
		hfsplustest.OverflowRecord{FileID: 99, StartBlock: 4, Extents: []hfsplustest.Extent{ext(200, 1)}},
		hfsplustest.OverflowRecord{FileID: 100, StartBlock: 8, Extents: []hfsplustest.Extent{ext(30, 6), ext(40, 6)}},
		hfsplustest.OverflowRecord{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}},
		hfsplustest.OverflowRecord{FileID: 100, Resource: true, StartBlock: 4, Extents: []hfsplustest.Extent{ext(100, 16)}},
		hfsplustest.OverflowRecord{FileID: 101, StartBlock: 4, Extents: []hfsplustest.Extent{ext(201, 1)}},
	)
	exts, complete, err := f.ForkExtents(100, false, fork20)
	want := [][2]uint32{{10, 4}, {20, 4}, {30, 6}, {40, 6}}
	if err != nil || !complete || !slices.Equal(exts, want) {
		t.Fatalf("data fork = %v complete=%v err=%v, want %v", exts, complete, err, want)
	}
	// The resource fork of the same file has its own records.
	exts, complete, err = f.ForkExtents(100, true, hfsplus.NewForkData(20*4096, 20, [2]uint32{50, 4}))
	if err != nil || !complete || !slices.Equal(exts, [][2]uint32{{50, 4}, {100, 16}}) {
		t.Errorf("resource fork = %v complete=%v err=%v", exts, complete, err)
	}
	// A fork that is complete inline never looks at the tree.
	exts, complete, err = f.ForkExtents(100, false, hfsplus.NewForkData(4*4096, 4, [2]uint32{10, 2}, [2]uint32{40, 2}))
	if err != nil || !complete || len(exts) != 2 {
		t.Errorf("inline fork = %v %v %v", exts, complete, err)
	}
	// An empty fork.
	if exts, complete, err := f.ForkExtents(100, false, hfsplus.NewForkData(0, 0)); err != nil || !complete || len(exts) != 0 {
		t.Errorf("empty fork = %v %v %v", exts, complete, err)
	}
	// Extents after the first empty inline one are ignored.
	exts, _, err = f.ForkExtents(100, false, hfsplus.NewForkData(4096, 1, [2]uint32{10, 1}, [2]uint32{0, 0}, [2]uint32{77, 3}))
	if err != nil || !slices.Equal(exts, [][2]uint32{{10, 1}}) {
		t.Errorf("fork with a stray extent after the end marker = %v, %v", exts, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}

func TestExtentsOverflowMissingRecordIsTrustedPrefix(t *testing.T) {
	for name, recs := range map[string][]hfsplustest.OverflowRecord{
		"no records": nil,
		"first only": {{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}}},
		"gap":        {{FileID: 100, StartBlock: 6, Extents: []hfsplustest.Extent{ext(20, 4)}}},
		"other file": {{FileID: 7, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := forgedVolume(t, recs...)
			exts, complete, err := f.ForkExtents(100, false, fork20)
			if err != nil || complete {
				t.Fatalf("err = %v complete = %v, want a trusted prefix", err, complete)
			}
			want := [][2]uint32{{10, 4}}
			if name == "first only" {
				want = append(want, [2]uint32{20, 4})
			}
			if !slices.Equal(exts, want) {
				t.Errorf("prefix = %v, want %v", exts, want)
			}
			if !hasWarning(f.Info(), "no extents-overflow record") {
				t.Errorf("Warnings = %q", f.Info().Warnings)
			}
		})
	}
}

func TestExtentsOverflowNoProgressIsCorrupt(t *testing.T) {
	cases := map[string][]hfsplustest.OverflowRecord{
		"a record of zero blocks":   {{FileID: 100, StartBlock: 4}},
		"a record of empty extents": {{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 0)}}},
		"a repeated start block": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}},
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(30, 4)}},
		},
		"a record that overshoots totalBlocks": {{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 40)}}},
		"an overshoot in the second extent": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 10), ext(40, 10)}},
		},
		"a record that ends exactly one block over": {{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 17)}}},
		"a second record that overshoots": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 8)}},
			{FileID: 100, StartBlock: 12, Extents: []hfsplustest.Extent{ext(40, 9)}},
		},
	}
	for name, recs := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, _ := forgedVolume(t, recs...)
			_, _, err := f.ForkExtents(100, false, fork20)
			wantCorrupt(t, err)
		})
	}
	t.Run("an extents tree whose leaf chain loops", func(t *testing.T) {
		// The record at block 4 covers up to block 8; the lookup for block 8
		// runs off the end of the only leaf, whose forward link points at itself.
		_, img, lay := forgedVolume(t, hfsplustest.OverflowRecord{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}})
		if len(lay.ExtentsLevels) == 0 {
			t.Fatal("no extents leaf")
		}
		leaf := lay.ExtentsLevels[0][0]
		be.PutUint32(img[lay.ExtentsOffset(int64(leaf)*int64(lay.ExtentsNodeSize)):], leaf)
		f := open(t, img)
		_, _, err := f.ForkExtents(100, false, fork20)
		wantCorrupt(t, err)
		if !strings.Contains(err.Error(), "loops") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an extents tree whose leaf chain loops back", func(t *testing.T) {
		// Enough records for several leaves; the last leaf links to the first.
		var recs []hfsplustest.OverflowRecord
		for id := uint32(1000); id < 1100; id++ {
			recs = append(recs, hfsplustest.OverflowRecord{FileID: id, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}})
		}
		_, img, lay := forgedVolume(t, recs...)
		leaves := lay.ExtentsLevels[0]
		if len(leaves) < 3 {
			t.Fatalf("%d extents leaves", len(leaves))
		}
		be.PutUint32(img[lay.ExtentsOffset(int64(leaves[len(leaves)-1])*int64(lay.ExtentsNodeSize)):], leaves[0])
		f := open(t, img)
		// File 2000 sorts after every record: the lookup walks off the last leaf.
		_, _, err := f.ForkExtents(2000, false, hfsplus.NewForkData(8*4096, 8, [2]uint32{10, 4}))
		wantCorrupt(t, err)
	})
	t.Run("a corrupt extents tree header", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{}, nil)
		be.PutUint16(img[lay.ExtentsOffset(14+18):], 3) // node size 3
		f := open(t, img)
		_, _, err := f.ForkExtents(100, false, fork20)
		wantCorrupt(t, err)
	})
}

func TestExtentsFileOverflowIsCorrupt(t *testing.T) {
	// A record for CNID 3 exists, but the extents file never consults the tree.
	f, _, _ := forgedVolume(t, hfsplustest.OverflowRecord{FileID: 3, StartBlock: 1, Extents: []hfsplustest.Extent{ext(20, 4)}})
	_, _, err := f.ForkExtents(3, false, hfsplus.NewForkData(5*4096, 5, [2]uint32{3, 1}))
	wantCorrupt(t, err)
	if !strings.Contains(err.Error(), "extents-overflow file") {
		t.Errorf("err = %v", err)
	}
	// Exactly its inline blocks: fine.
	if exts, complete, err := f.ForkExtents(3, false, hfsplus.NewForkData(5*4096, 5, [2]uint32{3, 2}, [2]uint32{9, 3})); err != nil || !complete || len(exts) != 2 {
		t.Errorf("complete extents file = %v %v %v", exts, complete, err)
	}
	// ... and through the volume header: the hostile-geometry table covers Open.
	img, lay := buildFiles(t, hfsplustest.Options{}, nil)
	patchHeaders(img, lay, func(v []byte) { be.PutUint32(v[vExtents+fBlocks:], 2) })
	_, err = hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	wantCorrupt(t, err)
}

func TestExtentOutsideVolumeIsCorrupt(t *testing.T) {
	f, _, _ := forgedVolume(t,
		hfsplustest.OverflowRecord{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(250, 10)}},
		hfsplustest.OverflowRecord{FileID: 101, StartBlock: 4, Extents: []hfsplustest.Extent{ext(0xFFFFFFFF, 2)}},
		hfsplustest.OverflowRecord{FileID: 102, StartBlock: 4, Extents: []hfsplustest.Extent{ext(256, 1)}},
		hfsplustest.OverflowRecord{FileID: 103, StartBlock: 4, Extents: []hfsplustest.Extent{ext(0, 0xFFFFFFFF)}},
	)
	total := hfsplus.NewForkData(1<<40, 1<<30, [2]uint32{10, 4})
	for _, id := range []uint32{100, 101, 102, 103} {
		_, _, err := f.ForkExtents(id, false, total)
		wantCorrupt(t, err)
	}
	for name, fd := range map[string]hfsplus.ForkData{
		"inline extent past the end":      hfsplus.NewForkData(4096*10, 10, [2]uint32{250, 10}),
		"inline extent starting past it":  hfsplus.NewForkData(4096, 1, [2]uint32{256, 1}),
		"inline start + count overflows":  hfsplus.NewForkData(4096, 3, [2]uint32{0xFFFFFFFF, 3}),
		"second inline extent outside":    hfsplus.NewForkData(4096*9, 9, [2]uint32{0, 4}, [2]uint32{255, 5}),
		"inline extents beyond totalBlks": hfsplus.NewForkData(4096*3, 3, [2]uint32{10, 4}),
		"zero total but inline extents":   hfsplus.NewForkData(0, 0, [2]uint32{10, 1}),
	} {
		_, _, err := f.ForkExtents(100, false, fd)
		if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v, want a corrupt error", name, err)
		}
	}
}

func TestCatalogForkWithOverflowExtents(t *testing.T) {
	o := hfsplustest.Options{BlockSize: 512, Blocks: 2048, NodeSize: 512, ExtentsNodeSize: 512, CatalogFragment: 2}
	files := manyFiles(30)
	img, lay := buildFiles(t, o, files)
	if len(lay.CatalogExtents) <= 8 || lay.ExtentsDepth < 1 {
		t.Fatalf("catalog has %d extents, extents tree depth %d: the catalog must overflow", len(lay.CatalogExtents), lay.ExtentsDepth)
	}
	f := open(t, img)
	fd := vhFork(img, lay, vCatalog)
	exts, complete, err := f.ForkExtents(4, false, fd)
	if err != nil || !complete {
		t.Fatalf("ForkExtents = %d extents, complete=%v, %v", len(exts), complete, err)
	}
	if len(exts) != len(lay.CatalogExtents) {
		t.Fatalf("%d extents, want %d", len(exts), len(lay.CatalogExtents))
	}
	for i, e := range lay.CatalogExtents {
		if exts[i] != [2]uint32{e.Start, e.Count} {
			t.Errorf("extent %d = %v, want %v", i, exts[i], e)
		}
	}
	// The whole catalog is usable: label, lookups and a scan across all extents.
	if info := f.Info(); info.Label != "" || len(info.Warnings) != 0 {
		t.Errorf("Info = %+v", info)
	}
	_, tr := catalogTree(t, img)
	if n, err := countRecords(tr); err != nil || n != 2+2*len(files) {
		t.Errorf("scan = %d, %v", n, err)
	}
	for _, fl := range files {
		if _, _, err := f.FindRecord(2, strings.TrimPrefix(fl.Path, "/")); err != nil {
			t.Errorf("FindRecord(%s): %v", fl.Path, err)
		}
	}

	t.Run("overflow record erased", func(t *testing.T) {
		img2 := bytes.Clone(img)
		// Empty the extents tree's only leaf: the catalog's extents past the
		// eighth are lost, so the catalog is only a prefix.
		for _, l := range lay.ExtentsLevels[0] {
			be.PutUint16(img2[lay.ExtentsOffset(int64(l)*int64(lay.ExtentsNodeSize)+10):], 0)
		}
		f2 := open(t, img2)
		tr2, err := f2.Tree(hfsplus.TreeCatalog)
		if err == nil {
			_, err = countRecords(tr2)
		}
		if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("err = %v, want an error wrapping ErrCorrupt (nodes beyond the prefix are unreadable)", err)
		}
		if !hasWarning(f2.Info(), "no extents-overflow record") {
			t.Errorf("Warnings = %q", f2.Info().Warnings)
		}
	})
}

func TestForkReadAtAcrossExtents(t *testing.T) {
	o := hfsplustest.Options{BlockSize: 1024, Blocks: 1024, NodeSize: 4096, CatalogFragment: 3}
	img, lay := buildFiles(t, o, manyFiles(40))
	f := open(t, img)
	fd := vhFork(img, lay, vCatalog)
	fm, err := f.MapFork(4, false, fd)
	if err != nil {
		t.Fatal(err)
	}
	// Reference content: the image bytes of the extents, concatenated.
	var want []byte
	for _, e := range lay.CatalogExtents {
		want = append(want, img[int64(e.Start)*1024:int64(e.Start+e.Count)*1024]...)
	}
	if len(lay.CatalogExtents) < 4 {
		t.Fatalf("%d extents", len(lay.CatalogExtents))
	}
	got := make([]byte, len(want))
	if n, err := fm.ReadAt(got, 0); n != len(want) || err != nil || !bytes.Equal(got, want) {
		t.Fatalf("whole read = %d, %v", n, err)
	}
	// Reads that start before, inside and exactly on extent boundaries and span 1, 2 and 3 extents.
	for _, off := range []int{0, 1, 1023, 1024, 3071, 3072, 3073, 5000, 6143, 6144, 7777} {
		for _, n := range []int{1, 2, 1000, 1024, 1025, 3072, 3073, 7000} {
			if off+n > len(want) {
				continue
			}
			buf := make([]byte, n)
			if m, err := fm.ReadAt(buf, int64(off)); m != n || err != nil || !bytes.Equal(buf, want[off:off+n]) {
				t.Errorf("ReadAt(%d bytes at %d) = %d, %v", n, off, m, err)
			}
		}
	}
	// Past the mapped blocks: an error wrapping ErrCorrupt, never zeros.
	buf := make([]byte, 100)
	n, err := fm.ReadAt(buf, int64(len(want)-10))
	if n != 10 || !errors.Is(err, filesys.ErrCorrupt) || !bytes.Equal(buf[:10], want[len(want)-10:]) {
		t.Errorf("read across the end = %d, %v", n, err)
	}
	if n, err := fm.ReadAt(buf, int64(len(want))); n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read at the end = %d, %v", n, err)
	}
	if n, err := fm.ReadAt(buf, 1<<62); n != 0 || err == nil {
		t.Errorf("read far past the end = %d, %v", n, err)
	}
	if n, err := fm.ReadAt(buf, -1); n != 0 || err == nil {
		t.Errorf("negative offset = %d, %v", n, err)
	}
	if n, err := fm.ReadAt(nil, 5); n != 0 || err != nil {
		t.Errorf("empty read = %d, %v", n, err)
	}
}

func TestForkRunsMergeByHand(t *testing.T) {
	const bs = 4096
	img, _ := buildFiles(t, hfsplustest.Options{}, nil)
	f := open(t, img)
	fd := hfsplus.NewForkData(9*bs, 9, [2]uint32{10, 2}, [2]uint32{12, 3}, [2]uint32{20, 1}, [2]uint32{21, 1}, [2]uint32{40, 2})
	fm, err := f.MapFork(100, false, fd)
	if err != nil {
		t.Fatal(err)
	}
	full := []filesys.Run{{Offset: 10 * bs, Length: 5 * bs}, {Offset: 20 * bs, Length: 2 * bs}, {Offset: 40 * bs, Length: 2 * bs}}
	if got := fm.Runs(9 * bs); !slices.Equal(got, full) {
		t.Errorf("Runs(all) = %v, want %v", got, full)
	}
	// Trimmed inside the last extent, and inside a merged run.
	if got := fm.Runs(7*bs + 100); !slices.Equal(got, []filesys.Run{full[0], full[1], {Offset: 40 * bs, Length: 100}}) {
		t.Errorf("Runs(7 blocks + 100) = %v", got)
	}
	if got := fm.Runs(4*bs - 1); !slices.Equal(got, []filesys.Run{{Offset: 10 * bs, Length: 4*bs - 1}}) {
		t.Errorf("Runs(trimmed in the first run) = %v", got)
	}
	// More than the fork maps: just what it maps.
	if got := fm.Runs(1 << 40); !slices.Equal(got, full) {
		t.Errorf("Runs(huge) = %v", got)
	}
	if got := fm.Runs(0); len(got) != 0 {
		t.Errorf("Runs(0) = %v", got)
	}
	// File order, not volume order: a descending pair is not merged or sorted.
	desc, _ := f.MapFork(100, false, hfsplus.NewForkData(5*bs, 5, [2]uint32{12, 3}, [2]uint32{10, 2}))
	if got := desc.Runs(5 * bs); !slices.Equal(got, []filesys.Run{{Offset: 12 * bs, Length: 3 * bs}, {Offset: 10 * bs, Length: 2 * bs}}) {
		t.Errorf("descending extents = %v", got)
	}
	// A wrapped volume: runs are relative to the start of the ReaderAt.
	wimg, lay := buildFiles(t, hfsplustest.Options{Wrapper: true}, nil)
	wf := open(t, wimg)
	wm, err := wf.MapFork(100, false, hfsplus.NewForkData(2*bs, 2, [2]uint32{10, 2}))
	if err != nil {
		t.Fatal(err)
	}
	if got := wm.Runs(2 * bs); !slices.Equal(got, []filesys.Run{{Offset: lay.Base + 10*bs, Length: 2 * bs}}) {
		t.Errorf("wrapped runs = %v, base %d", got, lay.Base)
	}
}
