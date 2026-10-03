package ext4_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// pat returns n deterministic non-zero bytes, so a zero in the output is
// always a hole or an uninitialized extent, never data.
func pat(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(1 + (i*7+int(seed)*13+i/251)%250)
	}
	return b
}

// openNum opens inode n through an entry built from it.
func openNum(t *testing.T, f *ext4.FS, n uint32) filesys.File {
	t.Helper()
	fl, err := tryOpenNum(f, n)
	if err != nil {
		t.Fatalf("Open(inode %d): %v", n, err)
	}
	return fl
}

func tryOpenNum(f *ext4.FS, n uint32) (filesys.File, error) {
	in, err := f.Inode(n)
	if err != nil {
		return nil, err
	}
	return f.Open(ext4.ToEntry("x", nil, in))
}

// expected lays the initialized pieces out in a zeroed buffer of size bytes.
func expected(bs int, ps []ext4test.Piece, size int) []byte {
	buf := make([]byte, size)
	for _, p := range ps {
		if p.Uninit || p.Block*bs >= size {
			continue
		}
		copy(buf[p.Block*bs:], p.Data)
	}
	return buf
}

// runBytes reads the image at the runs; a hole contributes zeros.
func runBytes(t *testing.T, img []byte, runs []filesys.Run) []byte {
	t.Helper()
	var out []byte
	for _, r := range runs {
		if r.Offset < 0 {
			out = append(out, make([]byte, r.Length)...)
			continue
		}
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

// checkFile asserts everything the filesys.File contract promises about inode n
// and returns the file: the size, the whole content, ranged reads and EOF
// handling, the Runs contract, and that reading the image at the runs gives
// the same bytes.
func checkFile(t *testing.T, img []byte, f *ext4.FS, n uint32, want []byte) filesys.File {
	t.Helper()
	fl := openNum(t, f, n)
	if fl.Size() != int64(len(want)) {
		t.Fatalf("inode %d: Size = %d, want %d", n, fl.Size(), len(want))
	}
	got := make([]byte, len(want))
	if k, err := fl.ReadAt(got, 0); (err != nil && !errors.Is(err, io.EOF)) || k != len(want) {
		t.Fatalf("inode %d: ReadAt = %d, %v", n, k, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("inode %d: content differs (%d bytes)", n, len(want))
	}
	if runs := fl.Runs(); runs != nil {
		if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
			t.Fatalf("inode %d: %v (runs %v)", n, err, runs)
		}
		if rb := runBytes(t, img, runs); !bytes.Equal(rb, want) {
			t.Fatalf("inode %d: the image at the runs differs from the content", n)
		}
	}
	// Ranged reads across every kind of boundary.
	for _, off := range []int{0, 1, 511, 1023, 1024, 1025, len(want) / 2, len(want) - 1} {
		for _, l := range []int{1, 7, 1025, 5000} {
			if off < 0 || off >= len(want) {
				continue
			}
			p := make([]byte, l)
			k, err := fl.ReadAt(p, int64(off))
			end := min(off+l, len(want))
			if k != end-off || !bytes.Equal(p[:k], want[off:end]) {
				t.Fatalf("inode %d: ReadAt(%d bytes at %d) = %d bytes, differs", n, l, off, k)
			}
			if off+l > len(want) && !errors.Is(err, io.EOF) {
				t.Fatalf("inode %d: short read at the end returned %v, want io.EOF", n, err)
			}
			if off+l <= len(want) && err != nil {
				t.Fatalf("inode %d: ReadAt(%d at %d): %v", n, l, off, err)
			}
		}
	}
	if k, err := fl.ReadAt(make([]byte, 4), fl.Size()); k != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("inode %d: ReadAt at Size = %d, %v, want 0, EOF", n, k, err)
	}
	if _, err := fl.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatalf("inode %d: ReadAt at a negative offset succeeded", n)
	}
	return fl
}

func TestExtentsContiguousAndFragmented(t *testing.T) {
	const bs = 1024
	for _, csum := range []bool{false, true} {
		a := pat(5*bs+500, 1)
		frag := []ext4test.Piece{{Block: 0, Data: pat(2*bs, 2)}, {Block: 2, Data: pat(bs, 3)}, {Block: 3, Data: pat(500, 4)}}
		files := []ext4test.File{
			{Path: "/a", Data: a},
			{Path: "/b", Pieces: frag, Scatter: true},
			{Path: "/c", Pieces: frag}, // physically adjacent: one run
		}
		img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum, BlockSize: bs}, files)
		f := mustOpen(t, img)

		ra := checkFile(t, img, f, ext4test.InodeNumber(0), a).Runs()
		if len(ra) != 1 || ra[0].Length != int64(len(a)) || ra[0].Offset%bs != 0 {
			t.Errorf("csum=%v: contiguous file runs = %v, want one block-aligned run of %d bytes", csum, ra, len(a))
		}
		wantFrag := expected(bs, frag, 3*bs+500)
		rb := checkFile(t, img, f, ext4test.InodeNumber(1), wantFrag).Runs()
		if len(rb) != 3 || rb[0].Length != 2*bs || rb[1].Length != bs || rb[2].Length != 500 {
			t.Errorf("csum=%v: fragmented file runs = %v, want 3 runs of 2048, 1024 and 500 bytes", csum, rb)
		}
		for i := 1; i < len(rb); i++ {
			if rb[i].Offset == rb[i-1].Offset+rb[i-1].Length {
				t.Errorf("csum=%v: runs %d and %d are adjacent: %v", csum, i-1, i, rb)
			}
		}
		rc := checkFile(t, img, f, ext4test.InodeNumber(2), wantFrag).Runs()
		if len(rc) != 1 || rc[0].Length != 3*bs+500 {
			t.Errorf("csum=%v: adjacent pieces runs = %v, want one merged run trimmed to 3572 bytes", csum, rc)
		}
	}
}

func TestExtentTreeDepth1(t *testing.T) {
	const bs = 1024
	var ps []ext4test.Piece
	for i := range 7 {
		d := pat(bs, byte(i))
		if i == 6 {
			d = d[:300]
		}
		ps = append(ps, ext4test.Piece{Block: i * 2, Data: d}) // a hole block between pieces
	}
	size := 12*bs + 300
	for _, csum := range []bool{false, true} {
		for leaves := 1; leaves <= 4; leaves++ {
			files := []ext4test.File{{Path: "/t", Pieces: ps, Scatter: true, ExtentLeaves: leaves}}
			img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum, BlockSize: bs}, files)
			f := mustOpen(t, img)
			n := ext4test.InodeNumber(0)
			ib := inodeOff(t, f, bs, 256, 128, int(n)) + 0x28
			if hdr := img[ib : ib+8]; hdr[0] != 0x0A || hdr[1] != 0xF3 || hdr[2] != byte(leaves) || hdr[6] != 1 {
				t.Fatalf("leaves=%d: i_block header % x is not a depth-1 index with %d entries", leaves, hdr, leaves)
			}
			runs := checkFile(t, img, f, n, expected(bs, ps, size)).Runs()
			// 7 data runs and 6 holes between them.
			if len(runs) != 13 {
				t.Errorf("csum=%v leaves=%d: %d runs, want 13: %v", csum, leaves, len(runs), runs)
			}
		}
	}
}

func TestBlockMapDirectIndirectDouble(t *testing.T) {
	for _, bs := range []int{1024, 2048, 4096} {
		ppb := bs / 4
		n := 12 + ppb + 40 // reaches into the double-indirect range
		whole := pat(n*bs+17, 5)
		ps := []ext4test.Piece{
			{Block: 0, Data: pat(12*bs, 1)},                       // direct
			{Block: 12, Data: pat(ppb*bs, 2)},                     // single indirect
			{Block: 12 + ppb, Data: pat(40*bs+17, 3)},             // double indirect
			{Block: 12 + ppb + 100, Data: pat(5*bs, 4)},           // double indirect, after a hole
			{Block: 12 + ppb + ppb + 7, Data: pat(bs/2+3, 6)},     // second ind block of the double range
			{Block: 12 + ppb + 2*ppb + 9, Data: pat(2*bs, 7)},     // third
			{Block: 12 + ppb + 2*ppb + 9 + 2, Data: pat(bs-1, 8)}, // adjacent logical piece
		}
		files := []ext4test.File{{Path: "/whole", Data: whole}, {Path: "/pieces", Pieces: ps, Scatter: true}}
		img := ext4test.Build(ext4test.Options{BlockSize: bs, BlocksPerGroup: 4096}, files)
		f := mustOpen(t, img)
		in, err := f.Inode(ext4test.InodeNumber(0))
		if err != nil || in.Fields().Flags&0x80000 != 0 {
			t.Fatalf("bs=%d: block-mapped file has the extents flag (%v)", bs, err)
		}
		rw := checkFile(t, img, f, ext4test.InodeNumber(0), whole).Runs()
		if len(rw) != 1 || rw[0].Length != int64(len(whole)) {
			t.Errorf("bs=%d: whole file runs = %v, want one run", bs, rw)
		}
		size := ps[len(ps)-1].Block*bs + len(ps[len(ps)-1].Data)
		rp := checkFile(t, img, f, ext4test.InodeNumber(1), expected(bs, ps, size)).Runs()
		holes := 0
		for _, r := range rp {
			if r.Offset < 0 {
				holes++
			}
		}
		if holes < 3 || len(rp) < 8 {
			t.Errorf("bs=%d: scattered file has %d runs (%d holes): %v", bs, len(rp), holes, rp)
		}
	}
}

func TestBlockMapTripleIndirect(t *testing.T) {
	const bs, ppb = 1024, 256
	first := 12 + ppb + ppb*ppb // first block of the triple-indirect range
	tail := pat(bs+5, 9)
	ps := []ext4test.Piece{{Block: 0, Data: pat(bs, 1)}, {Block: first + 3, Data: tail}}
	img := ext4test.Build(ext4test.Options{BlockSize: bs}, []ext4test.File{{Path: "/big", Pieces: ps}})
	f := mustOpen(t, img)
	fl := openNum(t, f, ext4test.InodeNumber(0))
	size := int64(first+3)*bs + int64(len(tail))
	if fl.Size() != size {
		t.Fatalf("Size = %d, want %d", fl.Size(), size)
	}
	runs := fl.Runs()
	if err := filesys.CheckRuns(runs, size, f.Info().Size); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].Length != bs || runs[1] != (filesys.Run{Offset: -1, Length: int64(first+3-1) * bs}) || runs[2].Length != int64(len(tail)) {
		t.Fatalf("runs = %v", runs)
	}
	got := make([]byte, len(tail))
	if _, err := fl.ReadAt(got, int64(first+3)*bs); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tail) {
		t.Error("triple-indirect data differs")
	}
	zeros := make([]byte, 3*bs)
	if _, err := fl.ReadAt(zeros, int64(first-1)*bs); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(zeros, make([]byte, 3*bs)) {
		t.Error("the hole before the triple-indirect data is not zero")
	}
}

func TestSparseFileHole(t *testing.T) {
	const bs = 1024
	for _, extents := range []bool{true, false} {
		ps := []ext4test.Piece{{Block: 0, Data: pat(bs, 1)}, {Block: 3, Data: pat(bs, 2)}}
		size := 6*bs + 100 // two blocks of hole, then 1124 bytes of tail hole
		files := []ext4test.File{
			{Path: "/sparse", Pieces: ps, Size: int64(size)},
			{Path: "/allhole", Size: 10 * bs},
			{Path: "/empty"},
			{Path: "/leading", Pieces: []ext4test.Piece{{Block: 2, Data: pat(10, 3)}}},
		}
		img := ext4test.Build(ext4test.Options{Extents: extents, BlockSize: bs}, files)
		f := mustOpen(t, img)

		runs := checkFile(t, img, f, ext4test.InodeNumber(0), expected(bs, ps, size)).Runs()
		want := []filesys.Run{{Offset: runs[0].Offset, Length: bs}, {Offset: -1, Length: 2 * bs}, {Offset: runs[2].Offset, Length: bs}, {Offset: -1, Length: 2*bs + 100}}
		if len(runs) != 4 || runs[0].Offset < 0 || runs[2].Offset < 0 || runs[1] != want[1] || runs[3] != want[3] || runs[0].Length != bs || runs[2].Length != bs {
			t.Errorf("extents=%v: sparse runs = %v, want data, 2-block hole, data, tail hole", extents, runs)
		}
		if r := checkFile(t, img, f, ext4test.InodeNumber(1), make([]byte, 10*bs)).Runs(); len(r) != 1 || r[0] != (filesys.Run{Offset: -1, Length: 10 * bs}) {
			t.Errorf("extents=%v: all-hole runs = %v", extents, r)
		}
		if r := checkFile(t, img, f, ext4test.InodeNumber(2), nil).Runs(); r != nil {
			t.Errorf("extents=%v: empty file runs = %v, want nil", extents, r)
		}
		lead := expected(bs, []ext4test.Piece{{Block: 2, Data: pat(10, 3)}}, 2*bs+10)
		if r := checkFile(t, img, f, ext4test.InodeNumber(3), lead).Runs(); len(r) != 2 || r[0] != (filesys.Run{Offset: -1, Length: 2 * bs}) || r[1].Length != 10 {
			t.Errorf("extents=%v: leading-hole runs = %v", extents, r)
		}
	}
}

func TestUninitializedExtentReadsZeros(t *testing.T) {
	const bs = 1024
	garbage := pat(2*bs, 1)
	ps := []ext4test.Piece{{Block: 0, Data: garbage, Uninit: true}, {Block: 2, Data: pat(bs, 2)}, {Block: 3, Data: pat(bs+10, 3), Uninit: true}}
	img := ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs}, []ext4test.File{{Path: "/prealloc", Pieces: ps}})
	if !bytes.Contains(img, garbage) {
		t.Fatal("the builder did not write the preallocated blocks, so the test would prove nothing")
	}
	f := mustOpen(t, img)
	runs := checkFile(t, img, f, ext4test.InodeNumber(0), expected(bs, ps, 4*bs+10)).Runs()
	if len(runs) != 3 || runs[0] != (filesys.Run{Offset: -1, Length: 2 * bs}) || runs[1].Offset < 0 || runs[1].Length != bs || runs[2] != (filesys.Run{Offset: -1, Length: bs + 10}) {
		t.Errorf("runs = %v, want hole, data, hole", runs)
	}
}

func TestInlineDataFile(t *testing.T) {
	small, exact, over, bigger := pat(20, 1), pat(60, 2), pat(61, 3), pat(200, 4)
	spill := pat(3000, 5)
	files := []ext4test.File{
		{Path: "/small", Data: small, Inline: true},
		{Path: "/exact", Data: exact, Inline: true},
		{Path: "/over", Data: over, Inline: true},
		{Path: "/bigger", Data: bigger, Inline: true},
		{Path: "/empty", Inline: true},
		{Path: "/spill", Data: spill, Inline: true, XattrBlock: true},
		{Path: "/dir/x", Data: pat(10, 6)}, // a normal file next to them
	}
	img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true, InodeSize: 512, BlockSize: 4096}, files)
	f := mustOpen(t, img)
	for i, want := range [][]byte{small, exact, over, bigger, nil, spill} {
		fl := checkFile(t, img, f, ext4test.InodeNumber(i), want)
		if fl.Runs() != nil {
			t.Errorf("%s: inline content has runs %v", files[i].Path, fl.Runs())
		}
	}
	if fl := checkFile(t, img, f, ext4test.InodeNumber(6), pat(10, 6)); fl.Runs() == nil {
		t.Error("a normal file on an inline-data filesystem has no runs")
	}

	// A 128-byte inode has no room for an attribute: up to 60 bytes still work.
	img = ext4test.Build(ext4test.Options{Extents: true, InlineData: true, InodeSize: 128}, []ext4test.File{{Path: "/s", Data: exact, Inline: true}})
	checkFile(t, img, mustOpen(t, img), ext4test.InodeNumber(0), exact)
}

func TestInlineDataHostile(t *testing.T) {
	files := []ext4test.File{
		{Path: "/small", Data: pat(20, 1), Inline: true},
		{Path: "/bigger", Data: pat(200, 2), Inline: true},
	}
	for name, tc := range map[string]struct {
		inode int
		size  uint32
	}{
		"size beyond what the xattr stores": {0, 100},
		"size beyond the inline limit":      {1, 60 + 4096 + 1},
		"huge size":                         {1, 1 << 30},
	} {
		img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true, InodeSize: 512}, files)
		f := mustOpen(t, img)
		put32(img, inodeOff(t, f, 1024, 512, 128, int(ext4test.InodeNumber(tc.inode)))+4, tc.size)
		f = mustOpen(t, img)
		if _, err := tryOpenNum(f, ext4test.InodeNumber(tc.inode)); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v, want a CorruptError", name, err)
		}
	}
	// The system.data attribute removed entirely: the inode is corrupt, not empty.
	img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true, InodeSize: 512}, files)
	f := mustOpen(t, img)
	in := inodeOff(t, f, 1024, 512, 128, int(ext4test.InodeNumber(1)))
	put32(img, in+128+32, 0) // the in-inode xattr area starts after i_extra_isize (32); clear its magic
	f = mustOpen(t, img)
	if _, err := tryOpenNum(f, ext4test.InodeNumber(1)); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("missing system.data: err = %v, want a CorruptError", err)
	}
}

func TestFastAndSlowSymlink(t *testing.T) {
	short := "/short/target"
	fast59 := "/" + strings.Repeat("a", 58)
	slow60 := "/" + strings.Repeat("b", 59)
	long := "/" + strings.Repeat("c", 199)
	for _, extents := range []bool{true, false} {
		files := []ext4test.File{
			{Path: "/l1", Symlink: short},
			{Path: "/l2", Symlink: fast59},
			{Path: "/l3", Symlink: slow60},
			{Path: "/l4", Symlink: long},
		}
		img := ext4test.Build(ext4test.Options{Extents: extents}, files)
		f := mustOpen(t, img)
		for i, target := range []string{short, fast59, slow60, long} {
			n := ext4test.InodeNumber(i)
			in, err := f.Inode(n)
			if err != nil {
				t.Fatal(err)
			}
			if e := ext4.ToEntry("l", nil, in); e.Type != filesys.TypeSymlink || e.Size != int64(len(target)) {
				t.Fatalf("extents=%v %q: entry %+v", extents, target, e)
			}
			fl := checkFile(t, img, f, n, []byte(target))
			if fast := len(target) < 60; fast != (fl.Runs() == nil) {
				t.Errorf("extents=%v %d-byte target: runs = %v (fast symlinks have none, slow ones have blocks)", extents, len(target), fl.Runs())
			}
		}
	}
}

// Reading the image at the runs must reproduce the content for a spread of
// random layouts, in both mapping styles.
func TestRunsReproduceContent(t *testing.T) {
	const bs = 1024
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	for iter := range 40 {
		extents := iter%2 == 0
		var files []ext4test.File
		type want struct {
			ps   []ext4test.Piece
			size int
		}
		var wants []want
		for fi := range 3 {
			var ps []ext4test.Piece
			block := rng.IntN(3)
			for range 1 + rng.IntN(4) { // at most 4 extents fit in i_block
				nb := 1 + rng.IntN(3)
				ln := nb*bs - rng.IntN(bs)
				ps = append(ps, ext4test.Piece{Block: block, Data: pat(ln, byte(rng.IntN(250))), Uninit: extents && rng.IntN(4) == 0})
				block += nb + rng.IntN(3)
			}
			last := ps[len(ps)-1]
			size := last.Block*bs + len(last.Data)
			if rng.IntN(2) == 0 {
				size += rng.IntN(3 * bs) // sparse tail
			}
			files = append(files, ext4test.File{Path: fmt.Sprintf("/f%d", fi), Pieces: ps, Size: int64(size), Scatter: rng.IntN(2) == 0})
			wants = append(wants, want{ps, size})
		}
		img := ext4test.Build(ext4test.Options{Extents: extents, BlockSize: bs, MetadataCsum: iter%3 == 0}, files)
		f := mustOpen(t, img)
		for fi, w := range wants {
			checkFile(t, img, f, ext4test.InodeNumber(fi), expected(bs, w.ps, w.size))
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a", Data: pat(100, 1)},
		{Path: "/d", Dir: true},
		{Path: "/gone", Data: pat(100, 2), Deleted: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)

	// A deleted entry (its ID is the record's location), also with a forged
	// Deleted=false: deleted-ness comes from the ID alone.
	gone := byName(t, readDir(t, f, "/"), "gone")
	if !gone.Deleted || !strings.HasPrefix(gone.ID, "dirent:") {
		t.Fatalf("gone = %+v, want a deleted entry with a dirent: ID", gone)
	}
	if _, err := f.Open(gone); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("deleted entry: err = %v, want ErrDeleted", err)
	}
	gone.Deleted = false
	if _, err := f.Open(gone); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("deleted entry with forged Deleted=false: err = %v, want ErrDeleted", err)
	}
	// Directories, by entry type and by the inode's own mode.
	din, _ := f.Inode(ext4test.InodeNumber(1))
	de := ext4.ToEntry("d", nil, din)
	if _, err := f.Open(de); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("directory: err = %v, want ErrUnsupported", err)
	}
	de.Type = filesys.TypeFile // a stale or forged entry type must not be believed
	if _, err := f.Open(de); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("directory entry typed as a file: err = %v, want ErrUnsupported", err)
	}
	// A device node keeps its numbers in i_block: never read them as data.
	put16(img, inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0))), 0x2000|0o644)
	f = mustOpen(t, img)
	if _, err := tryOpenNum(f, ext4test.InodeNumber(0)); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("char device: err = %v, want ErrUnsupported", err)
	}

	// Entry ids are parsed strictly.
	for _, id := range []string{"", "inode:", "inode:0", "inode:+11", "inode:011", "inode:-1", "inode:1e1", "inode: 11", "inode:11 ", "Inode:11", "nid:11", "inode:99999999999", "id:inode:11"} {
		if _, err := f.Open(filesys.Entry{ID: id, Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("id %q: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := f.Open(filesys.Entry{ID: "inode:4000000", Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("out-of-range inode: err = %v, want a CorruptError", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "inode:11", Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("char device through a plain id: err = %v, want ErrUnsupported", err)
	}
}

func TestEncryptedContentIsReturnedRaw(t *testing.T) {
	cipher := pat(3000, 9)
	img := ext4test.Build(ext4test.Options{Extents: true, Encrypt: true}, []ext4test.File{{Path: "/enc/f", Data: cipher}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	put32(img, off+0x20, le32(img, off+0x20)|0x800)
	f = mustOpen(t, img)
	in, _ := f.Inode(ext4test.InodeNumber(0))
	if e := ext4.ToEntry("f", nil, in); !e.Encrypted {
		t.Fatal("the entry is not marked encrypted")
	}
	checkFile(t, img, f, ext4test.InodeNumber(0), cipher)
}

// hostile is an image with, in inode order: a 4-block extent file with a
// size of 64 blocks (so a walk does not stop early), a depth-1 file with two
// leaves, a 2-leaf-free spare file whose blocks tests may overwrite, and an
// ext2-style file with an indirect and a double-indirect block.
type hostile struct {
	img         []byte
	f           *ext4.FS
	ib0, ib1    int // i_block offsets of files 0 and 1
	ino0        int // inode offset of file 0
	leaf0       int // the first leaf block of file 1
	spare       int // first block of the spare file (8 blocks)
	blocksCount int // blocks in the filesystem
	bs          int
}

func newHostile(t *testing.T) *hostile {
	t.Helper()
	const bs = 1024
	files := []ext4test.File{
		{Path: "/t", Data: pat(4*bs, 1)},
		{Path: "/two", Pieces: []ext4test.Piece{{Block: 0, Data: pat(bs, 2)}, {Block: 2, Data: pat(bs, 3)}, {Block: 4, Data: pat(bs, 4)}, {Block: 6, Data: pat(bs, 5)}}, Scatter: true, ExtentLeaves: 2},
		{Path: "/spare", Data: pat(8*bs, 6)},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs}, files)
	f := mustOpen(t, img)
	h := &hostile{img: img, f: f, bs: bs}
	h.ino0 = inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(0)))
	h.ib0 = h.ino0 + 0x28
	h.ib1 = inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(1))) + 0x28
	h.leaf0 = int(le32(img, h.ib1+12+4))
	spare := openNum(t, f, ext4test.InodeNumber(2)).Runs()
	h.spare = int(spare[0].Offset / bs)
	h.blocksCount = int(f.Info().Size / bs)
	put32(img, h.ino0+4, 64*bs) // the walk must not stop after the first extent
	return h
}

func (h *hostile) blk(n int) int { return n * h.bs }

func TestExtentHostile(t *testing.T) {
	cases := map[string]func(h *hostile){
		"depth 6":                func(h *hostile) { put16(h.img, h.ib0+6, 6) },
		"entries over max":       func(h *hostile) { put16(h.img, h.ib0+2, 5) },
		"max over capacity":      func(h *hostile) { put16(h.img, h.ib0+4, 5) },
		"bad root magic":         func(h *hostile) { put16(h.img, h.ib0, 0) },
		"zero length extent":     func(h *hostile) { put16(h.img, h.ib0+12+4, 0) },
		"extent beyond fs":       func(h *hostile) { put32(h.img, h.ib0+12+8, uint32(h.blocksCount)+100) },
		"extent runs off fs end": func(h *hostile) { put32(h.img, h.ib0+12+8, uint32(h.blocksCount)-2) },
		"extent far beyond fs":   func(h *hostile) { put32(h.img, h.ib0+12+8, 1<<30); put16(h.img, h.ib0+12+6, 0x7FFF) },
		"logical range beyond 32 bits": func(h *hostile) {
			put32(h.img, h.ib0+12, 0xFFFFFFFF)
		},
		"overlapping extents": func(h *hostile) {
			put16(h.img, h.ib0+2, 2)
			put32(h.img, h.ib0+24, 2) // second extent starts inside the first
			put16(h.img, h.ib0+24+4, 2)
			put32(h.img, h.ib0+24+8, le32(h.img, h.ib0+12+8)+4)
		},
		"extents out of order": func(h *hostile) {
			put16(h.img, h.ib0+2, 2)
			put32(h.img, h.ib0+12, 10) // first extent now starts after the second
			put32(h.img, h.ib0+24, 5)
			put16(h.img, h.ib0+24+4, 1)
			put32(h.img, h.ib0+24+8, le32(h.img, h.ib0+12+8))
		},
		"index beyond fs": func(h *hostile) { put32(h.img, h.ib1+12+4, 1<<30) },
		"index to block 0": func(h *hostile) {
			put32(h.img, h.ib1+12+4, 0)
		},
		"child bad magic": func(h *hostile) { put16(h.img, h.blk(h.leaf0), 0) },
		"child depth wrong": func(h *hostile) {
			put16(h.img, h.blk(h.leaf0)+6, 3)
		},
		"child entries over max": func(h *hostile) { put16(h.img, h.blk(h.leaf0)+2, 85) },
		"index cycle (entry points at its own block)": func(h *hostile) {
			x := h.blk(h.leaf0)
			put16(h.img, x+6, 1)                  // the node claims to be an index
			put32(h.img, x+12, 0)                 // ei_block
			put32(h.img, x+12+4, uint32(h.leaf0)) // -> itself
			put16(h.img, x+12+8, 0)
		},
		"index entries not ascending": func(h *hostile) {
			put32(h.img, h.ib1+12+12, le32(h.img, h.ib1+12)) // second index starts where the first does
		},
		"child leaf extent beyond fs": func(h *hostile) { put32(h.img, h.blk(h.leaf0)+12+8, 1<<30) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHostile(t)
			mutate(h)
			f := mustOpen(t, h.img)
			start := time.Now()
			e0, e1 := tryOpenNum2(f, ext4test.InodeNumber(0)), tryOpenNum2(f, ext4test.InodeNumber(1))
			if !errors.Is(e0, filesys.ErrCorrupt) && !errors.Is(e1, filesys.ErrCorrupt) {
				t.Errorf("neither file reported corruption: %v / %v", e0, e1)
			}
			t.Logf("file 0: %v", e0)
			t.Logf("file 1: %v", e1)
			for _, err := range []error{e0, e1} {
				var ce *filesys.CorruptError
				if err != nil && !errors.As(err, &ce) {
					t.Errorf("error %v is not a CorruptError", err)
				}
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

// tryOpenNum2 returns only the error of opening inode n.
func tryOpenNum2(f *ext4.FS, n uint32) error {
	_, err := tryOpenNum(f, n)
	return err
}

// Many index entries that all point at one leaf: each node is read at most once,
// so the second reference is corruption and the walk ends at once.
func TestExtentHostileSharedNodes(t *testing.T) {
	t.Run("shared leaf under one index", func(t *testing.T) {
		h := newHostile(t)
		x, leaf := h.spare, h.leaf0
		// root: depth 2, one index entry -> x. x: depth 1, 84 entries -> leaf.
		put16(h.img, h.ib0+2, 1)
		put16(h.img, h.ib0+6, 2)
		put32(h.img, h.ib0+12, 0)
		put32(h.img, h.ib0+12+4, uint32(x))
		put16(h.img, h.ib0+12+8, 0)
		xb := h.blk(x)
		clear(h.img[xb : xb+h.bs])
		put16(h.img, xb, 0xF30A)
		put16(h.img, xb+2, 84)
		put16(h.img, xb+4, 84)
		put16(h.img, xb+6, 1)
		for i := range 84 {
			put32(h.img, xb+12+12*i, uint32(i)) // ascending, so only the sharing is wrong
			put32(h.img, xb+12+12*i+4, uint32(leaf))
		}
		f := mustOpen(t, h.img)
		start := time.Now()
		_, err := tryOpenNum(f, ext4test.InodeNumber(0))
		if !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("err = %v, want a CorruptError about a node referenced twice", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("took %v", d)
		}
	})
	t.Run("root entries sharing a leaf", func(t *testing.T) {
		h := newHostile(t)
		put16(h.img, h.ib0+2, 4)
		put16(h.img, h.ib0+6, 1)
		for i := range 4 {
			put32(h.img, h.ib0+12+12*i, uint32(i))
			put32(h.img, h.ib0+12+12*i+4, uint32(h.leaf0))
			put16(h.img, h.ib0+12+12*i+8, 0)
		}
		f := mustOpen(t, h.img)
		if _, err := tryOpenNum(f, ext4test.InodeNumber(0)); !errors.Is(err, filesys.ErrCorrupt) {
			t.Fatalf("err = %v, want a CorruptError", err)
		}
	})
}

// bigTree rewrites file 0 as a depth-3 extent tree of e one-block extents
// separated by holes, with alternating physical blocks so no two runs merge.
// Nodes are written into the blocks of the spare file.
func bigTree(t *testing.T, e int) (*ext4.FS, uint32) {
	t.Helper()
	const bs, perNode = 4096, (4096 - 12) / 12
	files := []ext4test.File{
		{Path: "/t", Data: pat(bs, 1)},
		{Path: "/spare", Data: pat(1700*bs, 2)},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs, BlocksPerGroup: 4096}, files)
	f := mustOpen(t, img)
	n := ext4test.InodeNumber(0)
	ino := inodeOff(t, f, bs, 256, 128, int(n))
	next := int(openNum(t, f, ext4test.InodeNumber(1)).Runs()[0].Offset / bs)
	node := func(depth, entries int) int {
		blk := next
		next++
		b := img[blk*bs : (blk+1)*bs]
		clear(b)
		put16(b, 0, 0xF30A)
		put16(b, 2, uint16(entries))
		put16(b, 4, perNode)
		put16(b, 6, uint16(depth))
		return blk
	}
	nLeaves := (e + perNode - 1) / perNode
	// Level by level, bottom up: leaves, then index nodes until one is left.
	type ref struct{ blk, first int } // a node and the first logical block it covers
	var level []ref
	for i := range nLeaves {
		cnt := min(perNode, e-i*perNode)
		blk := node(0, cnt)
		b := img[blk*bs:]
		for j := range cnt {
			k := i*perNode + j
			put32(b, 12+12*j, uint32(2*k))
			put16(b, 12+12*j+4, 1)
			put32(b, 12+12*j+8, uint32(100+(k%2)*100))
		}
		level = append(level, ref{blk, 2 * i * perNode})
	}
	for depth := 1; len(level) > 4 || depth == 1; depth++ {
		var up []ref
		for i := 0; i < len(level); i += perNode {
			chunk := level[i:min(i+perNode, len(level))]
			blk := node(depth, len(chunk))
			b := img[blk*bs:]
			for j, c := range chunk {
				put32(b, 12+12*j, uint32(c.first))
				put32(b, 12+12*j+4, uint32(c.blk))
			}
			up = append(up, ref{blk, chunk[0].first})
		}
		level = up
	}
	depth := 0
	if len(level) > 0 {
		depth = int(le16(img[level[0].blk*bs:], 6)) + 1
	}
	ib := ino + 0x28
	clear(img[ib : ib+60])
	put16(img, ib, 0xF30A)
	put16(img, ib+2, uint16(len(level)))
	put16(img, ib+4, 4)
	put16(img, ib+6, uint16(depth))
	for j, c := range level {
		put32(img, ib+12+12*j, uint32(c.first))
		put32(img, ib+12+12*j+4, uint32(c.blk))
	}
	size := uint64(2*e) * bs
	put32(img, ino+4, uint32(size))
	put32(img, ino+0x6C, uint32(size>>32))
	return mustOpen(t, img), n
}

func le16(b []byte, off int) uint16 { return uint16(b[off]) | uint16(b[off+1])<<8 }

func TestExtentRunCap(t *testing.T) {
	// A big but legitimate tree: every extent is a data run followed by a hole.
	const small = 2000
	f, n := bigTree(t, small)
	fl := openNum(t, f, n)
	runs := fl.Runs()
	if len(runs) != 2*small {
		t.Fatalf("%d runs, want %d", len(runs), 2*small)
	}
	if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
		t.Fatal(err)
	}

	// Past 1<<20 runs the file is refused, quickly.
	f, n = bigTree(t, 530000)
	start := time.Now()
	_, err := tryOpenNum(f, n)
	if !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "runs") {
		t.Fatalf("err = %v, want a CorruptError about the run count", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}

// bmImage locates the pieces of a block-mapped hostile image.
type bmImage struct {
	img        []byte
	ib0, ib1   int // i_block offsets of file a (direct + indirect) and file b (double indirect)
	ind        int // file a's indirect block
	dind, ind2 int // file b's double-indirect block and its first indirect block
}

func TestBlockMapHostile(t *testing.T) {
	const bs = 1024
	ppb := bs / 4
	mk := func(t *testing.T) *bmImage {
		files := []ext4test.File{
			{Path: "/a", Pieces: []ext4test.Piece{{Block: 0, Data: pat(12*bs, 1)}, {Block: 12, Data: pat(3*bs, 2)}}},
			{Path: "/b", Pieces: []ext4test.Piece{{Block: 12 + ppb, Data: pat(5*bs, 3)}, {Block: 12 + ppb + ppb, Data: pat(bs, 4)}}},
		}
		img := ext4test.Build(ext4test.Options{BlockSize: bs}, files)
		f := mustOpen(t, img)
		m := &bmImage{img: img}
		m.ib0 = inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(0))) + 0x28
		m.ib1 = inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(1))) + 0x28
		m.ind = int(le32(img, m.ib0+12*4))
		m.dind = int(le32(img, m.ib1+13*4))
		m.ind2 = int(le32(img, m.dind*bs))
		if m.ind == 0 || m.dind == 0 || m.ind2 == 0 {
			t.Fatal("builder did not create the indirect blocks")
		}
		// File a claims 40 blocks so its walk continues past the real data; file b
		// reaches into the triple-indirect range, so its walk visits slot 14.
		put32(img, m.ib0-0x28+4, 40*bs)
		put32(img, m.ib1-0x28+4, uint32((12+ppb+ppb*ppb+100)*bs))
		return m
	}
	cases := map[string]func(m *bmImage){
		"direct pointer beyond fs":     func(m *bmImage) { put32(m.img, m.ib0, 1<<30) },
		"indirect pointer beyond fs":   func(m *bmImage) { put32(m.img, m.ib0+12*4, 1<<30) },
		"indirect pointer at fs end":   func(m *bmImage) { put32(m.img, m.ib0+12*4, uint32(len(m.img)/bs)) },
		"data pointer beyond fs":       func(m *bmImage) { put32(m.img, m.ind*bs, 1<<30) },
		"double pointer beyond fs":     func(m *bmImage) { put32(m.img, m.ib1+13*4, 0xFFFFFFFF) },
		"entry of double beyond fs":    func(m *bmImage) { put32(m.img, m.dind*bs, 1<<30) },
		"triple pointer beyond fs":     func(m *bmImage) { put32(m.img, m.ib1+14*4, 1<<31) },
		"double lists one block twice": func(m *bmImage) { put32(m.img, m.dind*bs+4, uint32(m.ind2)) },
		"triple points at the double":  func(m *bmImage) { put32(m.img, m.ib1+14*4, uint32(m.dind)) },
		"triple cycles on itself": func(m *bmImage) {
			put32(m.img, m.ib1+14*4, uint32(m.ind2))
			put32(m.img, m.ind2*bs, uint32(m.ind2))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := mk(t)
			mutate(m)
			f := mustOpen(t, m.img)
			start := time.Now()
			e0, e1 := tryOpenNum2(f, ext4test.InodeNumber(0)), tryOpenNum2(f, ext4test.InodeNumber(1))
			t.Logf("file a: %v", e0)
			t.Logf("file b: %v", e1)
			if !errors.Is(e0, filesys.ErrCorrupt) && !errors.Is(e1, filesys.ErrCorrupt) {
				t.Errorf("neither file reported corruption: %v / %v", e0, e1)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

func TestDataPointerIntoMetadataWarns(t *testing.T) {
	const bs = 1024
	build := func(extents bool) ([]byte, *ext4.FS, int) {
		img := ext4test.Build(ext4test.Options{Extents: extents, BlockSize: bs}, []ext4test.File{{Path: "/f", Data: pat(bs, 1)}})
		f := mustOpen(t, img)
		return img, f, inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(0)))
	}
	const msg = "file data pointer into filesystem metadata (inode 11)"

	// A clean file warns about nothing.
	for _, ext := range []bool{true, false} {
		img, f, _ := build(ext)
		checkFile(t, img, f, ext4test.InodeNumber(0), pat(bs, 1))
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("extents=%v: warnings on a clean image: %v", ext, w)
		}
	}

	_, f0, _ := build(true)
	_, bitmap, itable, _, _ := f0.Group(0)
	for name, target := range map[string]uint32{
		"block 0 (boot block)":    0,
		"superblock":              1,
		"descriptor table":        2,
		"inode bitmap":            uint32(bitmap),
		"inode table, first":      uint32(itable),
		"inode table, last block": uint32(itable) + 31, // 128 inodes x 256 bytes = 32 blocks
	} {
		for _, ext := range []bool{true, false} {
			img, _, ino := build(ext)
			if ext {
				put32(img, ino+0x28+12+8, target) // ee_start_lo
				put16(img, ino+0x28+12+6, 0)      // ee_start_hi
			} else {
				put32(img, ino+0x28, target) // i_block[0]
			}
			f := mustOpen(t, img)
			if target == 0 && !ext {
				// A zero block pointer is a hole, not a pointer into block 0.
				if hasWarning(f.Info(), "metadata") {
					t.Errorf("%s: a hole was reported as a metadata pointer", name)
				}
				continue
			}
			fl, err := tryOpenNum(f, ext4test.InodeNumber(0))
			if err != nil {
				t.Fatalf("%s extents=%v: reading must go on: %v", name, ext, err)
			}
			if fl.Size() != bs {
				t.Errorf("%s extents=%v: size %d", name, ext, fl.Size())
			}
			if !hasWarning(f.Info(), msg) {
				t.Errorf("%s extents=%v: no %q in %v", name, ext, msg, f.Info().Warnings)
			}
		}
	}
}

// buildLen is a 1-block extent file on a 5-group image big enough to hold a
// 32768-block extent.
func buildLen(t *testing.T) (img []byte, f *ext4.FS, ino int, phys int64) {
	t.Helper()
	const bs = 1024
	img = ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs, Groups: 5, BlocksPerGroup: 8192}, []ext4test.File{{Path: "/f", Data: pat(bs, 1)}})
	f = mustOpen(t, img)
	ino = inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(0)))
	return img, f, ino, openNum(t, f, ext4test.InodeNumber(0)).Runs()[0].Offset
}

func TestExtentLengthBoundary(t *testing.T) {
	const bs = 1024
	ee := func(ino int) int { return ino + 0x28 + 12 }

	// ee_len 32768 is an initialized extent of 32768 blocks.
	img, _, ino, phys := buildLen(t)
	put16(img, ee(ino)+4, 32768)
	put32(img, ino+4, 32768*bs)
	f := mustOpen(t, img)
	fl := openNum(t, f, ext4test.InodeNumber(0))
	if runs := fl.Runs(); len(runs) != 1 || runs[0] != (filesys.Run{Offset: phys, Length: 32768 * bs}) {
		t.Errorf("ee_len 32768: runs = %v, want one initialized run of 32768 blocks at %d", runs, phys)
	}
	got := make([]byte, bs)
	if _, err := fl.ReadAt(got, 0); err != nil || !bytes.Equal(got, pat(bs, 1)) {
		t.Errorf("ee_len 32768: first block differs (%v)", err)
	}

	// ee_len 32769 is an uninitialized extent of one block: it reads as zeros.
	img, _, ino, _ = buildLen(t)
	put16(img, ee(ino)+4, 32769)
	f = mustOpen(t, img)
	fl = openNum(t, f, ext4test.InodeNumber(0))
	if runs := fl.Runs(); len(runs) != 1 || runs[0] != (filesys.Run{Offset: -1, Length: bs}) {
		t.Errorf("ee_len 32769: runs = %v, want one hole of 1 block", runs)
	}
	if _, err := fl.ReadAt(got, 0); err != nil || !bytes.Equal(got, make([]byte, bs)) {
		t.Errorf("ee_len 32769: content is not zeros (%v)", err)
	}
}

// sparseReader is a huge image of which only the first bytes are non-zero.
type sparseReader struct {
	head []byte
	size int64
}

func (s sparseReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.size-off))
	clear(p[:n])
	if off < int64(len(s.head)) {
		copy(p[:n], s.head[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestExtentStartHighBits(t *testing.T) {
	const bs = 4096
	img := ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs, Bit64: true, BlocksPerGroup: 16384}, []ext4test.File{{Path: "/f", Data: pat(bs, 1)}})
	f0 := mustOpen(t, img)
	ino := inodeOff(t, f0, bs, 256, 128, int(ext4test.InodeNumber(0)))

	// Declare a 16 TiB filesystem (2^32 + 100000 blocks); everything past the
	// head of the image reads as zeros. The file's extent starts at physical
	// block 2^32 + 10, so ee_start_hi is 1.
	const total = 1<<32 + 100000
	put32(img, 1024+0x4, uint32(total&0xFFFFFFFF))
	put32(img, 1024+0x150, uint32(total>>32))
	put16(img, ino+0x28+12+6, 1)
	put32(img, ino+0x28+12+8, 10)
	size := int64(total) * bs
	f, err := ext4.Open(sparseReader{head: img[:1<<20], size: size}, size)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := tryOpenNum(f, ext4test.InodeNumber(0))
	if err != nil {
		t.Fatal(err)
	}
	want := filesys.Run{Offset: (1<<32 + 10) * bs, Length: bs}
	if runs := fl.Runs(); len(runs) != 1 || runs[0] != want {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	got := make([]byte, bs)
	if n, err := fl.ReadAt(got, 0); n != bs || (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(got, make([]byte, bs)) {
		t.Errorf("ReadAt = %d, %v; want %d zero bytes", n, err, bs)
	}
	if hasWarning(f.Info(), "metadata") {
		t.Errorf("a high pointer was reported as metadata: %v", f.Info().Warnings)
	}

	// The same extent beyond the declared size is refused.
	put32(img, 1024+0x150, 0)
	put32(img, 1024+0x4, 100000)
	small := int64(100000) * bs
	f, err = ext4.Open(sparseReader{head: img[:1<<20], size: small}, small)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tryOpenNum(f, ext4test.InodeNumber(0)); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("extent beyond the filesystem: %v, want a CorruptError", err)
	}
}
