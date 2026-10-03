package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// The geometry of the default 1 KiB test image, written out here rather than
// asked of the reader: block 0 is the boot block, group g starts at block
// 1+g*1024, and a group that holds a superblock backup (groups 0, 1, 3, 5, 7,
// 9 ...) is laid out as
//
//	start: superblock, start+1: descriptor table (1 block), then
//	bitmap, inode bitmap and 32 inode table blocks, then data.
//
// A group without a backup begins with its block bitmap.
const (
	tBS     = 1024
	tBPG    = 1024
	tITable = 32 // 128 inodes of 256 bytes
	tGDT    = 2 * tBS
)

var tUUID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func tStart(g int) int { return 1 + g*tBPG }

// metaBlocks returns the blocks a default-layout group g keeps for metadata.
func metaBlocks(g int, hasSuper bool) (blocks []int, bitmap, ibitmap, itable int) {
	pos := tStart(g)
	if hasSuper {
		blocks = append(blocks, pos, pos+1)
		pos += 2
	}
	bitmap, ibitmap, itable = pos, pos+1, pos+2
	blocks = append(blocks, bitmap, ibitmap)
	for i := range tITable {
		blocks = append(blocks, itable+i)
	}
	return blocks, bitmap, ibitmap, itable
}

func openImg(t *testing.T, img []byte) *ext4.FS {
	t.Helper()
	f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// freeSet checks the Unallocated contract (sorted, merged, block aligned,
// inside the image) and returns the free blocks.
func freeSet(t *testing.T, img []byte, runs []filesys.Run) map[int]bool {
	t.Helper()
	free := map[int]bool{}
	end := int64(-1)
	for i, r := range runs {
		if r.Offset%tBS != 0 || r.Length%tBS != 0 || r.Length <= 0 || r.Offset < 0 {
			t.Fatalf("run %d = %+v is not a positive block-aligned range", i, r)
		}
		if r.Offset <= end {
			t.Fatalf("run %d = %+v overlaps or touches the previous run (end %d): not sorted and merged", i, r, end)
		}
		end = r.Offset + r.Length
		if end > int64(len(img)) {
			t.Fatalf("run %d = %+v lies beyond the %d-byte image", i, r, len(img))
		}
		for b := r.Offset / tBS; b < end/tBS; b++ {
			free[int(b)] = true
		}
	}
	return free
}

// compareFree asserts the free set equals the complement of used within the
// blocks [1, total).
func compareFree(t *testing.T, got map[int]bool, used map[int]bool, total int) {
	t.Helper()
	var wrong []string
	for b := 1; b < total; b++ {
		if want := !used[b]; got[b] != want {
			wrong = append(wrong, fmt.Sprintf("block %d: free=%v want %v", b, got[b], want))
		}
	}
	if got[0] {
		wrong = append(wrong, "block 0 (boot block) reported free")
	}
	for b := range got {
		if b >= total {
			wrong = append(wrong, fmt.Sprintf("block %d beyond the filesystem reported free", b))
		}
	}
	if len(wrong) > 0 {
		slices.Sort(wrong)
		if len(wrong) > 12 {
			wrong = append(wrong[:12], fmt.Sprintf("... %d more", len(wrong)-12))
		}
		t.Fatalf("free blocks differ from the expectation:\n%s", strings.Join(wrong, "\n"))
	}
}

func descOff(g, descSize int) int { return tGDT + g*descSize }

// fixDescCsum recomputes bg_checksum of group g's descriptor. mode is "meta"
// (crc32c over the UUID seed), "gdt" (crc16) or "" (none).
func fixDescCsum(img []byte, g, descSize int, mode string) {
	d := img[descOff(g, descSize) : descOff(g, descSize)+descSize]
	put16(d, 0x1E, 0)
	var num [4]byte
	binary.LittleEndian.PutUint32(num[:], uint32(g))
	switch mode {
	case "meta":
		c := ext4.RawCRC32C(ext4.RawCRC32C(0xFFFFFFFF, tUUID[:]), num[:])
		c = ext4.RawCRC32C(c, d[:0x1E])
		c = ext4.RawCRC32C(c, []byte{0, 0})
		c = ext4.RawCRC32C(c, d[0x20:])
		put16(d, 0x1E, uint16(c))
	case "gdt":
		c := ext4.CRC16(0xFFFF, tUUID[:])
		c = ext4.CRC16(c, num[:])
		c = ext4.CRC16(c, d[:0x1E])
		c = ext4.CRC16(c, d[0x20:])
		put16(d, 0x1E, c)
	}
}

func setFlags(img []byte, g int, flags uint16, mode string) {
	put16(img, descOff(g, 32)+0x12, flags)
	fixDescCsum(img, g, 32, mode)
}

// scribble overwrites a block with a fill byte.
func scribble(img []byte, blk int, fill byte) {
	for i := range tBS {
		img[blk*tBS+i] = fill
	}
}

func usedFrom(blocks ...[]int) map[int]bool {
	u := map[int]bool{}
	for _, bs := range blocks {
		for _, b := range bs {
			u[b] = true
		}
	}
	return u
}

func TestUnallocatedMatchesBitmap(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a.txt", Data: bytes.Repeat([]byte{'a'}, 5000)},
		{Path: "/d", Dir: true},
		{Path: "/d/b.bin", Data: bytes.Repeat([]byte{'b'}, 3000)},
	}
	for _, extents := range []bool{true, false} {
		t.Run(fmt.Sprintf("extents=%v", extents), func(t *testing.T) {
			img := ext4test.Build(ext4test.Options{Groups: 3, Extents: extents, UUID: tUUID}, files)
			f := openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			got := freeSet(t, img, runs)

			// The oracle is built from the layout and the file contents, not
			// from the bitmaps: metadata blocks, the blocks of every file and
			// directory, and nothing else.
			var meta [][]int
			for g, sup := range []bool{true, true, false} {
				m, _, _, _ := metaBlocks(g, sup)
				meta = append(meta, m)
			}
			used := usedFrom(meta...)
			for _, p := range []string{"/", "/a.txt", "/d", "/d/b.bin"} {
				e, err := f.Lookup(p)
				if err != nil {
					t.Fatalf("Lookup(%s): %v", p, err)
				}
				var rs []filesys.Run
				if e.Type == filesys.TypeDir {
					rs, err = f.DirRuns(e)
				} else {
					var fl filesys.File
					if fl, err = f.Open(e); err == nil {
						rs = fl.Runs()
					}
				}
				if err != nil {
					t.Fatalf("runs of %s: %v", p, err)
				}
				for _, r := range rs {
					for b := r.Offset / tBS; b < (r.Offset+r.Length+tBS-1)/tBS; b++ {
						used[int(b)] = true
					}
				}
			}
			compareFree(t, got, used, 1+3*tBPG)
			if len(got) == 0 || len(got) > 3*tBPG-100 {
				t.Fatalf("implausible free count %d", len(got))
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a clean image: %q", w)
			}
		})
	}

	t.Run("bitmap is authoritative", func(t *testing.T) {
		img := ext4test.Build(ext4test.Options{Groups: 2, Extents: true, UUID: tUUID},
			[]ext4test.File{{Path: "/a.txt", Data: bytes.Repeat([]byte{'a'}, 2048)}})
		_, bb, _, it := metaBlocks(0, true)
		data := it + tITable           // first data block of group 0
		set := func(blk int, v bool) { // bit of block blk in group 0's bitmap
			i := blk - tStart(0)
			if v {
				img[bb*tBS+i/8] |= 1 << (i % 8)
			} else {
				img[bb*tBS+i/8] &^= 1 << (i % 8)
			}
		}
		runs, _ := openImg(t, img).Unallocated()
		before := freeSet(t, img, runs)
		// Pick a block known free and one known used; swap them in the bitmap.
		freeBlk, usedBlk := -1, -1
		for b := data; b < tStart(1); b++ {
			if before[b] && freeBlk < 0 {
				freeBlk = b
			}
			if !before[b] && usedBlk < 0 {
				usedBlk = b
			}
		}
		if freeBlk < 0 || usedBlk < 0 {
			t.Fatalf("setup: free %d used %d", freeBlk, usedBlk)
		}
		set(freeBlk, true)
		set(usedBlk, false)
		runs, err := openImg(t, img).Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		after := freeSet(t, img, runs)
		if after[freeBlk] || !after[usedBlk] {
			t.Errorf("block %d free=%v (want false), block %d free=%v (want true)", freeBlk, after[freeBlk], usedBlk, after[usedBlk])
		}
		if len(after) != len(before) {
			t.Errorf("free blocks %d, want %d", len(after), len(before))
		}
	})
}

func TestUnallocatedBlockUninit(t *testing.T) {
	build := func(o ext4test.Options) []byte {
		o.Groups, o.UUID, o.Extents = 3, tUUID, true
		return ext4test.Build(o, nil)
	}
	for _, mode := range []string{"gdt", "meta"} {
		t.Run(mode, func(t *testing.T) {
			o := ext4test.Options{GDTCsum: mode == "gdt", MetadataCsum: mode == "meta"}
			img := build(o)
			// Groups 1 (with a superblock backup) and 2 (without) become
			// BLOCK_UNINIT; their on-disk bitmaps are replaced with garbage that
			// would give a different answer if it were read.
			for _, g := range []int{1, 2} {
				setFlags(img, g, 0x2|0x4, mode)
				m := binary.LittleEndian.Uint32(img[descOff(g, 32):])
				scribble(img, int(m), 0xFF)
			}
			f := openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			g0, _, _, _ := metaBlocks(0, true)
			g1, _, _, _ := metaBlocks(1, true)
			g2, _, _, _ := metaBlocks(2, false)
			// Group 0 is read from its bitmap; the root directory takes a data
			// block of it, so compare group 0 against the bitmap-based result and
			// groups 1 and 2 against the uninit rule.
			got := freeSet(t, img, runs)
			used := usedFrom(g1, g2)
			for b := tStart(1); b < 1+3*tBPG; b++ {
				if want := !used[b]; got[b] != want {
					t.Fatalf("block %d free=%v, want %v", b, got[b], want)
				}
			}
			for _, b := range g0 {
				if got[b] {
					t.Fatalf("group 0 metadata block %d reported free", b)
				}
			}
			if !got[tStart(1)+2+2+tITable] || !got[tStart(2)+2+tITable] {
				t.Error("first data blocks of the uninit groups should be free")
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %q", w)
			}
		})
	}

	t.Run("without descriptor checksums the flag is ignored", func(t *testing.T) {
		img := build(ext4test.Options{})
		ref, _ := openImg(t, img).Unallocated()
		setFlags(img, 1, 0x2, "")
		setFlags(img, 2, 0x2, "")
		got, err := openImg(t, img).Unallocated()
		if err != nil || !slices.Equal(got, ref) {
			t.Errorf("Unallocated = %v, %v; want the bitmap result %v", got, err, ref)
		}
	})

	t.Run("flag on a descriptor with a bad checksum is not trusted", func(t *testing.T) {
		img := build(ext4test.Options{GDTCsum: true})
		put16(img, descOff(1, 32)+0x12, 0x2) // flip the flag without fixing bg_checksum
		f := openImg(t, img)
		runs, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		got := freeSet(t, img, runs)
		for b := tStart(1); b < tStart(2); b++ {
			if got[b] {
				t.Fatalf("block %d of the distrusted group reported free", b)
			}
		}
		if !hasWarning(f.Info(), "BLOCK_UNINIT") {
			t.Errorf("no BLOCK_UNINIT warning: %q", f.Info().Warnings)
		}
	})
}

func TestUnallocatedFlexBG(t *testing.T) {
	const flexBG = 0x200
	for _, g2uninit := range []bool{false, true} {
		t.Run(fmt.Sprintf("holder uninit=%v", g2uninit), func(t *testing.T) {
			// Three groups (0 and 1 have a superblock). Group 1's bitmaps and inode
			// table move into group 2 (a group without a superblock backup, whose
			// own metadata stays in place); when g2uninit is set, group 2 is
			// BLOCK_UNINIT and so has no bitmap to say group 1's metadata is there.
			img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, GDTCsum: true, UUID: tUUID}, nil)
			put32(img, 1024+0x60, binary.LittleEndian.Uint32(img[1024+0x60:])|flexBG)

			_, _, _, it2 := metaBlocks(2, false)
			x := it2 + tITable + 5 // inside group 2's data area
			newBB, newIB, newIT := x, x+1, x+2
			d1 := descOff(1, 32)
			g1m, _, _, _ := metaBlocks(1, true)
			// Group 1's bitmap as it is now: only the superblock backup and the
			// descriptor table are used inside it.
			bm := make([]byte, tBS)
			for _, b := range g1m[:2] {
				i := b - tStart(1)
				bm[i/8] |= 1 << (i % 8)
			}
			for i := tBPG; i < tBS*8; i++ {
				bm[i/8] |= 1 << (i % 8)
			}
			// Old locations get garbage that would be wrong if they were read.
			oldBB := int(binary.LittleEndian.Uint32(img[d1:]))
			scribble(img, oldBB, 0xFF)
			copy(img[newBB*tBS:], bm)
			put32(img, d1+0x0, uint32(newBB))
			put32(img, d1+0x4, uint32(newIB))
			put32(img, d1+0x8, uint32(newIT))
			fixDescCsum(img, 1, 32, "gdt")

			// Group 2's bitmap: its own metadata plus group 1's three structures.
			g2m, bb2, _, _ := metaBlocks(2, false)
			used := usedFrom(g2m)
			for b := newBB; b < newIT+tITable; b++ {
				used[b] = true
			}
			if g2uninit {
				setFlags(img, 2, 0x2, "gdt")
				scribble(img, bb2, 0xFF)
			} else {
				bm2 := make([]byte, tBS)
				for b := range used {
					i := b - tStart(2)
					bm2[i/8] |= 1 << (i % 8)
				}
				for i := tBPG; i < tBS*8; i++ {
					bm2[i/8] |= 1 << (i % 8)
				}
				copy(img[bb2*tBS:], bm2)
			}

			f := openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			got := freeSet(t, img, runs)

			// Expected: group 0 per its (unchanged) bitmap, group 1 everything but
			// the superblock backup, group 2 everything but its metadata and the
			// moved structures.
			g0m, _, _, _ := metaBlocks(0, true)
			want := usedFrom(g0m, g1m[:2])
			for b := range used {
				want[b] = true
			}
			// Group 0's root directory is the one data block the builder spends.
			for b := tStart(0); b < tStart(1); b++ {
				if !want[b] && !got[b] {
					want[b] = true // the builder's root directory block
				}
			}
			compareFree(t, got, want, 1+3*tBPG)
			for _, b := range []int{newBB, newIB, newIT, newIT + tITable - 1, oldBB} {
				if b == oldBB {
					if !got[b] {
						t.Errorf("old bitmap block %d of group 1 should be free", b)
					}
					continue
				}
				if got[b] {
					t.Errorf("relocated metadata block %d reported free", b)
				}
			}
		})
	}
}

func TestUnallocatedShortLastGroup(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Groups: 2, Extents: true, UUID: tUUID}, nil)
	const total = 1 + 2*tBPG
	const cut = 100 // the last group is 100 blocks short
	put32(img, 1024+0x4, total-cut)
	img = img[:(total-cut)*tBS]
	f := openImg(t, img)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings: %q", w)
	}
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	got := freeSet(t, img, runs)
	if !got[total-cut-1] || got[total-cut] {
		t.Errorf("last block free=%v, block past the end free=%v", got[total-cut-1], got[total-cut])
	}
	if len(got) != 2*tBPG-2*(2+2+tITable)-cut-1 { // minus metadata, the short tail and the root directory block
		t.Errorf("free blocks = %d", len(got))
	}
}

func TestUnallocatedTruncatedImage(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, GDTCsum: true, UUID: tUUID}, nil)
	setFlags(img, 2, 0x2, "gdt") // the third group is uninit and entirely past the cut
	img = img[:(tStart(1)+tBPG/2)*tBS]
	f := openImg(t, img)
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	got := freeSet(t, img, runs)
	for b := range got {
		if b >= tStart(1)+tBPG/2 {
			t.Fatalf("block %d beyond the image reported free", b)
		}
	}
	if !got[tStart(1)+tBPG/2-1] {
		t.Error("last block inside the image should be free")
	}
}

// failAt is a ReaderAt that fails any read touching one block.
type failAt struct {
	r   io.ReaderAt
	blk int64
}

func (f failAt) ReadAt(p []byte, off int64) (int, error) {
	if off < (f.blk+1)*tBS && off+int64(len(p)) > f.blk*tBS {
		return 0, errors.New("injected I/O error")
	}
	return f.r.ReadAt(p, off)
}

func TestUnallocatedUnreadableBitmapIsSkipped(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, UUID: tUUID}, nil)
	_, bb1, _, _ := metaBlocks(1, true)
	f, err := ext4.Open(failAt{bytes.NewReader(img), int64(bb1)}, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	got := freeSet(t, img, runs)
	for b := tStart(1); b < tStart(2); b++ {
		if got[b] {
			t.Fatalf("block %d of the group with the unreadable bitmap reported free", b)
		}
	}
	if !got[tStart(2)+2+tITable] || !got[tStart(0)+60] {
		t.Error("blocks of the readable groups should still be free")
	}
	if !hasWarning(f.Info(), "bitmap") {
		t.Errorf("no bitmap warning: %q", f.Info().Warnings)
	}
	// Repeated calls do not repeat the warning.
	n := len(f.Info().Warnings)
	if _, err := f.Unallocated(); err != nil || len(f.Info().Warnings) != n {
		t.Errorf("second call: err %v, warnings %d -> %d", err, n, len(f.Info().Warnings))
	}
}

func TestUnallocatedBadGroupIsSkipped(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, UUID: tUUID}, nil)
	put32(img, descOff(1, 32), 0x7FFFFFFF) // group 1's block bitmap lies outside the filesystem
	f := openImg(t, img)
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	got := freeSet(t, img, runs)
	for b := tStart(1); b < tStart(2); b++ {
		if got[b] {
			t.Fatalf("block %d of the bad group reported free", b)
		}
	}
	if !got[tStart(2)+2+tITable] {
		t.Error("blocks of the good groups should still be free")
	}
	if !hasWarning(f.Info(), "unusable descriptors") {
		t.Errorf("no skipped-groups warning: %q", f.Info().Warnings)
	}
}

func TestUnallocatedBitmapChecksum(t *testing.T) {
	for _, bit64 := range []bool{false, true} {
		t.Run(fmt.Sprintf("64bit=%v", bit64), func(t *testing.T) {
			build := func() []byte {
				return ext4test.Build(ext4test.Options{Groups: 2, Extents: true, MetadataCsum: true, Bit64: bit64, UUID: tUUID}, nil)
			}
			img := build()
			f := openImg(t, img)
			if _, err := f.Unallocated(); err != nil {
				t.Fatal(err)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Fatalf("a correct bitmap checksum was reported: %q", w)
			}

			// Flip the bit of a free block without fixing the checksum.
			img = build()
			_, bb1, _, it1 := metaBlocks(1, true)
			blk := it1 + tITable + 3
			i := blk - tStart(1)
			img[bb1*tBS+i/8] |= 1 << (i % 8)
			f = openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			if !hasWarning(f.Info(), "bitmap checksum mismatch") {
				t.Errorf("no checksum warning: %q", f.Info().Warnings)
			}
			if freeSet(t, img, runs)[blk] {
				t.Error("the bitmap is used as stored: the flipped block should be allocated")
			}
		})
	}
}

// TestUnallocatedHostileDescriptors damages random descriptor and bitmap bytes:
// whatever the image says, Unallocated returns valid runs and never panics.
func TestUnallocatedHostileDescriptors(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input, not security
	for _, o := range []ext4test.Options{
		{Groups: 4, GDTCsum: true}, {Groups: 4, MetadataCsum: true, Bit64: true}, {Groups: 4},
	} {
		o.Extents, o.UUID = true, tUUID
		base := ext4test.Build(o, nil)
		ds := 32
		if o.Bit64 {
			ds = 64
		}
		for range 300 {
			img := slices.Clone(base)
			for range 1 + rng.IntN(6) {
				switch rng.IntN(3) {
				case 0:
					img[tGDT+rng.IntN(4*ds)] = byte(rng.IntN(256))
				case 1:
					img[tGDT+rng.IntN(4*ds)] ^= 1 << rng.IntN(8)
				default:
					img[(3+rng.IntN(3))*tBS+rng.IntN(tBS)] = byte(rng.IntN(256))
				}
			}
			f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				continue
			}
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatalf("Unallocated: %v", err)
			}
			freeSet(t, img, runs)
		}
	}
}
