package f2fs_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // bytes read from the start of each file (larger files are read up to here)
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzLookups    = 64      // Lookup calls per input (each scans every directory on its path)
	fuzzReadChunk  = 64 << 10

	// fuzzSeedMax bounds a seed. The fuzz engine hands each corpus entry to its
	// worker in a text form that spends up to four bytes per byte (a zero byte
	// is written as a four-character escape) in a 100 MiB shared buffer, and an
	// F2FS image is mostly zeros with its main area starting 16 MiB in, so a
	// seed above about 25 MiB makes the engine panic before it runs anything.
	fuzzSeedMax = 23 << 20
)

var errFuzzBudget = errors.New("walk budget reached")

// fuzzBuilderSeeds are small synthetic images covering the main on-disk
// variants: inline and block data, a file with a direct node, a multi-block
// directory, an inline directory, deleted dentries, symlinks, encrypted and
// compressed inodes, with and without extra attributes.
func fuzzBuilderSeeds() [][]byte {
	pat := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7+i/251) + seed
		}
		return b
	}
	files := []f2fstest.File{
		{Path: "/a.txt", Data: []byte("hello"), Inline: true, Times: [4]int64{1700000000, 1700000001, 1700000002, 1700000003}},
		{Path: "/small.bin", Data: pat(5000, 1)},
		{Path: "/d", Dir: true, Mode: 0o755},
		{Path: "/d/mid.bin", Data: pat(60*f2fstest.BlockSize+17, 2)},
		{Path: "/d/gone.txt", Data: []byte("deleted"), Deleted: true},
		{Path: "/d/inl", Dir: true, Inline: true},
		{Path: "/d/inl/x", Data: []byte("x"), Inline: true},
		{Path: "/link-short", Symlink: "a.txt"},
		{Path: "/link-long", Symlink: "/" + string(bytes.Repeat([]byte("long/"), 400)) + "end"},
		{Path: "/enc.bin", Data: pat(2*f2fstest.BlockSize, 4), Encrypted: true},
		{Path: "/compressed.bin", Data: pat(3*f2fstest.BlockSize, 5), Compressed: true},
		{Path: "/empty"},
	}
	for i := range 120 { // a directory of several dentry blocks
		files = append(files, f2fstest.File{Path: "/many/entry-with-a-long-name-" + string(rune('a'+i%26)) + string(rune('A'+i/26)) + ".txt", Data: []byte("m"), Inline: true})
	}
	// A direct node (more blocks than the inode holds addresses) is in the
	// second seed only: its data makes the image several megabytes longer.
	withNode := append([]f2fstest.File{{Path: "/big.bin", Data: pat(1000*f2fstest.BlockSize, 3)}}, files...)
	seed := func(o f2fstest.Options, files []f2fstest.File) []byte { return trimTail(f2fstest.Build(o, files)) }
	return [][]byte{
		seed(f2fstest.Options{Segments: 2, Label: "SEED"}, files),
		seed(f2fstest.Options{Segments: 5, ExtraAttr: true, InodeChksum: true, InodeCrtime: true, SBChksum: true, InlineXattr: true, CompactSum: true, Pack2Newer: true}, withNode),
	}
}

// fuzzForgedIDs are canonical-looking IDs that were never produced by a
// listing: node numbers at the edges of the id space and in the reserved range,
// and a deleted-slot ID. Open and ReadDir must reject or serve them without a
// panic whatever the image holds.
var fuzzForgedIDs = []string{
	"nid:1", "nid:2", "nid:3", "nid:4", "nid:5", "nid:6", "nid:7", "nid:8", "nid:9", "nid:10",
	"nid:100", "nid:1000", "nid:232960", "nid:1000000000", "nid:4294967295",
	"dentry:3:0:0", "dentry:3:7:12",
}

// FuzzF2FSOpen runs the reader over arbitrary bytes: Open, a bounded Walk, Open
// and read of every file (the first 1 MiB of a large one), Lookup of the paths
// the walk found, Open and ReadDir of forged IDs, and Unallocated. It calls the
// f2fs functions directly (not through detect.OpenWith), so a panic fails the
// fuzz.
//
// Run the 60 s gate as
//
//	go test ./internal/filesys/f2fs -run '^$' -fuzz FuzzF2FSOpen -fuzztime 60s -fuzzminimizetime 0 -parallel 3
//
// with -parallel 2 to 4: each worker holds seed-sized (up to 23 MiB) inputs, so
// one worker per CPU (the default) is memory-heavy on a many-core machine and
// leaves little of the 60 s for the reader itself.
func FuzzF2FSOpen(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Add(make([]byte, 16384))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := f2fs.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		_ = fsys.Info()
		visited, readTotal, lookups := 0, int64(0), 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > fuzzWalkBudget {
				return errFuzzBudget
			}
			if lookups < fuzzLookups {
				lookups++
				if got, lerr := fsys.Lookup(p); lerr == nil && got.ID == "" {
					t.Fatalf("Lookup(%q) returned an entry without an ID", p)
				}
			}
			if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
				return nil
			}
			file, err := fsys.Open(e)
			if err != nil || readTotal >= fuzzReadBudget {
				return nil
			}
			checkFuzzRuns(t, e.Name, file, fsys.Info().Size)
			end := min(file.Size(), fuzzFileMax) // only the first 1 MiB of a large file
			buf := make([]byte, min(end, fuzzReadChunk))
			for off := int64(0); off < end; off += int64(len(buf)) {
				n, rerr := file.ReadAt(buf, off)
				readTotal += int64(n)
				if rerr != nil {
					break
				}
			}
			return nil
		})
		for _, id := range fuzzForgedIDs {
			_, _ = fsys.Open(filesys.Entry{ID: id})
			_, _ = fsys.ReadDir(filesys.Entry{ID: id})
		}
		runs, err := fsys.Unallocated()
		if err != nil {
			return
		}
		size := fsys.Info().Size
		for _, r := range runs {
			if r.Length <= 0 || r.Offset < 0 || r.Offset+r.Length > size {
				t.Fatalf("unallocated run %+v outside the %d-byte filesystem", r, size)
			}
		}
	})
}

// checkFuzzRuns checks the File.Runs contract of a file the reader opened:
// the runs cover exactly [0, Size()) or, when the node chain is truncated or
// corrupt, a valid strict prefix of it, in which case a read at the end of
// the prefix must fail with an error wrapping filesys.ErrCorrupt. A file with
// no run at all is either inline (its bytes live in the inode, so the read
// succeeds) or corrupt from its first block (ErrCorrupt).
func checkFuzzRuns(t *testing.T, name string, file filesys.File, fsSize int64) {
	t.Helper()
	runs, size := file.Runs(), file.Size()
	covered, err := filesys.CheckRunsPrefix(runs, size, fsSize)
	if err != nil {
		t.Fatalf("runs of %s violate the File.Runs contract: %v", name, err)
	}
	if covered == size {
		if err := filesys.CheckRuns(runs, size, fsSize); err != nil { // the exact-cover rule
			t.Fatalf("runs of %s violate the File.Runs contract: %v", name, err)
		}
		return
	}
	n, err := file.ReadAt(make([]byte, 1), covered)
	if len(runs) == 0 && n == 1 && err == nil {
		return // inline data: no runs, the bytes live in the inode
	}
	if n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("read of %s at the end of its %d-byte run prefix (size %d) = %d, %v; want 0 bytes and ErrCorrupt", name, covered, size, n, err)
	}
}

// The builder seeds must be images the reader accepts and lists, or the fuzz
// would start from nothing.
func TestFuzzBuilderSeedsOpen(t *testing.T) {
	for i, s := range fuzzBuilderSeeds() {
		fsys, err := f2fs.Open(bytes.NewReader(s), int64(len(s)))
		if err != nil {
			t.Errorf("seed %d: %v", i, err)
			continue
		}
		n := 0
		if err := filesys.Walk(fsys, fsys.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
			if err != nil {
				return err
			}
			n++
			return nil
		}); err != nil || n < 100 {
			t.Errorf("seed %d: walk visited %d entries, err %v", i, n, err)
		}
	}
}

// trimmedFixture returns a fixture cut after the last block the oracle uses
// (a data block or an inode's node block) plus a slack, and in any case after
// fuzzSeedMax bytes: the first 2 MiB of an image never reach the main area,
// where everything of interest lives. The reader accepts an image shorter than
// its block_count, so the cut image is still a valid seed. Only the start of
// the main area of these fixtures fits under the cap, which covers the
// checkpoint, NAT and SIT areas, the root directory and the first inodes.
func trimmedFixture(t testing.TB, path string) []byte {
	t.Helper()
	img, want := gunzipFixture(t, path), loadOracle(t, path)
	last := int64(want.Root.NATBlkaddr)
	for _, in := range want.Inodes {
		last = max(last, int64(in.NATBlkaddr))
	}
	for _, ranges := range want.FileBlocks {
		for _, r := range ranges {
			last = max(last, r[1])
		}
	}
	end := min((last+1)*int64(want.BlockSize)+64<<10, fuzzSeedMax)
	return trimTail(img[:min(int64(len(img)), end)])
}

// trimTail cuts an image after its last non-zero byte plus a block's worth of
// slack: whatever follows is unused space, and the fuzz engine's throughput
// collapses on multi-megabyte inputs.
func trimTail(img []byte) []byte {
	end := len(img)
	for end > 0 && img[end-1] == 0 {
		end--
	}
	return img[:min(len(img), end+4096)]
}
