package apfs_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// Offsets of the space manager structures, written here from the format
// reference and not taken from the reader.
const (
	spmBlockSize   = 32
	spmBPC         = 36
	spmCPC         = 40
	spmDev0        = 48
	spmDev1        = 96
	spmDevChunks   = 8
	spmDevCibs     = 16
	spmDevCabs     = 20
	spmDevAddrOff  = 32
	spmIPBlocks    = 152
	spmIPBase      = 176
	cibIndex       = 32
	cibCount       = 36
	cibRecs        = 40
	ciSize         = 32
	ciAddr         = 8
	ciBlocks       = 16
	ciFree         = 20
	ciBitmap       = 24
	sbBlockedStart = 1240
	sbBlockedCount = 1248
)

func putU32(b []byte, off int, v uint32) { le.PutUint32(b[off:], v) }
func putU64(b []byte, off int, v uint64) { le.PutUint64(b[off:], v) }

// byteRuns converts half-open block ranges to byte runs.
func byteRuns(spans [][2]uint64, bs int) []filesys.Run {
	var out []filesys.Run
	for _, s := range spans {
		out = append(out, filesys.Run{Offset: int64(s[0]) * int64(bs), Length: int64(s[1]-s[0]) * int64(bs)})
	}
	return out
}

func wantFree(im *image) []filesys.Run { return byteRuns(im.g.FreeRuns(), im.bs) }

// checkRunsValid asserts the shape every result must have: sorted, merged, in
// the container and positive.
func checkRunsValid(t *testing.T, runs []filesys.Run, size int64) {
	t.Helper()
	var end int64 = -1
	for i, r := range runs {
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > size {
			t.Fatalf("run %d = %+v is outside [0, %d)", i, r, size)
		}
		if r.Offset <= end {
			t.Fatalf("run %d = %+v is not after the end of the previous run (%d): not sorted and merged", i, r, end)
		}
		end = r.Offset + r.Length
	}
}

func total(runs []filesys.Run) (n int64) {
	for _, r := range runs {
		n += r.Length
	}
	return n
}

func sameRuns(t *testing.T, what string, got, want []filesys.Run) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: %d runs (%d bytes) %v, want %d runs (%d bytes) %v", what, len(got), total(got), head(got), len(want), total(want), head(want))
	}
}

func head(r []filesys.Run) []filesys.Run { return r[:min(len(r), 6)] }

func unalloc(t *testing.T, f *apfs.FS) []filesys.Run {
	t.Helper()
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	checkRunsValid(t, runs, f.Info().Size)
	return runs
}

// clipRuns keeps the part of runs inside [lo, hi) bytes.
func clipRuns(runs []filesys.Run, lo, hi int64) []filesys.Run {
	var out []filesys.Run
	for _, r := range runs {
		s, e := max(r.Offset, lo), min(r.Offset+r.Length, hi)
		if s < e {
			out = append(out, filesys.Run{Offset: s, Length: e - s})
		}
	}
	return out
}

// allocVolume is a volume with live files and a snapshot with extents the live
// tree no longer has.
func allocVolume() apfstest.Volume {
	return snapVolume(
		[]apfstest.File{
			{Path: "/live.bin", Data: pattern(5*bs, 41)},
			{Path: "/dir", Dir: true},
			{Path: "/dir/frag.bin", Data: pattern(8*bs, 42), Fragments: 4},
		},
		apfstest.Snapshot{Name: "S", Files: []apfstest.File{
			{Path: "/gone.bin", Data: pattern(6*bs, 43)},
			{Path: "/live.bin", Data: pattern(3*bs, 44)},
		}},
	)
}

// The multi-chunk container: two chunks (the second a partial one that holds no
// allocated block, so it has no bitmap block, like the real multi-chunk
// fixture), one chunk-info block per chunk and a CIB-address block per CIB.
// Built once and shared by the tests that restore what they patch.
var (
	bigOnce sync.Once
	bigImg  *image
)

func bigOpts() apfstest.Options {
	return apfstest.Options{
		Blocks: 32768 + 1000, Xid: 40, ChunksPerCIB: 1, CibsPerCAB: 1,
		Volumes: []apfstest.Volume{allocVolume()},
	}
}

func bigImage(t *testing.T) *image {
	t.Helper()
	if testing.Short() {
		t.Skip("multi-chunk container skipped under -short")
	}
	bigOnce.Do(func() { bigImg = newImage(t, bigOpts()) })
	return bigImg
}

// patch applies fn to block n of the image and returns the undo.
func (im *image) patch(n uint64, fn func(b []byte)) (undo func()) {
	b := im.blk(n)
	saved := bytes.Clone(b)
	fn(b)
	return func() { copy(b, saved) }
}

// patchSealed is patch for an object block: the checksum is recomputed.
func (im *image) patchSealed(n uint64, fn func(b []byte)) (undo func()) {
	b := im.blk(n)
	saved := bytes.Clone(b)
	fn(b)
	im.seal(n)
	return func() { copy(b, saved) }
}

func TestUnallocatedFromSpaceman(t *testing.T) {
	cases := []struct {
		name string
		o    apfstest.Options
		big  bool
	}{
		{"single chunk", apfstest.Options{Blocks: 4096, Xid: 40, Volumes: []apfstest.Volume{allocVolume()}}, false},
		{"single chunk, CAB layer", apfstest.Options{Blocks: 4096, Xid: 40, ChunksPerCIB: 1, CibsPerCAB: 1, Volumes: []apfstest.Volume{allocVolume()}}, false},
		{"no volumes", apfstest.Options{Blocks: 2048}, false},
		{"two chunks", apfstest.Options{Blocks: 32768 + 1000, Xid: 40, Volumes: []apfstest.Volume{allocVolume()}}, true},
		{"two chunks, one CIB each", apfstest.Options{Blocks: 32768 + 1000, Xid: 40, ChunksPerCIB: 1, Volumes: []apfstest.Volume{allocVolume()}}, true},
		{"two chunks, CIB each, CAB layer", bigOpts(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.big && testing.Short() {
				t.Skip("multi-chunk container skipped under -short")
			}
			im := newImage(t, c.o)
			want := wantFree(im)
			if len(want) == 0 {
				t.Fatal("the builder left no free space")
			}
			f := im.mustOpen()
			got := unalloc(t, f)
			sameRuns(t, "Unallocated", got, want)
			noWarnings(t, f)
			if c.big {
				// The second chunk has no bitmap block: it is free as a whole.
				sp := im.g.Spaceman
				if len(sp.Chunks) != 2 || sp.Chunks[1].Bitmap != 0 || sp.Chunks[0].Bitmap == 0 {
					t.Fatalf("builder chunks = %+v", sp.Chunks)
				}
				last := got[len(got)-1]
				if last.Offset+last.Length != int64(len(im.b)) || last.Offset > int64(32768)*int64(bs) {
					t.Errorf("the last run %+v does not cover the bitmap-less chunk to the end of the container", last)
				}
			}
			// Repeated calls agree.
			sameRuns(t, "second call", unalloc(t, f), want)
		})
	}
}

// Free space is never guessed: a region whose metadata cannot be trusted is
// skipped with a warning, the rest is reported, and blocks of any file or of a
// snapshot only are never free.
func TestUnallocatedNeverFreesUsedBlocks(t *testing.T) {
	im := bigImage(t)
	f := im.mustOpen()
	base := unalloc(t, f)
	sameRuns(t, "baseline", base, wantFree(im))
	bsz := int64(bs)
	chunk1 := int64(32768) * bsz
	chunk0Free := clipRuns(base, 0, chunk1)
	chunk1Free := clipRuns(base, chunk1, int64(len(im.b)))
	if len(chunk0Free) == 0 || len(chunk1Free) == 0 {
		t.Fatal("both chunks need free space")
	}
	sp := im.g.Spaceman
	cibRec := func(k int) (uint64, int) { return sp.CIBs[k], cibRecs } // each CIB holds one record
	spaceman := im.g.Checkpoints[0].Spaceman

	type tc struct {
		name  string
		patch func() func() // applies the damage, returns the undo
		want  []filesys.Run // what must still be reported
		warn  string
	}
	cases := []tc{
		{"bad spaceman checksum", func() func() {
			return im.patch(spaceman, func(b []byte) { b[100] ^= 0xff })
		}, nil, "no free space is reported"},
		{"bad CIB checksum skips its chunk", func() func() {
			return im.patch(sp.CIBs[0], func(b []byte) { b[100] ^= 0xff })
		}, chunk1Free, "bad checksum"},
		{"bad CAB checksum skips its CIBs", func() func() {
			return im.patch(sp.CABs[1], func(b []byte) { b[100] ^= 0xff })
		}, chunk0Free, "bad checksum"},
		{"CIB with the wrong index", func() func() {
			return im.patchSealed(sp.CIBs[1], func(b []byte) { putU32(b, cibIndex, 0) })
		}, chunk0Free, "has index 0"},
		{"CIB claiming too many records", func() func() {
			return im.patchSealed(sp.CIBs[0], func(b []byte) { putU32(b, cibCount, 1<<20) })
		}, chunk1Free, "chunk-info records"},
		{"free count differing from the bitmap", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciFree, le.Uint32(b[off+ciFree:])-1) })
		}, chunk1Free, "clear bits"},
		{"chunk address wrong", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU64(b, off+ciAddr, 4096) })
		}, chunk1Free, "records address"},
		{"chunk address wrong (second chunk)", func() func() {
			a, off := cibRec(1)
			return im.patchSealed(a, func(b []byte) { putU64(b, off+ciAddr, 0) })
		}, chunk0Free, "records address"},
		{"bitmap address 0 with partial counts", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU64(b, off+ciBitmap, 0) })
		}, chunk1Free, "no bitmap block"},
		{"free count of a bitmap-less chunk below its blocks", func() func() {
			a, off := cibRec(1)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciFree, le.Uint32(b[off+ciBlocks:])-1) })
		}, chunk0Free, "no bitmap block"},
		{"reserved count bits", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciFree, le.Uint32(b[off+ciFree:])|1<<20) })
		}, chunk1Free, "reserved bits"},
		{"reserved block-count bits", func() func() {
			a, off := cibRec(1)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciBlocks, le.Uint32(b[off+ciBlocks:])|1<<31) })
		}, chunk0Free, "reserved bits"},
		{"block count wrong", func() func() {
			a, off := cibRec(1)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciBlocks, 999) })
		}, chunk0Free, "covers 999 blocks"},
		{"free count above the block count", func() func() {
			a, off := cibRec(1)
			return im.patchSealed(a, func(b []byte) { putU32(b, off+ciFree, 5000) })
		}, chunk0Free, "free of"},
		{"bitmap outside the container", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU64(b, off+ciBitmap, uint64(len(im.b)/bs)+7) })
		}, chunk1Free, "outside the container"},
		{"bitmap inside a checkpoint area", func() func() {
			a, off := cibRec(0)
			return im.patchSealed(a, func(b []byte) { putU64(b, off+ciBitmap, im.g.DescBase+1) })
		}, chunk1Free, "checkpoint area"},
		{"both chunks name one bitmap block", func() func() {
			a0, off0 := cibRec(0)
			a1, off1 := cibRec(1)
			u0 := im.patchSealed(a0, func([]byte) {})
			u1 := im.patchSealed(a1, func(b []byte) { putU64(b, off1+ciBitmap, le.Uint64(im.blk(a0)[off0+ciBitmap:])) })
			return func() { u1(); u0() }
		}, nil, "shares its bitmap block"},
		{"one more device (tier 2)", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU64(b, spmDev1, 1000) })
		}, nil, "tier 2"},
		{"blocks per chunk not one bitmap block", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU32(b, spmBPC, 4096) })
		}, nil, "blocks per chunk"},
		{"block size differing from the container's", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU32(b, spmBlockSize, 8192) })
		}, nil, "block size"},
		{"chunk count not following from the block count", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU64(b, spmDev0+spmDevChunks, 3) })
		}, nil, "chunks for"},
		{"address array outside the object", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU32(b, spmDev0+spmDevAddrOff, uint32(bs-8)) })
		}, nil, "address array"},
		{"internal pool outside the container", func() func() {
			return im.patchSealed(spaceman, func(b []byte) { putU64(b, spmIPBase, uint64(len(im.b)/bs)) })
		}, nil, "internal pool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := im.mustOpen() // opened intact: the free-space metadata is read fresh at each call
			undo := c.patch()
			defer undo()
			got, err := f.Unallocated()
			if err != nil {
				t.Fatalf("Unallocated: %v", err)
			}
			checkRunsValid(t, got, f.Info().Size)
			sameRuns(t, "after damage", got, c.want)
			if !hasWarn(f, c.warn) {
				t.Errorf("warnings %q, want one with %q", f.Info().Warnings, c.warn)
			}
			// Whatever the damage, nothing is reported that the intact metadata
			// did not report.
			if extra := subtractRuns(got, base); len(extra) != 0 {
				t.Errorf("damage freed blocks the intact image keeps used: %v", head(extra))
			}
		})
	}
	// After every undo the intact image reads as before.
	sameRuns(t, "after the undos", unalloc(t, im.mustOpen()), base)
}

// subtractRuns returns a minus b (both sorted, merged).
func subtractRuns(a, b []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range a {
		cur := r.Offset
		end := r.Offset + r.Length
		for _, x := range b {
			xs, xe := x.Offset, x.Offset+x.Length
			if xe <= cur || xs >= end {
				continue
			}
			if xs > cur {
				out = append(out, filesys.Run{Offset: cur, Length: xs - cur})
			}
			cur = max(cur, xe)
		}
		if cur < end {
			out = append(out, filesys.Run{Offset: cur, Length: end - cur})
		}
	}
	return out
}

// The bitmap can say a metadata block is free; the reader still never reports
// the first block, the checkpoint areas, the internal pool and its bitmaps, the
// space manager's blocks, every bitmap block or the blocked-out range. The
// blocks of every file (live or only in a snapshot) are never free either.
func TestUnallocatedMetadataAndFileBlocksNeverFree(t *testing.T) {
	o := apfstest.Options{Blocks: 4096, Xid: 40, ChunksPerCIB: 1, CibsPerCAB: 1, Volumes: []apfstest.Volume{allocVolume()}}
	im := newImage(t, o)
	g, sp := im.g, im.g.Spaceman
	// Blocks the bitmap will claim free although they are metadata, and a range
	// of really free blocks the superblock blocks out.
	meta := []uint64{0, g.DescBase, g.DescBase + 1, g.DataBase, g.DataBase + 1, sp.IPBmBase, sp.IPBase, sp.CABs[0], sp.CIBs[0], sp.Chunks[0].Bitmap, g.Checkpoints[0].Spaceman}
	blockedStart, blockedCount := g.Free+20, uint64(30)
	im.patchSupers(func(b []byte) {
		putU64(b, sbBlockedStart, blockedStart)
		putU64(b, sbBlockedCount, blockedCount)
	})
	// Clear the bits of the metadata blocks and adjust the free count so every
	// check on the chunk passes: only the exclusion list can keep them used.
	bm := im.blk(sp.Chunks[0].Bitmap)
	cleared := 0
	for _, n := range meta {
		if bm[n/8]&(1<<(n%8)) != 0 {
			bm[n/8] &^= 1 << (n % 8)
			cleared++
		}
	}
	cib := im.blk(sp.CIBs[0])
	putU32(cib, cibRecs+ciFree, le.Uint32(cib[cibRecs+ciFree:])+uint32(cleared))
	im.seal(sp.CIBs[0])

	f := im.mustOpen()
	got := unalloc(t, f)
	noWarnings(t, f)
	want := subtractRuns(wantFree(im), byteRuns([][2]uint64{{blockedStart, blockedStart + blockedCount}}, bs))
	sameRuns(t, "with metadata claimed free and a blocked-out range", got, want)
	for _, n := range meta {
		r := filesys.Run{Offset: int64(n) * int64(bs), Length: int64(bs)}
		if len(clipRuns(got, r.Offset, r.Offset+r.Length)) != 0 {
			t.Errorf("block %d is metadata and was reported free", n)
		}
	}

	// Every file's blocks, in the live tree and in the snapshot, are not free.
	im2 := newImage(t, o)
	f2 := im2.mustOpen()
	free := unalloc(t, f2)
	var files int
	err := filesys.Walk(f2, f2.Root(), "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil {
			return werr
		}
		if e.Type != filesys.TypeFile {
			return nil
		}
		fl, err := f2.Open(e)
		if err != nil {
			return err
		}
		files++
		for _, r := range fl.Runs() {
			if r.Offset >= 0 && len(clipRuns(free, r.Offset, r.Offset+r.Length)) != 0 {
				t.Errorf("%s: data run %+v overlaps free space", p, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 4 { // 2 live + 2 in the snapshot
		t.Errorf("walked only %d files", files)
	}
}

// The bit order and polarity are the reader's choice from the format reference
// (the bit of block i of a chunk is mask 1<<(i%8) of byte i/8, set = allocated);
// the real fixtures check it: the bits the independent decoder reports match,
// and the opposite conventions do not.
func TestUnallocatedBitOrderAndPolarity(t *testing.T) {
	for _, name := range orcFixtures {
		t.Run(name, func(t *testing.T) {
			orcSkipShort(t, name)
			img, exp := orcLoad(t, name)
			bsz := int(exp.Container.BlockSize)
			if len(exp.FreeRanges) == 0 || len(exp.Chunks) == 0 {
				t.Fatal("the oracle lists no free ranges or chunks")
			}
			c0 := exp.Chunks[0]
			bm := img[int(c0.BitmapAddr)*bsz : (int(c0.BitmapAddr)+1)*bsz]
			bit := func(b int) bool { return bm[b/8]&(1<<(b%8)) != 0 }
			if !bit(0) {
				t.Error("block 0 (the container superblock) must be allocated: set = allocated, bit 0 = first block")
			}
			first := int(exp.FreeRanges[0][0])
			if bit(first) || !bit(first-1) {
				t.Errorf("free range starts at block %d: bit %v, previous bit %v; want clear then set", first, bit(first), bit(first-1))
			}
			var zeros, zerosMSB int
			for i := 0; i < int(c0.BlockCount); i++ {
				if !bit(i) {
					zeros++
				}
				if bm[i/8]&(0x80>>(i%8)) == 0 {
					zerosMSB++
				}
			}
			if zeros != int(c0.FreeCount) {
				t.Errorf("clear bits (LSB first) = %d, record says %d free", zeros, c0.FreeCount)
			}
			if zerosMSB == int(c0.FreeCount) && zerosMSB != zeros {
				t.Error("the MSB-first reading matches too: the fixture cannot tell the bit orders apart")
			}
		})
	}
}

// The real containers: the free space equals the oracle's free ranges exactly
// (same runs, same count), none of it lies in the checkpoint areas, and there
// is no warning. apfs-multichunk's second chunk has no bitmap block.
func orcCheckUnallocated(t *testing.T, img []byte, exp *orcExpect) {
	t.Helper()
	f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bsz := int64(exp.Container.BlockSize)
	var want []filesys.Run
	var blocks uint64
	for _, r := range exp.FreeRanges { // inclusive block ranges
		want = append(want, filesys.Run{Offset: int64(r[0]) * bsz, Length: int64(r[1]-r[0]+1) * bsz})
		blocks += r[1] - r[0] + 1
	}
	got, err := f.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	sameRuns(t, "Unallocated", got, want)
	if blocks != exp.FreeBlocks || uint64(total(got)/bsz) != exp.FreeBlocks {
		t.Errorf("free blocks: reader %d, ranges %d, oracle %d", total(got)/bsz, blocks, exp.FreeBlocks)
	}
	cp := exp.Checkpoint
	for _, a := range []orcArea{cp.Desc, cp.Data} {
		lo, hi := int64(a.Base)*bsz, int64(a.Base+uint64(a.Blocks))*bsz
		if over := clipRuns(got, lo, hi); len(over) != 0 {
			t.Errorf("free space %v overlaps the checkpoint area [%d, %d)", over, lo, hi)
		}
	}
	if len(clipRuns(got, 0, bsz)) != 0 {
		t.Error("block 0 reported free")
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a real container: %q", w)
	}
}

func TestUnallocatedTruncatedImage(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 4096, Xid: 40, Volumes: []apfstest.Volume{allocVolume()}})
	full := wantFree(im)
	sp := im.g.Spaceman

	// Cut after the space manager's blocks: the declared container is larger than
	// the image, the free space is clipped to what the image holds.
	cut := int(sp.Chunks[0].Slot) + 50
	f, err := apfs.Open(bytes.NewReader(im.b[:cut*bs]), int64(cut*bs))
	if err != nil {
		t.Fatal(err)
	}
	got := unalloc(t, f)
	sameRuns(t, "clipped to the image", got, clipRuns(full, 0, int64(cut*bs)))
	if !hasWarn(f, "truncated image") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}

	// Cut through the middle of the pool: the CIB or the bitmap is unreadable, so
	// no free space is reported (a warning says why), never a guess.
	for _, blk := range []uint64{sp.CIBs[0], sp.Chunks[0].Bitmap} {
		t.Run("cut at block "+itoa(blk), func(t *testing.T) {
			f, err := apfs.Open(bytes.NewReader(im.b[:blk*uint64(bs)]), int64(blk)*int64(bs))
			if err != nil {
				t.Skipf("Open fails on this cut: %v", err) // the cut may also take the checkpoint objects
			}
			if got := unalloc(t, f); len(got) != 0 {
				t.Errorf("reported %v from an unreadable pool", head(got))
			}
			if !hasWarn(f, "truncated image") {
				t.Errorf("warnings %q", f.Info().Warnings)
			}
		})
	}
}

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for ok := true; ok; ok = n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// A free count says how many blocks the run list may hold; a cap on the runs
// cuts the list with a warning, and every run stays inside the container.
func TestUnallocatedBoundsAndCap(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 4096, Xid: 40, Volumes: []apfstest.Volume{allocVolume()}})
	sp := im.g.Spaceman
	// Make the free space fragmented: set every other bit of blocks Free..Free+200
	// allocated, and keep the record consistent.
	bm := im.blk(sp.Chunks[0].Bitmap)
	set := 0
	for n := im.g.Free; n < im.g.Free+200; n += 2 {
		bm[n/8] |= 1 << (n % 8)
		set++
	}
	cib := im.blk(sp.CIBs[0])
	putU32(cib, cibRecs+ciFree, le.Uint32(cib[cibRecs+ciFree:])-uint32(set))
	im.seal(sp.CIBs[0])

	f := im.mustOpen()
	all := unalloc(t, f)
	if len(all) < 100 {
		t.Fatalf("only %d runs; the test needs a fragmented free space", len(all))
	}
	for _, r := range all {
		if r.Offset+r.Length > int64(len(im.b)) {
			t.Fatalf("run %+v past the end of the image", r)
		}
	}
	noWarnings(t, f)

	f.SetUnallocCap(7)
	got := unalloc(t, f)
	if len(got) != 7 || !slices.Equal(got, all[:7]) {
		t.Errorf("capped to 7: %d runs %v", len(got), head(got))
	}
	if !hasWarn(f, "more than 7 free runs") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
}

// flakyReader fails reads that touch [from, to) once armed.
type flakyReader struct {
	r        io.ReaderAt
	from, to int64
	armed    atomic.Bool
	err      error
}

func (r *flakyReader) ReadAt(p []byte, off int64) (int, error) {
	if r.armed.Load() && off < r.to && off+int64(len(p)) > r.from {
		return 0, r.err
	}
	return r.r.ReadAt(p, off)
}

// A read error is an error, never free space: it is returned wrapped, with no
// runs.
func TestUnallocatedUnreadableBitmap(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 4096, Xid: 40, Volumes: []apfstest.Volume{allocVolume()}})
	sp := im.g.Spaceman
	boom := errors.New("disk on fire")
	for name, blk := range map[string]uint64{"bitmap": sp.Chunks[0].Bitmap, "CIB": sp.CIBs[0], "spaceman": im.g.Checkpoints[0].Spaceman} {
		t.Run(name, func(t *testing.T) {
			r := &flakyReader{r: bytes.NewReader(im.b), from: int64(blk) * int64(bs), to: int64(blk+1) * int64(bs), err: boom}
			f, err := apfs.Open(r, int64(len(im.b)))
			if err != nil {
				t.Fatal(err)
			}
			r.armed.Store(true)
			runs, err := f.Unallocated()
			if !errors.Is(err, boom) || len(runs) != 0 {
				t.Errorf("Unallocated = %d runs, %v; want the read error", len(runs), err)
			}
			r.armed.Store(false)
			if got := unalloc(t, f); len(got) == 0 { // the failure was not remembered
				t.Error("no free space after the reader recovered")
			}
		})
	}
}

// Warnings accumulate in Info(): a problem found by one call stays, a new one
// is added, and the same one is not repeated.
func TestUnallocatedWarningsAccumulate(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 4096, Xid: 40, ChunksPerCIB: 1, Volumes: []apfstest.Volume{allocVolume()}})
	sp := im.g.Spaceman
	f := im.mustOpen()
	noWarnings(t, f)
	im.patchSealed(sp.CIBs[0], func(b []byte) { putU32(b, cibRecs+ciFree, le.Uint32(b[cibRecs+ciFree:])-1) })
	if got := unalloc(t, f); len(got) != 0 {
		t.Fatalf("a chunk with inconsistent counts reported %v", head(got))
	}
	first := slices.Clone(f.Info().Warnings)
	if len(first) != 1 || !strings.Contains(first[0], "clear bits") {
		t.Fatalf("warnings %q", first)
	}
	unalloc(t, f)
	if got := f.Info().Warnings; !slices.Equal(got, first) {
		t.Errorf("the same problem was reported twice: %q", got)
	}
	im.patchSealed(im.g.Checkpoints[0].Spaceman, func(b []byte) { putU32(b, spmBlockSize, 8192) })
	unalloc(t, f)
	got := f.Info().Warnings
	if len(got) != 2 || got[0] != first[0] || !strings.Contains(got[1], "block size") {
		t.Errorf("warnings %q, want the first kept and a block-size warning added", got)
	}
}

// Whatever bytes of the space manager's objects are damaged, nothing panics,
// every result is a valid run list, and the always-excluded blocks are never in
// it.
func TestUnallocatedMutatedNeverPanics(t *testing.T) {
	o := apfstest.Options{Blocks: 4096, Xid: 40, ChunksPerCIB: 1, CibsPerCAB: 1, Volumes: []apfstest.Volume{allocVolume()}}
	base := newImage(t, o)
	g, sp := base.g, base.g.Spaceman
	targets := []uint64{g.Checkpoints[0].Spaceman, sp.CABs[0], sp.CIBs[0], sp.Chunks[0].Bitmap}
	rng := rand.New(rand.NewPCG(7, 13)) //nolint:gosec // deterministic test input, not security
	excluded := [][2]uint64{{0, 1}, {g.DescBase, g.DescBase + g.DescCount}, {g.DataBase, g.DataBase + g.DataCount}, {sp.IPBmBase, sp.IPBase + sp.IPBlocks}}
	pristine := bytes.Clone(base.b)
	for i := 0; i < 400; i++ {
		img := bytes.Clone(pristine)
		for j := rng.IntN(6) + 1; j > 0; j-- {
			blk := targets[rng.IntN(len(targets))]
			off := int(blk)*bs + rng.IntN(bs)
			if rng.IntN(2) == 0 {
				off = int(blk)*bs + rng.IntN(400) // the header and the hot fields
			}
			img[off] = byte(rng.IntN(256))
			if rng.IntN(3) != 0 && blk != sp.Chunks[0].Bitmap {
				apfstest.Seal(img, bs, blk) // mostly keep the object valid, so the fields are read
			}
		}
		f, err := apfs.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			continue
		}
		runs, err := f.Unallocated()
		if err != nil {
			continue
		}
		checkRunsValid(t, runs, f.Info().Size)
		for _, e := range excluded {
			if over := clipRuns(runs, int64(e[0])*int64(bs), int64(e[1])*int64(bs)); len(over) != 0 {
				t.Fatalf("iteration %d: free space %v inside the always-excluded blocks [%d, %d)", i, over, e[0], e[1])
			}
		}
	}
}
