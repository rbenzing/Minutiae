package ext4test_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

var lUUID = [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2, 3, 4, 5, 6}

const lBS = 1024

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7) + byte(i>>8)
	}
	return b
}

func openImg(t *testing.T, img []byte) *ext4.FS {
	t.Helper()
	f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func warnings(t *testing.T, img []byte) []string {
	t.Helper()
	f := openImg(t, img)
	if _, err := f.Unallocated(); err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	return f.Info().Warnings
}

func freeSet(t *testing.T, img []byte) map[int]bool {
	t.Helper()
	runs, err := openImg(t, img).Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	got := map[int]bool{}
	for _, r := range runs {
		for off := r.Offset; off < r.Offset+r.Length; off += lBS {
			got[int(off/lBS)] = true
		}
	}
	return got
}

func blocksEqual(img []byte, blocks []int, want []byte) bool {
	var got []byte
	for _, b := range blocks {
		got = append(got, img[b*lBS:(b+1)*lBS]...)
	}
	return bytes.Equal(got, want)
}

func checkDirent(t *testing.T, img []byte, loc ext4test.FileLoc, name string) {
	t.Helper()
	if loc.DirentBlock < 0 {
		t.Fatalf("%s: DirentBlock %d", loc.Path, loc.DirentBlock)
	}
	rec := img[loc.DirentBlock*lBS+loc.DirentOffset:]
	if got := binary.LittleEndian.Uint32(rec); got != loc.Inode {
		t.Errorf("%s: dirent inode %d, want %d", loc.Path, got, loc.Inode)
	}
	if n := int(rec[6]); n != len(name) || string(rec[8:8+n]) != name {
		t.Errorf("%s: dirent name %q, want %q", loc.Path, rec[8:8+int(rec[6])], name)
	}
}

func TestBuildLayoutLocatesFiles(t *testing.T) {
	small := pattern(3*lBS, 1)
	var pieces []ext4test.Piece
	var scattered []byte
	for i := range 12 {
		d := pattern(lBS, byte(40+i))
		pieces = append(pieces, ext4test.Piece{Block: i * 2, Data: d})
		scattered = append(scattered, d...)
	}
	gone := pattern(2*lBS, 90)
	o := ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID}
	files := []ext4test.File{
		{Path: "/a.txt", Data: small, Generation: 77},
		{Path: "/d", Dir: true},
		{Path: "/d/split.bin", Pieces: pieces, ExtentLeaves: 2, Scatter: true},
		{Path: "/gone.txt", Data: gone, Deleted: true},
	}
	img, l := ext4test.BuildLayout(o, files)
	if !bytes.Equal(img, ext4test.Build(o, files)) {
		t.Fatal("BuildLayout image differs from Build")
	}
	if l.BlockSize != lBS || l.BlocksPerGroup != 1024 || l.InodesPerGroup != 128 || l.InodeSize != 256 || l.FirstDataBlock != 1 {
		t.Fatalf("geometry %+v", l)
	}
	if len(l.BlockBitmap) != 1 || len(l.InodeBitmap) != 1 || len(l.InodeTable) != 1 {
		t.Fatalf("per-group slices: %v %v %v", l.BlockBitmap, l.InodeBitmap, l.InodeTable)
	}

	a := l.File("/a.txt")
	if a.Inode != ext4test.InodeNumber(0) || a.Generation != 77 {
		t.Errorf("a.txt inode %d gen %d", a.Inode, a.Generation)
	}
	if want := int64(l.InodeTable[0]*lBS + int(a.Inode-1)*l.InodeSize); a.InodeOffset != want {
		t.Errorf("a.txt InodeOffset %d, want %d", a.InodeOffset, want)
	}
	if got := binary.LittleEndian.Uint32(img[a.InodeOffset+0x64:]); got != 77 {
		t.Errorf("generation at InodeOffset: %d", got)
	}
	if len(a.Data) != 3 || !blocksEqual(img, a.Data, small) || len(a.Tree) != 0 {
		t.Errorf("a.txt data %v tree %v", a.Data, a.Tree)
	}
	checkDirent(t, img, a, "a.txt")

	sp := l.File("/d/split.bin")
	if len(sp.Data) != 12 || !blocksEqual(img, sp.Data, scattered) {
		t.Errorf("split.bin data %v", sp.Data)
	}
	if len(sp.Tree) != 2 {
		t.Fatalf("split.bin tree %v", sp.Tree)
	}
	for _, b := range sp.Tree {
		if img[b*lBS] != 0x0A || img[b*lBS+1] != 0xF3 {
			t.Errorf("tree block %d starts % x", b, img[b*lBS:b*lBS+2])
		}
	}
	checkDirent(t, img, sp, "split.bin")

	g := l.File("/gone.txt")
	if !g.Deleted || a.Deleted || len(g.Data) != 2 || !blocksEqual(img, g.Data, gone) {
		t.Errorf("gone.txt %+v", g)
	}
	checkDirent(t, img, g, "gone.txt")

	// A block-map image: the indirect block is the Tree.
	big := pattern(20*lBS, 3)
	bimg, bl := ext4test.BuildLayout(ext4test.Options{UUID: lUUID}, []ext4test.File{{Path: "/big.bin", Data: big}})
	b := bl.File("/big.bin")
	if len(b.Data) != 20 || !blocksEqual(bimg, b.Data, big) || len(b.Tree) != 1 {
		t.Fatalf("big.bin data %d tree %v", len(b.Data), b.Tree)
	}
	ind := bimg[b.Tree[0]*lBS:]
	if got := int(binary.LittleEndian.Uint32(ind)); got != b.Data[12] {
		t.Errorf("indirect block slot 0 = %d, want data block %d", got, b.Data[12])
	}
	checkDirent(t, bimg, b, "big.bin")

	// An inline parent has no dirent block; the offset is into the inline area.
	iimg, il := ext4test.BuildLayout(ext4test.Options{Extents: true, InlineData: true, UUID: lUUID},
		[]ext4test.File{{Path: "/idir", Dir: true, Inline: true}, {Path: "/idir/x", Data: []byte("hi")}})
	x, dir := il.File("/idir/x"), il.File("/idir")
	if x.DirentBlock != -1 {
		t.Fatalf("inline parent: DirentBlock %d", x.DirentBlock)
	}
	rec := iimg[int(dir.InodeOffset)+0x28+x.DirentOffset:]
	if binary.LittleEndian.Uint32(rec) != x.Inode || rec[6] != 1 || rec[8] != 'x' {
		t.Errorf("inline dirent at offset %d: % x", x.DirentOffset, rec[:12])
	}
}

func TestLayoutFilePanicsWhenAbsent(t *testing.T) {
	_, l := ext4test.BuildLayout(ext4test.Options{}, []ext4test.File{{Path: "/a", Data: []byte("x")}})
	defer func() {
		if recover() == nil {
			t.Fatal("File of an absent path did not panic")
		}
	}()
	l.File("/nope")
}

func TestSetBlockFreeKeepsChecksums(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a.txt", Data: pattern(2*lBS, 5)}})
	blk := l.File("/a.txt").Data[0]
	if before := warnings(t, img); len(before) != 0 {
		t.Fatalf("baseline warnings: %v", before)
	}
	if freeSet(t, img)[blk] {
		t.Fatalf("block %d free before the helper", blk)
	}

	l.SetBlockFree(img, blk, true)
	if w := warnings(t, img); len(w) != 0 {
		t.Errorf("warnings after SetBlockFree: %v", w)
	}
	if !freeSet(t, img)[blk] {
		t.Errorf("block %d not in Unallocated after SetBlockFree(true)", blk)
	}
	l.SetBlockFree(img, blk, false)
	if w := warnings(t, img); len(w) != 0 {
		t.Errorf("warnings after SetBlockFree(false): %v", w)
	}
	if freeSet(t, img)[blk] {
		t.Errorf("block %d still free after SetBlockFree(false)", blk)
	}

	// Without the helper the checksum catches the flip, so this test can fail.
	raw := slices.Clone(img)
	rel := blk - 1 - 0 // group 0 starts at block 1
	raw[l.BlockBitmap[0]*lBS+rel/8] ^= 1 << (rel % 8)
	if w := strings.Join(warnings(t, raw), "\n"); !strings.Contains(w, "bitmap checksum mismatch") {
		t.Errorf("raw flip not caught: %q", w)
	}
}

// inodeBitmapCsum recomputes the stored checksum fields independently of the helper.
func inodeBitmapCsum(t *testing.T, img []byte, l *ext4test.Layout, desc int) (stored, computed uint32) {
	t.Helper()
	gdt := img[(l.FirstDataBlock+1)*lBS:]
	d := gdt[:desc]
	stored = uint32(binary.LittleEndian.Uint16(d[0x1A:]))
	if desc >= 64 {
		stored |= uint32(binary.LittleEndian.Uint16(d[0x3A:])) << 16
	}
	computed = rawCRC32C(rawCRC32C(0xFFFFFFFF, lUUID[:]), img[l.InodeBitmap[0]*lBS:][:l.InodesPerGroup/8])
	if desc < 64 {
		computed &= 0xFFFF
	}
	return stored, computed
}

func TestSetInodeFreeKeepsChecksums(t *testing.T) {
	for _, bit64 := range []bool{false, true} {
		o := ext4test.Options{Extents: true, MetadataCsum: true, Bit64: bit64, UUID: lUUID}
		desc := 32
		if bit64 {
			desc = 64
		}
		img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a.txt", Data: []byte("x")}})
		ino := l.File("/a.txt").Inode
		bitAt := func() bool {
			i := int(ino - 1)
			return img[l.InodeBitmap[0]*lBS+i/8]>>(i%8)&1 == 1
		}
		if !bitAt() {
			t.Fatalf("bit64=%v: inode %d not allocated at start", bit64, ino)
		}
		if s, c := inodeBitmapCsum(t, img, l, desc); s != c {
			t.Fatalf("bit64=%v: baseline stored %#x computed %#x (test oracle wrong)", bit64, s, c)
		}
		for _, free := range []bool{true, false, true} {
			l.SetInodeFree(img, ino, free)
			if bitAt() == free {
				t.Errorf("bit64=%v: SetInodeFree(%v) left bit %v", bit64, free, bitAt())
			}
			if s, c := inodeBitmapCsum(t, img, l, desc); s != c {
				t.Errorf("bit64=%v free=%v: stored %#x computed %#x", bit64, free, s, c)
			}
			if w := warnings(t, img); len(w) != 0 {
				t.Errorf("bit64=%v free=%v: warnings %v", bit64, free, w)
			}
		}
	}
}

func inodeRaw(img []byte, loc ext4test.FileLoc, size int) []byte {
	return slices.Clone(img[loc.InodeOffset : loc.InodeOffset+int64(size)])
}

// diffBytes returns the inode offsets that differ.
func diffBytes(a, b []byte) []int {
	var out []int
	for i := range a {
		if a[i] != b[i] {
			out = append(out, i)
		}
	}
	return out
}

func TestUnlinkEffectsTouchOnlyTheirFields(t *testing.T) {
	// i_size is 3 KiB and i_blocks is set to 6 so that every flag has something to clear.
	mk := func(e ext4test.UnlinkEffects) (before, after []byte, l *ext4test.Layout, img []byte) {
		o := ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID}
		img, l = ext4test.BuildLayout(o, []ext4test.File{{Path: "/gone.txt", Data: pattern(3*lBS, 2), Deleted: true}})
		loc := l.File("/gone.txt")
		l.PatchInode(img, loc.Inode, func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[0x1C:], 6)
		})
		before = inodeRaw(img, loc, l.InodeSize)
		l.Unlink(img, "/gone.txt", e)
		return before, inodeRaw(img, loc, l.InodeSize), l, img
	}
	csumBytes := []int{0x7C, 0x7D, 0x82, 0x83}
	onlyCsum := func(t *testing.T, name string, d []int, want []int) {
		t.Helper()
		var got []int
		for _, i := range d {
			if !slices.Contains(csumBytes, i) {
				got = append(got, i)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: changed inode bytes %v, want %v (plus checksum)", name, got, want)
		}
	}

	t.Run("none", func(t *testing.T) {
		b, a, _, _ := mk(ext4test.UnlinkEffects{})
		onlyCsum(t, "none", diffBytes(b, a), nil)
	})
	t.Run("ZeroSize", func(t *testing.T) {
		b, a, _, _ := mk(ext4test.UnlinkEffects{ZeroSize: true})
		// 3072 = 0x0C00 little endian: bytes 4..7 are 00 0C 00 00, so only byte 5 changes.
		onlyCsum(t, "ZeroSize", diffBytes(b, a), []int{5})
	})
	t.Run("ZeroBlocksCount", func(t *testing.T) {
		b, a, _, _ := mk(ext4test.UnlinkEffects{ZeroBlocksCount: true})
		onlyCsum(t, "ZeroBlocksCount", diffBytes(b, a), []int{0x1C})
	})
	t.Run("ClearExtentRoot", func(t *testing.T) {
		b, a, _, _ := mk(ext4test.UnlinkEffects{ClearExtentRoot: true})
		// Header at 0x28: magic, entries (0x2A), max (0x2C, 4 already), depth (0x2E); 48 entry bytes at 0x34.
		d := diffBytes(b, a)
		want := []int{0x2A}
		for i := 0x34; i < 0x64; i++ {
			if b[i] != 0 {
				want = append(want, i)
			}
		}
		onlyCsum(t, "ClearExtentRoot", d, want)
		if a[0x28] != 0x0A || a[0x29] != 0xF3 || a[0x2A] != 0 || a[0x2B] != 0 || a[0x2C] != 4 || a[0x2E] != 0 {
			t.Errorf("header after ClearExtentRoot: % x", a[0x28:0x34])
		}
		if !bytes.Equal(a[0x34:0x64], make([]byte, 48)) {
			t.Errorf("entry bytes not zero: % x", a[0x34:0x64])
		}
	})
	t.Run("ZeroBlockPointers", func(t *testing.T) {
		b, a, _, _ := mk(ext4test.UnlinkEffects{ZeroBlockPointers: true})
		var want []int
		for i := 0x28; i < 0x64; i++ {
			if b[i] != 0 {
				want = append(want, i)
			}
		}
		onlyCsum(t, "ZeroBlockPointers", diffBytes(b, a), want)
		if !bytes.Equal(a[0x28:0x64], make([]byte, 60)) {
			t.Errorf("i_block not zero: % x", a[0x28:0x64])
		}
	})
	t.Run("FreeBlocks", func(t *testing.T) {
		_, _, l, img := mk(ext4test.UnlinkEffects{})
		loc := l.File("/gone.txt")
		for _, b := range loc.Data {
			if freeSet(t, img)[b] {
				t.Fatalf("block %d free before Unlink", b)
			}
		}
		l.Unlink(img, "/gone.txt", ext4test.UnlinkEffects{FreeBlocks: true})
		free := freeSet(t, img)
		for _, b := range slices.Concat(loc.Data, loc.Tree) {
			if !free[b] {
				t.Errorf("block %d not freed", b)
			}
		}
		if w := warnings(t, img); len(w) != 0 {
			t.Errorf("warnings: %v", w)
		}
		b2, a2, _, _ := mk(ext4test.UnlinkEffects{FreeBlocks: true})
		onlyCsum(t, "FreeBlocks inode", diffBytes(b2, a2), nil)
	})
	t.Run("FreeInode", func(t *testing.T) {
		b, a, l, img := mk(ext4test.UnlinkEffects{FreeInode: true})
		onlyCsum(t, "FreeInode inode", diffBytes(b, a), nil)
		i := int(l.File("/gone.txt").Inode - 1)
		if img[l.InodeBitmap[0]*lBS+i/8]>>(i%8)&1 != 0 {
			t.Error("inode bit still set")
		}
	})
	t.Run("DebugfsRm", func(t *testing.T) {
		if want := (ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true}); ext4test.DebugfsRm != want {
			t.Errorf("DebugfsRm = %+v", ext4test.DebugfsRm)
		}
	})
	t.Run("checksum valid after every flag", func(t *testing.T) {
		all := ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true, ZeroBlockPointers: true}
		_, _, _, img := mk(all)
		f := openImg(t, img)
		kids, err := f.ReadDir(f.Root())
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, k := range kids {
			if k.Name != "gone.txt" {
				continue
			}
			found = true
			if !k.Deleted || k.Size != 0 {
				t.Errorf("entry %+v", k)
			}
			for _, a := range k.Attrs {
				if a.Key == "checksum" {
					t.Errorf("inode checksum attr %q on the patched inode", a.Value)
				}
			}
		}
		if !found {
			t.Fatal("deleted entry not listed")
		}
	})
}

func TestUnlinkNeedsADeletedFile(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{}, []ext4test.File{{Path: "/live", Data: []byte("x")}})
	defer func() {
		if recover() == nil {
			t.Fatal("Unlink of a live file did not panic")
		}
	}()
	l.Unlink(img, "/live", ext4test.DebugfsRm)
}

func TestWriteLeafChecksum(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
	blk := l.File("/a").Data[0] + 5 // a free block past the file
	exts := []ext4test.Extent{{Lblk: 0, Len: 3, Phys: 400}, {Lblk: 10, Len: 2, Phys: 500, Uninit: true}}
	seed := rawCRC32C(0xFFFFFFFF, lUUID[:])
	tail := 12 + 84*12 // eh_max (1024-12)/12 = 84
	stored := func() uint32 { return binary.LittleEndian.Uint32(img[blk*lBS+tail:]) }
	want := func(ino, gen uint32) uint32 {
		var w [4]byte
		binary.LittleEndian.PutUint32(w[:], ino)
		c := rawCRC32C(seed, w[:])
		binary.LittleEndian.PutUint32(w[:], gen)
		c = rawCRC32C(c, w[:])
		return rawCRC32C(c, img[blk*lBS:blk*lBS+tail])
	}
	l.WriteLeaf(img, blk, 20, 5, exts, true)
	leaf := img[blk*lBS:]
	if leaf[0] != 0x0A || leaf[1] != 0xF3 || binary.LittleEndian.Uint16(leaf[2:]) != 2 || binary.LittleEndian.Uint16(leaf[4:]) != 84 || binary.LittleEndian.Uint16(leaf[6:]) != 0 {
		t.Errorf("header % x", leaf[:12])
	}
	// Entry 0: lblk 0, len 3, phys 400; entry 1: lblk 10, len 2+32768 (uninitialized), phys 500.
	e0, e1 := leaf[12:24], leaf[24:36]
	if binary.LittleEndian.Uint32(e0) != 0 || binary.LittleEndian.Uint16(e0[4:]) != 3 || binary.LittleEndian.Uint32(e0[8:]) != 400 {
		t.Errorf("entry 0 % x", e0)
	}
	if binary.LittleEndian.Uint32(e1) != 10 || binary.LittleEndian.Uint16(e1[4:]) != 2+32768 || binary.LittleEndian.Uint32(e1[8:]) != 500 {
		t.Errorf("entry 1 % x", e1)
	}
	if stored() != want(20, 5) {
		t.Errorf("good csum: stored %#x want %#x", stored(), want(20, 5))
	}
	l.WriteLeaf(img, blk, 20, 5, exts, false)
	if stored() == want(20, 5) {
		t.Error("goodCsum=false wrote the correct checksum")
	}
	if binary.LittleEndian.Uint16(img[blk*lBS+2:]) != 2 {
		t.Error("goodCsum=false changed the entries")
	}
}

func TestShareDirBlock(t *testing.T) {
	for _, extents := range []bool{true, false} {
		o := ext4test.Options{Extents: extents, UUID: lUUID}
		img, l := ext4test.BuildLayout(o, []ext4test.File{
			{Path: "/A", Dir: true},
			{Path: "/B", Dir: true},
			{Path: "/A/one", Data: []byte("1")},
			{Path: "/B/two", Data: []byte("2")},
			{Path: "/B/three", Data: []byte("3")},
		})
		names := func(p string) []string {
			f := openImg(t, img)
			e, err := f.Lookup(p)
			if err != nil {
				t.Fatal(err)
			}
			kids, err := f.ReadDir(e)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, k := range kids {
				out = append(out, k.Name)
			}
			slices.Sort(out)
			return out
		}
		if got := names("/A"); !slices.Equal(got, []string{"one"}) {
			t.Fatalf("extents=%v: baseline /A: %v", extents, got)
		}
		l.ShareDirBlock(img, "/A", "/B")
		a, b := names("/A"), names("/B")
		if !slices.Equal(a, []string{"three", "two"}) || !slices.Equal(a, b) {
			t.Errorf("extents=%v: /A %v /B %v", extents, a, b)
		}
	}
}

func TestPatchDescriptorKeepsChecksum(t *testing.T) {
	for _, o := range []ext4test.Options{
		{Extents: true, MetadataCsum: true, UUID: lUUID, Groups: 2},
		{Extents: true, GDTCsum: true, UUID: lUUID, Groups: 2},
	} {
		img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
		setFlag := func(d []byte) { binary.LittleEndian.PutUint16(d[0x12:], 0x1) } // INODE_UNINIT
		l.PatchDescriptor(img, 1, setFlag)
		if got := binary.LittleEndian.Uint16(img[(l.FirstDataBlock+1)*lBS+32+0x12:]); got != 0x1 {
			t.Errorf("%+v: group 1 flags %#x, want 1", o, got)
		}
		if w := warnings(t, img); len(w) != 0 {
			t.Errorf("%+v: warnings after PatchDescriptor: %v", o, w)
		}

		raw := slices.Clone(img)
		raw[(l.FirstDataBlock+1)*lBS+32+0x12] ^= 0x2 // group 1's descriptor is the second 32-byte one
		if w := strings.Join(openImg(t, raw).Info().Warnings, "\n"); !strings.Contains(w, "descriptor checksum mismatch") {
			t.Errorf("%+v: raw flip not caught: %q", o, w)
		}
	}
}

func TestPatchSuperKeepsChecksum(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, Journal: true, UUID: lUUID}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
	if w := openImg(t, img).Info().Warnings; len(w) != 0 {
		t.Fatalf("baseline warnings: %v", w)
	}
	l.PatchSuper(img, func(sb []byte) {
		binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) // needs_recovery
	})
	w := strings.Join(openImg(t, img).Info().Warnings, "\n")
	if !strings.Contains(w, "needs recovery") {
		t.Errorf("no needs-recovery warning: %q", w)
	}
	if strings.Contains(w, "superblock checksum") {
		t.Errorf("superblock checksum failed after PatchSuper: %q", w)
	}

	raw, _ := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
	raw[1024+0x60] |= 0x4
	if w := strings.Join(openImg(t, raw).Info().Warnings, "\n"); !strings.Contains(w, "superblock checksum mismatch") {
		t.Errorf("raw flip not caught: %q", w)
	}
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// rawCRC32C is the kernel's crc32c_le (no pre- or post-inversion), written out here
// so the helpers are checked against an oracle that shares no code with them.
func rawCRC32C(seed uint32, p []byte) uint32 { return ^crc32.Update(^seed, castagnoli, p) }

func TestUnlinkZeroSizeClearsSizeHigh(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/gone", Data: pattern(lBS, 4), Deleted: true}})
	loc := l.File("/gone")
	l.PatchInode(img, loc.Inode, func(raw []byte) { binary.LittleEndian.PutUint32(raw[0x6C:], 0x01020304) })
	l.Unlink(img, "/gone", ext4test.UnlinkEffects{ZeroSize: true})
	raw := inodeRaw(img, loc, l.InodeSize)
	if !bytes.Equal(raw[0x4:0x8], make([]byte, 4)) || !bytes.Equal(raw[0x6C:0x70], make([]byte, 4)) {
		t.Errorf("i_size_lo % x, i_size_high % x, want both zero", raw[0x4:0x8], raw[0x6C:0x70])
	}
}

func TestUnlinkFreeBlocksFreesTreeBlocks(t *testing.T) {
	var pieces []ext4test.Piece
	for i := range 3 {
		pieces = append(pieces, ext4test.Piece{Block: i * 2, Data: pattern(lBS, byte(i))})
	}
	for _, tc := range []struct {
		name  string
		o     ext4test.Options
		files []ext4test.File
	}{
		{
			"extent leaf",
			ext4test.Options{Extents: true, MetadataCsum: true, UUID: lUUID},
			[]ext4test.File{{Path: "/gone", Pieces: pieces, ExtentLeaves: 1, Scatter: true, Deleted: true}},
		},
		{
			"indirect block",
			ext4test.Options{UUID: lUUID},
			[]ext4test.File{{Path: "/gone", Data: pattern(20*lBS, 7), Deleted: true}},
		},
	} {
		img, l := ext4test.BuildLayout(tc.o, tc.files)
		loc := l.File("/gone")
		if len(loc.Tree) == 0 {
			t.Fatalf("%s: no tree blocks", tc.name)
		}
		before := freeSet(t, img)
		for _, b := range loc.Tree {
			if before[b] {
				t.Fatalf("%s: tree block %d free before Unlink", tc.name, b)
			}
		}
		l.Unlink(img, "/gone", ext4test.UnlinkEffects{FreeBlocks: true})
		after := freeSet(t, img)
		for _, b := range loc.Tree {
			if !after[b] {
				t.Errorf("%s: tree block %d not freed", tc.name, b)
			}
		}
	}
}

func TestPatchesReachBackupCopies(t *testing.T) {
	// Two groups: group 1 holds the backup superblock (block 1025) and the backup descriptor table (block 1026).
	const backupSB, backupGDT = 1025 * lBS, 1026 * lBS
	for _, o := range []ext4test.Options{
		{Extents: true, MetadataCsum: true, UUID: lUUID, Groups: 2},
		{Extents: true, GDTCsum: true, UUID: lUUID, Groups: 2},
	} {
		img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
		l.PatchDescriptor(img, 1, func(d []byte) { binary.LittleEndian.PutUint16(d[0x12:], 0x1) })
		primary := img[2*lBS+32 : 2*lBS+64]
		backup := img[backupGDT+32 : backupGDT+64]
		if binary.LittleEndian.Uint16(backup[0x12:]) != 1 {
			t.Errorf("%+v: backup descriptor lacks the edit: % x", o, backup[:0x20])
		}
		if !bytes.Equal(primary, backup) {
			t.Errorf("%+v: backup descriptor differs from the primary:\n% x\n% x", o, backup, primary)
		}
		if o.MetadataCsum {
			var num [4]byte
			binary.LittleEndian.PutUint32(num[:], 1)
			c := rawCRC32C(rawCRC32C(rawCRC32C(rawCRC32C(rawCRC32C(0xFFFFFFFF, lUUID[:]), num[:]), backup[:0x1E]), []byte{0, 0}), backup[0x20:])
			if got := binary.LittleEndian.Uint16(backup[0x1E:]); got != uint16(c) {
				t.Errorf("backup descriptor checksum %#x, want %#x", got, uint16(c))
			}
		}
	}

	o := ext4test.Options{Extents: true, MetadataCsum: true, Journal: true, UUID: lUUID, Groups: 2}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("x")}})
	l.PatchSuper(img, func(sb []byte) { sb[0x60] |= 0x4 })
	sb := img[backupSB : backupSB+1024]
	if sb[0x60]&0x4 == 0 {
		t.Error("backup superblock lacks the edit")
	}
	if got, want := binary.LittleEndian.Uint32(sb[0x3FC:]), rawCRC32C(0xFFFFFFFF, sb[:0x3FC]); got != want {
		t.Errorf("backup superblock checksum %#x, want %#x", got, want)
	}
}
