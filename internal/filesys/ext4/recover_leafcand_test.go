package ext4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// The kernel unlink as the probe measured it: the blocks and the inode are freed, the in-inode map is cleared.
// kernelKeepSize is the same with i_size kept.
var (
	kernelFlags    = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true, ZeroBlockPointers: true}
	kernelKeepSize = ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ClearExtentRoot: true}
)

const lbs = 1024

func onePieces(n int, seed byte) ([]ext4test.Piece, []byte) {
	data := pattern(n*lbs, seed)
	ps := make([]ext4test.Piece, n)
	for i := range ps {
		ps[i] = ext4test.Piece{Block: i, Data: data[i*lbs : (i+1)*lbs]}
	}
	return ps, data
}

// leafTwoLeaves is 90 one-block pieces under a depth-1 tree of two leaves (45 extents each).
func leafTwoLeaves() (ext4test.File, []byte) {
	ps, data := onePieces(90, 61)
	return ext4test.File{Path: "/gone.bin", Deleted: true, ExtentLeaves: 2, Scatter: true, Pieces: ps}, data
}

// leafImg builds the image, applies the unlink e, lets mutate edit it, opens it and (when prep) runs the free-space
// pass.
func leafImg(t *testing.T, o ext4test.Options, files []ext4test.File, e ext4test.UnlinkEffects, prep bool, mutate func(img []byte, l *ext4test.Layout)) (*ext4.FS, []byte, *ext4test.Layout) {
	t.Helper()
	f, img, l := recImageWith(t, o, files, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", e)
		if mutate != nil {
			mutate(img, l)
		}
	})
	if prep {
		prepare(t, f)
	}
	return f, img, l
}

func recoverOf(t *testing.T, f *ext4.FS) ([]filesys.Candidate, error) {
	t.Helper()
	return f.Recoverable(delEntry(t, f, "/gone.bin"))
}

func leafOnly(t *testing.T, f *ext4.FS) filesys.Candidate {
	t.Helper()
	cs, err := recoverOf(t, f)
	if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodExtentLeaf {
		t.Fatalf("Recoverable = %+v, %v; want exactly one %s candidate", cs, err, ext4.MethodExtentLeaf)
	}
	if _, err := filesys.CheckCandidate(cs[0], f.Info().Size); err != nil {
		t.Fatalf("the candidate fails CheckCandidate: %v", err)
	}
	return cs[0]
}

func hasAssumption(c filesys.Candidate, a string) bool { return slices.Contains(c.Assumptions, a) }

func hasBasis(c filesys.Candidate, b string) bool { return slices.Contains(c.Basis, b) }

func warned(c filesys.Candidate, sub string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// putLeaves writes leaves into the last free blocks of the deleted file, for the file's own inode number and
// generation. Each leaf is a list of extents whose Phys is an index into the file's remaining free data blocks.
func putLeaves(l *ext4test.Layout, img []byte, leaves ...[]ext4test.Extent) {
	loc := l.File("/gone.bin")
	data := loc.Data[:len(loc.Data)-len(leaves)]
	for i, exts := range leaves {
		ex := slices.Clone(exts)
		for k := range ex {
			ex[k].Phys = data[ex[k].Phys]
		}
		l.WriteLeaf(img, loc.Data[len(loc.Data)-1-i], loc.Inode, loc.Generation, ex, true)
	}
}

// bigDeleted is a deleted file with n contiguous data blocks.
func bigDeleted(n int) []ext4test.File {
	return []ext4test.File{{Path: "/gone.bin", Data: pattern(n*lbs, 70), Deleted: true}}
}

func TestRecoverableLeafAfterKernelUnlink(t *testing.T) {
	file, data := leafTwoLeaves()
	f, img, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelFlags, true, nil)
	c := leafOnly(t, f)
	if !bytes.Equal(concatRuns(img, c.Runs), data) {
		t.Fatal("the runs are not the file's data")
	}
	if c.Size != int64(len(data)) || hasStop(c) {
		t.Errorf("size %d (want %d), stop %q", c.Size, len(data), stopOf(c))
	}
	for _, a := range []string{"owner-by-crc32c-seed", "in-inode-map-cleared", "leaf-may-be-superseded", "size-from-extent-leaf-blocks"} {
		if !hasAssumption(c, a) {
			t.Errorf("assumptions %q lack %s", c.Assumptions, a)
		}
	}
	if !hasBasis(c, "index-leaves=2") {
		t.Errorf("basis %q lacks index-leaves=2", c.Basis)
	}
	// i_size is zero on a real kernel delete (GATE-S), so even a chain that ends cleanly may have lost its last leaf
	// or be an older, shorter copy: the end is unknown (3C-C3).
	if !hasAssumption(c, "file-end-unknown") {
		t.Errorf("a zero-size chain does not say its end is unknown: %q", c.Assumptions)
	}
}

func TestRecoverableLeafNeedsMetadataCsum(t *testing.T) {
	file, _ := leafTwoLeaves()
	f, _, _ := leafImg(t, ext4test.Options{Extents: true}, []ext4test.File{file}, kernelFlags, true, nil)
	cs, err := recoverOf(t, f)
	if err != nil || cs != nil {
		t.Fatalf("got %+v, %v; want nil, nil", cs, err)
	}
}

func TestRecoverableLeafChainStopsAtMissingLeaf(t *testing.T) {
	file, data := leafTwoLeaves()
	f, img, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelKeepSize, true, func(img []byte, l *ext4test.Layout) {
		clear(img[l.File("/gone.bin").Tree[1]*lbs:][:lbs]) // the second leaf is overwritten
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "leaf-gap@45" {
		t.Fatalf("stop = %q, want leaf-gap@45 (%+v)", got, c)
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data[:45*lbs]) {
		t.Error("the runs are not exactly the first leaf's blocks")
	}
	if c.Size != int64(len(data)) {
		t.Errorf("size %d, want i_size %d", c.Size, len(data))
	}
}

// conflictImage holds leaf 1 [0,45), leaf 2 [45,90) and a third [60,100) that overlaps leaf 2.
func conflictImage(t *testing.T) *ext4.FS {
	t.Helper()
	f, _, _ := leafImg(t, csumExt, bigDeleted(120), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img,
			[]ext4test.Extent{{Lblk: 0, Len: 45, Phys: 0}},
			[]ext4test.Extent{{Lblk: 45, Len: 45, Phys: 45}},
			[]ext4test.Extent{{Lblk: 60, Len: 40, Phys: 60}})
	})
	return f
}

func TestRecoverableLeafConflictStops(t *testing.T) {
	c := leafOnly(t, conflictImage(t))
	if got := stopOf(c); got != "leaf-conflict@45" {
		t.Fatalf("stop = %q, want leaf-conflict@45", got)
	}
	if coveredBytes(c) != 45*lbs {
		t.Errorf("covered %d, want the 45 blocks before the conflict", coveredBytes(c))
	}
	// the leaves beyond the stop make the file end unknown (3C-R23)
	if c.Size != 100*lbs || !hasAssumption(c, "file-end-unknown") {
		t.Errorf("size %d assumptions %q, want 100 blocks and file-end-unknown", c.Size, c.Assumptions)
	}
}

func TestRecoverableLeafBasisCountsIndexLeaves(t *testing.T) {
	c := leafOnly(t, conflictImage(t))
	if !hasBasis(c, "index-leaves=3") || !hasAssumption(c, "leaf-may-be-superseded") {
		t.Errorf("basis %q assumptions %q", c.Basis, c.Assumptions)
	}
}

func TestRecoverableLeafMissingHeadIsNoRuns(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(120), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img, []ext4test.Extent{{Lblk: 45, Len: 45, Phys: 45}})
	})
	c := leafOnly(t, f)
	if len(c.Runs) != 0 || c.Size != 90*lbs || stopOf(c) != "leaf-gap@0" {
		t.Errorf("runs %v size %d stop %q, want no runs, 90 blocks, leaf-gap@0", c.Runs, c.Size, stopOf(c))
	}
}

func TestRecoverableLeafOfOtherGenerationIsIgnored(t *testing.T) {
	file, data := leafTwoLeaves()
	files := []ext4test.File{file, {Path: "/junk.bin", Data: pattern(4*lbs, 9), Deleted: true}}
	base, _, _ := leafImg(t, csumExt, files, kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/junk.bin", ext4test.DebugfsRm)
	})
	// the same image plus a leaf of this inode with another generation and one of another inode
	f2, img2, _ := leafImg(t, csumExt, files, kernelFlags, false, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/junk.bin", ext4test.DebugfsRm)
		loc, junk := l.File("/gone.bin"), l.File("/junk.bin").Data
		l.WriteLeaf(img, junk[0], loc.Inode, loc.Generation+1, []ext4test.Extent{{Lblk: 0, Len: 200, Phys: junk[3]}}, true)
		l.WriteLeaf(img, junk[1], loc.Inode+1, loc.Generation, []ext4test.Extent{{Lblk: 0, Len: 200, Phys: junk[3]}}, true)
	})
	prepare(t, f2)
	want, got := leafOnly(t, base), leafOnly(t, f2)
	if !slices.Equal(want.Runs, got.Runs) || want.Size != got.Size || !hasBasis(got, "index-leaves=2") {
		t.Errorf("foreign leaves changed the answer: %+v vs %+v", got, want)
	}
	if !bytes.Equal(concatRuns(img2, got.Runs), data) {
		t.Error("runs do not reproduce the file")
	}
}

func TestRecoverableLeafUninitStops(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 0}, {Lblk: 3, Len: 3, Phys: 3, Uninit: true}})
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "uninit@3" {
		t.Fatalf("stop = %q, want uninit@3", got)
	}
	if coveredBytes(c) != 3*lbs || c.Size != 6*lbs || !hasAssumption(c, "file-end-unknown") {
		t.Errorf("covered %d size %d assumptions %q", coveredBytes(c), c.Size, c.Assumptions)
	}
}

func TestRecoverableLeafIndexFullIsUndecided(t *testing.T) {
	build := func(e ext4test.UnlinkEffects, total, perKey int) *ext4.FS {
		file, _ := leafTwoLeaves()
		f, _, _ := leafImg(t, csumExt, []ext4test.File{file}, e, false, nil)
		f.SetLeafCaps(total, perKey)
		prepare(t, f)
		return f
	}
	t.Run("no other candidate", func(t *testing.T) {
		cs, err := recoverOf(t, build(kernelFlags, 1, 64))
		if !errors.Is(err, filesys.ErrLimit) || cs != nil {
			t.Fatalf("got %+v, %v; want ErrLimit", cs, err)
		}
	})
	t.Run("with an intact candidate", func(t *testing.T) {
		cs, err := recoverOf(t, build(ext4test.DebugfsRm, 1, 64))
		if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodInodeIntact {
			t.Fatalf("got %+v, %v; want the intact candidate alone", cs, err)
		}
		if !warned(cs[0], "leaf index full: leaves of inode") {
			t.Errorf("warnings %q do not say leaves may be missing", cs[0].Warnings)
		}
	})
	t.Run("a capped key is unknown too", func(t *testing.T) {
		// two leaves of one key, one dropped: the survivor is not turned into a map
		cs, err := recoverOf(t, build(kernelFlags, 1000, 1))
		if !errors.Is(err, filesys.ErrLimit) || cs != nil {
			t.Fatalf("got %+v, %v; want ErrLimit", cs, err)
		}
	})
}

func TestRecoverableLeafAndIntactBothOffered(t *testing.T) {
	file, data := leafTwoLeaves()
	f, img, _ := leafImg(t, csumExt, []ext4test.File{file}, ext4test.DebugfsRm, true, nil)
	cs, err := recoverOf(t, f)
	if err != nil || len(cs) != 2 {
		t.Fatalf("got %d candidates, %v; want 2", len(cs), err)
	}
	if cs[0].Method != ext4.MethodInodeIntact || cs[1].Method != ext4.MethodExtentLeaf {
		t.Fatalf("methods %s, %s; want intact first", cs[0].Method, cs[1].Method)
	}
	for _, c := range cs {
		if !bytes.Equal(concatRuns(img, c.Runs), data) || c.Size != int64(len(data)) {
			t.Errorf("%s: wrong runs or size %d", c.Method, c.Size)
		}
	}
	if hasAssumption(cs[1], "in-inode-map-cleared") || hasAssumption(cs[1], "leaf-end-disagrees-with-inode-size") {
		t.Errorf("assumptions %q: the map is not cleared and i_size agrees", cs[1].Assumptions)
	}
	if !hasAssumption(cs[1], "size-from-inode") {
		t.Errorf("assumptions %q lack size-from-inode", cs[1].Assumptions)
	}
}

func TestRecoverableLeafGapSizeIsLastLeafEnd(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(220), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img,
			[]ext4test.Extent{{Lblk: 0, Len: 84, Phys: 0}},
			[]ext4test.Extent{{Lblk: 168, Len: 33, Phys: 168}}) // the leaf for 84-167 is gone
	})
	c := leafOnly(t, f)
	if c.Size != 201*lbs || coveredBytes(c) != 84*lbs {
		t.Errorf("size %d covered %d, want 201 and 84 blocks", c.Size, coveredBytes(c))
	}
	if !hasAssumption(c, "file-end-unknown") || stopOf(c) != "leaf-gap@84" {
		t.Errorf("assumptions %q", c.Assumptions)
	}
}

func TestRecoverableLeafEndDisagreesWithInodeSize(t *testing.T) {
	file, data := leafTwoLeaves()
	f, img, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelKeepSize, true, func(img []byte, l *ext4test.Layout) {
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) { binary.LittleEndian.PutUint32(raw[4:], 10*lbs) })
	})
	c := leafOnly(t, f)
	if c.Size != 10*lbs || !hasAssumption(c, "leaf-end-disagrees-with-inode-size") || !hasAssumption(c, "size-from-inode") {
		t.Fatalf("size %d assumptions %q", c.Size, c.Assumptions)
	}
	if len(c.Warnings) == 0 {
		t.Error("no warning for the disagreement")
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data[:10*lbs]) {
		t.Error("the runs are not the first 10 blocks, trimmed to i_size")
	}
}

// Superseded leaves under one key never make a map (3C-R31): an overlap stops the chain, and nothing is assembled
// from the older or the newer copy by guess.
func TestRecoverableSupersededLeavesAreNeverAssembled(t *testing.T) {
	cases := []struct {
		name   string
		leaves [][]ext4test.Extent
		stop   string
		runs   int64
		size   int64
	}{
		{"same start, longer copy", [][]ext4test.Extent{{{Lblk: 0, Len: 100, Phys: 0}}, {{Lblk: 0, Len: 200, Phys: 0}}}, "leaf-conflict@0", 0, 200},
		{"older copy spans a later leaf", [][]ext4test.Extent{{{Lblk: 0, Len: 100, Phys: 0}}, {{Lblk: 100, Len: 50, Phys: 100}}, {{Lblk: 0, Len: 200, Phys: 0}}}, "leaf-conflict@0", 0, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := leafImg(t, csumExt, bigDeleted(260), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
				putLeaves(l, img, tc.leaves...)
			})
			c := leafOnly(t, f)
			if stopOf(c) != tc.stop || coveredBytes(c) != tc.runs*lbs || c.Size != tc.size*lbs {
				t.Errorf("stop %q covered %d size %d; want %s, %d, %d blocks", stopOf(c), coveredBytes(c), c.Size, tc.stop, tc.runs, tc.size)
			}
		})
	}
	t.Run("an older prefix alone is a short map, not the file", func(t *testing.T) {
		f, _, _ := leafImg(t, csumExt, bigDeleted(260), kernelKeepSize, true, func(img []byte, l *ext4test.Layout) {
			putLeaves(l, img, []ext4test.Extent{{Lblk: 0, Len: 100, Phys: 0}})
			l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) { binary.LittleEndian.PutUint32(raw[4:], 200*lbs) })
		})
		c := leafOnly(t, f)
		if stopOf(c) != "leaf-gap@100" || coveredBytes(c) != 100*lbs || c.Size != 200*lbs {
			t.Errorf("stop %q covered %d size %d", stopOf(c), coveredBytes(c), c.Size)
		}
	})
}

func TestRecoverableUnpreparedLeafIsUndecided(t *testing.T) {
	file, _ := leafTwoLeaves()
	t.Run("kernel unlink", func(t *testing.T) {
		f, _, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelFlags, false, nil)
		cs, err := recoverOf(t, f)
		if !errors.Is(err, filesys.ErrUndecided) || cs != nil {
			t.Fatalf("got %+v, %v; want ErrUndecided", cs, err)
		}
		// the index is not built behind the caller's back (3C-R25)
		if leaves, _, read := f.LeafIndexStats(); leaves != -1 || read != 0 {
			t.Errorf("Recoverable built the index: %d leaves, %d bytes read", leaves, read)
		}
	})
	t.Run("debugfs rm keeps the intact candidate", func(t *testing.T) {
		f, _, _ := leafImg(t, csumExt, []ext4test.File{file}, ext4test.DebugfsRm, false, nil)
		cs, err := recoverOf(t, f)
		if err != nil || len(cs) != 1 || cs[0].Method != ext4.MethodInodeIntact {
			t.Fatalf("got %+v, %v", cs, err)
		}
		if !warned(cs[0], "stale extent leaves not searched (no free-space pass in this mode)") {
			t.Errorf("warnings %q", cs[0].Warnings)
		}
	})
	t.Run("a filesystem without metadata_csum is not undecided", func(t *testing.T) {
		f, _, _ := leafImg(t, ext4test.Options{Extents: true}, []ext4test.File{file}, kernelFlags, false, nil)
		if cs, err := recoverOf(t, f); err != nil || cs != nil {
			t.Fatalf("got %+v, %v; want nil, nil", cs, err)
		}
	})
	t.Run("a cancelled pass leaves it unknown", func(t *testing.T) {
		f, _, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelFlags, false, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.PrepareRecovery(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if _, err := recoverOf(t, f); !errors.Is(err, filesys.ErrUndecided) {
			t.Errorf("after a cancelled pass: %v, want ErrUndecided", err)
		}
	})
}

// Two extents of one leaf that share a physical block are not a map: the chain stops at the second.
func TestRecoverableLeafPhysicalOverlapStops(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 0}, {Lblk: 3, Len: 3, Phys: 2}})
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "map-overlap@3" || coveredBytes(c) != 3*lbs || c.Size != 6*lbs {
		t.Errorf("stop %q covered %d size %d, want map-overlap@3, 3 blocks, 6 blocks", got, coveredBytes(c), c.Size)
	}
}

// More runs than the cap stop the chain: the cap is a stop, never a silent cut.
func TestRecoverableLeafRunsCapStops(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, false, func(img []byte, l *ext4test.Layout) {
		var exts []ext4test.Extent
		for i := range 6 { // every other block, so no two extents are adjacent
			exts = append(exts, ext4test.Extent{Lblk: i, Len: 1, Phys: 2 * i})
		}
		putLeaves(l, img, exts)
	})
	f.SetMapStaleCaps(4)
	prepare(t, f)
	c := leafOnly(t, f)
	if got := stopOf(c); got != "runs-cap@4" || len(c.Runs) != 4 {
		t.Errorf("stop %q with %d runs, want runs-cap@4 and 4 runs", got, len(c.Runs))
	}
}

// With i_size zero the last leaf may be the one that was lost: the chain's end is not the file's end (3C-C3).
func TestRecoverableLeafZeroSizeLostLastLeafEndIsUnknown(t *testing.T) {
	file, data := leafTwoLeaves()
	f, img, _ := leafImg(t, csumExt, []ext4test.File{file}, kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		clear(img[l.File("/gone.bin").Tree[1]*lbs:][:lbs])
	})
	c := leafOnly(t, f)
	if !hasAssumption(c, "file-end-unknown") || !bytes.Equal(concatRuns(img, c.Runs), data[:45*lbs]) {
		t.Errorf("assumptions %q, covered %d", c.Assumptions, coveredBytes(c))
	}
}

// A data extent that points at a bound leaf block would make leaf metadata file content: the chain stops there.
func TestRecoverableLeafDataExtentOverLeafBlockStops(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		loc := l.File("/gone.bin")
		d, n := loc.Data, len(loc.Data)
		l.WriteLeaf(img, d[n-1], loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 0, Len: 2, Phys: d[0]}, {Lblk: 2, Len: 1, Phys: d[n-2]}}, true)
		l.WriteLeaf(img, d[n-2], loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 3, Len: 3, Phys: d[3]}}, true)
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "map-overlap@2" || coveredBytes(c) != 2*lbs {
		t.Errorf("stop %q covered %d, want map-overlap@2 and 2 blocks", got, coveredBytes(c))
	}
	if c.Size != 6*lbs || !hasAssumption(c, "file-end-unknown") {
		t.Errorf("size %d assumptions %q", c.Size, c.Assumptions)
	}
}

func TestRecoverableLeafPhysicalBlockZeroIsMapDamaged(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		loc := l.File("/gone.bin")
		d := loc.Data
		l.WriteLeaf(img, d[len(d)-1], loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 0, Len: 2, Phys: d[0]}, {Lblk: 2, Len: 1, Phys: 0}}, true)
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "map-damaged@2" || coveredBytes(c) != 2*lbs {
		t.Errorf("stop %q covered %d, want map-damaged@2 and 2 blocks", got, coveredBytes(c))
	}
}

// Past 16 leaves the basis states the count instead of naming each.
func TestRecoverableLeafBasisCapsNamedLeaves(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(60), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		var leaves [][]ext4test.Extent
		for k := range 17 {
			leaves = append(leaves, []ext4test.Extent{{Lblk: k, Len: 1, Phys: k}})
		}
		putLeaves(l, img, leaves...)
	})
	c := leafOnly(t, f)
	named := 0
	for _, b := range c.Basis {
		if strings.HasPrefix(b, "leaf=") {
			named++
		}
	}
	if named != 16 || !hasBasis(c, "leaves=17") || !hasBasis(c, "index-leaves=17") {
		t.Errorf("%d leaves named, basis %q", named, c.Basis)
	}
}

// A huge i_size must not overflow the block count.
func TestRecoverableLeafHugeInodeSize(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelKeepSize, true, func(img []byte, l *ext4test.Layout) {
		putLeaves(l, img, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 0}})
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[4:], 0xFFFFFFFF)
			binary.LittleEndian.PutUint32(raw[0x6C:], 0x7FFFFFFF)
		})
	})
	cs, err := recoverOf(t, f)
	if err != nil || len(cs) != 1 || stopOf(cs[0]) != "leaf-gap@3" {
		t.Fatalf("got %+v, %v; want a candidate with leaf-gap@3", cs, err)
	}
}

// When the overlap cut drops extents, the basis names only the leaves whose extents are still used.
func TestRecoverableLeafOverlapCutNamesOnlyUsedLeaves(t *testing.T) {
	var leafA, leafB int
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		loc := l.File("/gone.bin")
		d, n := loc.Data, len(loc.Data)
		leafA, leafB = d[n-1], d[n-2]
		l.WriteLeaf(img, leafA, loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 0, Len: 2, Phys: d[0]}, {Lblk: 2, Len: 1, Phys: leafB}}, true)
		l.WriteLeaf(img, leafB, loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 3, Len: 3, Phys: d[3]}}, true)
	})
	c := leafOnly(t, f)
	if stopOf(c) != "map-overlap@2" || !hasBasis(c, "leaf="+strconv.Itoa(leafA)) || hasBasis(c, "leaf="+strconv.Itoa(leafB)) {
		t.Errorf("stop %q basis %q, want map-overlap@2 naming leaf %d and not leaf %d", stopOf(c), c.Basis, leafA, leafB)
	}
}

// A bound leaf that the chain never reached (beyond a gap) is still not file content.
func TestRecoverableLeafDataExtentOverUnchainedLeafStops(t *testing.T) {
	f, _, _ := leafImg(t, csumExt, bigDeleted(40), kernelFlags, true, func(img []byte, l *ext4test.Layout) {
		loc := l.File("/gone.bin")
		d, n := loc.Data, len(loc.Data)
		l.WriteLeaf(img, d[n-1], loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 0, Len: 1, Phys: d[0]}, {Lblk: 1, Len: 1, Phys: d[n-2]}}, true)
		l.WriteLeaf(img, d[n-2], loc.Inode, loc.Generation, []ext4test.Extent{{Lblk: 10, Len: 3, Phys: d[3]}}, true) // beyond a gap
	})
	c := leafOnly(t, f)
	if got := stopOf(c); got != "map-overlap@1" || coveredBytes(c) != lbs {
		t.Errorf("stop %q covered %d, want map-overlap@1 and 1 block", got, coveredBytes(c))
	}
}
