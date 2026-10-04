package hfsplus_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// The expected free space below is computed from the builder's Layout (where
// each structure was put), independently of the allocation bitmap the reader
// reads: a volume's protected blocks are the boot area and volume header, the
// five system files' extents, the journal info block and the journal, and the
// alternate header; with files, every file fork too.

func ceilDiv(n, d int64) int64 { return (n + d - 1) / d }

func markBlocks(used []bool, start, n int64) {
	for i := start; i < start+n && i < int64(len(used)); i++ {
		used[i] = true
	}
}

// layoutBlocks returns, per allocation block, whether the layout puts something
// there. withFiles adds the forks of the listed files; extra adds more extents.
func layoutBlocks(lay *hfsplustest.Layout, withFiles bool, extra ...hfsplustest.Extent) []bool {
	bs := int64(lay.BlockSize)
	used := make([]bool, lay.Blocks)
	markBlocks(used, 0, ceilDiv(1536, bs))
	markBlocks(used, int64(lay.AllocBlock), int64(lay.AllocBlocks))
	markBlocks(used, int64(lay.ExtentsBlock), int64(lay.ExtentsBlocks))
	for _, e := range lay.CatalogExtents {
		markBlocks(used, int64(e.Start), int64(e.Count))
	}
	markBlocks(used, int64(lay.AttrBlock), int64(lay.AttrBlocks))
	if lay.JournalInfoBlock != 0 {
		markBlocks(used, int64(lay.JournalInfoBlock), 1)
		markBlocks(used, lay.JournalOffset/bs, ceilDiv(lay.JournalBytes, bs))
	}
	alt := ceilDiv(1024, bs)
	markBlocks(used, int64(lay.Blocks)-alt, alt)
	for _, e := range extra {
		markBlocks(used, int64(e.Start), int64(e.Count))
	}
	if withFiles {
		for _, fl := range lay.Files {
			for _, e := range append(append([]hfsplustest.Extent(nil), fl.Data...), fl.Rsrc...) {
				markBlocks(used, int64(e.Start), int64(e.Count))
			}
		}
	}
	return used
}

// freeRunsOf is the byte runs (from the start of the image) of the blocks below
// limit that are not used, adjacent ones merged.
func freeRunsOf(lay *hfsplustest.Layout, used []bool, limit int) []filesys.Run {
	var runs []filesys.Run
	bs := int64(lay.BlockSize)
	for b := 0; b < min(limit, len(used)); b++ {
		if used[b] {
			continue
		}
		off := lay.Base + int64(b)*bs
		if n := len(runs); n > 0 && runs[n-1].Offset+runs[n-1].Length == off {
			runs[n-1].Length += bs
			continue
		}
		runs = append(runs, filesys.Run{Offset: off, Length: bs})
	}
	return runs
}

func sameRuns(a, b []filesys.Run) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantRuns(t *testing.T, got, want []filesys.Run) {
	t.Helper()
	if !sameRuns(got, want) {
		t.Fatalf("Unallocated =\n%v\nwant\n%v", got, want)
	}
}

func allocTree(bs int) []hfsplustest.File {
	return []hfsplustest.File{
		{Path: "/frag", Data: pattern(12*bs, 1), Fragment: 2},
		{Path: "/small", Data: []byte("hi")},
		{Path: "/dir", Dir: true},
		{Path: "/dir/r", Rsrc: pattern(5000, 2)},
	}
}

func zeroBitmap(img []byte, lay *hfsplustest.Layout) {
	bs := int64(lay.BlockSize)
	clear(img[lay.Base+int64(lay.AllocBlock)*bs : lay.Base+int64(lay.AllocBlock+lay.AllocBlocks)*bs])
}

// putVHFork writes a fork data record into a volume header copy.
func putVHFork(vh []byte, off int, logical uint64, blocks uint32, exts ...[2]uint32) {
	be.PutUint64(vh[off:], logical)
	be.PutUint32(vh[off+12:], blocks)
	for i, e := range exts {
		be.PutUint32(vh[off+16+8*i:], e[0])
		be.PutUint32(vh[off+20+8*i:], e[1])
	}
}

const (
	vhFreeBlocks = 48
	vhAllocFork  = 112
	vhCatFork    = 272
	vhStartFork  = 432
)

func TestUnallocatedFromBitmap(t *testing.T) {
	for name, o := range map[string]hfsplustest.Options{
		"4k":        {Label: "A"},
		"1k":        {Label: "A", BlockSize: 1024, Blocks: 1024},
		"hfsx":      {Label: "A", HFSX: true, CaseSensitive: true},
		"journaled": {Label: "A", Journaled: true},
		"512":       {Label: "A", BlockSize: 512, Blocks: 1024, NodeSize: 512, ExtentsNodeSize: 512},
		"odd count": {Label: "A", BlockSize: 1024, Blocks: 301},
	} {
		t.Run(name, func(t *testing.T) {
			bs := int(o.BlockSize)
			if bs == 0 {
				bs = 4096
			}
			img, lay := hfsplustest.BuildLayout(o, allocTree(bs))
			f := open(t, img)
			got, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			want := freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks))
			if len(want) < 3 {
				t.Fatalf("test setup: %d expected runs, want several", len(want))
			}
			wantRuns(t, got, want)
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a consistent volume: %v", w)
			}
			// every run is inside the image and block aligned.
			for _, r := range got {
				if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > int64(len(img)) || (r.Offset-lay.Base)%int64(bs) != 0 || r.Length%int64(bs) != 0 {
					t.Errorf("bad run %+v", r)
				}
			}
		})
	}
}

// With every bitmap bit cleared, the protected structures are still never
// reported: boot area and volume header, alternate header, all five system
// files including their overflow extents, the bad-block file's extents, the
// journal info block and the journal.
func TestUnallocatedExcludesMetadata(t *testing.T) {
	files := append(bigDir(600), hfsplustest.File{Path: "/xattr", Attrs: []hfsplustest.Attr{{Name: "user.a", Value: []byte("v")}}})
	bad := hfsplustest.Extent{Start: 700, Count: 3}
	o := hfsplustest.Options{
		Label: "M", Blocks: 1024, NodeSize: 1024, CatalogFragment: 1, Journaled: true,
		OverflowRecords: []hfsplustest.OverflowRecord{{FileID: 5, StartBlock: 0, Extents: []hfsplustest.Extent{bad}}},
	}
	img, lay := hfsplustest.BuildLayout(o, files)
	if len(lay.CatalogExtents) <= 8 {
		t.Fatalf("test setup: catalog has %d extents, want overflow extents", len(lay.CatalogExtents))
	}
	if lay.AttrBlocks == 0 || lay.JournalInfoBlock == 0 {
		t.Fatalf("test setup: attributes %d blocks, journal info block %d", lay.AttrBlocks, lay.JournalInfoBlock)
	}
	startup := hfsplustest.Extent{Start: 900, Count: 2}
	patchHeaders(img, lay, func(vh []byte) {
		putVHFork(vh, vhStartFork, 2*4096, 2, [2]uint32{startup.Start, startup.Count})
	})
	zeroBitmap(img, lay)
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	want := freeRunsOf(lay, layoutBlocks(lay, false, bad, startup), int(lay.Blocks))
	wantRuns(t, got, want)
	if w := f.Info().Warnings; hasWarning(f.Info(), "bad block") || hasWarning(f.Info(), "no free space") {
		t.Errorf("warnings: %v", w)
	}
}

// Each protected structure on its own, with the bitmap cleared, on the small
// default volume.
func TestUnallocatedProtectsEachStructure(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "P", Journaled: true}, allocTree(4096))
	zeroBitmap(img, lay)
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	bs := int64(lay.BlockSize)
	covered := func(block int64) bool {
		for _, r := range got {
			if r.Offset <= lay.Base+block*bs && lay.Base+block*bs < r.Offset+r.Length {
				return true
			}
		}
		return false
	}
	for name, blk := range map[string]int64{
		"boot area and volume header": 0, // bytes 0..1536 share block 0 at 4 KiB blocks
		"alternate header":            int64(lay.Blocks) - 1,
		"allocation file":             int64(lay.AllocBlock),
		"extents file":                int64(lay.ExtentsBlock),
		"catalog file":                int64(lay.CatalogBlock),
		"journal info block":          int64(lay.JournalInfoBlock),
		"journal first block":         lay.JournalOffset / bs,
		"journal last block":          (lay.JournalOffset + lay.JournalBytes - 1) / bs,
	} {
		if covered(blk) {
			t.Errorf("%s: block %d reported free", name, blk)
		}
	}
	// File content is not protected by the reader: the bitmap decides.
	if blk := int64(lay.Files["/frag"].Data[0].Start); !covered(blk) {
		t.Errorf("file block %d is not reported free although the bitmap says so", blk)
	}
}

func TestUnallocatedBitmapTooShort(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "S"}, allocTree(4096))
	patchHeaders(img, lay, func(vh []byte) { be.PutUint64(vh[vhAllocFork:], 16) }) // 16 bytes cover 128 of 256 blocks
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), 128))
	if !hasWarning(f.Info(), "covering only 128 of 256") {
		t.Errorf("warnings = %v, want the short bitmap reported", f.Info().Warnings)
	}
	for _, r := range got {
		if r.Offset+r.Length > 128*4096 {
			t.Errorf("run %+v lies beyond the blocks the bitmap covers", r)
		}
	}
}

func TestUnallocatedPaddingBitsIgnored(t *testing.T) {
	for _, set := range []bool{false, true} {
		t.Run(fmt.Sprintf("padding set=%v", set), func(t *testing.T) {
			img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "Pad", Blocks: 250}, allocTree(4096))
			bm := img[int64(lay.AllocBlock)*4096:]
			if set {
				bm[31] |= 0x3F // bits 250..255 are past totalBlocks
			} else if bm[31]&0x3F != 0 {
				t.Fatal("test setup: padding bits are set")
			}
			f := open(t, img)
			got, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), 250))
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %v (padding bits must not matter)", w)
			}
			for _, r := range got {
				if r.Offset+r.Length > 250*4096 {
					t.Errorf("run %+v reaches past totalBlocks", r)
				}
			}
		})
	}
	// Padding bits that are clear while the block count is a multiple of 8 and
	// the bitmap is longer than needed (a whole spare block of zeros).
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "Pad2", BlockSize: 512, Blocks: 4096 + 8, NodeSize: 512, ExtentsNodeSize: 512}, nil)
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks)))
}

func TestUnallocatedWrapperOffsets(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "W", Wrapper: true}, allocTree(4096))
	if lay.Base != hfsplustest.WrapperBase {
		t.Fatalf("test setup: base %d", lay.Base)
	}
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks)))
	for _, r := range got {
		if r.Offset < lay.Base || r.Offset+r.Length > lay.Base+lay.VolumeBytes {
			t.Errorf("run %+v is outside the embedded volume [%d, %d)", r, lay.Base, lay.Base+lay.VolumeBytes)
		}
	}
	// With the bitmap cleared the HFS wrapper area (before the embedded volume)
	// and the volume's own boot area are still not free.
	zeroBitmap(img, lay)
	got, err = open(t, img).Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, false), int(lay.Blocks)))
	if len(got) == 0 || got[0].Offset < lay.Base+4096 {
		t.Errorf("first run %v reaches into the wrapper area or the volume header", got)
	}
}

func TestUnallocatedPendingJournalRefused(t *testing.T) {
	img, _ := hfsplustest.BuildLayout(hfsplustest.Options{Label: "J", JournalPending: true}, allocTree(4096))
	f := open(t, img)
	runs, err := f.Unallocated()
	if !errors.Is(err, filesys.ErrUnsupported) || runs != nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("Unallocated = %v, %v; want nil and an error wrapping ErrUnsupported", runs, err)
	}
}

func TestUnallocatedUnknownJournalRefused(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "J", Journaled: true}, allocTree(4096))
	be.PutUint32(img[lay.Base+lay.JournalOffset+4:], 0xDEADBEEF) // an unrecognised byte-order marker
	f := open(t, img)
	if f.JournalState() != "unknown" {
		t.Fatalf("test setup: journal state %q", f.JournalState())
	}
	runs, err := f.Unallocated()
	if !errors.Is(err, filesys.ErrUnsupported) || runs != nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("Unallocated = %v, %v; want nil and an error wrapping ErrUnsupported", runs, err)
	}
}

func TestUnallocatedCleanJournalOK(t *testing.T) {
	for _, le := range []bool{false, true} {
		img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "J", Journaled: true, JournalLittleEndian: le}, allocTree(4096))
		f := open(t, img)
		if f.JournalState() != "clean" {
			t.Fatalf("test setup: journal state %q", f.JournalState())
		}
		got, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks)))
		if lay.JournalInfoBlock == 0 {
			t.Fatal("test setup: no journal")
		}
	}
}

func TestFreeCountMismatchWarns(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "F"}, allocTree(4096))
	patchHeaders(img, lay, func(vh []byte) { be.PutUint32(vh[vhFreeBlocks:], be.Uint32(vh[vhFreeBlocks:])+7) })
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks)))
	if !hasWarning(f.Info(), "free blocks") {
		t.Errorf("warnings = %v, want a free-count mismatch", f.Info().Warnings)
	}
	// A consistent volume raises no such warning (TestUnallocatedFromBitmap).
}

func TestUnallocatedUncleanUnmountWarns(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "U", NotUnmounted: true}, allocTree(4096))
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks)))
	if !hasWarning(f.Info(), "may be out of date") {
		t.Errorf("warnings = %v, want a stale-bitmap warning", f.Info().Warnings)
	}
}

func TestUnallocatedRunCap(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "C"}, allocTree(4096))
	want := freeRunsOf(lay, layoutBlocks(lay, true), int(lay.Blocks))
	if len(want) < 6 {
		t.Fatalf("test setup: %d runs", len(want))
	}
	f := open(t, img)
	f.SetUnallocCap(3)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, want[:3])
	if !hasWarning(f.Info(), "more than 3 free runs") {
		t.Errorf("warnings = %v, want the cap reported", f.Info().Warnings)
	}
}

// The allocation file itself is in several extents, some of them reached
// through the extents-overflow tree, and read in the order the fork maps them.
func TestUnallocatedFragmentedBitmap(t *testing.T) {
	o := hfsplustest.Options{
		Label: "Frag", BlockSize: 512, Blocks: 16384, NodeSize: 512, ExtentsNodeSize: 512,
		OverflowRecords: []hfsplustest.OverflowRecord{{FileID: 6, StartBlock: 2, Extents: []hfsplustest.Extent{{Start: 7000, Count: 1}, {Start: 6000, Count: 1}}}},
	}
	img, lay := hfsplustest.BuildLayout(o, allocTree(512))
	if lay.AllocBlocks != 4 {
		t.Fatalf("test setup: the bitmap is %d blocks", lay.AllocBlocks)
	}
	moved := []hfsplustest.Extent{{Start: 9000, Count: 1}, {Start: 8000, Count: 1}, {Start: 7000, Count: 1}, {Start: 6000, Count: 1}}
	for i, e := range moved {
		src := lay.Base + int64(int(lay.AllocBlock)+i)*512
		copy(img[int64(e.Start)*512:], img[src:src+512])
	}
	clear(img[int64(lay.AllocBlock)*512 : int64(lay.AllocBlock+lay.AllocBlocks)*512]) // reading the old place would find an empty bitmap
	patchHeaders(img, lay, func(vh []byte) {
		putVHFork(vh, vhAllocFork, 4*512, 4, [2]uint32{9000, 1}, [2]uint32{8000, 1})
	})
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	// The bitmap's new blocks are in use whatever the bitmap says (their bits
	// are clear); the old place stays marked in the copied bits.
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true, moved...), int(lay.Blocks)))
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestUnallocatedClipsToImageEnd(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "T"}, nil)
	img = img[:200*4096+100] // the last 56 blocks and part of block 200 are missing
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true), 200))
	if !hasWarning(f.Info(), "truncated image") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

// A system file whose extents cannot be completely resolved means the blocks it
// holds are not all known, so no free space is reported at all.
func TestUnallocatedIncompleteSystemFileReportsNothing(t *testing.T) {
	img, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "I"}, allocTree(4096))
	patchHeaders(img, lay, func(vh []byte) { be.PutUint32(vh[vhCatFork+12:], be.Uint32(vh[vhCatFork+12:])+1) })
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil || got != nil {
		t.Fatalf("Unallocated = %v, %v; want nothing", got, err)
	}
	if !hasWarning(f.Info(), "no free space") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

func TestUnallocatedBadBlockExtentOutsideVolumeIsClippedAndWarned(t *testing.T) {
	o := hfsplustest.Options{Label: "B", OverflowRecords: []hfsplustest.OverflowRecord{
		{FileID: 5, StartBlock: 0, Extents: []hfsplustest.Extent{{Start: 250, Count: 100}}},
	}}
	img, lay := hfsplustest.BuildLayout(o, nil)
	f := open(t, img)
	got, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	wantRuns(t, got, freeRunsOf(lay, layoutBlocks(lay, true, hfsplustest.Extent{Start: 250, Count: 6}), int(lay.Blocks)))
	if !hasWarning(f.Info(), "bad block") {
		t.Errorf("warnings = %v", f.Info().Warnings)
	}
}

// --- real mkfs.hfsplus fixtures ----------------------------------------------

type freeOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	BlockSize    int64      `json:"block_size"`
	VolumeOffset int64      `json:"volume_offset"`
	TotalBlocks  int64      `json:"total_blocks"`
	FreeCount    int64      `json:"free_count"`
	FreeBlocks   [][2]int64 `json:"free_blocks"`
}

// The free space of every real fixture equals the oracle's (the free ranges the
// independent fsck-checked bitmap decoder found), exactly, with no warning.
func TestUnallocatedMatchesOracle(t *testing.T) {
	for _, name := range []string{"hfsplus-empty", "hfsx-empty", "hfsplus-journal", "hfsplus-1k", "hfsplus-wrapped"} {
		t.Run(name, func(t *testing.T) {
			img, _ := loadFixture(t, name)
			raw, err := os.ReadFile(filepath.Join("testdata", name+".expect.json"))
			if err != nil {
				t.Fatal(err)
			}
			var ex freeOracle
			if err := json.Unmarshal(raw, &ex); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != ex.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s, the oracle describes %s (stale oracle)", got, ex.Generator.ImageSHA256)
			}
			var want []filesys.Run
			var count int64
			for _, r := range ex.FreeBlocks {
				n := r[1] - r[0] + 1
				count += n
				want = append(want, filesys.Run{Offset: ex.VolumeOffset + r[0]*ex.BlockSize, Length: n * ex.BlockSize})
			}
			if count != ex.FreeCount || len(want) == 0 {
				t.Fatalf("oracle: %d free blocks in %d ranges, free_count %d", count, len(want), ex.FreeCount)
			}
			f := open(t, img)
			got, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			wantRuns(t, got, want)
			var total int64
			for _, r := range got {
				total += r.Length
			}
			if total != ex.FreeCount*ex.BlockSize {
				t.Errorf("free bytes %d, want %d", total, ex.FreeCount*ex.BlockSize)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a real volume: %v", w)
			}
		})
	}
}
