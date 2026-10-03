package exfat_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // files larger than this are opened but not read
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzReadChunk  = 64 << 10
)

var errFuzzBudget = errors.New("walk budget reached")

// fuzzBuilderSeeds are small synthetic images covering the main on-disk
// variants: contiguous (NoFatChain) and FAT-chained files, fragmentation,
// deleted sets, a valid data length below the data length, UTC offsets, long
// names, a bad set checksum, and 512-byte sectors with one-sector clusters.
func fuzzBuilderSeeds() [][]byte {
	data := bytes.Repeat([]byte("fuzz seed data "), 400)
	valid := int64(1000)
	when := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	files := []exfattest.File{
		{Path: "/a.txt", Data: []byte("hello"), Times: [3]time.Time{when, when, when}, Offsets: [3]exfattest.Offset{{Valid: true, Quarters: 4}, {Valid: true}, {}}},
		{Path: "/d", Dir: true},
		{Path: "/d/big.bin", Data: data, FatChain: true},
		{Path: "/d/frag.bin", Data: data, Fragmented: true},
		{Path: "/d/gone-with-a-long-name-of-several-entries.txt", Data: []byte("x"), Deleted: true},
		{Path: "/d/sparse.bin", Data: data, ValidLength: &valid},
		{Path: "/日本語.txt", Data: []byte("cjk")},
		{Path: "/badsum.txt", Data: []byte("b"), BadChecksum: true},
	}
	return [][]byte{
		exfattest.Build(exfattest.Options{Label: "SEED", Serial: 0x1234ABCD}, files),
		exfattest.Build(exfattest.Options{OneSectorClusters: true, ClusterCount: 512, FragmentBitmap: true}, files),
		exfattest.Build(exfattest.Options{BytesPerSectorShift: 12, SectorsPerClusterShift: 1, BadBootChecksum: true}, files[:3]),
		exfattest.Build(exfattest.Options{NoUpcase: true}, files[:2]),
	}
}

// FuzzExfatOpen runs the reader over arbitrary bytes: Open, a bounded Walk, Open
// and read of every file, and Unallocated. It calls the fat functions directly
// (not through detect.OpenWith), so a panic fails the fuzz.
func FuzzExfatOpen(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := exfat.Open(bytes.NewReader(b), int64(len(b)))
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
		if _, err := exfat.Open(bytes.NewReader(s), int64(len(s))); err != nil {
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
