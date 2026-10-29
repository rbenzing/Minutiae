package ext4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const leafBS = 1024

// leafImage builds a metadata_csum image with a deleted 12-block file whose blocks are free, and lets put write
// leaves into those free blocks (their numbers are passed in).
func leafImage(t *testing.T, put func(img []byte, l *ext4test.Layout, free []int)) []byte {
	t.Helper()
	img, l := ext4test.BuildLayout(csumExt, []ext4test.File{{Path: "/gone.bin", Data: pattern(12*leafBS, 31), Deleted: true}})
	l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	if put != nil {
		put(img, l, l.File("/gone.bin").Data)
	}
	return img
}

func freeBytes(t *testing.T, f *ext4.FS) (total int64, runs []filesys.Run) {
	t.Helper()
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		total += r.Length
	}
	return total, runs
}

func prepare(t *testing.T, f *ext4.FS) {
	t.Helper()
	if err := f.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func oneLeaf(img []byte, l *ext4test.Layout, free []int) {
	l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: free[1]}}, true)
}

func TestCRCSplitsIntoSeedAndData(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a fixed seed makes the test reproducible
	for range 1000 {
		d := make([]byte, rng.IntN(4097))
		for i := range d {
			d[i] = byte(rng.Uint32())
		}
		s := rng.Uint32()
		if got, want := ext4.RawCRC32C(s, d), ext4.RawCRC32C(s, make([]byte, len(d)))^ext4.RawCRC32C(0, d); got != want {
			t.Fatalf("len %d seed %#x: crc %#x, split %#x", len(d), s, got, want)
		}
	}
}

func TestLeafKeyMatchesInodeSeed(t *testing.T) {
	var leafBlk int
	img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
		oneLeaf(img, l, free)
		leafBlk = free[0]
	})
	f := openImg(t, img)
	prepare(t, f)
	tail := ext4.LeafTail(leafBS)
	if k := ext4.LeafKey(img[leafBlk*leafBS:(leafBlk+1)*leafBS], tail); k != ext4.SeedKey(f.InodeSeed(12, 7), tail) {
		t.Errorf("the key from the block is %#x, from the seed %#x", k, ext4.SeedKey(f.InodeSeed(12, 7), tail))
	}
	for _, c := range []struct {
		ino, gen uint32
		want     bool
	}{{12, 7, true}, {12, 8, false}, {13, 7, false}} {
		blks, capped := f.LeafLookup(ext4.SeedKey(f.InodeSeed(c.ino, c.gen), tail))
		if capped {
			t.Errorf("(%d,%d): index reported capped", c.ino, c.gen)
		}
		if got := len(blks) == 1; got != c.want {
			t.Errorf("(%d,%d): found %v, want found=%v", c.ino, c.gen, blks, c.want)
		}
	}
}

type hookReader struct {
	r  io.ReaderAt
	mu sync.Mutex
	fn func(p []byte, off int64)
}

func (h *hookReader) ReadAt(p []byte, off int64) (int, error) {
	h.mu.Lock()
	fn := h.fn
	h.mu.Unlock()
	if fn != nil {
		fn(p, off)
	}
	return h.r.ReadAt(p, off)
}

func (h *hookReader) set(fn func(p []byte, off int64)) {
	h.mu.Lock()
	h.fn = fn
	h.mu.Unlock()
}

func openHooked(t *testing.T, img []byte) (*ext4.FS, *hookReader) {
	t.Helper()
	h := &hookReader{r: bytes.NewReader(img)}
	f, err := ext4.Open(h, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f, h
}

func inRuns(runs []filesys.Run, off int64) bool {
	for _, r := range runs {
		if off >= r.Offset && off < r.Offset+r.Length {
			return true
		}
	}
	return false
}

func TestPrepareRecoveryReadsFreeBlocksOnce(t *testing.T) {
	f := openImg(t, leafImage(t, oneLeaf))
	free, _ := freeBytes(t, f)
	prepare(t, f)
	leaves, full, read := f.LeafIndexStats()
	if read != free || leaves != 1 || full {
		t.Fatalf("leaves %d full %v read %d; want 1, false, %d free bytes", leaves, full, read, free)
	}
	prepare(t, f)
	if _, _, again := f.LeafIndexStats(); again != read {
		t.Errorf("a second call read %d more bytes", again-read)
	}
}

func TestPrepareRecoveryHonoursCancel(t *testing.T) {
	f, h := openHooked(t, leafImage(t, oneLeaf))
	f.SetLeafChunk(2 * leafBS)
	_, runs := freeBytes(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	h.set(func(_ []byte, off int64) {
		if inRuns(runs, off) {
			if reads++; reads == 1 {
				cancel()
			}
		}
	})
	if err := f.PrepareRecovery(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if reads != 1 {
		t.Errorf("%d free reads, want exactly the first chunk before the cancel is seen", reads)
	}
	if leaves, _, _ := f.LeafIndexStats(); leaves != -1 {
		t.Errorf("a cancelled pass left an index with %d leaves", leaves)
	}
	h.set(nil)
	prepare(t, f)
	if leaves, _, _ := f.LeafIndexStats(); leaves != 1 {
		t.Errorf("the next call indexed %d leaves, want 1", leaves)
	}
}

// A reader that tells the cached view from the uncached one: the metadata cache reads one block at a time, the
// free-space pass reads its chunks. Every read that starts in free space must be exactly the pass's next chunk.
func TestPrepareRecoveryBypassesTheMetadataCache(t *testing.T) {
	f, h := openHooked(t, leafImage(t, nil))
	const chunk = 2 * leafBS
	f.SetLeafChunk(chunk)
	_, runs := freeBytes(t, f)
	type rd struct{ off, n int64 }
	var got []rd
	h.set(func(p []byte, off int64) {
		if inRuns(runs, off) {
			got = append(got, rd{off, int64(len(p))})
		}
	})
	prepare(t, f)
	var want []rd
	for _, r := range runs {
		for off := r.Offset; off < r.Offset+r.Length; off += chunk {
			want = append(want, rd{off, min(chunk, r.Offset+r.Length-off)})
		}
	}
	if len(want) < 2 {
		t.Fatal("the image has too little free space to tell the views apart")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%d free-space reads, want the %d sequential chunks %v; first reads %v", len(got), len(want), want[:2], got[:min(2, len(got))])
	}
}

func TestLeafIndexSkipsImplausibleBlocks(t *testing.T) {
	good := []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}, {Lblk: 10, Len: 2, Phys: 120}}
	le16 := binary.LittleEndian.PutUint16
	le32 := binary.LittleEndian.PutUint32
	ent := func(b []byte, i int) []byte { return b[12+12*i:][:12] }
	rows := []struct {
		name string
		edit func(b []byte)
	}{
		{"wrong magic", func(b []byte) { le16(b, 0xF30B) }},
		{"depth 1", func(b []byte) { le16(b[6:], 1) }},
		{"eh_max 4", func(b []byte) { le16(b[4:], 4) }},
		{"entries 0", func(b []byte) { le16(b[2:], 0) }},
		{"entries above max", func(b []byte) { le16(b[2:], 85) }},
		{"unsorted", func(b []byte) { le32(ent(b, 0), 20) }},
		{"overlapping", func(b []byte) { le32(ent(b, 1), 2) }},
		{"length 0", func(b []byte) { le16(ent(b, 1)[4:], 0) }},
		{"physical beyond the end", func(b []byte) { le32(ent(b, 1)[8:], 0xFFFFFF00) }},
		{"logical range past 2^32", func(b []byte) { le32(ent(b, 1), 0xFFFFFFFF); le16(ent(b, 1)[4:], 5) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
				l.WriteLeaf(img, free[0], 12, 7, good, true)
				l.WriteLeaf(img, free[1], 12, 7, good, true) // control, left intact
				r.edit(img[free[0]*leafBS : (free[0]+1)*leafBS])
			})
			f := openImg(t, img)
			prepare(t, f)
			if leaves, _, _ := f.LeafIndexStats(); leaves != 1 {
				t.Errorf("%d leaves indexed, want only the intact control", leaves)
			}
		})
	}
	t.Run("an uninitialized extent is plausible", func(t *testing.T) {
		img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
			l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}, {Lblk: 32768, Len: 5, Phys: 200, Uninit: true}}, true)
		})
		f := openImg(t, img)
		prepare(t, f)
		if leaves, _, _ := f.LeafIndexStats(); leaves != 1 {
			t.Errorf("%d leaves indexed, want 1", leaves)
		}
	})
}

// Superseded leaves verify under the same (inode, generation) and are all kept (3C-R31).
func TestLeafIndexKeepsSupersededLeavesUnderOneKey(t *testing.T) {
	img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
		l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 100, Phys: 100}}, true)
		l.WriteLeaf(img, free[1], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 200, Phys: 100}}, true)
		l.WriteLeaf(img, free[2], 12, 8, []ext4test.Extent{{Lblk: 0, Len: 5, Phys: 100}}, true)
	})
	f := openImg(t, img)
	prepare(t, f)
	key := ext4.SeedKey(f.InodeSeed(12, 7), ext4.LeafTail(leafBS))
	blks, capped := f.LeafLookup(key)
	if len(blks) != 2 || capped {
		t.Fatalf("key (12,7): blocks %v capped %v, want two, not capped", blks, capped)
	}
	if blks[0] >= blks[1] {
		t.Errorf("blocks %v are not in ascending block order", blks)
	}
}

func TestLeafIndexCapIsRecorded(t *testing.T) {
	t.Run("total", func(t *testing.T) {
		img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
			for i, g := range []uint32{7, 8, 9} {
				l.WriteLeaf(img, free[i], 12, g, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}}, true)
			}
		})
		f := openImg(t, img)
		f.SetLeafCaps(2, 64)
		prepare(t, f)
		if leaves, full, _ := f.LeafIndexStats(); leaves != 2 || !full {
			t.Errorf("leaves %d full %v, want 2, true", leaves, full)
		}
		// a full index says so on every lookup, so a miss proves nothing
		if _, capped := f.LeafLookup(0xdeadbeef); !capped {
			t.Error("a lookup in a full index does not say it is capped")
		}
	})
	t.Run("per key", func(t *testing.T) {
		img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
			for i := range 3 {
				l.WriteLeaf(img, free[i], 12, 7, []ext4test.Extent{{Lblk: 0, Len: i + 1, Phys: 100}}, true)
			}
			l.WriteLeaf(img, free[3], 12, 8, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}}, true)
		})
		f := openImg(t, img)
		f.SetLeafCaps(1000, 2)
		prepare(t, f)
		tail := ext4.LeafTail(leafBS)
		if blks, capped := f.LeafLookup(ext4.SeedKey(f.InodeSeed(12, 7), tail)); len(blks) != 2 || !capped {
			t.Errorf("key (12,7): %v capped %v, want two kept and capped", blks, capped)
		}
		if blks, capped := f.LeafLookup(ext4.SeedKey(f.InodeSeed(12, 8), tail)); len(blks) != 1 || capped {
			t.Errorf("key (12,8): %v capped %v, want one, not capped", blks, capped)
		}
		if _, full, _ := f.LeafIndexStats(); !full {
			t.Error("stats do not show that leaves were dropped")
		}
	})
}

// The eh_max rule of plausibleLeaf holds for every leaf of every live file of a real image (3C-R12).
func TestLiveLeavesHaveFullBlockCapacity(t *testing.T) {
	const p = "testdata/ext4-4k-csum.img.gz"
	img := gunzipFixture(t, p)
	want := loadOracle(t, p)
	f := openImg(t, img)
	le := binary.LittleEndian
	leaves := 0
	var walk func(node []byte, depth int)
	walk = func(node []byte, depth int) {
		if le.Uint16(node) != 0xF30A {
			t.Fatalf("bad extent magic %#x", le.Uint16(node))
		}
		if le.Uint16(node[6:]) == 0 {
			if depth > 0 {
				leaves++
				if m := int(le.Uint16(node[4:])); m != (4096-12)/12 {
					t.Errorf("leaf eh_max %d, want %d", m, (4096-12)/12)
				}
			}
			return
		}
		for i := range int(le.Uint16(node[2:])) {
			e := node[12+12*i:]
			blk := int64(le.Uint32(e[4:])) | int64(le.Uint16(e[8:]))<<32
			walk(img[blk*4096:(blk+1)*4096], depth+1)
		}
	}
	for _, of := range want.Files {
		if of.Type != "file" {
			continue
		}
		e, err := f.Lookup(of.Path)
		if err != nil {
			t.Fatal(err)
		}
		in, err := f.Inode(inodeNum(t, e.ID))
		if err != nil {
			t.Fatal(err)
		}
		if blk := in.IBlock(); le.Uint16(blk[6:]) > 0 {
			walk(blk, 0)
		}
	}
	if leaves == 0 {
		t.Fatal("the image holds no depth-1 file, the test would pass vacuously")
	}
}

func TestPrepareRecoveryWithoutMetadataCsumIsNoop(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/gone.bin", Data: pattern(4*leafBS, 3), Deleted: true}})
	f := openImg(t, img)
	prepare(t, f)
	if leaves, _, read := f.LeafIndexStats(); read != 0 || leaves > 0 {
		t.Errorf("leaves %d read %d, want no index and no read", leaves, read)
	}
}

func TestPrepareRecoveryAddsNoFilesystemWarning(t *testing.T) {
	img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
		l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}}, false)
	})
	f := openImg(t, img)
	before := slices.Clone(f.Info().Warnings)
	prepare(t, f)
	if after := f.Info().Warnings; !slices.Equal(before, after) {
		t.Errorf("warnings %q became %q", before, after)
	}
}

// A leaf whose last extent ends exactly at logical block 2^32 is plausible, and its end does not wrap to 0.
func TestPlausibleLeafEndAtTheLogicalLimitDoesNotWrap(t *testing.T) {
	var blk int
	img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
		l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0xFFFFFFF0, Len: 16, Phys: 100}}, true)
		blk = free[0]
	})
	first, end, _, ok := ext4.PlausibleLeaf(img[blk*leafBS:(blk+1)*leafBS], leafBS, uint64(len(img)/leafBS))
	if !ok || first != 0xFFFFFFF0 || end != 1<<32 {
		t.Fatalf("first %#x end %#x ok %v, want 0xfffffff0, 1<<32, true", first, end, ok)
	}
}

// A cancel seen only after the last chunk was read is still honoured, and nothing half-built is kept.
func TestPrepareRecoveryHonoursCancelDuringTheLastChunk(t *testing.T) {
	f, h := openHooked(t, leafImage(t, oneLeaf))
	_, runs := freeBytes(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.set(func(_ []byte, off int64) {
		if off == runs[len(runs)-1].Offset {
			cancel()
		}
	})
	if err := f.PrepareRecovery(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if leaves, _, _ := f.LeafIndexStats(); leaves != -1 {
		t.Errorf("a cancelled pass left an index with %d leaves", leaves)
	}
}

type failReader struct {
	r    io.ReaderAt
	runs []filesys.Run
	err  error
}

func (r *failReader) ReadAt(p []byte, off int64) (int, error) {
	if inRuns(r.runs, off) {
		return 0, r.err
	}
	return r.r.ReadAt(p, off)
}

func TestPrepareRecoveryReturnsADataReadErrorAsIs(t *testing.T) {
	img := leafImage(t, oneLeaf)
	boom := errors.New("disk on fire")
	fr := &failReader{r: bytes.NewReader(img), err: boom}
	f, err := ext4.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	_, fr.runs = freeBytes(t, f)
	if err := f.PrepareRecovery(context.Background()); err != boom {
		t.Fatalf("err = %v, want the reader's error unchanged", err)
	}
	if leaves, _, _ := f.LeafIndexStats(); leaves != -1 {
		t.Errorf("a failed pass left an index with %d leaves", leaves)
	}
}

func TestLeafIndexRejectsEhMaxAboveCapacity(t *testing.T) {
	img := leafImage(t, func(img []byte, l *ext4test.Layout, free []int) {
		l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 100}}, true)
		binary.LittleEndian.PutUint16(img[free[0]*leafBS+4:], (leafBS-12)/12+1)
	})
	f := openImg(t, img)
	prepare(t, f)
	if leaves, _, _ := f.LeafIndexStats(); leaves != 0 {
		t.Errorf("%d leaves indexed, want none for eh_max above the block capacity", leaves)
	}
}

// 4 KiB blocks: the tail offset and the keying follow the block size.
func TestLeafKeyAt4KiBBlocks(t *testing.T) {
	const bs = 4096
	o := ext4test.Options{Extents: true, MetadataCsum: true, BlockSize: bs}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/gone.bin", Data: pattern(6*bs, 5), Deleted: true}})
	l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	free := l.File("/gone.bin").Data
	l.WriteLeaf(img, free[0], 12, 7, []ext4test.Extent{{Lblk: 0, Len: 3, Phys: free[1]}}, true)
	f := openImg(t, img)
	prepare(t, f)
	if got, want := ext4.LeafTail(bs), 12+12*((bs-12)/12); got != want {
		t.Fatalf("tail %d, want %d", got, want)
	}
	blks, capped := f.LeafLookup(ext4.SeedKey(f.InodeSeed(12, 7), ext4.LeafTail(bs)))
	if len(blks) != 1 || blks[0] != uint64(free[0]) || capped {
		t.Errorf("blocks %v capped %v, want [%d]", blks, capped, free[0])
	}
	if blks, _ := f.LeafLookup(ext4.SeedKey(f.InodeSeed(12, 8), ext4.LeafTail(bs))); len(blks) != 0 {
		t.Errorf("generation 8 found %v", blks)
	}
}

// A chunk size that is not a block multiple is rounded to whole blocks: nothing is read twice or split.
func TestPrepareRecoveryChunkNotABlockMultiple(t *testing.T) {
	f, h := openHooked(t, leafImage(t, oneLeaf))
	f.SetLeafChunk(2*leafBS + 300)
	free, runs := freeBytes(t, f)
	var got []int64
	h.set(func(p []byte, off int64) {
		if inRuns(runs, off) {
			got = append(got, int64(len(p)))
		}
	})
	prepare(t, f)
	for _, n := range got {
		if n%leafBS != 0 {
			t.Errorf("a read of %d bytes is not a whole number of blocks", n)
		}
	}
	if leaves, _, read := f.LeafIndexStats(); leaves != 1 || read != free {
		t.Errorf("leaves %d read %d, want 1 and %d", leaves, read, free)
	}
}
