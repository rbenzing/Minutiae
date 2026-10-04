package hfsplus_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// pattern is n deterministic, non-repeating-looking bytes.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+i>>8*7) + seed
	}
	return b
}

// openPath looks a path up and opens it.
func openPath(t testing.TB, f *hfsplus.FS, p string) (filesys.Entry, filesys.File) {
	t.Helper()
	e, err := f.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	return e, fl
}

// readAll reads a whole file through ReadAt.
func readAll(t testing.TB, fl filesys.File) []byte {
	t.Helper()
	buf := make([]byte, fl.Size())
	n, err := fl.ReadAt(buf, 0)
	if n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt(all %d) = %d, %v", len(buf), n, err)
	}
	return buf
}

// readRuns concatenates the image bytes at the runs.
func readRuns(img []byte, runs []filesys.Run) []byte {
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

func extRuns(lay *hfsplustest.Layout, exts []hfsplustest.Extent, size int64) []filesys.Run {
	bs := int64(lay.BlockSize)
	var runs []filesys.Run
	for _, e := range exts {
		if size <= 0 {
			break
		}
		n := min(int64(e.Count)*bs, size)
		size -= n
		off := lay.Base + int64(e.Start)*bs
		if l := len(runs); l > 0 && runs[l-1].Offset+runs[l-1].Length == off {
			runs[l-1].Length += n
			continue
		}
		runs = append(runs, filesys.Run{Offset: off, Length: n})
	}
	return runs
}

func TestSingleExtentFile(t *testing.T) {
	data := pattern(3*4096, 1)
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/a.bin", Data: data}, {Path: "/b.bin", Data: pattern(100, 2)},
	})
	e, fl := openPath(t, f, "/a.bin")
	if e.Size != int64(len(data)) || fl.Size() != int64(len(data)) {
		t.Errorf("sizes = %d, %d", e.Size, fl.Size())
	}
	want := extRuns(lay, lay.Files["/a.bin"].Data, int64(len(data)))
	if got := fl.Runs(); !slices.Equal(got, want) || len(want) != 1 {
		t.Errorf("Runs = %v, want %v", got, want)
	}
	if got := readAll(t, fl); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}
	_, fl2 := openPath(t, f, "/b.bin")
	if got := readAll(t, fl2); !bytes.Equal(got, pattern(100, 2)) {
		t.Error("second file differs")
	}
	if got := fl.Runs(); got[0].Offset == fl2.Runs()[0].Offset {
		t.Error("two files share their blocks")
	}
	// Reading past the end is io.EOF, a read crossing it is short.
	buf := make([]byte, 10)
	if n, err := fl.ReadAt(buf, fl.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf, fl.Size()-4); n != 4 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:4], data[len(data)-4:]) {
		t.Errorf("ReadAt across the end = %d, %v", n, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestLogicalSizeShorterThanAllocation(t *testing.T) {
	data := pattern(5000, 3) // two blocks
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/two.bin", Data: data},
		{Path: "/trim.bin", Data: pattern(3*4096, 4), DataLogical: 5000}, // extents past the logical size are ignored
	})
	for _, name := range []string{"/two.bin", "/trim.bin"} {
		_, fl := openPath(t, f, name)
		if fl.Size() != 5000 {
			t.Fatalf("%s size = %d", name, fl.Size())
		}
		want := extRuns(lay, lay.Files[name].Data, 5000)
		if got := fl.Runs(); !slices.Equal(got, want) {
			t.Errorf("%s Runs = %v, want %v", name, got, want)
		}
		var total int64
		for _, r := range fl.Runs() {
			total += r.Length
		}
		if total != 5000 {
			t.Errorf("%s runs cover %d bytes, want 5000 (the tail of the last block is trimmed)", name, total)
		}
		if got := readAll(t, fl); len(got) != 5000 {
			t.Errorf("%s read %d bytes", name, len(got))
		}
		if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
			t.Error(err)
		}
	}
	_, fl := openPath(t, f, "/trim.bin")
	if !bytes.Equal(readAll(t, fl), pattern(3*4096, 4)[:5000]) {
		t.Error("trimmed content differs")
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestExtentsOverflowResolvesFragmentedFile(t *testing.T) {
	const n = 40
	data := pattern(n*4096, 5)
	img, lay, f := buildTree(t, hfsplustest.Options{Blocks: 512}, []hfsplustest.File{
		{Path: "/frag.bin", Data: data, Fragment: 1},
	})
	exts := lay.Files["/frag.bin"].Data
	if len(exts) != n {
		t.Fatalf("builder placed %d extents, want %d (8 inline, the rest in the overflow tree)", len(exts), n)
	}
	_, fl := openPath(t, f, "/frag.bin")
	want := extRuns(lay, exts, int64(len(data)))
	if got := fl.Runs(); !slices.Equal(got, want) || len(want) != n {
		t.Errorf("Runs = %v, want %d runs %v", got, n, want)
	}
	if got := readAll(t, fl); !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	if !bytes.Equal(readRuns(img, fl.Runs()), data) {
		t.Error("the image bytes at the runs differ from the content")
	}
	// A read in the middle that spans several extents.
	buf := make([]byte, 3*4096+100)
	if n, err := fl.ReadAt(buf, 9*4096-50); n != len(buf) || err != nil || !bytes.Equal(buf, data[9*4096-50:9*4096-50+len(buf)]) {
		t.Errorf("ReadAt across extents = %d, %v", n, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestRunsReproduceContent(t *testing.T) {
	files := []hfsplustest.File{
		{Path: "/one", Data: pattern(10000, 6)},
		{Path: "/frag", Data: pattern(20*4096-17, 7), Fragment: 3},
		{Path: "/frag2", Data: pattern(12*4096, 8), Fragment: 1},
		{Path: "/empty"},
	}
	for _, o := range []hfsplustest.Options{{Blocks: 512}, {Blocks: 512, Wrapper: true}, {Blocks: 1024, BlockSize: 1024, NodeSize: 1024}} {
		img, _, f := buildTree(t, o, files)
		for _, fi := range files {
			_, fl := openPath(t, f, fi.Path)
			if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
				t.Errorf("%s: %v", fi.Path, err)
			}
			if got := readRuns(img, fl.Runs()); !bytes.Equal(got, fi.Data) {
				t.Errorf("%s: the image at the runs differs from the content (%d vs %d bytes)", fi.Path, len(got), len(fi.Data))
			}
			if !bytes.Equal(readAll(t, fl), fi.Data) && len(fi.Data) > 0 {
				t.Errorf("%s: ReadAt differs", fi.Path)
			}
		}
	}
}

func TestWrapperRunsAreRelativeToWrapperStart(t *testing.T) {
	data := pattern(9*4096+1, 9)
	img, lay, f := buildTree(t, hfsplustest.Options{Wrapper: true, Blocks: 512}, []hfsplustest.File{
		{Path: "/w.bin", Data: data, Fragment: 2},
	})
	if lay.Base == 0 || f.Base() != lay.Base {
		t.Fatalf("base = %d (layout %d)", f.Base(), lay.Base)
	}
	_, fl := openPath(t, f, "/w.bin")
	want := extRuns(lay, lay.Files["/w.bin"].Data, int64(len(data)))
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v (volume offsets plus the wrapper base %d)", got, want, lay.Base)
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
		t.Error(err)
	}
	if got := readRuns(img, fl.Runs()); !bytes.Equal(got, data) {
		t.Error("reading the whole image at the runs does not reproduce the content")
	}
	if got := readAll(t, fl); !bytes.Equal(got, data) {
		t.Error("ReadAt differs")
	}
}

func TestZeroLengthFileHasNoRuns(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/empty"}})
	e, fl := openPath(t, f, "/empty")
	if e.Size != 0 || fl.Size() != 0 || len(fl.Runs()) != 0 {
		t.Errorf("size %d/%d runs %v", e.Size, fl.Size(), fl.Runs())
	}
	if n, err := fl.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt = %d, %v", n, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestSymlinkTarget(t *testing.T) {
	long := strings.Repeat("a/", 2100) // 4200 bytes
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/dir", Dir: true},
		{Path: "/dir/link", Mode: 0o120777, Data: []byte("../target/file")},
		{Path: "/exact", Mode: 0o120777, Data: []byte(strings.Repeat("x", 4096))},
		{Path: "/long", Mode: 0o120777, Data: []byte(long)},
	})
	e, err := f.Lookup("/dir/link")
	if err != nil || e.Type != filesys.TypeSymlink || e.LinkTarget != "../target/file" {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	fl, err := f.Open(e)
	if err != nil || string(readAll(t, fl)) != "../target/file" {
		t.Errorf("Open: %v", err)
	}
	// ReadDir fills the target too.
	for _, c := range readDir(t, f, child(t, readDir(t, f, f.Root()), "dir")) {
		if c.LinkTarget != "../target/file" {
			t.Errorf("ReadDir target = %q", c.LinkTarget)
		}
	}
	if e, _ := f.Lookup("/exact"); len(e.LinkTarget) != 4096 {
		t.Errorf("a 4096-byte target: %d bytes", len(e.LinkTarget))
	}
	e, _ = f.Lookup("/long")
	if e.LinkTarget != "" || e.Size != int64(len(long)) {
		t.Errorf("a target over 4096 bytes: LinkTarget %d bytes, size %d", len(e.LinkTarget), e.Size)
	}
	if fl, err := f.Open(e); err != nil || string(readAll(t, fl)) != long {
		t.Errorf("Open(long symlink): %v", err)
	}
	// A symlink whose data cannot be read gets no target and a warning, not a failure.
	_, _, f2 := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/l", Mode: 0o120777, DataLogical: 50}})
	e, err = f2.Lookup("/l")
	if err != nil || e.LinkTarget != "" || !hasWarning(f2.Info(), "symlink") {
		t.Errorf("unreadable symlink: %+v, %v, warnings %q", e, err, f2.Info().Warnings)
	}
}

func TestForkShorterThanSizeIsTrustedPrefix(t *testing.T) {
	const bs = 4096
	data := pattern(2*bs, 10)
	img, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/short.bin", Data: data, DataLogical: 5 * bs},
		{Path: "/none.bin", DataLogical: 7000}, // a size and no blocks at all
	})
	_, fl := openPath(t, f, "/short.bin")
	if fl.Size() != 5*bs {
		t.Fatalf("size = %d", fl.Size())
	}
	want := extRuns(lay, lay.Files["/short.bin"].Data, 2*bs)
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Fatalf("Runs = %v, want the prefix %v", got, want)
	}
	covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size)
	if err != nil || covered != 2*bs {
		t.Errorf("CheckRunsPrefix = %d, %v", covered, err)
	}
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err == nil {
		t.Error("CheckRuns accepts a prefix")
	}
	buf := make([]byte, bs)
	if n, err := fl.ReadAt(buf, bs); n != bs || err != nil || !bytes.Equal(buf, data[bs:]) {
		t.Errorf("read inside the prefix = %d, %v", n, err)
	}
	if n, err := fl.ReadAt(buf, 2*bs); n != 0 || !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, io.EOF) {
		t.Errorf("read at the prefix end = %d, %v; want ErrCorrupt (never zeros, never EOF)", n, err)
	}
	if n, err := fl.ReadAt(make([]byte, 2*bs), bs); n != bs || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read across the prefix end = %d, %v", n, err)
	}
	if !hasWarning(f.Info(), "short.bin") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
	_ = img
	// No blocks at all: an empty prefix.
	_, fl = openPath(t, f, "/none.bin")
	if len(fl.Runs()) != 0 || fl.Size() != 7000 {
		t.Errorf("none.bin: runs %v size %d", fl.Runs(), fl.Size())
	}
	if _, err := fl.ReadAt(make([]byte, 1), 0); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read of an unallocated file = %v", err)
	}
}

// patchOverflow rewrites the first extent of the first record of the
// extents-overflow tree (a single leaf, node 1).
func patchOverflow(img []byte, lay *hfsplustest.Layout, start, count uint32) {
	off := lay.ExtentsOffset(int64(lay.ExtentsRoot)*int64(lay.ExtentsNodeSize) + 14 + 12)
	be.PutUint32(img[off:], start)
	be.PutUint32(img[off+4:], count)
}

func TestBrokenOverflowExtentKeepsTrustedPrefix(t *testing.T) {
	const bs = 4096
	data := pattern(20*bs, 11)
	cases := map[string][2]uint32{
		"outside the volume": {0xFFFFFFF0, 4},
		"overshoots":         {100, 400},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			img, lay, _ := buildTree(t, hfsplustest.Options{Blocks: 256}, []hfsplustest.File{{Path: "/f.bin", Data: data, Fragment: 1}})
			patchOverflow(img, lay, patch[0], patch[1])
			f := open(t, img)
			_, fl := openPath(t, f, "/f.bin")
			exts := lay.Files["/f.bin"].Data
			want := extRuns(lay, exts[:8], 8*bs)
			if got := fl.Runs(); !slices.Equal(got, want) {
				t.Fatalf("Runs = %v, want the 8 inline extents %v", got, want)
			}
			if c, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size); err != nil || c != 8*bs {
				t.Errorf("CheckRunsPrefix = %d, %v", c, err)
			}
			if got := make([]byte, 8*bs); true {
				if n, err := fl.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, data[:8*bs]) {
					t.Errorf("prefix read = %d, %v", n, err)
				}
			}
			if n, err := fl.ReadAt(make([]byte, 16), 8*bs); n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("read at the prefix end = %d, %v", n, err)
			}
			if len(f.Info().Warnings) == 0 {
				t.Error("no warning")
			}
		})
	}
}

func TestRunCapKeepsPrefix(t *testing.T) {
	// The cap is 1<<20 extents; a test hook lowers it so a 40-extent file stands in for a larger one.
	const bs = 4096
	data := pattern(40*bs, 12)
	img, lay, _ := buildTree(t, hfsplustest.Options{Blocks: 512}, []hfsplustest.File{{Path: "/f.bin", Data: data, Fragment: 1}})
	f := open(t, img)
	f.SetExtentCap(10)
	_, fl := openPath(t, f, "/f.bin")
	want := extRuns(lay, lay.Files["/f.bin"].Data[:16], 16*bs)
	if got := fl.Runs(); !slices.Equal(got, want) {
		t.Fatalf("Runs = %d runs, want the %d mapped before the cap bit", len(got), len(want))
	}
	if c, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size); err != nil || c != 16*bs {
		t.Errorf("CheckRunsPrefix = %d, %v", c, err)
	}
	if _, err := fl.ReadAt(make([]byte, 1), 16*bs); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("read past the cap = %v", err)
	}
	if !hasWarning(f.Info(), "more than 10 extents") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
}

func TestOpenForgedEntryFieldsAreIgnored(t *testing.T) {
	data := pattern(6000, 13)
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/dir", Dir: true}, {Path: "/dir/real.bin", Data: data},
	})
	id := idOf(lay.CNIDs["/dir/real.bin"])
	forged := filesys.Entry{
		ID: id, Name: "other", Type: filesys.TypeDir, Size: 3, Mode: 0o40777, Deleted: true, Encrypted: true,
		LinkTarget: "/etc/passwd", Attrs: []filesys.KV{{Key: "compressed", Value: "zlib"}, {Key: "hardlink", Value: "dir"}},
	}
	fl, err := f.Open(forged)
	if err != nil || !bytes.Equal(readAll(t, fl), data) {
		t.Fatalf("Open of a forged entry: %v", err)
	}
	if fl, err := f.Open(filesys.Entry{ID: id}); err != nil || fl.Size() != 6000 { // built outside ReadDir
		t.Errorf("Open of a hand-made entry: %v", err)
	}
	// A directory stays a directory whatever the entry says.
	if _, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/dir"]), Type: filesys.TypeFile}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open of a folder = %v", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "cnid:2"}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open of the root = %v", err)
	}
	// Forged and non-canonical ids fail at once.
	before := f.DirBudget()
	for _, bad := range []string{"", "cnid:", "cnid:016", "cnid:+16", "cnid: 16", "CNID:16", "cnid:0x10", "cnid:16 ", "cnid:5", "cnid:4294967296", "id:cnid:16", "inode:16", "cnid:99999"} {
		if _, err := f.Open(filesys.Entry{ID: bad}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	if f.DirBudget() != before {
		t.Errorf("forged ids spent %d bytes of the directory budget", before-f.DirBudget())
	}
}

func TestOpenFileWithoutThreadIsNotFound(t *testing.T) {
	_, lay, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{{Path: "/legacy", NoThread: true, Data: pattern(10, 1)}})
	// Without a thread record the catalog cannot be searched by id: the entry is
	// flagged thread=missing in listings and cannot be opened.
	_, err := f.Open(filesys.Entry{ID: idOf(lay.CNIDs["/legacy"])})
	if !errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), "thread") {
		t.Errorf("Open = %v", err)
	}
}

// An extent that lies past the end of a truncated image ends the trusted
// prefix: Runs is that prefix (a valid CheckRunsPrefix list inside Info.Size),
// a read at or after its end wraps filesys.ErrCorrupt (never io.ErrUnexpectedEOF
// and never zeros), and the cut is reported as a warning.
func TestTruncatedImageEndsTrustedPrefix(t *testing.T) {
	const bs = 4096
	data := pattern(12*bs, 7)
	img0, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "T"}, []hfsplustest.File{{Path: "/frag", Data: data, Fragment: 2}})
	exts := lay.Files["/frag"].Data
	if len(exts) != 6 {
		t.Fatalf("test setup: %d extents", len(exts))
	}
	for name, tc := range map[string]struct {
		cut        int64  // image length
		wantBlocks uint32 // blocks of the file that stay mapped
	}{
		"between extents":        {int64(exts[2].Start)*bs + 100, 4},            // extents 0 and 1 are whole, 2 starts at the cut
		"inside an extent":       {(int64(exts[1].Start)+1)*bs + 10, 3},         // extent 1 keeps its first block
		"inside the first":       {(int64(exts[0].Start)+1)*bs + 10, 1},         // extent 0 keeps one block
		"before the file":        {int64(exts[0].Start) * bs, 0},                // the whole file is cut away
		"on the last block edge": {int64(exts[5].Start+exts[5].Count) * bs, 12}, // nothing of the file is lost
	} {
		t.Run(name, func(t *testing.T) {
			img := img0[:tc.cut]
			f := open(t, img)
			_, fl := openPath(t, f, "/frag")
			size := fl.Size()
			covered, err := filesys.CheckRunsPrefix(fl.Runs(), size, f.Info().Size)
			if err != nil {
				t.Fatalf("Runs %v: %v", fl.Runs(), err)
			}
			if want := int64(tc.wantBlocks) * bs; covered != want {
				t.Fatalf("runs cover %d bytes, want %d (runs %v)", covered, want, fl.Runs())
			}
			if got := readRuns(img, fl.Runs()); !bytes.Equal(got, data[:covered]) {
				t.Error("the bytes at the runs differ from the file content")
			}
			buf := make([]byte, size-covered)
			if len(buf) > 0 {
				n, err := fl.ReadAt(buf, covered)
				if !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, io.ErrUnexpectedEOF) || n != 0 {
					t.Errorf("read at the prefix end = %d, %v; want 0 and an error wrapping ErrCorrupt", n, err)
				}
				if !hasWarning(f.Info(), "truncated") {
					t.Errorf("warnings = %q, want one about the truncated image", f.Info().Warnings)
				}
			}
			if covered > 0 {
				got := make([]byte, covered)
				if n, err := fl.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, data[:covered]) {
					t.Errorf("read of the prefix = %d, %v", n, err)
				}
			}
		})
	}
}
