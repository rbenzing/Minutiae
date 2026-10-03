package exfat_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// TestExfatFreshInstanceOpensStoredIDs: entries (even stripped to their ID)
// stored from one FS instance work on another that has not read anything: the
// extents are found on the disk, not carried by the Entry.
func TestExfatFreshInstanceOpensStoredIDs(t *testing.T) {
	files := []exfattest.File{
		{Path: "/top.txt", Data: []byte("top")},
		{Path: "/nofat", Dir: true},
		{Path: "/nofat/inner", Dir: true},
		{Path: "/nofat/inner/deep.bin", Data: pattern(9000, 5), FatChain: true},
		{Path: "/chain", Dir: true, FatChain: true},
		{Path: "/chain/gone", Data: []byte("g"), Deleted: true},
		{Path: "/chain/empty", Dir: true},
	}
	for i := 0; i < 120; i++ { // three directory clusters
		files = append(files, exfattest.File{Path: fmt.Sprintf("/chain/f%03d", i), Data: []byte(strconv.Itoa(i))})
	}
	img := exfattest.Build(exfattest.Options{ClusterCount: 512}, files)
	a := openImg(t, img)
	stored := walkAll(t, a)
	if len(stored) < 120 {
		t.Fatalf("walk found %d entries", len(stored))
	}
	for path, e := range stored {
		for _, bare := range []bool{false, true} {
			b := openImg(t, img) // fresh: knows nothing but the root
			x := e
			if bare {
				x = filesys.Entry{ID: e.ID}
			}
			switch {
			case e.Deleted:
				if _, err := b.Open(x); !errors.Is(err, filesys.ErrDeleted) {
					t.Errorf("%s: Open(deleted) = %v", path, err)
				}
			case e.Type == filesys.TypeDir:
				got, err := b.ReadDir(x)
				want, _ := a.ReadDir(e)
				if err != nil || len(got) != len(want) {
					t.Errorf("%s (bare=%v): fresh ReadDir = %d entries, %v; want %d", path, bare, len(got), err, len(want))
				}
			default:
				fl, err := b.Open(x)
				if err != nil {
					t.Errorf("%s (bare=%v): fresh Open: %v", path, bare, err)
					continue
				}
				if want := readAll(t, a, e); !bytes.Equal(readAll(t, b, x), want) || fl.Size() != int64(len(want)) {
					t.Errorf("%s: content differs on a fresh instance", path)
				}
			}
		}
	}
	c := openImg(t, img)
	e, err := c.Lookup("/nofat/inner/deep.bin")
	if err != nil || !bytes.Equal(readAll(t, c, e), pattern(9000, 5)) {
		t.Errorf("Lookup+Open on a fresh instance: %v", err)
	}
	if w := a.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

// TestExfatCrossLinkedDirectory: two entries naming the same first cluster with
// different extents. The first one met on disk decides the extent; the
// conflict is a warning; Walk reports the second as a cycle.
func TestExfatCrossLinkedDirectory(t *testing.T) {
	files := []exfattest.File{{Path: "/a", Dir: true}, {Path: "/b", Dir: true}}
	for i := 0; i < 5; i++ {
		files = append(files, exfattest.File{Path: fmt.Sprintf("/a/x%d", i), Data: []byte("x")})
	}
	img, lay := exfattest.BuildLayout(exfattest.Options{}, files)
	b := lay.Find("/b", false)
	a := lay.Find("/a", false)
	bad := bytes.Clone(img)
	le32(bad, int(b.Offset)+32+20, a.FirstCluster) // b points at a's cluster ...
	le64(bad[b.Offset+32:], 24, 2*4096)            // ... with a different DataLength
	le64(bad[b.Offset+32:], 8, 2*4096)
	exfattest.FixSetChecksum(bad, b.Offset)
	f := openImg(t, bad)
	root, _ := f.ReadDir(f.Root())
	ea, eb := byName(t, root, "a"), byName(t, root, "b")
	if ea.ID != eb.ID {
		t.Fatalf("IDs differ: %q %q", ea.ID, eb.ID)
	}
	la, err1 := f.ReadDir(ea)
	lb, err2 := f.ReadDir(eb)
	if err1 != nil || err2 != nil || len(la) != 5 || len(lb) != 5 {
		t.Errorf("listings: %d %d %v %v", len(la), len(lb), err1, err2)
	}
	if !hasWarn(f, "cross-linked") {
		t.Errorf("no cross-link warning: %v", f.Info().Warnings)
	}
	g := openImg(t, bad)
	if lb2, err := g.ReadDir(eb); err != nil || len(lb2) != 5 {
		t.Errorf("fresh ReadDir(b) = %d, %v", len(lb2), err)
	}
	cycles := 0
	_ = filesys.Walk(f, f.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
		if errors.Is(err, filesys.ErrCorrupt) {
			cycles++
		}
		return nil
	})
	if cycles != 1 {
		t.Errorf("Walk reported %d cycles, want 1", cycles)
	}
}

func TestExfatDirectoryReadBudget(t *testing.T) {
	var files []exfattest.File
	for i := 0; i < 200; i++ {
		files = append(files, exfattest.File{Path: fmt.Sprintf("/f%03d", i), Data: []byte("x")})
	}
	img := exfattest.Build(exfattest.Options{ClusterCount: 400}, files)
	f := openImg(t, img)
	full, _ := f.ReadDir(f.Root())
	if len(full) != 200 || hasWarn(f, "budget") {
		t.Fatalf("%d entries, warnings %v", len(full), f.Info().Warnings)
	}
	f.SetDirBudget(5000) // one 4096-byte chunk and a bit
	part, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(part) == 0 || len(part) >= 200 || !hasWarn(f, "budget") {
		t.Errorf("%d entries, warnings %v", len(part), f.Info().Warnings)
	}
	// Spent before any entry is read: an error, never an empty listing.
	if got, err := f.ReadDir(f.Root()); err == nil || len(got) != 0 || !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "directory read budget exhausted") {
		t.Errorf("after exhaustion: %d entries, %v; want a CorruptError \"directory read budget exhausted\"", len(got), err)
	}
	// Opening a file reads only a small window of its directory: not counted.
	if _, err := f.Open(full[150]); err != nil {
		t.Errorf("Open after the budget is spent: %v", err)
	}
}

// TestExfatUnallocatedBitmapShapes: a bitmap that spans several clusters, is
// fragmented, and is read in chunks that straddle cluster boundaries.
func TestExfatUnallocatedBitmapShapes(t *testing.T) {
	files := []exfattest.File{
		{Path: "/a.bin", Data: pattern(70000, 1)},
		{Path: "/gone.bin", Data: pattern(30000, 2), Deleted: true},
		{Path: "/frag.bin", Data: pattern(20000, 3), Fragmented: true},
		{Path: "/d", Dir: true},
		{Path: "/d/x", Data: []byte("x")},
	}
	for _, o := range []exfattest.Options{
		{BytesPerSectorShift: 9, OneSectorClusters: true, ClusterCount: 8192}, // 1024-byte bitmap, 2 clusters of 512
		{BytesPerSectorShift: 9, OneSectorClusters: true, ClusterCount: 8000}, // not a multiple of 8 either... of 512 bits
		{BytesPerSectorShift: 9, OneSectorClusters: true, ClusterCount: 9999, FragmentBitmap: true},
		{BytesPerSectorShift: 9, OneSectorClusters: true, ClusterCount: 4097, FragmentBitmap: true},
	} {
		img, lay := exfattest.BuildLayout(o, files)
		if len(lay.BitmapClusters) < 2 {
			t.Fatalf("%+v: the bitmap has %d clusters", o, len(lay.BitmapClusters))
		}
		if o.FragmentBitmap && lay.BitmapClusters[1] == lay.BitmapClusters[0]+1 {
			t.Fatalf("the bitmap is not fragmented: %v", lay.BitmapClusters)
		}
		var want []filesys.Run
		for _, c := range lay.FreeClusters() {
			want = append(want, filesys.Run{Offset: lay.ClusterOffset(c), Length: int64(lay.ClusterSize)})
		}
		want = filesys.MergeRuns(want)
		for _, chunk := range []int{1, 7, 100, 511, 512, 513, 1000, 1 << 20} {
			f := openImg(t, img)
			f.SetBitmapChunk(chunk)
			got, err := f.Unallocated()
			if err != nil || !slices.Equal(got, want) {
				t.Errorf("%+v chunk %d: Unallocated differs (%d runs, want %d): %v", o, chunk, len(got), len(want), err)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("%+v chunk %d: warnings %v", o, chunk, w)
			}
		}
	}
}
