package f2fs_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

func ptr16(v uint16) *uint16 { return &v }

// offsets returns the block offsets lo..hi-1.
func offsets(lo, hi int) []int {
	var o []int
	for i := lo; i < hi; i++ {
		o = append(o, i)
	}
	return o
}

// freeOf returns the byte runs of every main-area block of the image laid out
// by l that is not in valid, built block by block with no help from the
// reader (and none from filesys.MergeRuns).
func freeOf(l f2fstest.Layout, valid map[uint32]bool) []filesys.Run {
	var rs []filesys.Run
	for a := l.Main; a < l.BlockCount; a++ {
		if valid[a] {
			continue
		}
		if n := len(rs); n > 0 && rs[n-1].Offset+rs[n-1].Length == int64(a)*bs {
			rs[n-1].Length += bs
			continue
		}
		rs = append(rs, filesys.Run{Offset: int64(a) * bs, Length: bs})
	}
	return rs
}

// segBlocks marks the blocks at the given offsets of main-area segment seg.
func segBlocks(l f2fstest.Layout, valid map[uint32]bool, seg uint32, offs []int) {
	for _, o := range offs {
		valid[l.Main+seg*f2fstest.BlocksPerSeg+uint32(o)] = true
	}
}

func checkFree(t *testing.T, got, want []filesys.Run) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("Unallocated differs from the blocks whose SIT valid bit is clear:\n got  %d runs %v\n want %d runs %v", len(got), clip(got), len(want), clip(want))
	}
}

func clip(r []filesys.Run) []filesys.Run {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

func unalloc(t *testing.T, f *f2fs.FS) []filesys.Run {
	t.Helper()
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	return runs
}

// The SIT the builder writes for the blocks it lays out (a file spread over
// two segments with gaps, and a file that fills a whole segment and more)
// yields exactly the other main-area blocks.
func TestUnallocatedFromSIT(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stride uint32
		blocks int
	}{
		{"gaps between blocks", 2, 300},
		{"consecutive blocks over a segment boundary", 1, 600},
		{"one block", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := f2fstest.Options{Segments: 3}
			a := f2fstest.NewAlloc(o, firstNode)
			a.Stride = tc.stride
			nodes, data := a.File(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: uint64(tc.blocks) * bs},
				f2fstest.FileData{Blocks: blocksOf(dpat(tc.blocks*bs, 1), 0)})
			o.Nodes, o.Data = nodes, data
			img := f2fstest.Build(o, nil)
			f := mustOpen(t, img)

			l := f2fstest.Geometry(o)
			used := map[uint32]bool{}
			for _, n := range nodes {
				used[n.Addr] = true
			}
			for _, d := range data {
				used[d.Addr] = true
			}
			got := unalloc(t, f)
			checkFree(t, got, freeOf(l, used))
			for _, r := range got {
				for b := r.Offset / bs; b < (r.Offset+r.Length)/bs; b++ {
					if used[uint32(b)] {
						t.Fatalf("block %d is in use but reported free (run %+v)", b, r)
					}
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings on a consistent SIT: %v", w)
			}
		})
	}
}

// An empty image (no entries at all) is entirely free, and so is a segment
// whose entry says vblocks 0.
func TestUnallocatedEmptyAndZeroSegments(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	l := f2fstest.Geometry(o)
	f := mustOpen(t, f2fstest.Build(o, nil))
	checkFree(t, unalloc(t, f), []filesys.Run{{Offset: int64(l.Main) * bs, Length: 2 * f2fstest.BlocksPerSeg * bs}})

	// A segment with an explicit all-clear entry is free as well.
	o.SIT = []f2fstest.SITEntry{{Segno: 0, Valid: []int{7}}, {Segno: 1}}
	f = mustOpen(t, f2fstest.Build(o, nil))
	valid := map[uint32]bool{}
	segBlocks(l, valid, 0, []int{7})
	checkFree(t, unalloc(t, f), freeOf(l, valid))
}

// A SIT journal entry replaces the whole entry stored in the SIT block (it is
// newer than the last SIT flush); later journal entries for the same segment
// replace earlier ones, as the kernel applies the journal in order at mount.
func TestSITJournalOverridesBlock(t *testing.T) {
	for _, compact := range []bool{false, true} {
		name := "full summary blocks"
		if compact {
			name = "compact summary"
		}
		t.Run(name, func(t *testing.T) {
			o := f2fstest.Options{
				Segments: 3, NoSIT: true, CompactSum: compact,
				SIT: []f2fstest.SITEntry{
					{Segno: 1, Valid: offsets(0, 10)},
					{Segno: 0, Valid: []int{5}},
				},
				SITJournal: []f2fstest.SITEntry{
					{Segno: 1, Valid: offsets(100, 110)},
					{Segno: 2, Valid: []int{0}},
					{Segno: 2, Valid: []int{1, 2}},
				},
			}
			l := f2fstest.Geometry(o)
			f := mustOpen(t, f2fstest.Build(o, nil))
			valid := map[uint32]bool{}
			segBlocks(l, valid, 0, []int{5})
			segBlocks(l, valid, 1, offsets(100, 110))
			segBlocks(l, valid, 2, []int{1, 2})
			checkFree(t, unalloc(t, f), freeOf(l, valid))
			if !hasWarning(f.Info(), "more than once") {
				t.Errorf("no warning about the duplicate journal entry: %v", f.Info().Warnings)
			}
		})
	}
}

// The SIT version bitmap selects which of the two copies of each SIT block is
// current; the other copy (here describing everything as valid) is ignored.
func TestSITBitmapSelectsCopy(t *testing.T) {
	// 58 segments need two SIT blocks (55 entries each): bit 0 and bit 1 of
	// the bitmap (most significant bit first) select the copy of each.
	for _, tc := range []struct {
		name   string
		bitmap []byte
	}{
		{"both first copies", nil},
		{"first block second copy", []byte{0x80}},
		{"second block second copy", []byte{0x40}},
		{"both second copies", []byte{0xc0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := f2fstest.Options{
				Segments: 58, NoSIT: true, SITDecoy: true, SITBitmap: tc.bitmap,
				SIT: []f2fstest.SITEntry{
					{Segno: 0, Valid: []int{0, 1}},
					{Segno: 54, Valid: []int{511}},
					{Segno: 55, Valid: []int{3}},
					{Segno: 57, Valid: offsets(0, 512)},
				},
			}
			l := f2fstest.Geometry(o)
			f := mustOpen(t, f2fstest.Build(o, nil))
			valid := map[uint32]bool{}
			segBlocks(l, valid, 0, []int{0, 1})
			segBlocks(l, valid, 54, []int{511})
			segBlocks(l, valid, 55, []int{3})
			segBlocks(l, valid, 57, offsets(0, 512))
			checkFree(t, unalloc(t, f), freeOf(l, valid))
		})
	}
}

// Inconsistent entries never turn an allocated block into a free one: the
// valid map is authoritative, and an entry that cannot be right is skipped.
func TestSITInconsistentEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry f2fstest.SITEntry
		valid []int  // blocks of segment 0 that must not be reported free
		skip  bool   // the whole segment is unreported
		warn  string // expected warning
	}{
		{"vblocks 0 but bits set", f2fstest.SITEntry{Segno: 0, Valid: []int{3, 4}, VBlocks: ptr16(0)}, []int{3, 4}, false, "valid-block count"},
		{"vblocks too high", f2fstest.SITEntry{Segno: 0, Valid: []int{3}, VBlocks: ptr16(9)}, []int{3}, false, "valid-block count"},
		{"vblocks above 512", f2fstest.SITEntry{Segno: 0, Valid: []int{3}, VBlocks: ptr16(600)}, nil, true, "600"},
		{"segment type bits are not part of the count", f2fstest.SITEntry{Segno: 0, Type: 5, Valid: []int{3}}, []int{3}, false, ""},
	} {
		for _, journal := range []bool{false, true} {
			name := tc.name + " in the SIT block"
			if journal {
				name = tc.name + " in the journal"
			}
			t.Run(name, func(t *testing.T) {
				o := f2fstest.Options{Segments: 2, NoSIT: true}
				if journal {
					o.SITJournal = []f2fstest.SITEntry{tc.entry}
				} else {
					o.SIT = []f2fstest.SITEntry{tc.entry}
				}
				l := f2fstest.Geometry(o)
				f := mustOpen(t, f2fstest.Build(o, nil))
				valid := map[uint32]bool{}
				segBlocks(l, valid, 0, tc.valid)
				if tc.skip {
					segBlocks(l, valid, 0, offsets(0, 512))
				}
				checkFree(t, unalloc(t, f), freeOf(l, valid))
				if tc.warn == "" {
					if w := f.Info().Warnings; len(w) != 0 {
						t.Errorf("unexpected warnings: %v", w)
					}
				} else if !hasWarning(f.Info(), tc.warn) {
					t.Errorf("no warning containing %q: %v", tc.warn, f.Info().Warnings)
				}
			})
		}
	}
}

// The current segments of the checkpoint (cur_node/data_segno and blkoff) do
// not change the answer: the SIT entries of the current segments are flushed
// by the checkpoint and already list every block written to them; blocks
// beyond the write cursor have a clear bit because nothing was written there.
func TestUnallocatedIgnoresCurrentSegmentCursor(t *testing.T) {
	o := f2fstest.Options{Segments: 2, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 0, Valid: offsets(0, 10)}}}
	l := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)
	for _, pack := range []int{1, 2} {
		base := int(l.CP)
		if pack == 2 {
			base += f2fstest.BlocksPerSeg
		}
		h := img[base*bs:]
		h[36] = 0    // cur_node_segno[0]
		h[68] = 5    // cur_node_blkoff[0]
		h[84] = 0    // cur_data_segno[0]
		h[116] = 100 // cur_data_blkoff[0]
		f2fstest.SealCheckpoint(img, pack)
	}
	f := mustOpen(t, img)
	valid := map[uint32]bool{}
	segBlocks(l, valid, 0, offsets(0, 10))
	checkFree(t, unalloc(t, f), freeOf(l, valid))
}

// A journal entry for a segment outside the main area is ignored with a
// warning, and an impossible entry count is capped at what fits in the block.
func TestSITJournalHostile(t *testing.T) {
	t.Run("segment beyond the main area", func(t *testing.T) {
		o := f2fstest.Options{Segments: 2, NoSIT: true, SITJournal: []f2fstest.SITEntry{{Segno: 99, Valid: []int{1}}}}
		l := f2fstest.Geometry(o)
		f := mustOpen(t, f2fstest.Build(o, nil))
		checkFree(t, unalloc(t, f), freeOf(l, nil))
		if !hasWarning(f.Info(), "segment 99") {
			t.Errorf("no warning naming segment 99: %v", f.Info().Warnings)
		}
	})
	t.Run("entry count beyond the block", func(t *testing.T) {
		o := f2fstest.Options{Segments: 2, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 1, Valid: []int{9}}}}
		l := f2fstest.Geometry(o)
		img := f2fstest.Build(o, nil)
		for _, base := range []int{int(l.CP), int(l.CP) + f2fstest.BlocksPerSeg} {
			j := (base+int(l.StartSum)+2)*bs + 3584
			img[j], img[j+1] = 0xff, 0xff // n_sits = 65535
		}
		f := mustOpen(t, img)
		runs := unalloc(t, f)
		valid := map[uint32]bool{}
		segBlocks(l, valid, 1, []int{9})
		// The six decodable entries are all-zero entries for segment 0.
		checkFree(t, runs, freeOf(l, valid))
		if !hasWarning(f.Info(), "at most 6") {
			t.Errorf("no warning about the journal count: %v", f.Info().Warnings)
		}
	})
}

// rangeFault fails reads that touch [lo, hi) while armed.
type rangeFault struct {
	r      io.ReaderAt
	lo, hi int64
	armed  bool
	err    error
}

func (f *rangeFault) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off < f.hi && off+int64(len(p)) > f.lo {
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

// A SIT block that cannot be read gives a warning and its segments are not
// reported (never taken for free); an unreadable journal means no segment can
// be trusted. Genuine I/O errors are not cached.
func TestUnallocatedUnreadableSIT(t *testing.T) {
	injected := errors.New("device unplugged")
	o := f2fstest.Options{Segments: 58, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 56, Valid: []int{0}}}}
	l := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)

	t.Run("one SIT block", func(t *testing.T) {
		r := &rangeFault{r: bytes.NewReader(img), lo: int64(l.SIT) * bs, hi: int64(l.SIT+1) * bs, err: injected}
		f, err := f2fs.Open(r, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		runs, err := f.Unallocated()
		if err != nil {
			t.Fatalf("Unallocated: %v", err)
		}
		// Segments 0..54 are skipped; 55..57 are reported.
		valid := map[uint32]bool{}
		segBlocks(l, valid, 56, []int{0})
		for s := uint32(0); s < 55; s++ {
			segBlocks(l, valid, s, offsets(0, 512))
		}
		checkFree(t, runs, freeOf(l, valid))
		if !hasWarning(f.Info(), "SIT block 0") || !hasWarning(f.Info(), "device unplugged") {
			t.Errorf("no warning naming the unreadable SIT block: %v", f.Info().Warnings)
		}
		// The device comes back: a fresh call reports everything.
		r.armed = false
		runs, err = f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		valid = map[uint32]bool{}
		segBlocks(l, valid, 56, []int{0})
		checkFree(t, runs, freeOf(l, valid))
	})
	t.Run("journal", func(t *testing.T) {
		probe := mustOpen(t, img)
		cp := probe.Checkpoint()
		lo := int64(cp.Addr+cp.StartSum) * bs
		r := &rangeFault{r: bytes.NewReader(img), lo: lo, hi: lo + 3*bs, err: injected}
		f, err := f2fs.Open(r, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		runs, err := f.Unallocated()
		if err != nil || len(runs) != 0 {
			t.Fatalf("Unallocated = %v, %v; want nothing reported", runs, err)
		}
		if !hasWarning(f.Info(), "SIT journal") {
			t.Errorf("no warning about the SIT journal: %v", f.Info().Warnings)
		}
	})
}

// A truncated image never reports blocks the image does not hold, and the
// number of runs is capped (with a warning).
func TestUnallocatedBoundsAndCap(t *testing.T) {
	t.Run("truncated image", func(t *testing.T) {
		o := f2fstest.Options{Segments: 3}
		l := f2fstest.Geometry(o)
		img := f2fstest.Build(o, nil)
		cut := int64(l.Main)*bs + 768*bs
		f := mustOpen(t, img[:cut])
		runs := unalloc(t, f)
		if want := []filesys.Run{{Offset: int64(l.Main) * bs, Length: 768 * bs}}; !slices.Equal(runs, want) {
			t.Errorf("runs = %v, want %v", runs, want)
		}
	})
	t.Run("run cap", func(t *testing.T) {
		o := f2fstest.Options{Segments: 2, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 0, Valid: []int{1, 3, 5, 7, 9, 11}}}}
		f := mustOpen(t, f2fstest.Build(o, nil))
		f.SetUnallocatedRunCap(3)
		runs := unalloc(t, f)
		if len(runs) != 3 {
			t.Errorf("%d runs, want the cap of 3: %v", len(runs), runs)
		}
		if !hasWarning(f.Info(), "more than 3") {
			t.Errorf("no warning about the cap: %v", f.Info().Warnings)
		}
	})
}

// Random damage to the SIT blocks and the summaries never panics, and every
// run that comes back lies inside the main area, sorted and disjoint.
func TestUnallocatedMutatedNeverPanics(t *testing.T) {
	o := f2fstest.Options{Segments: 3}
	a := f2fstest.NewAlloc(o, firstNode)
	a.Stride = 3
	nodes, data := a.File(o, f2fstest.Inode{NID: dataNID, Mode: regMode, Size: 200 * bs},
		f2fstest.FileData{Blocks: blocksOf(dpat(200*bs, 2), 0)})
	o.Nodes, o.Data = nodes, data
	o.SITJournal = []f2fstest.SITEntry{{Segno: 1, Valid: []int{4}}}
	base := f2fstest.Build(o, nil)
	l := f2fstest.Geometry(o)
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test input, not security
	for range 300 {
		img := bytes.Clone(base)
		for range 1 + rng.Intn(24) {
			var off int
			switch rng.Intn(3) {
			case 0:
				off = int(l.SIT)*bs + rng.Intn(4*bs)
			case 1:
				off = (int(l.CP)+int(l.StartSum)+rng.Intn(3))*bs + 3584 + rng.Intn(512)
			default:
				off = int(l.SIT)*bs + rng.Intn(16)*74 + rng.Intn(74)
			}
			img[off] = byte(rng.Intn(256))
		}
		f, err := open(img)
		if err != nil {
			continue
		}
		runs, err := f.Unallocated()
		if err != nil {
			continue
		}
		lo, hi := int64(l.Main)*bs, int64(l.BlockCount)*bs
		prev := lo
		for _, r := range runs {
			if r.Length <= 0 || r.Offset < prev || r.Offset+r.Length > hi || r.Offset%bs != 0 || r.Length%bs != 0 {
				t.Fatalf("bad run %+v after %d (main area %d..%d)", r, prev, lo, hi)
			}
			prev = r.Offset + r.Length
		}
	}
}

// Info().Warnings stays live and deduplicated across repeated Unallocated calls.
func TestUnallocatedWarningsAccumulate(t *testing.T) {
	o := f2fstest.Options{Segments: 2, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 0, Valid: []int{3}, VBlocks: ptr16(600)}}}
	f := mustOpen(t, f2fstest.Build(o, nil))
	before := len(f.Info().Warnings)
	unalloc(t, f)
	n1 := len(f.Info().Warnings)
	unalloc(t, f)
	if n1 != before+1 || len(f.Info().Warnings) != n1 {
		t.Errorf("warnings %d -> %d -> %d, want one new warning, not repeated", before, n1, len(f.Info().Warnings))
	}
	if !strings.Contains(strings.Join(f.Info().Warnings, "\n"), "segment 0") {
		t.Errorf("warnings do not name the segment: %v", f.Info().Warnings)
	}
}
