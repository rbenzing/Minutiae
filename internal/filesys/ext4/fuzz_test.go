package ext4_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	fuzzSeedBytes  = 1 << 20 // seed with the first MiB of each fixture, not whole images
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
	}
}

// FuzzExt4Open runs the reader over arbitrary bytes: Open, a bounded Walk,
// Open and read of every file, and Unallocated. It calls the ext4 functions
// directly (not through detect.OpenWith), so a panic fails the fuzz.
func FuzzExt4Open(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, path := range fixturePaths(f) {
		img := gunzipFixture(f, path)
		f.Add(img[:min(len(img), fuzzSeedBytes)])
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := ext4.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		_ = fsys.Info()
		visited, readTotal := 0, int64(0)
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > fuzzWalkBudget {
				return errFuzzBudget
			}
			if e.Type != filesys.TypeFile && e.Type != filesys.TypeSymlink {
				return nil
			}
			file, err := fsys.Open(e)
			if err != nil || file.Size() > fuzzFileMax || readTotal >= fuzzReadBudget {
				return nil
			}
			_ = file.Runs()
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
