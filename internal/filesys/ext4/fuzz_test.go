package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // files larger than this are opened but not read
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzReadChunk  = 64 << 10
	fuzzEAOpens    = 20 // EA-inode values opened per input: each is hashed whole, up to 16 MiB
)

var errFuzzBudget = errors.New("walk budget reached")

// fuzzBuilderSeeds are small synthetic images covering the main on-disk
// variants: extents, block maps, htree, inline data, deleted entries, checksums.
func fuzzBuilderSeeds() [][]byte {
	data := bytes.Repeat([]byte("fuzz seed data "), 400)
	return [][]byte{
		ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, Label: "seed", Groups: 2},
			[]ext4test.File{
				{Path: "/a.txt", Data: []byte("hello")},
				{Path: "/d", Dir: true},
				{Path: "/d/big.bin", Data: data},
				{Path: "/d/gone.txt", Data: []byte("x"), Deleted: true},
				{Path: "/link", Symlink: "a.txt"},
				{Path: "/long", Symlink: string(bytes.Repeat([]byte("p/"), 60))},
			}),
		ext4test.Build(ext4test.Options{BlockSize: 1024, Extents: false},
			[]ext4test.File{
				{Path: "/m.bin", Data: bytes.Repeat([]byte{7}, 300<<10)},
				{Path: "/e", Dir: true},
			}),
		ext4test.Build(ext4test.Options{BlockSize: 4096, Extents: true, InlineData: true, MetadataCsum: true},
			[]ext4test.File{
				{Path: "/tiny", Data: []byte("inline"), Inline: true},
				{Path: "/idir", Dir: true, Inline: true},
				{Path: "/idir/x", Data: []byte("y"), Inline: true},
			}),
		// values held in EA inodes: a plain one, one that names itself, and inline content whose system.data is in an EA
		// inode (the unsupported layout)
		ext4test.Build(ext4test.Options{Extents: true, InlineData: true, MetadataCsum: true},
			[]ext4test.File{
				{Path: "/e", Data: []byte("e"), Xattrs: []ext4test.Xattr{
					{Name: "user.ea", Value: bytes.Repeat([]byte("ea value "), 300), EAInode: true},
					{Name: "user.loop", Value: []byte("loop"), EAInode: true, EAXattrs: []ext4test.Xattr{{Name: "user.self", Inum: 14, Size: 4}}},
				}},
				{Path: "/sd", Data: bytes.Repeat([]byte("s"), 150), Inline: true, SystemDataInEAInode: true},
			}),
		// an xattr block under metadata_csum, so a mutation of the header, the entries or h_checksum reaches the
		// verification
		ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true},
			[]ext4test.File{
				{Path: "/x", Data: []byte("x"), XattrBlock: true, Xattrs: []ext4test.Xattr{
					{Name: "user.alpha", Value: []byte("first value")},
					{Name: "security.selinux", Value: bytes.Repeat([]byte("v"), 200)},
				}},
			}),
		ext4test.Build(ext4test.Options{Extents: true},
			[]ext4test.File{
				{Path: "/h", Dir: true, HTree: true},
				{Path: "/h/one", Data: []byte("1")},
				{Path: "/h/two", Data: []byte("2")},
			}),
		// casefold directories: mixed case, a name that folds to several others, non-UTF-8 and a
		// shape the comparison was not measured for
		ext4test.Build(ext4test.Options{Extents: true, Encoding: 1},
			[]ext4test.File{
				{Path: "/cf", Dir: true, Flags: 0x40000000},
				{Path: "/cf/File.TXT"},
				{Path: "/cf/Stra" + string(rune(0xDF)) + "e"},
				{Path: "/cf/STRASSE"},
				{Path: "/cf/A" + string([]byte{0xFF})},
				{Path: "/cf/x" + string([]byte{0xFF}) + "y"},
				{Path: "/cf/a" + string([]byte{0x80}) + "b"},
			}),
		ext4test.Build(ext4test.Options{Extents: true, Encoding: 1, EncodingFlags: 1},
			[]ext4test.File{
				{Path: "/cf", Dir: true, Flags: 0x40000000},
				{Path: "/cf/Ab", Data: []byte("1")},
				{Path: "/cf/aB", Data: []byte("2")},
			}),
	}
}

// fuzzCasefoldSeedMax (4 MiB, ruling Z49) cuts the real casefold image: the start (superblock, descriptors, bitmaps, the first
// inode tables and directories) is what a mutation can usefully damage, and the reader accepts an image
// shorter than its block count.
const fuzzCasefoldSeedMax = 4 << 20

// fuzzSeedBudget is what the fuzz engine can take: it refuses a seed set above 100 MiB in total ("value
// length larger than shared memory capacity") and the whole target then cannot run at all.
const fuzzSeedBudget = 64 << 20

// fuzzOpenSeeds are the seeds of FuzzExt4Open: the builder images, the mke2fs fixtures, the first 4 MiB of the
// kernel-made casefold image (by name: its expect schema is not the one of the fixture loops), and zeros.
func fuzzOpenSeeds(t testing.TB) [][]byte {
	t.Helper()
	seeds := fuzzBuilderSeeds()
	for _, path := range fixturePaths(t) {
		seeds = append(seeds, gunzipFixture(t, path))
	}
	cf := gunzipFixture(t, filepath.Join("testdata", "ext4-casefold.img.gz"))
	seeds = append(seeds, cf[:min(len(cf), fuzzCasefoldSeedMax)])
	seeds = append(seeds, fuzzTruncatedSeeds()...)
	return append(seeds, make([]byte, 2048))
}

// A seed set above the engine's limit makes `go test -fuzz FuzzExt4Open` panic before it runs one input
// (the 32 MiB casefold image added whole did exactly that). The summed seed bytes stay within the budget.
func TestFuzzExt4OpenSeedsFitTheEngine(t *testing.T) {
	total := 0
	for _, s := range fuzzOpenSeeds(t) {
		total += len(s)
	}
	if total > fuzzSeedBudget {
		t.Fatalf("the FuzzExt4Open seeds total %d bytes, above the %d byte budget: `go test -fuzz` would panic (the engine's limit is 100 MiB); trim a seed", total, fuzzSeedBudget)
	}
	t.Logf("seed bytes: %d of %d", total, fuzzSeedBudget)
}

// FuzzExt4Open runs the reader over arbitrary bytes: Open, a bounded Walk,
// Open and read of every file, and Unallocated. It calls the ext4 functions
// directly (not through detect.OpenWith), so a panic fails the fuzz.
func FuzzExt4Open(f *testing.F) {
	for _, s := range fuzzOpenSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := ext4.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		_ = fsys.Info()
		visited, readTotal := 0, int64(0)
		eaOpened := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > fuzzWalkBudget {
				return errFuzzBudget
			}
			if visited <= 200 { // a casefold lookup of every listed name, upper-cased: an answer or a typed error, never a panic
				_, _ = fsys.Lookup(strings.ToUpper(p))
			}
			if eaOpened < fuzzEAOpens {
				eaOpened += fuzzEAValues(t, fsys, e)
			}
			if visited <= fuzzXattrEntries {
				fuzzXattrReader(t, fsys, e)
			}
			if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
				return nil
			}
			file, err := fsys.Open(e)
			if err != nil || file.Size() > fuzzFileMax || readTotal >= fuzzReadBudget {
				return nil
			}
			covered := file.Size()
			if runs := file.Runs(); runs != nil {
				// a file cut by a truncated image lists a prefix (L32): the runs are valid, and a read past them is ErrCorrupt
				cov, err := filesys.CheckRunsPrefix(runs, file.Size(), fsys.Info().Size)
				if err != nil {
					t.Fatalf("runs of %s violate the File.Runs contract: %v", e.Name, err)
				}
				covered = cov
			}
			if covered < file.Size() {
				if _, rerr := file.ReadAt(make([]byte, 1), covered); !errors.Is(rerr, filesys.ErrCorrupt) {
					t.Fatalf("%s: a read at the end of its %d-byte prefix returned %v, want ErrCorrupt", e.Name, covered, rerr)
				}
			}
			buf := make([]byte, min(file.Size(), fuzzReadChunk))
			for off := int64(0); off < file.Size(); off += int64(len(buf)) {
				n, rerr := file.ReadAt(buf, off)
				readTotal += int64(n)
				if rerr != nil {
					break
				}
			}
			return nil
		})
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

// FuzzExt4CasefoldLookup mutates small casefold images (the large fixtures of FuzzExt4Open exceed the fuzz
// engine's seed limit) and looks up names in every directory, in upper and lower case and a missing one: an
// answer or a typed error, never a panic, and a found entry always comes with a name.
func FuzzExt4CasefoldLookup(f *testing.F) {
	for _, s := range fuzzBuilderSeeds()[4:] {
		f.Add(s)
	}
	f.Add(fuzzCutSeed(f)) // a directory whose second block has a forged rec_len: the cut property is exercised by the seed
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := ext4.Open(bytes.NewReader(b), int64(len(b)))
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
			for _, q := range []string{strings.ToUpper(p), strings.ToLower(p), p + "~missing"} {
				got, err := fsys.Lookup(q)
				if err == nil && got.Name == "" && q != "/" {
					t.Fatalf("Lookup(%q) found an entry with no name", q)
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
// examine (the cuts of dir.go).
var scanCutWarnings = []string{
	"its block map is damaged", "(sparse directory)", "is unreadable", "exceeds the 64 MiB directory limit",
	"too short for a record", "the rest of the block is not read", "does not fit rec_len", "entries; the rest are not read",
}

// assertNoCutBehindNotFound fails when Lookup(q) said "not found" in a directory about which a scan warned that
// it did not read all of it: the lookup must then have answered with the incomplete error. Warnings are
// deduplicated and capped, so a missing warning proves nothing; a present one does.
func assertNoCutBehindNotFound(t *testing.T, fsys filesys.FileSystem, q string) {
	t.Helper()
	i := strings.LastIndexByte(q, '/')
	if i < 0 || len(q)-i-1 > 255 {
		return // no parent, or a name no directory record can hold
	}
	parent := q[:i]
	if parent == "" {
		parent = "/"
	}
	pe, err := fsys.Lookup(parent)
	if err != nil {
		return
	}
	prefix := "directory " + strings.Replace(pe.ID, ":", " ", 1)
	for _, w := range fsys.Info().Warnings {
		if !strings.Contains(w, prefix+":") && !strings.Contains(w, prefix+",") {
			continue
		}
		for _, c := range scanCutWarnings {
			if strings.Contains(w, c) {
				t.Fatalf("Lookup(%q) said not found in a directory that was not read to the end: %s", q, w)
			}
		}
	}
}

// fuzzCutSeed is a two-block directory /d whose second block starts with a record of rec_len 0, so a lookup in
// /d that finds nothing must be the incomplete error (and the scan warns that the rest of the block is not read).
func fuzzCutSeed(tb testing.TB) []byte {
	tb.Helper()
	img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: []byte("a")},
		{Path: "/d/b", Data: []byte("b"), NewBlock: true},
	})
	fsys, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		tb.Fatal(err)
	}
	d, err := fsys.Lookup("/d")
	if err != nil {
		tb.Fatal(err)
	}
	runs, err := fsys.DirRuns(d)
	if err != nil || len(runs) == 0 || runs[0].Length < 2048 {
		tb.Fatalf("directory runs %+v, %v: want two blocks in the first run", runs, err)
	}
	binary.LittleEndian.PutUint16(img[runs[0].Offset+1024+4:], 0)
	return img
}

// fuzzEAValues opens every attribute value of entry e that is held in an EA inode and reads it in bounded
// chunks: a value or a typed error, never a panic, and its runs obey the File.Runs contract. It returns how many
// values it opened.
func fuzzEAValues(t *testing.T, fsys *ext4.FS, e filesys.Entry) int {
	t.Helper()
	var n uint32
	if _, err := fmt.Sscanf(e.ID, "inode:%d", &n); err != nil {
		return 0
	}
	in, err := fsys.Inode(n)
	if err != nil {
		return 0
	}
	xs, _ := fsys.RawXattrs(in)
	opened := 0
	for _, x := range xs {
		if x.Inum == 0 {
			continue
		}
		opened++
		fl, _, err := fsys.EAInodeValue(n, x.Name)
		if err != nil {
			continue
		}
		if runs := fl.Runs(); runs != nil {
			if err := filesys.CheckRuns(runs, fl.Size(), fsys.Info().Size); err != nil {
				t.Fatalf("runs of the EA-inode value %s violate the File.Runs contract: %v", x.Name, err)
			}
		}
		buf := make([]byte, min(fl.Size(), fuzzReadChunk))
		for off := int64(0); off < min(fl.Size(), fuzzFileMax); off += int64(len(buf)) {
			if _, rerr := fl.ReadAt(buf, off); rerr != nil {
				break
			}
		}
	}
	return opened
}

// fuzzTruncatedSeeds are builder images cut inside a file's data (L32), so the fuzz body opens and reads files that
// carry a prefix. Two groups of 64 blocks: group 0 holds data blocks 9..64, group 1's data starts at block 73, and the
// root directory fills a gap of group 0. A file too big for group 0 lands in group 1, and the cut falls inside it
// with the directory intact: an extent file (cut inside its third extent), the same with its extents in two leaf
// blocks (the index node is cut), and a block-map file (cut inside its direct pointers).
func fuzzTruncatedSeeds() [][]byte {
	const bs = 1024
	opts := ext4test.Options{BlockSize: bs, Extents: true, Groups: 2, BlocksPerGroup: 64, InodesPerGroup: 16}
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: 20 * i, Data: bytes.Repeat([]byte{byte(i + 1)}, 20*bs)})
	}
	ext := ext4test.Build(opts, []ext4test.File{{Path: "/small", Data: []byte("s")}, {Path: "/f", Pieces: ps, Scatter: true}})
	leaves := ext4test.Build(opts, []ext4test.File{{Path: "/small", Data: []byte("s")}, {Path: "/f", Pieces: ps, Scatter: true, ExtentLeaves: 2}})
	opts.Extents = false
	blk := ext4test.Build(opts, []ext4test.File{{Path: "/fill", Data: bytes.Repeat([]byte{8}, 30*bs)}, {Path: "/m", Data: bytes.Repeat([]byte{9}, 40*bs)}})
	return [][]byte{ext[:74*bs+10], leaves[:74*bs+10], blk[:80*bs]}
}

// fuzzXattrEntries is how many entries of one input have their attributes listed and opened through the
// XattrReader (every attribute of those, each read in bounded chunks).
const fuzzXattrEntries = 200

// fuzzXattrReader lists the attributes of entry e and opens each: a list or a typed error, never a panic; a
// failed listing is a nil list; every opened value obeys the File.Runs contract and is read with a bound.
func fuzzXattrReader(t *testing.T, fsys *ext4.FS, e filesys.Entry) {
	t.Helper()
	xs, err := fsys.Xattrs(e)
	if err != nil && xs != nil {
		t.Fatalf("Xattrs of %s returned a list (%d attributes) with the error %v: a damaged set is never offered in part", e.Name, len(xs), err)
	}
	for _, x := range xs {
		if x.Size < -1 || (x.Storage != filesys.XattrInline && x.Storage != filesys.XattrStream) {
			t.Fatalf("Xattrs of %s lists %+v: a size below -1 or a storage kind the reader does not produce", e.Name, x)
		}
		fl, err := fsys.OpenXattr(e, x.Name)
		if err != nil {
			continue
		}
		if x.Size >= 0 && fl.Size() != x.Size {
			t.Fatalf("attribute %q of %s is listed with %d bytes but opens with %d", x.Name, e.Name, x.Size, fl.Size())
		}
		if runs := fl.Runs(); runs != nil {
			if err := filesys.CheckRuns(runs, fl.Size(), fsys.Info().Size); err != nil {
				t.Fatalf("runs of attribute %q of %s violate the File.Runs contract: %v", x.Name, e.Name, err)
			}
		}
		buf := make([]byte, min(fl.Size(), fuzzReadChunk))
		for off := int64(0); off < min(fl.Size(), fuzzFileMax); off += int64(len(buf)) {
			if _, rerr := fl.ReadAt(buf, off); rerr != nil {
				break
			}
		}
	}
}
