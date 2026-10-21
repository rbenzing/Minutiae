package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
		// casefold directory: mixed case, names that fold to each other, non-UTF-8 and a shape the
		// comparison was not measured for
		{Path: "/cf", Dir: true, Casefold: true},
		{Path: "/cf/File.TXT", Inline: true},
		{Path: "/cf/Stra" + string(rune(0xDF)) + "e", Inline: true},
		{Path: "/cf/STRASSE", Inline: true},
		{Path: "/cf/bad", RawName: []byte{'A', 0xFF}, Inline: true},
		{Path: "/cf/odd", RawName: []byte{'x', 0xFF, 'y'}, Inline: true},
		{Path: "/cf/stray", RawName: []byte{'a', 0x80, 'b'}, Inline: true},
	}
	for i := range 120 { // a directory of several dentry blocks
		files = append(files, f2fstest.File{Path: "/many/entry-with-a-long-name-" + string(rune('a'+i%26)) + string(rune('A'+i/26)) + ".txt", Data: []byte("m"), Inline: true})
	}
	// A direct node (more blocks than the inode holds addresses) is in the
	// second seed only: its data makes the image several megabytes longer.
	withNode := append([]f2fstest.File{{Path: "/big.bin", Data: pat(1000*f2fstest.BlockSize, 3)}}, files...)
	seed := func(o f2fstest.Options, files []f2fstest.File) []byte { return trimTail(f2fstest.Build(o, files)) }
	return [][]byte{
		seed(f2fstest.Options{Segments: 2, Label: "SEED", Encoding: 1}, files),
		seed(f2fstest.Options{Segments: 5, ExtraAttr: true, InodeChksum: true, InodeCrtime: true, SBChksum: true, InlineXattr: true, CompactSum: true, Pack2Newer: true, Encoding: 1, EncodingFlags: 1}, withNode),
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

// fuzzSeedBudget bounds the seeds of one target in total. The engine's own limit is 100 MiB of shared memory
// (it panics above it). The current set, 85,620,306 bytes, ran 30 s through heavy.sh for 181,492 execs; the
// budget is 84 MiB (88,080,384 bytes), a little above that measured size and well under the limit.
const fuzzSeedBudget = 84 << 20

// fuzzOpenSeeds are the seeds of FuzzF2FSOpen: the builder images, the sload.f2fs fixtures cut after their
// last used block, and zeros. The kernel-made casefold image is not a seed: its directories start 16 MiB in and
// a seed that large would push the set over the engine's limit; the builder seeds carry casefold directories.
func fuzzOpenSeeds(t testing.TB) [][]byte {
	t.Helper()
	seeds := fuzzBuilderSeeds()
	for _, path := range fixturePaths(t) {
		seeds = append(seeds, trimmedFixture(t, path))
	}
	return append(seeds, make([]byte, 16384))
}

// A seed set above the engine's limit makes `go test -fuzz` panic before it runs one input. The summed seed
// bytes stay within the budget, and no seed exceeds fuzzSeedMax.
func TestFuzzF2FSOpenSeedsFitTheEngine(t *testing.T) {
	total := 0
	for i, s := range fuzzOpenSeeds(t) {
		if len(s) > fuzzSeedMax+4096 {
			t.Errorf("seed %d has %d bytes, above the %d byte cap of one seed", i, len(s), fuzzSeedMax)
		}
		total += len(s)
	}
	if total > fuzzSeedBudget {
		t.Fatalf("the FuzzF2FSOpen seeds total %d bytes, above the %d byte budget: `go test -fuzz` would panic (the engine's limit is 100 MiB); trim a seed", total, fuzzSeedBudget)
	}
	t.Logf("seed bytes: %d of %d", total, fuzzSeedBudget)
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
	for _, s := range fuzzOpenSeeds(f) {
		f.Add(s)
	}
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

// lookupBase is the image FuzzF2FSLookup edits: a plain directory of two dentry blocks, a casefold directory with
// names that fold to each other, and a few files. An F2FS image has a metadata area of 16 MiB or more, which the
// fuzz engine cannot mutate at speed as a whole input (it spends seconds per execution on it), so the fuzzed
// input is a small list of byte edits applied to this image instead.
type lookupBase struct {
	img    []byte
	tree   *f2fstest.Tree
	blocks []int // the 4 KiB blocks of img with a nonzero byte, the only ones edits address
}

var (
	lookupBaseOnce sync.Once
	lookupBaseVal  lookupBase
)

func fuzzLookupBase() *lookupBase {
	lookupBaseOnce.Do(func() {
		files := []f2fstest.File{{Path: "/d", Dir: true}, {Path: "/cf", Dir: true, Casefold: true}, {Path: "/a.txt", Data: []byte("hello"), Inline: true}}
		for i := range 250 {
			files = append(files, f2fstest.File{Path: fmt.Sprintf("/d/f%03d", i), Inline: true})
		}
		for _, n := range []string{"Stra" + string(rune(0xDF)) + "e", "STRASSE", "File.TXT", "x"} {
			files = append(files, f2fstest.File{Path: "/cf/" + n, Inline: true})
		}
		files = append(files, f2fstest.File{Path: "/cf/bad", RawName: []byte{'A', 0xFF}, Inline: true})
		img, tree := f2fstest.BuildTree(f2fstest.Options{Segments: 2, Encoding: 1}, files)
		b := lookupBase{img: img, tree: tree}
		for blk := 0; blk*bs < len(img); blk++ {
			if bytes.ContainsFunc(img[blk*bs:min(len(img), (blk+1)*bs)], func(r rune) bool { return r != 0 }) {
				b.blocks = append(b.blocks, blk)
			}
		}
		lookupBaseVal = b
	})
	return &lookupBaseVal
}

// edit encodes one byte edit of the base image: block selector (2 bytes, an index into the nonzero blocks),
// offset in the block (2 bytes) and the new value.
func (b *lookupBase) edit(blockAddr, off int, val byte) []byte {
	idx := slices.Index(b.blocks, blockAddr)
	if idx < 0 {
		panic("block is not in the base image")
	}
	return []byte{byte(idx), byte(idx >> 8), byte(off), byte(off >> 8), val}
}

// apply returns a copy of the base image with the edits of in applied (trailing bytes that do not make a whole
// edit are ignored).
func (b *lookupBase) apply(in []byte) []byte {
	img := bytes.Clone(b.img)
	for ; len(in) >= 5; in = in[5:] {
		blk := b.blocks[(int(in[0])|int(in[1])<<8)%len(b.blocks)]
		img[blk*bs+(int(in[2])|int(in[3])<<8)%bs] = in[4]
	}
	return img
}

// FuzzF2FSLookup applies byte edits to a small builder image (see lookupBase), then looks up, in every directory
// the walk finds, the name in upper case and a name that is not there: an answer or a typed error, never a panic,
// and a found entry always has an ID. It also holds the cut property (Z13): a lookup that said "not found" in a
// directory about which a scan warned that part of it was not read is a failure; it must have answered with the
// incomplete error. The second seed damages a dentry slot of /d, so the property is exercised by the seeds.
//
// Run the gate as
//
//	go test ./internal/filesys/f2fs -run '^$' -fuzz '^FuzzF2FSLookup$' -fuzztime 30s -fuzzminimizetime 0 -parallel 2
func FuzzF2FSLookup(f *testing.F) {
	base := fuzzLookupBase()
	f.Add([]byte{})
	// name_len of the record in slot 5 of /d's first dentry block becomes 0: a damaged slot
	f.Add(base.edit(int(base.tree.DirBlocks["/d"][0]), 30+5*11+8, 0))
	f.Fuzz(func(t *testing.T, in []byte) {
		b := base.apply(in)
		fsys, err := f2fs.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		visited := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(p string, _ filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > 300 {
				return errFuzzBudget
			}
			for _, q := range []string{strings.ToUpper(p), p + "~missing"} {
				got, err := fsys.Lookup(q)
				if err == nil && got.ID == "" {
					t.Fatalf("Lookup(%q) returned an entry without an ID", q)
				}
				if errors.Is(err, filesys.ErrNotFound) {
					assertNoCutBehindNotFound(t, fsys, q)
				}
			}
			return nil
		})
	})
}

// scanCutWarnings are the texts of the warnings a directory scan gives for a part of a directory it did not
// read (the cuts of dir.go). The entry cap's text is not among them: the walk's ReadDir counts deleted records
// toward the cap and a live-only lookup does not, so that warning does not prove the lookup was cut (the cap has
// unit tests).
var scanCutWarnings = []string{
	"its block map is damaged", "is unreadable", "damaged dentry slots", "only the first 64 MiB are read", "directory read budget of",
}

// assertNoCutBehindNotFound fails when Lookup(q) said "not found" in a directory about which a scan warned that
// it did not read all of it. Warnings are deduplicated and capped, so a missing warning proves nothing; a present
// one does.
func assertNoCutBehindNotFound(t *testing.T, fsys filesys.FileSystem, q string) {
	t.Helper()
	i := strings.LastIndexByte(q, '/')
	if i < 0 {
		return
	}
	parent := q[:i]
	if parent == "" {
		parent = "/"
	}
	pe, err := fsys.Lookup(parent)
	if err != nil || pe.Type != filesys.TypeDir {
		return
	}
	nid, ok := strings.CutPrefix(pe.ID, "nid:")
	if !ok {
		return
	}
	for _, w := range fsys.Info().Warnings {
		cut := strings.Contains(w, "shared by directories") && strings.HasSuffix(w, "and "+nid+"; skipped")
		if !cut && mentionsDirInode(w, nid) {
			for _, c := range scanCutWarnings {
				cut = cut || strings.Contains(w, c)
			}
		}
		if cut {
			t.Fatalf("Lookup(%q) said not found in a directory that was not read to the end: %s", q, w)
		}
	}
}

// mentionsDirInode reports whether warning w names "directory inode <nid>" (not a longer number).
func mentionsDirInode(w, nid string) bool {
	prefix := "directory inode " + nid
	for off := 0; ; {
		j := strings.Index(w[off:], prefix)
		if j < 0 {
			return false
		}
		off += j + len(prefix)
		if off == len(w) || w[off] < '0' || w[off] > '9' {
			return true
		}
	}
}
