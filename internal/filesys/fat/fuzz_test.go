package fat_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // files larger than this are opened but not read
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzReadChunk  = 64 << 10
)

var errFuzzBudget = errors.New("walk budget reached")

// fuzzBuilderSeeds are small synthetic images covering the main on-disk
// variants: the three FAT types, long names, deleted entries, directories of
// several clusters, fragmented chains and a FAT32 chain root.
func fuzzBuilderSeeds() [][]byte {
	data := bytes.Repeat([]byte("fuzz seed data "), 400)
	when := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	files := []fattest.File{
		{Path: "/A.TXT", Data: []byte("hello"), Times: [3]time.Time{when, when, when}},
		{Path: "/lower.txt", Data: []byte("short lower case")},
		{Path: "/D", Dir: true},
		{Path: "/D/big.bin", Data: data, Fragmented: true},
		{Path: "/D/gone-with-a-long-name.txt", Data: []byte("x"), LongName: true, Deleted: true},
		{Path: "/mixed-Case-Long-Name.Txt", Data: []byte("long"), LongName: true},
		{Path: "/日本語.txt", Data: []byte("cjk"), LongName: true},
		{Path: "/tail.bin", Data: bytes.Repeat([]byte{7}, 5000)},
	}
	// A FAT32 volume has at least 65525 clusters (tens of megabytes), too big a
	// seed for the fuzz engine: each seed is cut after its first clusters, which
	// hold everything the files use (the reader clamps a volume that is larger
	// than its image).
	seed := func(o fattest.Options, files []fattest.File) []byte {
		img := fattest.Build(o, files)
		return img[:min(int64(len(img)), fattest.Layout(o).ClusterOffset(600))]
	}
	return [][]byte{
		seed(fattest.Options{Type: 12, Label: "SEED12", VolID: 0x1234ABCD}, files),
		seed(fattest.Options{Type: 16, Label: "SEED16"}, files),
		seed(fattest.Options{Type: 32, Label: "SEED32", SecPerClus: 2}, files),
		seed(fattest.Options{Type: 16, SectorSize: 1024, NumFATs: 1}, files[:4]),
	}
}

// FuzzFATOpen runs the reader over arbitrary bytes: Open, a bounded Walk, Open
// and read of every file, and Unallocated. It calls the fat functions directly
// (not through detect.OpenWith), so a panic fails the fuzz.
func FuzzFATOpen(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := fat.Open(bytes.NewReader(b), int64(len(b)))
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

// The builder seeds must be images the reader accepts, or the fuzz would
// start from nothing.
func TestFuzzBuilderSeedsOpen(t *testing.T) {
	for i, s := range fuzzBuilderSeeds() {
		if _, err := fat.Open(bytes.NewReader(s), int64(len(s))); err != nil {
			t.Errorf("seed %d: %v", i, err)
		}
	}
}

// trimmedFixture returns a fixture cut after its last used cluster (plus a
// slack): the fuzz engine cannot take a seed of tens of megabytes, and the
// reader clamps a volume that is larger than its image, so the cut image is
// still a valid seed (and exercises that path).
func trimmedFixture(t testing.TB, path string) []byte {
	t.Helper()
	img, want := gunzipFixture(t, path), loadOracle(t, path)
	var last int64 = 2
	for _, f := range want.Files {
		for _, c := range f.Clusters {
			last = max(last, c[1])
		}
	}
	end := want.DataStart + (last-1)*int64(want.BlockSize) + 64<<10
	return img[:min(int64(len(img)), end)]
}
