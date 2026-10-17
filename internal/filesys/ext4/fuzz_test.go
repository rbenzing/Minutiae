package ext4_test

import (
	"bytes"
	"errors"
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

// fuzzCasefoldSeedMax cuts the real casefold image: the start (superblock, descriptors, bitmaps, the first
// inode tables and directories) is what a mutation can usefully damage, and the reader accepts an image
// shorter than its block count.
const fuzzCasefoldSeedMax = 8 << 20

// fuzzSeedBudget is what the fuzz engine can take: it refuses a seed set above 100 MiB in total ("value
// length larger than shared memory capacity") and the whole target then cannot run at all.
const fuzzSeedBudget = 64 << 20

// fuzzOpenSeeds are the seeds of FuzzExt4Open: the builder images, the mke2fs fixtures, the first 8 MiB of the
// kernel-made casefold image (by name: its expect schema is not the one of the fixture loops), and zeros.
func fuzzOpenSeeds(t testing.TB) [][]byte {
	t.Helper()
	seeds := fuzzBuilderSeeds()
	for _, path := range fixturePaths(t) {
		seeds = append(seeds, gunzipFixture(t, path))
	}
	cf := gunzipFixture(t, filepath.Join("testdata", "ext4-casefold.img.gz"))
	seeds = append(seeds, cf[:min(len(cf), fuzzCasefoldSeedMax)])
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
			if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
				return nil
			}
			file, err := fsys.Open(e)
			if err != nil || file.Size() > fuzzFileMax || readTotal >= fuzzReadBudget {
				return nil
			}
			if runs := file.Runs(); runs != nil {
				if err := filesys.CheckRuns(runs, file.Size(), fsys.Info().Size); err != nil {
					t.Fatalf("runs of %s violate the File.Runs contract: %v", e.Name, err)
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
			}
			return nil
		})
	})
}
