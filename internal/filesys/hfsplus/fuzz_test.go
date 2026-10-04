package hfsplus_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
)

const (
	fuzzWalkBudget = 5000    // entries visited per input
	fuzzFileMax    = 1 << 20 // files larger than this are opened but not read
	fuzzReadBudget = 8 << 20 // bytes read per input
	fuzzReadChunk  = 64 << 10
)

var errFuzzBudget = errors.New("walk budget reached")

// FuzzHFSPlusOpen runs the reader over arbitrary bytes: Open, a bounded Walk,
// Open and read of every file (symlinks and compressed files included), the
// File.Runs contract, and Unallocated. It calls the hfsplus functions directly
// (not through detect), so a panic fails the fuzz. Seeds: the builder images
// (HFS+, HFSX, wrapper, hard links, overflow extents, attributes, compressed,
// journal) and every real fixture cut after its last used byte.
func FuzzHFSPlusOpen(f *testing.F) {
	for _, b := range builderImages() {
		f.Add(b.img)
	}
	for _, name := range fixtureNames {
		img, _ := loadFixture(f, name)
		f.Add(trimTail(img))
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := hfsplus.Open(bytes.NewReader(b), int64(len(b)))
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
			checkFuzzRuns(t, e.Name, file, fsys.Info().Size)
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

// checkFuzzRuns checks the File.Runs contract of a file the reader opened: the
// runs cover exactly [0, Size()) or, when the allocation is truncated or
// corrupt, a valid strict prefix of it, in which case a read at the end of the
// prefix must fail with an error wrapping filesys.ErrCorrupt. A file whose
// content is not on disk (inline, decompressed) has no runs and is not checked.
func checkFuzzRuns(t testing.TB, name string, file filesys.File, fsSize int64) {
	t.Helper()
	runs, size := file.Runs(), file.Size()
	if len(runs) == 0 {
		return
	}
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
	if n, err := file.ReadAt(make([]byte, 1), covered); n != 0 || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("read of %s at the end of its %d-byte run prefix (size %d) = %d, %v; want 0 bytes and ErrCorrupt", name, covered, size, n, err)
	}
}

// trimTail cuts an image after its last non-zero byte plus a sector's worth of
// slack: whatever follows is unused space. The reader clamps a volume that is
// larger than its image (with a warning), so the cut image is still a valid
// seed and the fuzz engine's throughput does not collapse on large inputs.
func trimTail(img []byte) []byte {
	end := len(img)
	for end > 0 && img[end-1] == 0 {
		end--
	}
	return img[:min(len(img), end+4096)]
}

// Every real fixture, trimmed the way the fuzz seeds are, still opens and
// passes the same checks as a fuzz input (a truncated volume opens clamped).
func TestFuzzFixtureSeedsOpen(t *testing.T) {
	for _, name := range fixtureNames {
		img, _ := loadFixture(t, name)
		seed := trimTail(img)
		fsys, err := hfsplus.Open(bytes.NewReader(seed), int64(len(seed)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		err = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err != nil {
				t.Errorf("%s: walk: %v", name, err)
				return nil
			}
			if e.Type == filesys.TypeFile {
				if file, err := fsys.Open(e); err == nil {
					checkFuzzRuns(t, e.Name, file, fsys.Info().Size)
				}
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		runs, err := fsys.Unallocated()
		if err != nil {
			t.Errorf("%s: Unallocated: %v", name, err)
		}
		for _, r := range runs {
			if r.Offset+r.Length > fsys.Info().Size {
				t.Errorf("%s: free run %+v outside the filesystem", name, r)
			}
		}
	}
}
