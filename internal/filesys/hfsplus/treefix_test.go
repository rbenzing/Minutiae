package hfsplus_test

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

func TestBTreeLeafChainEndsEarly(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, bigDir(600))
	leaves := lay.CatalogLevels[0]
	if len(leaves) < 12 {
		t.Fatalf("only %d leaves", len(leaves))
	}
	putCat32(img, lay, leaves[10], 0, 0) // the leaf's fLink: the chain now ends before the last leaf
	f := open(t, img)
	big, err := f.Lookup("/big")
	if err != nil {
		t.Fatal(err)
	}
	es, err := f.ReadDir(big)
	if err != nil || len(es) == 0 || len(es) >= 600 {
		t.Fatalf("ReadDir = %d entries, %v: want a partial listing", len(es), err)
	}
	if !hasWarning(f.Info(), "leaf chain ends early at node") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
	// A miss that lies behind the cut is not "not found": the records after
	// the cut were never looked at.
	last := es[len(es)-1].Name
	_, err = f.Lookup("/big/" + last + "x")
	wantCorrupt(t, err)
	if errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("the miss is reported as not found: %v", err)
	}
	// A name before the cut is still found.
	if e, err := f.Lookup("/big/" + es[0].Name); err != nil || e.Name != es[0].Name {
		t.Errorf("Lookup before the cut = %+v, %v", e, err)
	}
}

func TestIndexKeyLayouts(t *testing.T) {
	for name, mode := range map[string]int{"variable": 0, "fixed, length field maxKeyLength": 1, "fixed, real length field": 2} {
		t.Run(name, func(t *testing.T) {
			_, lay, f := buildTree(t, hfsplustest.Options{NodeSize: 4096, Blocks: 512, IndexKeys: mode}, bigDir(600))
			if mode != 0 && lay.CatalogDepth < 3 {
				t.Fatalf("catalog depth %d: no index levels to exercise", lay.CatalogDepth)
			}
			big, err := f.Lookup("/big")
			if err != nil {
				t.Fatal(err)
			}
			if es := readDir(t, f, big); len(es) != 600 {
				t.Errorf("%d entries", len(es))
			}
			for i := 0; i < 600; i += 53 {
				if _, err := f.Lookup(fmt.Sprintf("/big/f%04d", i)); err != nil {
					t.Errorf("Lookup %d: %v", i, err)
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %v", w)
			}
		})
	}
}

func TestBTreeHeaderSecondChecks(t *testing.T) {
	cases := []struct {
		name        string
		patch       func(img []byte, lay *hfsplustest.Layout)
		unsupported bool
	}{
		{"big keys mask clear", func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+38, 4) }, true},
		{"free nodes above total", func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+26, lay.CatalogNodes+1) }, false},
		{"btree type 255", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, 0, 14+36)] = 255 }, false},
		{"btree type 1", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, 0, 14+36)] = 1 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := buildFiles(t, small, manyFiles(8))
			c.patch(img, lay)
			f := open(t, img)
			_, err := f.Tree(hfsplus.TreeCatalog)
			if c.unsupported {
				if !errors.Is(err, filesys.ErrUnsupported) {
					t.Errorf("err = %v, want ErrUnsupported", err)
				}
				return
			}
			wantCorrupt(t, err)
		})
	}
	t.Run("user B-tree type is tolerated", func(t *testing.T) {
		img, lay := buildFiles(t, small, manyFiles(8))
		img[catOff(lay, 0, 14+36)] = 128
		if _, err := open(t, img).Tree(hfsplus.TreeCatalog); err != nil {
			t.Error(err)
		}
	})
	t.Run("nodes beyond the blocks the fork maps", func(t *testing.T) {
		// The catalog is fragmented into more extents than fit the header; its
		// overflow records are made unfindable, so only the first 8 extents
		// are mapped while the logical size still says the whole tree.
		o := hfsplustest.Options{BlockSize: 512, Blocks: 2048, NodeSize: 512, ExtentsNodeSize: 512, CatalogFragment: 2}
		img, lay := buildFiles(t, o, manyFiles(30))
		leaf := lay.ExtentsLevels[0][0]
		off := lay.ExtentsOffset(int64(leaf)*int64(lay.ExtentsNodeSize) + 14 + 4) // the first record's fileID
		be.PutUint32(img[off:], 99)
		f := open(t, img)
		_, err := f.Tree(hfsplus.TreeCatalog)
		wantCorrupt(t, err)
		if !hasWarning(f.Info(), "no extents-overflow record") {
			t.Errorf("warnings = %v", f.Info().Warnings)
		}
	})
}

func TestForkExtentsErrorKeepsValidatedPrefix(t *testing.T) {
	cases := map[string][]hfsplustest.OverflowRecord{
		"overshoot": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}},
			{FileID: 100, StartBlock: 8, Extents: []hfsplustest.Extent{ext(30, 6), ext(40, 6), ext(50, 6)}},
		},
		"outside the volume": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}},
			{FileID: 100, StartBlock: 8, Extents: []hfsplustest.Extent{ext(30, 2), ext(250, 40)}},
		},
		"no progress": {
			{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4)}},
			{FileID: 100, StartBlock: 8},
		},
	}
	for name, recs := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, _ := forgedVolume(t, recs...)
			exts, complete, err := f.ForkExtents(100, false, fork20)
			wantCorrupt(t, err)
			if complete {
				t.Error("complete with an error")
			}
			// Exactly the extents that were validated before the bad record.
			if want := [][2]uint32{{10, 4}, {20, 4}}; !slices.Equal(exts, want) {
				t.Errorf("prefix = %v, want %v", exts, want)
			}
		})
	}
	t.Run("inline extent outside the volume", func(t *testing.T) {
		f, _, _ := forgedVolume(t)
		fd := hfsplus.NewForkData(8*4096, 8, [2]uint32{10, 2}, [2]uint32{4000, 2})
		exts, _, err := f.ForkExtents(100, false, fd)
		wantCorrupt(t, err)
		if want := [][2]uint32{{10, 2}}; !slices.Equal(exts, want) {
			t.Errorf("prefix = %v, want %v", exts, want)
		}
	})
}

func TestExtentCapKeepsPrefix(t *testing.T) {
	var inline [][2]uint32
	for i := range 8 {
		inline = append(inline, [2]uint32{uint32(10 + 2*i), 1})
	}
	fd := hfsplus.NewForkData(40*4096, 40, inline...)
	rec := func(start uint32) hfsplustest.OverflowRecord {
		var es []hfsplustest.Extent
		for i := range 8 {
			es = append(es, ext(100+start*4+uint32(2*i), 1))
		}
		return hfsplustest.OverflowRecord{FileID: 100, StartBlock: start, Extents: es}
	}
	f, _, _ := forgedVolume(t, rec(8), rec(16), rec(24), rec(32))
	exts, complete, err := f.ForkExtents(100, false, fd)
	if err != nil || !complete || len(exts) != 40 {
		t.Fatalf("with the default cap: %d extents, complete=%v, %v", len(exts), complete, err)
	}
	f, _, _ = forgedVolume(t, rec(8), rec(16), rec(24), rec(32))
	f.SetExtentCap(10)
	exts, complete, err = f.ForkExtents(100, false, fd)
	if err != nil || complete || len(exts) != 16 {
		t.Fatalf("with a cap of 10: %d extents, complete=%v, %v; want the 16 mapped before the cap bit", len(exts), complete, err)
	}
	if !hasWarning(f.Info(), "more than 10 extents") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

func TestTruncatedImageDuringTreeReadIsCorrupt(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512}, bigDir(600))
	cut := lay.CatalogOffset(int64(lay.CatalogNodes/2) * int64(lay.NodeSize)) // the image ends inside the catalog
	f := open(t, img[:cut])
	big, err := f.Lookup("/big")
	if err == nil {
		_, err = f.ReadDir(big)
	}
	wantCorrupt(t, err)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		t.Errorf("a short read is reported as a bare EOF: %v", err)
	}
}

func TestBTreeCorruptionOffsetIsVolumeRelative(t *testing.T) {
	img, lay := buildFiles(t, hfsplustest.Options{NodeSize: 1024, Blocks: 512, Wrapper: true, Label: "VOL"}, manyFiles(8))
	root := lay.CatalogRoot
	putCat16(img, lay, root, int(lay.NodeSize)-2, 7) // record 0's offset: odd
	f := open(t, img)
	_, err := f.Lookup("/f003")
	wantCorrupt(t, err)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) {
		t.Fatal(err)
	}
	if want := lay.CatalogOffset(int64(root)*int64(lay.NodeSize)) - lay.Base; ce.Offset != want {
		t.Errorf("Offset = %d, want the volume-relative %d (image offset %d, base %d)", ce.Offset, want, want+lay.Base, lay.Base)
	}
}

func TestExtentsOverflowKeyLengthMustBeTen(t *testing.T) {
	img, lay := buildFiles(t, hfsplustest.Options{OverflowRecords: []hfsplustest.OverflowRecord{
		{FileID: 100, StartBlock: 4, Extents: []hfsplustest.Extent{ext(20, 4), ext(30, 12)}},
	}}, nil)
	leaf := lay.ExtentsLevels[0][0]
	be.PutUint16(img[lay.ExtentsOffset(int64(leaf)*int64(lay.ExtentsNodeSize)+14):], 12) // the first record's keyLength
	f := open(t, img)
	_, _, err := f.ForkExtents(100, false, fork20)
	wantCorrupt(t, err)
}
