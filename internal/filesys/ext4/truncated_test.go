package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// L32: a file whose mapped blocks run past the end of a truncated image opens with the bytes the image holds;
// every read beyond them is ErrCorrupt, never zeros. The cut classes: a block below the effective block count is
// readable, one between it and the declared count is valid but cut away, one at or above the declared count is
// corrupt (outside the filesystem).

const ttBS = 1024

func ttPat(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(seed*31 + i*7 + i/251)
	}
	return b
}

// ttRuns opens path on the whole image and returns its runs (offsets in the filesystem).
func ttRuns(t *testing.T, img []byte, path string) []filesys.Run {
	t.Helper()
	f := mustOpen(t, img)
	e, err := f.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", path, err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return fl.Runs()
}

// ttPhys is the filesystem offset of byte off of a file whose runs are runs.
func ttPhys(t *testing.T, runs []filesys.Run, off int64) int64 {
	t.Helper()
	for _, r := range runs {
		if off < r.Length {
			if r.Offset < 0 {
				t.Fatalf("file offset %d is in a hole", off)
			}
			return r.Offset + off
		}
		off -= r.Length
	}
	t.Fatalf("file offset beyond the runs")
	return 0
}

// ttOpenCut opens path on img[:cut] and returns the file and the FS.
func ttOpenCut(t *testing.T, img []byte, cut int64, path string) (*ext4.FS, filesys.File) {
	t.Helper()
	e, err := mustOpen(t, img).Lookup(path) // the cut may take the directory too: the entry comes from the whole image, Open reads the inode by ID
	if err != nil {
		t.Fatalf("Lookup(%s): %v", path, err)
	}
	f := mustOpen(t, img[:cut])
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%s) on the cut image: %v (want the trusted prefix)", path, err)
	}
	return f, fl
}

// ttCheckPrefix checks the L32 contract of a cut file against the original bytes: the runs are a valid prefix
// covering want bytes, those bytes equal orig, the first read past them is ErrCorrupt with no byte written, and
// CheckRuns (full coverage) rejects the list.
func ttCheckPrefix(t *testing.T, fl filesys.File, orig []byte, want int64, fsSize int64) {
	t.Helper()
	runs := fl.Runs()
	covered, err := filesys.CheckRunsPrefix(runs, fl.Size(), fsSize)
	if err != nil {
		t.Fatalf("Runs() is not a valid prefix: %v", err)
	}
	if covered != want {
		t.Fatalf("Runs() cover %d bytes, want %d", covered, want)
	}
	if fl.Size() != int64(len(orig)) {
		t.Fatalf("Size() = %d, want the unchanged %d", fl.Size(), len(orig))
	}
	if covered < fl.Size() && filesys.CheckRuns(runs, fl.Size(), fsSize) == nil {
		t.Errorf("CheckRuns accepted a prefix list")
	}
	got := make([]byte, covered)
	if n, err := fl.ReadAt(got, 0); n != len(got) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt of the prefix = %d, %v", n, err)
	}
	if !bytes.Equal(got, orig[:covered]) {
		t.Errorf("the prefix bytes differ from the original")
	}
	buf := bytes.Repeat([]byte{0xAA}, 100)
	n, err := fl.ReadAt(buf, covered)
	if !errors.Is(err, filesys.ErrCorrupt) || n != 0 {
		t.Errorf("ReadAt at the prefix end = %d, %v; want 0 bytes and ErrCorrupt", n, err)
	}
	for i, c := range buf {
		if c != 0xAA {
			t.Fatalf("a failed read wrote byte %d of the buffer (%#x): zeros must never stand for unread bytes", i, c)
		}
	}
	if covered > 0 { // a read that straddles the end returns the bytes before it, then the error
		buf = make([]byte, 64)
		n, err = fl.ReadAt(buf, covered-10)
		if n != 10 || !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadAt across the prefix end = %d, %v; want 10 bytes and ErrCorrupt", n, err)
		}
	}
}

func ttScatterImage() (img []byte, data []byte) {
	data = ttPat(8*ttBS, 1)
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: 2 * i, Data: data[2*i*ttBS : (2*i+2)*ttBS]})
	}
	img = ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Pieces: ps, Scatter: true},
	})
	return img, data
}

func TestTruncatedImageKeepsExtentPrefix(t *testing.T) {
	img, data := ttScatterImage()
	runs := ttRuns(t, img, "/f")
	if len(runs) != 4 {
		t.Fatalf("builder: %d runs, want 4 scattered extents", len(runs))
	}
	// cut inside extent 3 (index 2): its first block lies in the image, the second does not
	cut := runs[2].Offset + ttBS + 100
	f, fl := ttOpenCut(t, img, cut, "/f")
	ttCheckPrefix(t, fl, data, 5*ttBS, f.Info().Size)
}

func TestTruncatedImageBlockMapPrefix(t *testing.T) {
	data := ttPat(20*ttBS, 2)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: false}, []ext4test.File{{Path: "/f", Data: data}})
	runs := ttRuns(t, img, "/f")
	last := runs[len(runs)-1]
	ind := (last.Offset + last.Length) / ttBS // the builder allocates the indirect block right after the data
	if got := binary.LittleEndian.Uint32(img[ind*ttBS:]); int64(got)*ttBS != ttPhys(t, runs, 12*ttBS) {
		t.Fatalf("block %d is not the indirect block (first pointer %d)", ind, got)
	}
	t.Run("data pointer cut", func(t *testing.T) {
		// the indirect block lies in the image; its pointers 3.. are moved into the cut class
		patched := bytes.Clone(img)
		for i := 3; i < 8; i++ {
			binary.LittleEndian.PutUint32(patched[ind*ttBS+int64(i)*4:], uint32(900+i))
		}
		f, fl := ttOpenCut(t, patched, (ind+1)*ttBS, "/f")
		ttCheckPrefix(t, fl, data, 15*ttBS, f.Info().Size)
	})
	t.Run("indirect block cut", func(t *testing.T) {
		f, fl := ttOpenCut(t, img, ind*ttBS, "/f")
		ttCheckPrefix(t, fl, data, 12*ttBS, f.Info().Size)
	})
}

// ttFindAll returns the offsets of every occurrence of pat in img.
func ttFindAll(img, pat []byte) []int {
	var out []int
	for off := 0; ; {
		i := bytes.Index(img[off:], pat)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + 1
	}
}

// ttExtentRoot returns the offset of the only extent header in img with the given entry count and capacity 4
// (the i_block of the file under test).
func ttExtentRoot(t *testing.T, img []byte, entries byte, depth byte) int {
	t.Helper()
	at := ttFindAll(img, []byte{0x0a, 0xf3, entries, 0, 4, 0, depth, 0})
	if len(at) != 1 {
		t.Fatalf("%d extent roots with %d entries and depth %d, want exactly 1", len(at), entries, depth)
	}
	return at[0]
}

func TestTruncatedImageCutInsideIndexNode(t *testing.T) {
	data := ttPat(8*ttBS, 3)
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: 2 * i, Data: data[2*i*ttBS : (2*i+2)*ttBS]})
	}
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Pieces: ps, Scatter: true, ExtentLeaves: 2},
	})
	root := ttExtentRoot(t, img, 2, 1)
	leaf2 := int64(binary.LittleEndian.Uint32(img[root+12+12+4:]))
	f, fl := ttOpenCut(t, img, leaf2*ttBS, "/f")
	// the second leaf is cut away although the data it maps lies inside the image
	ttCheckPrefix(t, fl, data, 4*ttBS, f.Info().Size)
	if !hasWarning(f.Info(), "4096 bytes") {
		t.Errorf("warnings %q do not name the 4096 readable bytes", f.Info().Warnings)
	}
}

func TestTruncatedImageHoleBeforeCutIsKept(t *testing.T) {
	data := ttPat(2*ttBS, 4)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Scatter: true, Pieces: []ext4test.Piece{{Block: 2, Data: data}, {Block: 6, Data: data}}},
	})
	runs := ttRuns(t, img, "/f")
	var orig []byte
	orig = append(orig, make([]byte, 2*ttBS)...)
	orig = append(orig, data...)
	orig = append(orig, make([]byte, 2*ttBS)...)
	orig = append(orig, data...)
	cut := ttPhys(t, runs, 6*ttBS) + ttBS + 1 // second piece: first block inside, second beyond
	f, fl := ttOpenCut(t, img, cut, "/f")
	ttCheckPrefix(t, fl, orig, 7*ttBS, f.Info().Size)
	got := fl.Runs()
	if len(got) != 4 || got[0].Offset != -1 || got[0].Length != 2*ttBS || got[2].Offset != -1 || got[2].Length != 2*ttBS {
		t.Errorf("runs %+v: want hole, data, hole, one block of data", got)
	}
}

func TestUninitExtentInsideDeclaredButBeyondImageIsAHole(t *testing.T) {
	data := ttPat(2*ttBS, 5)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Scatter: true, Pieces: []ext4test.Piece{{Block: 0, Data: data}, {Block: 2, Data: ttPat(2*ttBS, 6), Uninit: true}}},
	})
	runs := ttRuns(t, img, "/f")
	var phys []int64
	for _, r := range runs {
		if r.Offset >= 0 {
			phys = append(phys, r.Offset)
		}
	}
	if len(phys) != 1 {
		t.Fatalf("runs %+v: want one data run and one hole", runs)
	}
	f := mustOpen(t, img)
	e, _ := f.Lookup("/f")
	whole, err := f.Open(e)
	if err != nil || len(whole.Runs()) != 2 {
		t.Fatalf("uncut open: %v, runs %+v", err, whole.Runs())
	}
	// cut right after the initialized extent: the uninitialized one (its data blocks are beyond the image) needs no data
	cut := phys[0] + 2*ttBS + 1
	cf, fl := ttOpenCut(t, img, cut, "/f")
	if err := filesys.CheckRuns(fl.Runs(), fl.Size(), cf.Info().Size); err != nil {
		t.Fatalf("a hole beyond the image must leave full coverage: %v", err)
	}
	got := readFile(t, fl)
	if !bytes.Equal(got, append(bytes.Clone(data), make([]byte, 2*ttBS)...)) {
		t.Errorf("content differs: want the data then zeros")
	}
}

// ttOpenFails opens /f on img and on img cut at the given offset: both must fail Open with ErrCorrupt.
func ttOpenFails(t *testing.T, img []byte, cuts ...int64) {
	t.Helper()
	for _, cut := range append([]int64{int64(len(img))}, cuts...) {
		f := mustOpen(t, img[:cut])
		e, err := f.Lookup("/f")
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		fl, err := f.Open(e)
		if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("image of %d bytes: Open = %v, want ErrCorrupt (no prefix)", cut, err)
		}
		if fl != nil {
			t.Errorf("image of %d bytes: Open returned a file for a damaged map", cut)
		}
	}
}

func TestExtentBeyondDeclaredFilesystemStaysCorrupt(t *testing.T) {
	img, _ := ttScatterImage()
	root := ttExtentRoot(t, img, 4, 0)
	// the first extent names a block beyond the 1024 blocks the superblock declares: outside the filesystem, not cut
	binary.LittleEndian.PutUint32(img[root+12+8:], 5000)
	ttOpenFails(t, img, int64(len(img))/2)
}

func TestOtherDamagedMapsStillFailToOpen(t *testing.T) {
	img, _ := ttScatterImage()
	root := ttExtentRoot(t, img, 4, 0)
	t.Run("forged depth", func(t *testing.T) {
		b := bytes.Clone(img)
		binary.LittleEndian.PutUint16(b[root+6:], 9)
		ttOpenFails(t, b, int64(len(b))*3/4)
	})
	t.Run("out of order extent", func(t *testing.T) {
		b := bytes.Clone(img)
		binary.LittleEndian.PutUint32(b[root+12+12*1:], 5) // extent 1 now ends after the start of extent 2
		ttOpenFails(t, b, int64(len(b))*3/4)
	})
	t.Run("looping index", func(t *testing.T) {
		data := ttPat(4*ttBS, 11)
		var ps []ext4test.Piece
		for i := range 4 {
			ps = append(ps, ext4test.Piece{Block: i, Data: data[i*ttBS : (i+1)*ttBS]})
		}
		b := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
			{Path: "/f", Pieces: ps, Scatter: true, ExtentLeaves: 2},
		})
		r := ttExtentRoot(t, b, 2, 1)
		copy(b[r+24+4:r+24+8], b[r+12+4:r+12+8]) // index entry 1 names the node of entry 0
		ttOpenFails(t, b, int64(len(b))*3/4)
	})
}

func TestTruncatedImageFirstBlockCutHasNoRuns(t *testing.T) {
	data := ttPat(3*ttBS, 7)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{{Path: "/f", Data: data}})
	runs := ttRuns(t, img, "/f")
	f, fl := ttOpenCut(t, img, runs[0].Offset, "/f")
	if r := fl.Runs(); r != nil {
		t.Errorf("Runs() = %+v, want nil for a file cut before its first block", r)
	}
	ttCheckPrefix(t, fl, data, 0, f.Info().Size)
}

func TestTruncatedImageWarningNamesTheLoss(t *testing.T) {
	data := ttPat(6*ttBS, 8)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Scatter: true, Pieces: []ext4test.Piece{
			{Block: 0, Data: data[:2*ttBS]}, {Block: 2, Data: data[2*ttBS : 4*ttBS]}, {Block: 4, Data: data[4*ttBS:]},
		}},
	})
	runs := ttRuns(t, img, "/f")
	// swap the physical blocks of extents 2 and 3: extent 2 now lies beyond the cut, extent 3 inside the image
	root := ttExtentRoot(t, img, 3, 0)
	e1, e2 := root+12+12, root+12+24
	p1, p2 := binary.LittleEndian.Uint32(img[e1+8:]), binary.LittleEndian.Uint32(img[e2+8:])
	binary.LittleEndian.PutUint32(img[e1+8:], p2)
	binary.LittleEndian.PutUint32(img[e2+8:], p1)
	cut := ttPhys(t, runs, 4*ttBS) + ttBS // extent 2's new block: one block inside, one beyond
	f, fl := ttOpenCut(t, img, cut, "/f")
	// logical blocks 2-3 now read the data that lies at extent 3's old place, 4-5 the data of extent 2
	swapped := slices.Concat(data[:2*ttBS], data[4*ttBS:], data[2*ttBS:4*ttBS])
	ttCheckPrefix(t, fl, swapped, 3*ttBS, f.Info().Size)
	var w string
	for _, s := range f.Info().Warnings {
		if strings.Contains(s, "inode") && strings.Contains(s, "truncated") {
			w = s
		}
	}
	if !strings.Contains(w, "3072 bytes") {
		t.Errorf("warning %q does not name the 3072 covered bytes", w)
	}
	if !strings.Contains(w, "2048 bytes") || !strings.Contains(w, "after the cut") {
		t.Errorf("warning %q does not say that 2048 bytes are mapped after the cut", w)
	}
}

func TestTruncatedFixtureEqualsUntruncatedPrefix(t *testing.T) {
	const path = "testdata/ext4-4k-csum.img.gz"
	img := gunzipFixture(t, path)
	want := loadOracle(t, path)
	whole := mustOpen(t, img)
	cut := ttDataMidpoint(t, whole, want)
	cutFS := mustOpen(t, img[:cut])
	var inside, partial int
	for _, o := range want.Files {
		if o.Type != "file" || o.Size == 0 {
			continue
		}
		e, err := whole.Lookup(o.Path)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", o.Path, err)
		}
		wf, err := whole.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		orig := readFile(t, wf)
		var end int64
		for _, r := range wf.Runs() {
			if r.Offset >= 0 {
				end = max(end, r.Offset+r.Length)
			}
		}
		cf, err := cutFS.Open(e) // Open reads the inode by ID only
		if err != nil {
			t.Fatalf("Open(%s) on the cut image: %v", o.Path, err)
		}
		if end <= cut {
			inside++
			sum := sha256.Sum256(readFile(t, cf))
			if hex.EncodeToString(sum[:]) != o.SHA256 {
				t.Errorf("%s lies inside the cut but differs from the oracle", o.Path)
			}
			continue
		}
		partial++
		covered, err := filesys.CheckRunsPrefix(cf.Runs(), cf.Size(), cutFS.Info().Size)
		if err != nil {
			t.Fatalf("%s: %v", o.Path, err)
		}
		ttCheckPrefix(t, cf, orig, covered, cutFS.Info().Size)
	}
	if inside == 0 || partial == 0 {
		t.Fatalf("the cut must split the fixture's files: %d inside, %d cut", inside, partial)
	}
}

func TestTruncatedPrefixNeverAllocatesFromSize(t *testing.T) {
	data := ttPat(2*ttBS, 9)
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Data: data, Size: 1 << 44},
	})
	runs := ttRuns(t, img, "/f")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, fl := ttOpenCut(t, img, runs[0].Offset, "/f")
	if fl.Size() != 1<<44 {
		t.Fatalf("Size() = %d", fl.Size())
	}
	buf := make([]byte, 64)
	if _, err := fl.ReadAt(buf, 0); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("first read = %v, want ErrCorrupt", err)
	}
	runtime.ReadMemStats(&after)
	if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
		t.Errorf("opening a cut file of declared size 1<<44 allocated %d bytes", d)
	}
	if n := len(fl.Runs()); n > 1<<20 {
		t.Errorf("%d runs, above maxFileRuns", n)
	}
}

func TestCutDoesNotLoop(t *testing.T) {
	data := ttPat(4*ttBS, 10)
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: i, Data: data[i*ttBS : (i+1)*ttBS]})
	}
	img := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Pieces: ps, Scatter: true, ExtentLeaves: 2},
	})
	root := ttExtentRoot(t, img, 2, 1)
	// both index entries name the same block, beyond the cut
	binary.LittleEndian.PutUint32(img[root+12+4:], 900)
	binary.LittleEndian.PutUint32(img[root+24+4:], 900)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f := mustOpen(t, img[:200*ttBS])
		e, err := f.Lookup("/f")
		if err != nil {
			t.Error(err)
			return
		}
		fl, err := f.Open(e)
		if err != nil {
			t.Errorf("Open: %v", err)
			return
		}
		if len(fl.Runs()) != 0 {
			t.Errorf("runs %+v behind a cut index node", fl.Runs())
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the walk over index nodes pointing into the cut region did not finish")
	}
}

// ttDataMidpoint is the offset halfway through the span the fixture's regular files occupy, so the cut leaves
// the metadata (descriptors, bitmaps, inode tables) and splits the files.
func ttDataMidpoint(t *testing.T, f *ext4.FS, o oracle) int64 {
	t.Helper()
	lo, hi := int64(1<<62), int64(0)
	for _, of := range o.Files {
		if of.Type != "file" || of.Size == 0 {
			continue
		}
		e, err := f.Lookup(of.Path)
		if err != nil {
			t.Fatal(err)
		}
		fl, err := f.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range fl.Runs() {
			if r.Offset >= 0 {
				lo, hi = min(lo, r.Offset), max(hi, r.Offset+r.Length)
			}
		}
	}
	return (lo + hi) / 2
}

// The cut seeds of FuzzExt4Open must carry files that open with a prefix, or the fuzz premise (a prefix is
// opened, listed and read) would silently not be exercised.
func TestFuzzTruncatedSeedsCarryPrefixes(t *testing.T) {
	for i, seed := range fuzzTruncatedSeeds() {
		f := mustOpen(t, seed)
		cutFiles := 0
		_ = filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err != nil || e.Type != filesys.TypeFile {
				return nil
			}
			fl, err := f.Open(e)
			if err != nil {
				return nil
			}
			if covered, err := filesys.CheckRunsPrefix(fl.Runs(), fl.Size(), f.Info().Size); err == nil && covered < fl.Size() {
				cutFiles++
			}
			return nil
		})
		if cutFiles == 0 {
			t.Errorf("seed %d: no file opens with a prefix", i)
		}
	}
}

// ttCutWarning returns the truncation warning of the reader.
func ttCutWarning(t *testing.T, f *ext4.FS) string {
	t.Helper()
	for _, s := range f.Info().Warnings {
		if strings.Contains(s, "the image is truncated") && strings.Contains(s, "inode") {
			return s
		}
	}
	t.Fatalf("no truncation warning in %q", f.Info().Warnings)
	return ""
}

// Z67: the after-cut figure comes from the cut extent's own node only. Under an index node the other nodes are not
// examined, so the warning says "at least" and that the count may be incomplete, also when the figure is 0.
func TestTruncatedWarningUnderIndexNodeSaysCountMayBeIncomplete(t *testing.T) {
	data := ttPat(12*ttBS, 12)
	var ps []ext4test.Piece
	for i := range 6 {
		ps = append(ps, ext4test.Piece{Block: 2 * i, Data: data[2*i*ttBS : (2*i+2)*ttBS]})
	}
	base := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Pieces: ps, Scatter: true, ExtentLeaves: 2},
	})
	root := ttExtentRoot(t, base, 2, 1)
	leaf1 := int64(binary.LittleEndian.Uint32(base[root+12+4:]))
	leaf2 := int64(binary.LittleEndian.Uint32(base[root+24+4:]))
	// the second leaf is cut away; in the first, extent 1 is moved beyond the image (a cut inside leaf 1)
	patch := func(exts ...int) []byte {
		b := bytes.Clone(base)
		for _, k := range exts {
			binary.LittleEndian.PutUint32(b[leaf1*ttBS+12+int64(k)*12+8:], 900)
		}
		return b
	}
	t.Run("figure above zero", func(t *testing.T) {
		f, fl := ttOpenCut(t, patch(1), leaf2*ttBS, "/f")
		ttCheckPrefix(t, fl, data, 2*ttBS, f.Info().Size)
		w := ttCutWarning(t, f)
		for _, want := range []string{"at least 2048 bytes", "not examined", "the count may be incomplete"} {
			if !strings.Contains(w, want) {
				t.Errorf("warning %q lacks %q", w, want)
			}
		}
	})
	t.Run("figure zero", func(t *testing.T) {
		f, fl := ttOpenCut(t, patch(1, 2), leaf2*ttBS, "/f")
		ttCheckPrefix(t, fl, data, 2*ttBS, f.Info().Size)
		w := ttCutWarning(t, f)
		if !strings.Contains(w, "may be incomplete") || !strings.Contains(w, "not examined") || strings.Contains(w, "at least") {
			t.Errorf("warning %q: want the may-be-incomplete sentence without a figure", w)
		}
	})
	t.Run("root node is complete", func(t *testing.T) {
		img, d := ttScatterImage()
		runs := ttRuns(t, img, "/f")
		f, fl := ttOpenCut(t, img, runs[2].Offset+ttBS+100, "/f")
		ttCheckPrefix(t, fl, d, 5*ttBS, f.Info().Size)
		if w := ttCutWarning(t, f); strings.Contains(w, "may be incomplete") {
			t.Errorf("warning %q: an extent in the inode has no unexamined nodes", w)
		}
	})
}

// Z68: the count stops at the first later entry that overlaps or does not increase: nothing after a damaged
// entry is trusted.
func TestTruncatedWarningCountStopsAtADamagedEntry(t *testing.T) {
	data := ttPat(8*ttBS, 13)
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: 2 * i, Data: data[2*i*ttBS : (2*i+2)*ttBS]})
	}
	base := ext4test.Build(ext4test.Options{BlockSize: ttBS, Extents: true}, []ext4test.File{
		{Path: "/f", Pieces: ps, Scatter: true},
	})
	root := ttExtentRoot(t, base, 4, 0)
	binary.LittleEndian.PutUint32(base[root+12+12+8:], 900) // extent 1 is beyond the image: the cut
	warn := func(b []byte) string {
		f := mustOpen(t, b[:200*ttBS])
		e, err := f.Lookup("/f")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Open(e); err != nil {
			t.Fatal(err)
		}
		return ttCutWarning(t, f)
	}
	if w := warn(base); !strings.Contains(w, "at least 4096 bytes") {
		t.Errorf("control: warning %q does not count the two intact entries", w)
	} else if strings.Contains(w, "damaged entry") {
		t.Errorf("control: warning %q names a damaged entry in an intact node", w)
	}
	forged := bytes.Clone(base)
	binary.LittleEndian.PutUint32(forged[root+12+24:], 1) // extent 2 starts inside extent 0..1: overlaps
	if w := warn(forged); strings.Contains(w, "also mapped after the cut") {
		t.Errorf("warning %q counts entries after an overlapping one", w)
	}
	if w := warn(forged); !strings.Contains(w, "stopped at a damaged entry and may be incomplete") {
		t.Errorf("warning %q does not say the count stopped at a damaged entry (root node)", w)
	}
}
