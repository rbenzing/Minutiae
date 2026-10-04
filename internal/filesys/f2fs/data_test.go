package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

const (
	bs        = f2fstest.BlockSize
	dataNID   = 5  // the nid of the file under test
	firstNode = 10 // first nid handed to the allocator
	regMode   = 0o100644
	linkMode  = 0o120777
	maxFileOK = 8 << 20 // largest file the tests read whole
)

// dpat returns n bytes of a position-dependent pattern.
func dpat(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+i/251) + seed
	}
	return b
}

// blocksOf splits content into logical blocks (the last one short).
func blocksOf(content []byte, first int64) map[int64][]byte {
	m := map[int64][]byte{}
	for i := 0; i*bs < len(content); i++ {
		m[first+int64(i)] = content[i*bs : min((i+1)*bs, len(content))]
	}
	return m
}

// dataFile builds an image holding one regular file (nid 5) laid out by the
// allocator, and opens it.
func dataFile(t *testing.T, o f2fstest.Options, in f2fstest.Inode, d f2fstest.FileData) (*f2fs.FS, []byte, *f2fstest.Alloc) {
	t.Helper()
	in.NID = dataNID
	if in.Mode == 0 {
		in.Mode = regMode
	}
	a := f2fstest.NewAlloc(o, firstNode)
	nodes, data := a.File(o, in, d)
	o.Nodes, o.Data = nodes, data
	img := f2fstest.Build(o, nil)
	return mustOpen(t, img), img, a
}

func openNID(f *f2fs.FS, nid uint32) (filesys.File, error) {
	return f.Open(filesys.Entry{ID: fmt.Sprintf("nid:%d", nid)})
}

func mustOpenFile(t *testing.T, f *f2fs.FS, nid uint32) filesys.File {
	t.Helper()
	fl, err := openNID(f, nid)
	if err != nil {
		t.Fatalf("Open nid:%d: %v", nid, err)
	}
	return fl
}

func readWhole(t *testing.T, fl filesys.File) []byte {
	t.Helper()
	if fl.Size() > maxFileOK {
		t.Fatalf("test bug: reading a %d-byte file whole", fl.Size())
	}
	b, err := io.ReadAll(io.NewSectionReader(fl, 0, fl.Size()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func sumRuns(runs []filesys.Run) (n int64) {
	for _, r := range runs {
		n += r.Length
	}
	return n
}

// fromImage reassembles the content a run list describes straight from img.
func fromImage(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		if r.Offset < 0 {
			out = append(out, make([]byte, r.Length)...)
		} else {
			out = append(out, img[r.Offset:r.Offset+r.Length]...)
		}
	}
	return out
}

func checkRuns(t *testing.T, fl filesys.File, img []byte) []filesys.Run {
	t.Helper()
	runs := fl.Runs()
	if err := filesys.CheckRuns(runs, fl.Size(), int64(len(img))); err != nil {
		t.Fatalf("CheckRuns: %v (runs %v)", err, runs)
	}
	return runs
}

func mainOff(o f2fstest.Options) int64 { return int64(f2fstest.Geometry(o).Main) * bs }

func TestDirectAddrsFile(t *testing.T) {
	o := smallOpts()
	content := dpat(5*bs-100, 1)
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content)), Links: 1}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	fl := mustOpenFile(t, f, dataNID)
	if fl.Size() != int64(len(content)) {
		t.Fatalf("size %d", fl.Size())
	}
	if got := readWhole(t, fl); !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
	// Five physically consecutive blocks are one run, trimmed to the size.
	runs := checkRuns(t, fl, img)
	if want := []filesys.Run{{Offset: mainOff(o), Length: int64(len(content))}}; !slices.Equal(runs, want) {
		t.Errorf("runs = %v, want %v", runs, want)
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings: %v", f.Info().Warnings)
	}
	// ReadAt edge cases.
	var one [1]byte
	if n, err := fl.ReadAt(one[:], fl.Size()); n != 0 || err != io.EOF {
		t.Errorf("ReadAt at size = %d, %v", n, err)
	}
	if _, err := fl.ReadAt(one[:], -1); err == nil {
		t.Error("negative offset accepted")
	}
	buf := make([]byte, 10)
	if n, err := fl.ReadAt(buf, fl.Size()-4); n != 4 || err != io.EOF || !bytes.Equal(buf[:4], content[len(content)-4:]) {
		t.Errorf("short tail read = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(nil, 3); n != 0 || err != nil {
		t.Errorf("empty read = %d, %v", n, err)
	}
}

func TestDirectAddrsWithExtraAttr(t *testing.T) {
	o := smallOpts()
	o.ExtraAttr, o.InodeChksum = true, true
	content := dpat(3*bs+7, 2)
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content)), Extra: true}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	fl := mustOpenFile(t, f, dataNID)
	if !bytes.Equal(readWhole(t, fl), content) {
		t.Fatal("content differs")
	}
	checkRuns(t, fl, img)
}

func TestDirectNodeFile(t *testing.T) {
	// 923 blocks fit i_addr; the next ones come from the first direct node.
	o := f2fstest.Options{Segments: 4}
	content := dpat((923+40)*bs-1, 3)
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content))}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	v, _, err := f.Inode(dataNID)
	if err != nil {
		t.Fatal(err)
	}
	if v.NIDs[0] == 0 || v.NIDs[1] != 0 || v.NIDs[2] != 0 || v.NIDs[4] != 0 {
		t.Fatalf("i_nid = %v, want only i_nid[0] set", v.NIDs)
	}
	fl := mustOpenFile(t, f, dataNID)
	if !bytes.Equal(readWhole(t, fl), content) {
		t.Fatal("content differs")
	}
	checkRuns(t, fl, img)
	// A read straddling the i_addr / direct node boundary.
	buf := make([]byte, 2*bs)
	if _, err := fl.ReadAt(buf, 922*bs+bs/2); err != nil || !bytes.Equal(buf, content[922*bs+bs/2:][:2*bs]) {
		t.Errorf("boundary read: %v", err)
	}
}

func TestIndirectAndDoubleIndirectFile(t *testing.T) {
	o := smallOpts()
	const (
		per    = int64(f2fstest.AddrsPerBlock)
		inAddr = int64(923)
	)
	d0 := inAddr         // first block of i_nid[0]
	d1 := d0 + per       // i_nid[1]
	in2 := d1 + per      // i_nid[2]: indirect, 1018 direct nodes
	in3 := in2 + per*per // i_nid[3]
	dbl := in3 + per*per // i_nid[4]: double indirect
	idx := []int64{0, inAddr - 1, d0 + 1, d1 + per - 2, in2, in2 + per + 5, in3 + 7*per + 3, dbl, dbl + per*per - 1, dbl + 3*per*per/2}
	last := idx[len(idx)-1]
	blocks := map[int64][]byte{}
	for _, i := range idx {
		blocks[i] = dpat(bs, byte(i))
	}
	size := last*bs + 123
	blocks[last] = blocks[last][:123]
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(size)}, f2fstest.FileData{Blocks: blocks})
	v, _, err := f.Inode(dataNID)
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range v.NIDs {
		if n == 0 {
			t.Fatalf("i_nid[%d] is unused: %v", i, v.NIDs)
		}
	}
	fl := mustOpenFile(t, f, dataNID)
	if fl.Size() != size {
		t.Fatalf("size %d, want %d", fl.Size(), size)
	}
	runs := checkRuns(t, fl, img)
	data := 0
	for _, r := range runs {
		if r.Offset >= 0 {
			data++
		}
	}
	if data != len(idx) {
		t.Errorf("%d data runs, want %d: %v", data, len(idx), runs[:min(len(runs), 12)])
	}
	for _, i := range idx {
		want := blocks[i]
		got := make([]byte, len(want))
		if _, err := fl.ReadAt(got, i*bs); err != nil && err != io.EOF {
			t.Fatalf("block %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("block %d content differs", i)
		}
	}
	// The sparse space in between reads as zeros.
	probe := make([]byte, bs)
	for _, i := range []int64{1, 500, d1 + 10, in2 + 3*per, in3 - 1, in3 + 2, dbl - 1, dbl + per*per} {
		if _, err := fl.ReadAt(probe, i*bs); err != nil || !bytes.Equal(probe, make([]byte, bs)) {
			t.Errorf("hole block %d: err %v, nonzero data", i, err)
		}
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings: %v", f.Info().Warnings)
	}
}

func TestHolesNullAndNewAddr(t *testing.T) {
	o := smallOpts()
	d1, d3 := dpat(bs, 1), dpat(bs, 3)
	// block 0 data, 1 NULL, 2 NEW, 3 data, 4 NEW, 5 NULL, 6 NEW, 7 NULL (size cuts block 7 short)
	size := int64(7*bs + 10)
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(size)}, f2fstest.FileData{
		Blocks: map[int64][]byte{0: d1, 3: d3},
		New:    []int64{2, 4, 6},
	})
	fl := mustOpenFile(t, f, dataNID)
	runs := checkRuns(t, fl, img)
	m := mainOff(o)
	want := []filesys.Run{
		{Offset: m, Length: bs},
		{Offset: -1, Length: 2 * bs},
		{Offset: m + bs, Length: bs},
		{Offset: -1, Length: 2*bs + 10 + 2*bs - 2*bs + bs - bs},
	}
	// holes 4..7 are one run: 3 blocks (4,5,6) plus the 10-byte tail of block 7
	want[3].Length = 3*bs + 10
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	got := readWhole(t, fl)
	exp := make([]byte, size)
	copy(exp, d1)
	copy(exp[3*bs:], d3)
	if !bytes.Equal(got, exp) {
		t.Error("holes do not read as zeros")
	}
	// A hole read into a dirty buffer must overwrite it.
	buf := bytes.Repeat([]byte{0xAA}, bs)
	if _, err := fl.ReadAt(buf, bs); err != nil || !bytes.Equal(buf, make([]byte, bs)) {
		t.Errorf("NULL hole read: %v", err)
	}
	buf = bytes.Repeat([]byte{0xAA}, bs)
	if _, err := fl.ReadAt(buf, 2*bs); err != nil || !bytes.Equal(buf, make([]byte, bs)) {
		t.Errorf("NEW_ADDR hole read: %v", err)
	}
}

func TestEmptyAndAllHoleFiles(t *testing.T) {
	o := smallOpts()
	f, _, _ := dataFile(t, o, f2fstest.Inode{Size: 0}, f2fstest.FileData{})
	fl := mustOpenFile(t, f, dataNID)
	if fl.Size() != 0 || fl.Runs() != nil {
		t.Errorf("empty file: size %d runs %v", fl.Size(), fl.Runs())
	}
	if n, err := fl.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Errorf("empty read = %d, %v", n, err)
	}
	// A size with nothing mapped at all is one hole.
	var img []byte
	f, img, _ = dataFile(t, o, f2fstest.Inode{Size: 3*bs + 1}, f2fstest.FileData{})
	fl = mustOpenFile(t, f, dataNID)
	if runs := checkRuns(t, fl, img); !slices.Equal(runs, []filesys.Run{{Offset: -1, Length: 3*bs + 1}}) {
		t.Errorf("runs = %v", runs)
	}
	// Data beyond the size is ignored: the tail is trimmed.
	f, img, _ = dataFile(t, o, f2fstest.Inode{Size: bs + 5}, f2fstest.FileData{Blocks: blocksOf(dpat(4*bs, 1), 0)})
	fl = mustOpenFile(t, f, dataNID)
	runs := checkRuns(t, fl, img)
	if !slices.Equal(runs, []filesys.Run{{Offset: mainOff(o), Length: bs + 5}}) {
		t.Errorf("runs = %v", runs)
	}
}

func inlineFS(t *testing.T, o f2fstest.Options, in f2fstest.Inode) *f2fs.FS {
	t.Helper()
	in.NID = dataNID
	o.Nodes = []f2fstest.Node{{NID: dataNID, Block: f2fstest.InodeBlock(o, in)}}
	return mustOpen(t, f2fstest.Build(o, nil))
}

func TestInlineDataFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     f2fstest.Options
		extra bool
		max   int
	}{
		{"plain", smallOpts(), false, 4 * (923 - 1)},
		{"extra attr", f2fstest.Options{Segments: 1, ExtraAttr: true, InodeChksum: true}, true, 4 * (923 - 9 - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, n := range []int{0, 1, 100, tc.max} {
				content := dpat(n, 5)
				in := f2fstest.Inode{Mode: regMode, Size: uint64(n), Extra: tc.extra, InlineData: content}
				if n == 0 {
					in.InlineData = []byte{}
				}
				f := inlineFS(t, tc.o, in)
				fl := mustOpenFile(t, f, dataNID)
				if fl.Size() != int64(n) || fl.Runs() != nil {
					t.Fatalf("n=%d: size %d runs %v, want nil runs", n, fl.Size(), fl.Runs())
				}
				if got := readWhole(t, fl); !bytes.Equal(got, content) {
					t.Errorf("n=%d: content differs", n)
				}
				if m, err := fl.ReadAt(make([]byte, 3), int64(n)); m != 0 || err != io.EOF {
					t.Errorf("n=%d: read at size = %d, %v", n, m, err)
				}
			}
			// One byte more than fits the inode: the size is a lie. The bytes the
			// inode can hold are a trusted prefix; reads beyond them are errors.
			want := dpat(tc.max, 1)
			f := inlineFS(t, tc.o, f2fstest.Inode{Mode: regMode, Size: uint64(tc.max + 1), Extra: tc.extra, InlineData: want})
			fl, err := openNID(f, dataNID)
			if err != nil {
				t.Fatalf("oversized inline file: %v, want it opened with a trusted prefix", err)
			}
			if fl.Size() != int64(tc.max+1) || fl.Runs() != nil {
				t.Errorf("size %d runs %v", fl.Size(), fl.Runs())
			}
			buf := make([]byte, tc.max)
			if n, err := fl.ReadAt(buf, 0); n != tc.max || err != nil || !bytes.Equal(buf, want) {
				t.Errorf("prefix read = %d, %v", n, err)
			}
			big := make([]byte, 8)
			if n, err := fl.ReadAt(big, int64(tc.max-3)); n != 3 || !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("straddling read = %d, %v, want 3 and ErrCorrupt", n, err)
			}
			if n, err := fl.ReadAt(big, int64(tc.max)); n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("read at the end of the prefix = %d, %v, want 0 and ErrCorrupt", n, err)
			}
			if !hasWarning(f.Info(), "exceeds") {
				t.Errorf("no warning: %v", f.Info().Warnings)
			}
		})
	}
}

func TestSymlinkTarget(t *testing.T) {
	o := smallOpts()
	// Inline target.
	f := inlineFS(t, o, f2fstest.Inode{Mode: linkMode, Size: 11, InlineData: []byte("../a/b/c.txt")[:11]})
	fl := mustOpenFile(t, f, dataNID)
	if got := readWhole(t, fl); string(got) != "../a/b/c.tx" || fl.Runs() != nil {
		t.Errorf("inline symlink = %q runs %v", got, fl.Runs())
	}
	// Target stored in a data block.
	target := "/" + strings.Repeat("long/", 300) + "end"
	f, img, _ := dataFile(t, o, f2fstest.Inode{Mode: linkMode, Size: uint64(len(target))}, f2fstest.FileData{Blocks: blocksOf([]byte(target), 0)})
	fl = mustOpenFile(t, f, dataNID)
	if got := readWhole(t, fl); string(got) != target {
		t.Errorf("block symlink = %q", got)
	}
	checkRuns(t, fl, img)
}

func TestRawEncryptedContent(t *testing.T) {
	o := smallOpts()
	content := dpat(2*bs, 9) // "ciphertext": returned as stored
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content)), Flags: 0x800}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	fl := mustOpenFile(t, f, dataNID)
	if !bytes.Equal(readWhole(t, fl), content) {
		t.Error("encrypted content is not returned raw")
	}
	checkRuns(t, fl, img)
}

func TestCompressedFileUnsupported(t *testing.T) {
	o := smallOpts()
	f, _, _ := dataFile(t, o, f2fstest.Inode{Size: 100, Flags: 0x4}, f2fstest.FileData{Blocks: blocksOf(dpat(100, 1), 0)})
	_, err := openNID(f, dataNID)
	if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "compressed file") {
		t.Fatalf("compressed file: %v, want ErrUnsupported mentioning \"compressed file\"", err)
	}
	// An inline compressed file is refused too.
	f = inlineFS(t, o, f2fstest.Inode{Mode: regMode, Size: 4, Flags: 0x4, InlineData: []byte("abcd")})
	if _, err := openNID(f, dataNID); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("inline compressed file: %v", err)
	}
	// The compression flag on a symlink means nothing: it opens.
	f = inlineFS(t, o, f2fstest.Inode{Mode: linkMode, Size: 4, Flags: 0x4, InlineData: []byte("abcd")})
	if _, err := openNID(f, dataNID); err != nil {
		t.Errorf("symlink with the compression flag: %v", err)
	}
}

func TestOpenIDsAndTypes(t *testing.T) {
	o := smallOpts()
	o.Nodes = []f2fstest.Node{
		{NID: 5, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 5, Mode: regMode, Size: 4, InlineData: []byte("data")})},
		{NID: 6, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 6, Mode: 0o040755, Size: 4096})},
		{NID: 7, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 7, Mode: 0o020644})}, // char device
	}
	f := mustOpen(t, f2fstest.Build(o, nil))

	for _, id := range []string{
		"", "nid:", "nid:0", "nid:05", "nid:+5", "nid: 5", "nid:5 ", "nid:0x5", "NID:5", "nid:-5",
		"nid:4294967296", "nid:99999999999999999999", "nid:5junk", "ino:5", "5", "dentry:5", "dentry:1:2", "dentry:1:2:3:4",
		"dentry:01:2:3", "dentry:1:-2:3", "dentry:1:2:3x", "dentry:1:2:+3",
	} {
		if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", id, err)
		}
	}
	// A deleted dentry slot is reported as deleted, never opened.
	if _, err := f.Open(filesys.Entry{ID: "dentry:3:0:12"}); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("deleted entry: %v, want ErrDeleted", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "nid:6"}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("directory: %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "nid:7"}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("device node: %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "nid:8"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("free nid: %v, want ErrNotFound", err)
	}
	// A canonical ID whose nid cannot be an inode of the volume is a forged or
	// stale ID: not found, not corruption.
	if _, err := f.Open(filesys.Entry{ID: "nid:1000000000"}); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("nid beyond the NAT: %v, want ErrNotFound and not ErrCorrupt", err)
	}
	// Forged Entry fields are ignored: the inode on disk decides.
	fl, err := f.Open(filesys.Entry{ID: "nid:5", Size: 1 << 40, Type: filesys.TypeDir, Deleted: true, Attrs: []filesys.KV{{Key: "compressed", Value: "true"}}})
	if err != nil {
		t.Fatalf("forged entry fields: %v", err)
	}
	if fl.Size() != 4 || string(readWhole(t, fl)) != "data" {
		t.Errorf("forged entry: size %d", fl.Size())
	}
	// A forged directory entry for a file's nid cannot make a directory open.
	if _, err := f.Open(filesys.Entry{ID: "nid:6", Type: filesys.TypeFile, Size: 4}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("forged type: %v", err)
	}
}

// brokenChain describes a hostile file mapped by hand.
type brokenChain struct {
	name    string
	nodes   func() []f2fstest.Node
	prefix  int64  // bytes that must stay readable
	size    uint64 // file size
	warning string
}

func TestBrokenChainsKeepTrustedPrefix(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	l := f2fstest.Geometry(o)
	main := l.Main
	dataAddr := func(i uint32) uint32 { return main + 100 + i%500 }
	inode := func(size uint64, addrs []uint32, nids [5]uint32) f2fstest.Node {
		return f2fstest.Node{NID: dataNID, Addr: main, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: size, Addrs: addrs, NIDs: nids})}
	}
	node := func(nid, addr uint32, blk []byte) f2fstest.Node {
		return f2fstest.Node{NID: nid, Addr: addr, Block: blk}
	}
	seq := func(from uint32, n int) []uint32 {
		s := make([]uint32, n)
		for i := range s {
			s[i] = dataAddr(from + uint32(i))
		}
		return s
	}
	const idxNode, dirNode = 20, 21 // nids
	cases := []brokenChain{
		{
			name: "indirect node listing its own nid",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{
					inode(uint64(923+2*1018+10)*bs, seq(0, 923), [5]uint32{0, 0, idxNode, 0, 0}),
					// first slot is the node itself
					node(idxNode, main+1, f2fstest.IndirectNodeBlock(idxNode, dataNID, []uint32{idxNode})),
				}
			},
			prefix:  int64(923+2*1018) * bs,
			size:    uint64(923+2*1018+10) * bs,
			warning: "twice",
		},
		{
			name: "double indirect pointing back at the inode",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{
					inode(uint64(923+2*1018+2*1018*1018+10)*bs, seq(0, 923), [5]uint32{0, 0, 0, 0, idxNode}),
					node(idxNode, main+1, f2fstest.IndirectNodeBlock(idxNode, dataNID, []uint32{dataNID})),
				}
			},
			prefix:  int64(923+2*1018+2*1018*1018) * bs,
			size:    uint64(923+2*1018+2*1018*1018+10) * bs,
			warning: "twice",
		},
		{
			name: "direct node shared by two slots",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{
					inode(uint64(923+2*1018)*bs, seq(0, 923), [5]uint32{dirNode, dirNode, 0, 0, 0}),
					node(dirNode, main+1, f2fstest.DirectNodeBlock(dirNode, dataNID, seq(923, 1018))),
				}
			},
			prefix:  int64(923+1018) * bs,
			size:    uint64(923+2*1018) * bs,
			warning: "twice",
		},
		{
			name: "node id that is free",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{inode(uint64(923+50)*bs, seq(0, 923), [5]uint32{dirNode, 0, 0, 0, 0})}
			},
			prefix:  923 * bs,
			size:    uint64(923+50) * bs,
			warning: "free",
		},
		{
			name: "node block with the wrong footer nid",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{
					inode(uint64(923+50)*bs, seq(0, 923), [5]uint32{dirNode, 0, 0, 0, 0}),
					node(dirNode, main+1, f2fstest.DirectNodeBlock(dirNode+1, dataNID, seq(923, 50))),
				}
			},
			prefix: 923 * bs,
			size:   uint64(923+50) * bs,
		},
		{
			name: "node id beyond the NAT",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{inode(uint64(923+2*1018)*bs, seq(0, 923), [5]uint32{0, 0xFFFFFF00, 0, 0, 0})}
			},
			prefix: (923 + 1018) * bs, // i_nid[0] is NULL: a hole
			size:   uint64(923+2*1018) * bs,
		},
		{
			name: "node outside the main area",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{
					inode(uint64(923+50)*bs, seq(0, 923), [5]uint32{dirNode, 0, 0, 0, 0}),
					node(dirNode, 3, nil),
				}
			},
			prefix: 923 * bs,
			size:   uint64(923+50) * bs,
		},
		{
			name: "data address below the main area, in i_addr",
			nodes: func() []f2fstest.Node {
				a := seq(0, 10)
				a[7] = 5
				return []f2fstest.Node{inode(10*bs, a, [5]uint32{})}
			},
			prefix:  7 * bs,
			size:    10 * bs,
			warning: "main area",
		},
		{
			name: "data address at the end of the main area, in a direct node",
			nodes: func() []f2fstest.Node {
				a := seq(923, 20)
				a[3] = l.BlockCount
				return []f2fstest.Node{
					inode(uint64(923+20)*bs, seq(0, 923), [5]uint32{dirNode, 0, 0, 0, 0}),
					node(dirNode, main+1, f2fstest.DirectNodeBlock(dirNode, dataNID, a)),
				}
			},
			prefix: (923 + 3) * bs,
			size:   uint64(923+20) * bs,
		},
		{
			name: "COMPRESS_ADDR in a file not flagged compressed",
			nodes: func() []f2fstest.Node {
				a := seq(0, 4)
				a[2] = 0xFFFFFFFE
				return []f2fstest.Node{inode(4*bs, a, [5]uint32{})}
			},
			prefix: 2 * bs,
			size:   4 * bs,
		},
		{
			name: "size beyond what the node tree can address",
			nodes: func() []f2fstest.Node {
				return []f2fstest.Node{inode(1<<62, seq(0, 3), [5]uint32{})}
			},
			// Everything the tree can address is mapped (the i_addr slots, two
			// direct, two indirect and one double-indirect node; unset ones are
			// holes) and the rest of the size is not.
			prefix:  (923 + 2*1018 + 2*1018*1018 + 1018*1018*1018) * bs,
			size:    1 << 62,
			warning: "addressable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o2 := o
			o2.Nodes = tc.nodes()
			img := f2fstest.Build(o2, nil)
			f := mustOpen(t, img)
			fl, err := openNID(f, dataNID)
			if err != nil {
				t.Fatalf("Open: %v (a broken chain must open with the trusted prefix)", err)
			}
			if fl.Size() != int64(tc.size) {
				t.Fatalf("size %d, want %d", fl.Size(), tc.size)
			}
			runs := fl.Runs()
			covered, err := filesys.CheckRunsPrefix(runs, fl.Size(), int64(len(img)))
			if err != nil {
				t.Fatalf("CheckRunsPrefix: %v", err)
			}
			if covered >= fl.Size() {
				t.Fatalf("runs cover %d of %d bytes: not a strict prefix", covered, fl.Size())
			}
			if tc.prefix != 0 && covered != tc.prefix {
				t.Errorf("trusted prefix = %d bytes, want %d", covered, tc.prefix)
			}
			// Reads inside the prefix work; at and beyond its end they fail
			// with ErrCorrupt and never return zeros.
			if covered > 0 {
				buf := make([]byte, min(covered, bs))
				if _, err := fl.ReadAt(buf, covered-int64(len(buf))); err != nil {
					t.Errorf("read of the last prefix block: %v", err)
				}
			}
			for _, off := range []int64{covered, covered + 1, fl.Size() - 1} {
				buf := bytes.Repeat([]byte{0xEE}, 16)
				n, err := fl.ReadAt(buf, off)
				if !errors.Is(err, filesys.ErrCorrupt) || n != 0 {
					t.Errorf("read at %d (prefix end %d) = %d, %v, want 0 bytes and ErrCorrupt", off, covered, n, err)
				}
			}
			// A read that starts inside the prefix and runs past it returns the
			// good bytes together with the error.
			if covered > 4 {
				buf := make([]byte, 16)
				n, err := fl.ReadAt(buf, covered-4)
				if n != 4 || !errors.Is(err, filesys.ErrCorrupt) {
					t.Errorf("straddling read = %d, %v, want 4 and ErrCorrupt", n, err)
				}
			}
			if !hasWarning(f.Info(), "") {
				t.Error("no Info warning for a broken chain")
			}
			if tc.warning != "" && !hasWarning(f.Info(), tc.warning) {
				t.Errorf("warnings %q do not mention %q", f.Info().Warnings, tc.warning)
			}
		})
	}
}

// TestNodeChainCycleIsCorrupt: an indirect node listing its own nid must
// terminate, and the file is opened with the trusted prefix and the cycle
// reported as corruption.
func TestNodeChainCycleIsCorrupt(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	l := f2fstest.Geometry(o)
	const idx = 20
	size := uint64(923+2*1018+3) * bs
	o.Nodes = []f2fstest.Node{
		{NID: dataNID, Addr: l.Main, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: size, NIDs: [5]uint32{0, 0, idx, 0, 0}})},
		{NID: idx, Addr: l.Main + 1, Block: f2fstest.IndirectNodeBlock(idx, dataNID, []uint32{idx, idx, idx})},
	}
	f := mustOpen(t, f2fstest.Build(o, nil))
	fl, err := openNID(f, dataNID)
	if err != nil {
		t.Fatal(err)
	}
	covered := sumRuns(fl.Runs())
	if want := int64(923+2*1018) * bs; covered != want {
		t.Errorf("prefix = %d, want %d", covered, want)
	}
	_, err = fl.ReadAt(make([]byte, 1), covered)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "twice") {
		t.Errorf("read past the cycle: %v, want a CorruptError naming the repeated node", err)
	}
}

// A file whose map yields more than 1<<20 runs keeps the first 1<<20 as its
// trusted prefix; the rest is an error.
func TestRunCapIsCorrupt(t *testing.T) {
	o := f2fstest.Options{Segments: 3}
	l := f2fstest.Geometry(o)
	const (
		perNode = f2fstest.AddrsPerBlock
		direct  = 1038 // direct nodes: 1018 under i_nid[2], 20 under i_nid[3]
		idxA    = 5000
		idxB    = 5001
		first   = 6000
	)
	zig := make([]uint32, perNode) // data, hole, data, hole ...: every block its own run
	for i := 0; i < perNode; i += 2 {
		zig[i] = l.Main
	}
	nodes := []f2fstest.Node{}
	var under [2][]uint32
	for i := range direct {
		nid := uint32(first + i)
		nodes = append(nodes, f2fstest.Node{NID: nid, Addr: l.Main + 10 + uint32(i), Block: f2fstest.DirectNodeBlock(nid, dataNID, zig)})
		k := 0
		if i >= perNode {
			k = 1
		}
		under[k] = append(under[k], nid)
	}
	blocks := int64(923 + 2*perNode + direct*perNode)
	nodes = append(nodes,
		f2fstest.Node{NID: dataNID, Addr: l.Main + 1, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(blocks) * bs, NIDs: [5]uint32{0, 0, idxA, idxB, 0}})},
		f2fstest.Node{NID: idxA, Addr: l.Main + 2, Block: f2fstest.IndirectNodeBlock(idxA, dataNID, under[0])},
		f2fstest.Node{NID: idxB, Addr: l.Main + 3, Block: f2fstest.IndirectNodeBlock(idxB, dataNID, under[1])},
	)
	o.Nodes = nodes
	img := f2fstest.Build(o, nil)
	f := mustOpen(t, img)
	fl, err := openNID(f, dataNID)
	if err != nil {
		t.Fatal(err)
	}
	runs := fl.Runs()
	if len(runs) != 1<<20 {
		t.Fatalf("%d runs, want exactly the cap %d", len(runs), 1<<20)
	}
	covered, err := filesys.CheckRunsPrefix(runs, fl.Size(), int64(len(img)))
	if err != nil || covered >= fl.Size() {
		t.Fatalf("CheckRunsPrefix = %d, %v", covered, err)
	}
	if _, err := fl.ReadAt(make([]byte, 8), covered); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read past the capped runs: %v, want ErrCorrupt", err)
	}
	if !hasWarning(f.Info(), "more than") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
}

// TestDataAddrOutsideMainIsCorrupt: every data address must lie in the main
// area (and inside the image); the blocks before the bad one stay readable.
func TestDataAddrOutsideMainIsCorrupt(t *testing.T) {
	o := smallOpts()
	l := f2fstest.Geometry(o)
	content := dpat(6*bs, 4)
	for _, bad := range []struct {
		name string
		addr uint32
	}{
		{"below main", l.Main - 1},
		{"zero page", 1},
		{"main end", l.BlockCount},
		{"far beyond", 0xFFFFFFF0},
		{"compress addr", 0xFFFFFFFE},
	} {
		t.Run(bad.name, func(t *testing.T) {
			a := f2fstest.NewAlloc(o, firstNode)
			nodes, data := a.File(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(len(content))}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
			// Patch i_addr[4] of the inode block.
			le.PutUint32(nodes[0].Block[360+4*4:], bad.addr)
			o2 := o
			o2.Nodes, o2.Data = nodes, data
			f := mustOpen(t, f2fstest.Build(o2, nil))
			fl, err := openNID(f, dataNID)
			if err != nil {
				t.Fatal(err)
			}
			if got := sumRuns(fl.Runs()); got != 4*bs {
				t.Fatalf("prefix = %d bytes, want %d", got, 4*bs)
			}
			buf := make([]byte, 4*bs)
			if _, err := fl.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, content[:4*bs]) {
				t.Errorf("prefix read: %v", err)
			}
			if _, err := fl.ReadAt(buf[:8], 4*bs); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("read of the bad block: %v, want ErrCorrupt", err)
			}
			if len(f.Info().Warnings) == 0 {
				t.Error("no warning for the bad address")
			}
		})
	}
}

func TestRunsReproduceContent(t *testing.T) {
	o := f2fstest.Options{Segments: 4}
	content := dpat((923+30)*bs-300, 6)
	blocks := blocksOf(content, 0)
	// Punch holes and scatter: every third block is missing, two are NEW.
	var newBlocks []int64
	for i := int64(0); i < 953; i += 3 {
		delete(blocks, i)
	}
	delete(blocks, 400)
	delete(blocks, 401)
	newBlocks = append(newBlocks, 400, 401, 930)
	delete(blocks, 930)
	want := slices.Clone(content)
	for i := int64(0); i < 953; i++ {
		if _, ok := blocks[i]; !ok {
			clear(want[min(i*bs, int64(len(want))):min((i+1)*bs, int64(len(want)))])
		}
	}
	for _, stride := range []uint32{1, 2} {
		t.Run(fmt.Sprintf("stride %d", stride), func(t *testing.T) {
			o2 := o
			in := f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(len(content))}
			a := f2fstest.NewAlloc(o2, firstNode)
			a.Stride = stride
			nodes, data := a.File(o2, in, f2fstest.FileData{Blocks: blocks, New: newBlocks})
			o2.Nodes, o2.Data = nodes, data
			img := f2fstest.Build(o2, nil)
			f := mustOpen(t, img)
			fl := mustOpenFile(t, f, dataNID)
			runs := checkRuns(t, fl, img)
			got := readWhole(t, fl)
			if !bytes.Equal(got, want) {
				t.Fatal("ReadAt content differs from the expected content")
			}
			if re := fromImage(img, runs); !bytes.Equal(re, got) {
				t.Fatal("the runs, read straight from the image, do not reproduce the content")
			}
			// Runs are in file order, holes are exactly -1, never two adjacent
			// holes, and with stride 1 physically consecutive blocks merge.
			for i := 1; i < len(runs); i++ {
				a, b := runs[i-1], runs[i]
				if a.Offset < 0 && b.Offset < 0 {
					t.Fatalf("adjacent holes at run %d: %v %v", i, a, b)
				}
				if a.Offset >= 0 && b.Offset >= 0 && a.Offset+a.Length == b.Offset {
					t.Fatalf("physically adjacent runs not merged at %d: %v %v", i, a, b)
				}
			}
			if len(runs) > 0 && runs[0].Offset >= 0 && runs[0].Length%bs != 0 && len(runs) > 1 {
				t.Errorf("a non-final run is not whole blocks: %v", runs[0])
			}
			// Runs() returns a copy.
			if len(runs) > 0 {
				runs[0].Length = -7
				if fl.Runs()[0].Length == -7 {
					t.Error("Runs exposes the file's internal slice")
				}
			}
		})
	}
}

func TestOpenIsConcurrentSafe(t *testing.T) {
	o := smallOpts()
	content := dpat(8*bs, 2)
	f, _, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content))}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	done := make(chan error, 8)
	for range 8 {
		go func() {
			fl, err := openNID(f, dataNID)
			if err != nil {
				done <- err
				return
			}
			b := make([]byte, len(content))
			if _, err := fl.ReadAt(b, 0); err != nil && err != io.EOF {
				done <- err
				return
			}
			if !bytes.Equal(b, content) {
				err = errors.New("content differs")
			}
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestDataIOErrorIsNotCorruption(t *testing.T) {
	o := smallOpts()
	content := dpat(2*bs, 2)
	f, img, _ := dataFile(t, o, f2fstest.Inode{Size: uint64(len(content))}, f2fstest.FileData{Blocks: blocksOf(content, 0)})
	_ = f
	boom := errors.New("disk on fire")
	r := &failingReader{b: img, failAt: mainOff(o) + bs, err: boom}
	g, err := f2fs.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	fl, err := openNID(g, dataNID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fl.ReadAt(make([]byte, bs), bs); !errors.Is(err, boom) || errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("I/O error read = %v, want the original error, not corruption", err)
	}
}

// failingReader fails reads that touch failAt.
type failingReader struct {
	b      []byte
	failAt int64
	err    error
}

func (r *failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off <= r.failAt && r.failAt < off+int64(len(p)) {
		return 0, r.err
	}
	return bytes.NewReader(r.b).ReadAt(p, off)
}

// INLINE_DATA without DATA_EXIST: the kernel reads such a file as empty. The
// stored bytes are still returned, with a warning and the attr data_exist=false;
// an empty inline file legitimately lacks the flag and is silent.
func TestInlineDataWithoutDataExist(t *testing.T) {
	hasAttr := func(e filesys.Entry) bool { _, ok := attrOf(e, "data_exist"); return ok }
	content := []byte("stale inline bytes")
	in := f2fstest.Inode{Mode: regMode, Size: uint64(len(content)), InlineData: content, RawOverrides: map[int][]byte{3: {f2fstest.InlineData}}}
	f := inlineFS(t, smallOpts(), in)
	_, e, err := f.Inode(dataNID)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := attrOf(e, "data_exist"); v != "false" {
		t.Errorf("attrs %+v lack data_exist=false", e.Attrs)
	}
	fl := mustOpenFile(t, f, dataNID)
	if got := readWhole(t, fl); !bytes.Equal(got, content) {
		t.Errorf("content = %q", got)
	}
	if !hasWarning(f.Info(), "DATA_EXIST") {
		t.Errorf("no warning: %v", f.Info().Warnings)
	}
	// A normal inline file carries no such attr and no warning.
	f = inlineFS(t, smallOpts(), f2fstest.Inode{Mode: regMode, Size: 3, InlineData: []byte("abc")})
	mustOpenFile(t, f, dataNID)
	if _, e, _ := f.Inode(dataNID); hasAttr(e) || len(f.Info().Warnings) != 0 {
		t.Errorf("normal inline file: attrs %+v warnings %v", e.Attrs, f.Info().Warnings)
	}
	// An empty inline file lacks the flag by design.
	f = inlineFS(t, smallOpts(), f2fstest.Inode{Mode: regMode, Inline: f2fstest.InlineData})
	mustOpenFile(t, f, dataNID)
	if _, e, _ := f.Inode(dataNID); hasAttr(e) || len(f.Info().Warnings) != 0 {
		t.Errorf("empty inline file: attrs %+v warnings %v", e.Attrs, f.Info().Warnings)
	}
}

// A direct or indirect node records its owning inode in its footer. A different
// owner is a warning; a node that is itself an inode (footer ino == own nid)
// cannot be part of a node tree and is corrupt for that subtree.
func TestNodeFooterOwnership(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	main := f2fstest.Geometry(o).Main
	const dirNode = 20
	seq := func(from, n int) []uint32 {
		s := make([]uint32, n)
		for i := range s {
			s[i] = main + 100 + uint32((from+i)%500)
		}
		return s
	}
	build := func(owner uint32) *f2fs.FS {
		o2 := o
		o2.Nodes = []f2fstest.Node{
			{NID: dataNID, Addr: main, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(923+5) * bs, Addrs: seq(0, 923), NIDs: [5]uint32{dirNode}})},
			{NID: dirNode, Addr: main + 1, Block: f2fstest.DirectNodeBlock(dirNode, owner, seq(923, 5))},
		}
		return mustOpen(t, f2fstest.Build(o2, nil))
	}

	f := build(dataNID)
	fl := mustOpenFile(t, f, dataNID)
	if got := sumRuns(fl.Runs()); got != int64(923+5)*bs || len(f.Info().Warnings) != 0 {
		t.Errorf("proper owner: covered %d, warnings %v", got, f.Info().Warnings)
	}

	f = build(77)
	fl = mustOpenFile(t, f, dataNID)
	if got := sumRuns(fl.Runs()); got != int64(923+5)*bs {
		t.Errorf("foreign owner: covered %d, want the whole file (a warning only)", got)
	}
	if !hasWarning(f.Info(), "records inode 77") {
		t.Errorf("no ownership warning: %v", f.Info().Warnings)
	}

	f = build(dirNode)
	fl = mustOpenFile(t, f, dataNID)
	covered := sumRuns(fl.Runs())
	if covered != 923*bs {
		t.Errorf("node that is an inode: covered %d, want the trusted prefix %d", covered, 923*bs)
	}
	if _, err := fl.ReadAt(make([]byte, 1), covered); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read past the prefix: %v, want ErrCorrupt", err)
	}
	if !hasWarning(f.Info(), "is an inode") {
		t.Errorf("no warning: %v", f.Info().Warnings)
	}
}

// Forged Entry fields (Size, Type, Deleted, Attrs, names) never change what
// Open or ReadDir read from the volume: the ID decides, the disk answers.
func TestForgedEntryFieldsAreIgnored(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/file", Data: []byte("real content")},
		{Path: "/dir", Dir: true},
		{Path: "/dir/inner", Data: []byte("i")},
		{Path: "/packed", Data: []byte("squeezed"), Compressed: true},
		{Path: "/gone", Data: []byte("gone"), Deleted: true},
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	fileID, dirID := nidID(tree.NID["/file"]), nidID(tree.NID["/dir"])
	forged := []filesys.Entry{
		{ID: fileID},
		{ID: fileID, Size: 1 << 40, Type: filesys.TypeDir, Deleted: true, Attrs: []filesys.KV{{Key: "compressed", Value: "true"}, {Key: "inline", Value: "data"}}},
		{ID: fileID, Size: 0, Type: filesys.TypeSymlink, Encrypted: true, Name: "other", RawName: []byte("other"), LinkTarget: "/etc/passwd"},
	}
	for _, e := range forged {
		fl, err := f.Open(e)
		if err != nil || fl.Size() != 12 || string(readWhole(t, fl)) != "real content" {
			t.Errorf("Open(%+v) = %v, %v", e, fl, err)
		}
	}
	// A directory forged as a file, and a compressed file forged as plain.
	if _, err := f.Open(filesys.Entry{ID: dirID, Type: filesys.TypeFile, Size: 4}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("directory forged as a file: %v, want ErrUnsupported", err)
	}
	if _, err := f.Open(filesys.Entry{ID: nidID(tree.NID["/packed"]), Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("compressed file forged as plain: %v, want ErrUnsupported", err)
	}
	// A deleted slot stays deleted whatever the entry claims.
	gone := entryByName(t, readDirPath(t, f, "/"), "gone")
	if _, err := f.Open(filesys.Entry{ID: gone.ID, Deleted: false, Type: filesys.TypeFile, Size: 4}); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("deleted slot forged as live: %v, want ErrDeleted", err)
	}
	for _, e := range []filesys.Entry{{ID: dirID}, {ID: dirID, Type: filesys.TypeFile, Size: 1 << 40, Deleted: true, Attrs: []filesys.KV{{Key: "inline", Value: "dentry"}}}} {
		es, err := f.ReadDir(e)
		if err != nil || len(es) != 1 || es[0].Name != "inner" {
			t.Errorf("ReadDir(%+v) = %q, %v", e, entryNames(es), err)
		}
	}
}
