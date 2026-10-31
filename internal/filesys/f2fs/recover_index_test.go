package f2fs_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The footer index (3D Task 4). The images have 6 main segments of which the
// last two (4 and 5) are node segments typed warm node, with SSA summaries.
// The checkpoint version is idxVer, so a copy qualifies with a footer version
// below it. plant writes a stale node block (footer plus summary entry) where
// the kernel would leave one: a clear SIT bit in a node segment.

const (
	idxVer     = 20
	idxNodeSeg = 4 // first node segment
)

type idxEnv struct {
	o   f2fstest.Options
	l   f2fstest.Layout
	img []byte
}

func newIdxEnv(mod func(*f2fstest.Options)) *idxEnv {
	o := f2fstest.Options{Segments: 6, NodeSegs: 2, SSA: true, Version: idxVer}
	if mod != nil {
		mod(&o)
	}
	return &idxEnv{o: o, l: f2fstest.Geometry(o), img: f2fstest.Build(o, nil)}
}

func (e *idxEnv) addr(seg, j uint32) uint32 { return e.l.Main + seg*f2fstest.BlocksPerSeg + j }

// plant leaves a stale node block of nid with footer version ver at block j of
// segment seg, its summary entry naming nid.
func (e *idxEnv) plant(seg, j, nid uint32, ver uint64) uint32 {
	a := e.addr(seg, j)
	f2fstest.SetFooter(e.img, a, f2fstest.Footer{NID: nid, Ino: nid, CPVer: ver})
	f2fstest.SetSSAEntry(e.img, e.o, a, nid)
	return a
}

func (e *idxEnv) open(t *testing.T) *f2fs.FS {
	t.Helper()
	return mustOpen(t, e.img)
}

func idxLookup(t *testing.T, f *f2fs.FS, nid uint32) ([]f2fs.StaleNode, f2fs.NodeMeta) {
	t.Helper()
	n, m, err := f.NodeIndexLookup(nid)
	if err != nil {
		t.Fatalf("lookup(%d): %v", nid, err)
	}
	return n, m
}

func vers(ns []f2fs.StaleNode) []uint32 {
	out := make([]uint32, len(ns))
	for i, n := range ns {
		out[i] = n.Ver
	}
	return out
}

func TestNodeIndexFindsStaleCopies(t *testing.T) {
	e := newIdxEnv(nil)
	a3 := e.plant(4, 0, 10, 3)
	a7 := e.plant(4, 1, 10, 7)
	a5 := e.plant(5, 2, 10, 5)
	a4 := e.plant(5, 0, 11, 4)
	f2fstest.SetFooter(e.img, a4, f2fstest.Footer{NID: 11, Ino: 9, Flag: 3 << 3, CPVer: 4})
	f := e.open(t)

	got, _ := idxLookup(t, f, 10)
	if !slices.Equal(vers(got), []uint32{7, 5, 3}) {
		t.Fatalf("lookup(10) versions = %v, want [7 5 3]", vers(got))
	}
	for i, wantAddr := range []uint32{a7, a5, a3} {
		if got[i].Addr != wantAddr || got[i].NID != 10 || got[i].Ino != 10 || got[i].Ofs != 0 || !got[i].SSA {
			t.Errorf("copy %d = %+v, want addr %d nid=ino=10 ofs 0 ssa", i, got[i], wantAddr)
		}
	}
	one, _ := idxLookup(t, f, 11)
	if len(one) != 1 || one[0].Addr != a4 || one[0].Ino != 9 || one[0].Ofs != 3 {
		t.Errorf("lookup(11) = %+v, want one copy at %d with ino 9, ofs 3", one, a4)
	}
	if none, m := idxLookup(t, f, 999); len(none) != 0 || m != (f2fs.NodeMeta{}) {
		t.Errorf("lookup(999) = %+v %+v, want nothing", none, m)
	}
}

// A live block (valid SIT bit) is never indexed, and a block the kernel wrote
// for an unlinked file is, through the builder's own unlink layout.
func TestNodeIndexSkipsLiveBlocks(t *testing.T) {
	t.Run("valid bit set", func(t *testing.T) {
		e := newIdxEnv(nil)
		a := e.plant(4, 0, 12, 3)
		f2fstest.SetSITBit(e.img, e.o, a, true)
		e.plant(4, 1, 12, 2)
		got, _ := idxLookup(t, e.open(t), 12)
		if !slices.Equal(vers(got), []uint32{2}) {
			t.Errorf("lookup(12) versions = %v, want only the stale copy [2]", vers(got))
		}
	})
	t.Run("builder tree", func(t *testing.T) {
		o := f2fstest.Options{Segments: 6, NodeSegs: 2, SSA: true, Version: idxVer}
		img, tree := f2fstest.BuildTree(o, []f2fstest.File{
			{Path: "/live.bin", Data: bytes.Repeat([]byte{1}, 3*bs)},
			{Path: "/gone.bin", Data: bytes.Repeat([]byte{2}, 3*bs), Deleted: true, Unlinked: true},
		})
		f := mustOpen(t, img)
		live, gone := tree.NID["/live.bin"], tree.NID["/gone.bin"]
		if got, _ := idxLookup(t, f, live); len(got) != 0 {
			t.Errorf("the live inode %d is indexed: %+v", live, got)
		}
		got, _ := idxLookup(t, f, gone)
		if len(got) != 1 || got[0].Addr != tree.Addr[gone] || got[0].Ino != gone || got[0].Ver != 0 || !got[0].SSA {
			t.Errorf("lookup(unlinked %d) = %+v, want its inode block at %d", gone, got, tree.Addr[gone])
		}
	})
}

// A node-looking block in a segment typed data, or in a node segment that is a
// current DATA segment of the checkpoint, is old user data and not indexed.
func TestNodeIndexSkipsDataSegments(t *testing.T) {
	t.Run("data typed segment", func(t *testing.T) {
		e := newIdxEnv(nil)
		e.plant(1, 3, 13, 3) // segment 1 is a data segment
		e.plant(4, 0, 14, 3)
		f := e.open(t)
		if got, _ := idxLookup(t, f, 13); len(got) != 0 {
			t.Errorf("a block of a data segment is indexed: %+v", got)
		}
		if got, _ := idxLookup(t, f, 14); len(got) != 1 {
			t.Errorf("the node segment copy is missing: %+v", got)
		}
	})
	t.Run("current data segment", func(t *testing.T) {
		e := newIdxEnv(nil)
		for _, pack := range []int{1, 2} {
			base := int(e.l.CP)
			if pack == 2 {
				base += f2fstest.BlocksPerSeg
			}
			e.img[base*bs+84] = idxNodeSeg // cur_data_segno[0]
			f2fstest.SealCheckpoint(e.img, pack)
		}
		e.plant(4, 0, 15, 3)
		e.plant(5, 0, 16, 3)
		f := e.open(t)
		if got, _ := idxLookup(t, f, 15); len(got) != 0 {
			t.Errorf("a block of a current data segment is indexed: %+v", got)
		}
		if got, _ := idxLookup(t, f, 16); len(got) != 1 {
			t.Errorf("the other node segment copy is missing: %+v", got)
		}
	})
}

// In a current node segment only the blocks below the cursor are taken (the
// rest is what an earlier use of the segment left), and its summary block is
// not on disk, so it is neither read nor used to drop a copy.
func TestNodeIndexIgnoresBlocksBeyondCurrentCursor(t *testing.T) {
	e := newIdxEnv(func(o *f2fstest.Options) { o.CurNode[1] = f2fstest.CurSeg{Seg: idxNodeSeg, Off: 3} })
	e.plant(4, 5, 20, 3)
	a := e.plant(4, 2, 21, 3)
	f2fstest.SetSSAEntry(e.img, e.o, a, 0) // an unwritten summary of a current segment
	f := e.open(t)
	if got, _ := idxLookup(t, f, 20); len(got) != 0 {
		t.Errorf("a block at offset 5 of a segment with cursor 3 is indexed: %+v", got)
	}
	got, _ := idxLookup(t, f, 21)
	if len(got) != 1 || got[0].Addr != a || got[0].SSA {
		t.Errorf("lookup(21) = %+v, want the copy at offset 2, kept, without an SSA confirmation", got)
	}
	if s := f.NodeIndexStats(); s.SSADropped != 0 || s.BlocksRead != 3+512 {
		t.Errorf("stats = %+v, want 3 blocks read of the current segment and 512 of the other, none dropped", s)
	}
}

func TestNodeIndexReadsEachBlockOnce(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 22, 3)
	e.plant(5, 7, 23, 4)
	f := e.open(t)
	for range 2 {
		if err := f.BuildNodeIndex(); err != nil {
			t.Fatal(err)
		}
		idxLookup(t, f, 22)
		idxLookup(t, f, 23)
	}
	s := f.NodeIndexStats()
	if s.Builds != 1 || s.SegmentsScanned != 2 || s.BlocksRead != 2*f2fstest.BlocksPerSeg || s.SegmentsSkipped != 0 || s.Entries != 2 {
		t.Errorf("stats = %+v, want one build, 2 segments, 1024 blocks read once, 2 entries", s)
	}
}

func TestNodeIndexPerNIDCap(t *testing.T) {
	e := newIdxEnv(nil)
	for v := uint32(1); v <= 10; v++ {
		e.plant(4, v-1, 30, uint64(v))
	}
	f := e.open(t)
	got, m := idxLookup(t, f, 30)
	if !slices.Equal(vers(got), []uint32{10, 9, 8, 7, 6, 5, 4, 3}) {
		t.Errorf("kept versions = %v, want 10..3", vers(got))
	}
	if s := f.NodeIndexStats(); s.Evicted != 2 || s.Entries != 8 {
		t.Errorf("stats = %+v, want 2 evicted and 8 entries", s)
	}
	if !m.Evicted || m.NID != 30 {
		t.Errorf("meta = %+v, want evicted for nid 30", m)
	}
	if _, other := idxLookup(t, f, 31); other.Evicted {
		t.Errorf("an unrelated nid is flagged evicted")
	}
}

func TestNodeIndexDropsPostCheckpointCopies(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 32, 5)
	e.plant(4, 1, 32, idxVer)   // written after the checkpoint
	e.plant(4, 2, 32, idxVer+1) // ditto
	e.plant(4, 3, 33, idxVer)   // only a post-checkpoint copy
	f := e.open(t)
	got, m := idxLookup(t, f, 32)
	if !slices.Equal(vers(got), []uint32{5}) || m.PostCheckpoint != 2 || m.Evicted {
		t.Errorf("lookup(32) = %v %+v, want only version 5 and 2 post-checkpoint copies", vers(got), m)
	}
	got, m = idxLookup(t, f, 33)
	if len(got) != 0 || m.PostCheckpoint != 1 || m.NID != 33 {
		t.Errorf("lookup(33) = %+v %+v, want no copy and 1 post-checkpoint copy", got, m)
	}
	if s := f.NodeIndexStats(); s.PostCheckpoint != 3 || s.Entries != 1 {
		t.Errorf("stats = %+v, want 3 post-checkpoint copies and 1 entry", s)
	}
}

// Q5: only the low 32 bits of a footer cp_ver count (the kernel ORs a CRC into
// the high half), and a checkpoint whose own high half is set cannot be
// compared at all.
func TestNodeIndexVersionComparison(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 34, 1<<32|5)
	got, _ := idxLookup(t, e.open(t), 34)
	if !slices.Equal(vers(got), []uint32{5}) {
		t.Errorf("a footer with a high half and low half 5 gives versions %v, want [5]", vers(got))
	}
	hi := newIdxEnv(func(o *f2fstest.Options) { o.Version = 1<<32 | 5 })
	if err := hi.open(t).BuildNodeIndex(); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("a checkpoint version with a high half gave %v, want ErrUnsupported", err)
	}
}

// Free space holds anything: a footer that is not a plausible node is not
// indexed, and is not damage either.
func TestNodeIndexFooterSanity(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 3, 1)                 // the root inode number
	e.plant(4, 1, e.l.NATCapacity(), 1) // beyond the NAT
	a := e.plant(4, 2, 40, 1)
	f2fstest.SetFooter(e.img, a, f2fstest.Footer{NID: 40, Ino: 0, CPVer: 1}) // no owner
	f := e.open(t)
	for _, nid := range []uint32{0, 3, e.l.NATCapacity(), 40} {
		if got, _, err := f.NodeIndexLookup(nid); err != nil || len(got) != 0 {
			t.Errorf("lookup(%d) = %+v, %v, want nothing", nid, got, err)
		}
	}
	if s := f.NodeIndexStats(); s.SegmentsSkipped != 0 || s.Entries != 0 {
		t.Errorf("stats = %+v, want nothing indexed and nothing skipped", s)
	}
}

// A truncated image: a node segment past the clamped end is neither scanned nor
// counted as skipped, and one cut by the end is scanned up to it (3D-R21).
func TestNodeIndexTruncatedImage(t *testing.T) {
	e := newIdxEnv(nil)
	a := e.plant(4, 50, 41, 3)
	e.plant(4, 150, 42, 3) // past the cut
	e.plant(5, 0, 43, 3)   // in the segment past the end
	cut := int(e.addr(4, 100)) * bs
	f := mustOpen(t, e.img[:cut])
	got, _ := idxLookup(t, f, 41)
	if len(got) != 1 || got[0].Addr != a {
		t.Errorf("lookup(41) = %+v, want the copy before the cut", got)
	}
	for _, nid := range []uint32{42, 43} {
		if got, _ := idxLookup(t, f, nid); len(got) != 0 {
			t.Errorf("lookup(%d) = %+v, want nothing from beyond the end of the image", nid, got)
		}
	}
	if s := f.NodeIndexStats(); s.SegmentsScanned != 1 || s.SegmentsSkipped != 0 || s.BlocksRead != 100 {
		t.Errorf("stats = %+v, want 1 segment scanned (100 blocks) and none skipped", s)
	}
}

func TestNodeIndexMemoryPerEntry(t *testing.T) {
	if f2fs.StaleNodeSize != 24 || f2fs.NodeMetaSize != 8 {
		t.Errorf("sizes = %d and %d bytes, want 24 and 8 (3D-R22)", f2fs.StaleNodeSize, f2fs.NodeMetaSize)
	}
}

// BenchmarkNodeIndexBuild reports measured B/op and allocs (ReportAllocs); the
// index-B/entry metric is an ESTIMATE from the struct sizes (flat entry plus
// the share of the per-nid record), not a measurement. No threshold.
func BenchmarkNodeIndexBuild(b *testing.B) {
	e := newIdxEnv(nil)
	for j := range uint32(f2fstest.BlocksPerSeg) {
		e.plant(4, j, 100+j, 3)
		e.plant(5, j, 100+j/2, 4)
	}
	b.ReportAllocs()
	var entries int
	for b.Loop() {
		f, err := f2fs.Open(bytes.NewReader(e.img), int64(len(e.img)))
		if err != nil {
			b.Fatal(err)
		}
		if err := f.BuildNodeIndex(); err != nil {
			b.Fatal(err)
		}
		entries = f.NodeIndexStats().Entries
	}
	b.ReportMetric(float64(f2fs.StaleNodeSize)+float64(f2fs.NodeMetaSize)*float64(512*3/2)/float64(entries), "index-B/entry")
}

func TestNodeIndexTotalCapIsLimit(t *testing.T) {
	e := newIdxEnv(nil)
	for i := range uint32(5) {
		e.plant(4, i, 60+i, 3)
	}
	f := e.open(t)
	f.SetNodeIndexCap(3)
	for _, nid := range []uint32{60, 64, 999} {
		_, _, err := f.NodeIndexLookup(nid)
		if !errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("lookup(%d) = %v, want ErrLimit and never ErrNotFound", nid, err)
		}
	}
	// Exactly at the cap is not capped.
	g := e.open(t)
	g.SetNodeIndexCap(5)
	if _, _, err := g.NodeIndexLookup(60); err != nil {
		t.Errorf("an index of exactly cap entries gave %v", err)
	}
}

func TestNodeIndexUnreadableSegmentIsUndecided(t *testing.T) {
	zero := uint16(5) // claims 5 valid blocks, its map has none
	t.Run("inconsistent SIT entry", func(t *testing.T) {
		e := newIdxEnv(func(o *f2fstest.Options) {
			o.SIT = []f2fstest.SITEntry{{Segno: 4, Type: 4, VBlocks: &zero}}
		})
		e.plant(5, 0, 50, 3)
		f := e.open(t)
		for _, nid := range []uint32{50, 999} {
			if _, _, err := f.NodeIndexLookup(nid); !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("lookup(%d) = %v, want ErrUndecided", nid, err)
			}
		}
		if !hasWarning(f.Info(), "segment 4") {
			t.Errorf("no warning naming segment 4: %v", f.Info().Warnings)
		}
		if s := f.NodeIndexStats(); s.SegmentsSkipped != 1 || s.SegmentsScanned != 1 {
			t.Errorf("stats = %+v, want 1 skipped and 1 scanned segment", s)
		}
	})
	t.Run("SIT journal cannot be interpreted", func(t *testing.T) {
		e := newIdxEnv(nil)
		for _, base := range []int{int(e.l.CP), int(e.l.CP) + f2fstest.BlocksPerSeg} {
			j := (base+int(e.l.StartSum)+2)*bs + 3584
			e.img[j], e.img[j+1] = 0xff, 0xff // n_sits = 65535
		}
		e.plant(4, 0, 51, 3)
		if _, _, err := e.open(t).NodeIndexLookup(51); !errors.Is(err, filesys.ErrUndecided) {
			t.Errorf("lookup with an unusable SIT journal = %v, want ErrUndecided", err)
		}
	})
}

func TestNodeIndexIOErrorIsNotCached(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 52, 3)
	injected := errors.New("injected read failure")
	r := &rangeFault{r: bytes.NewReader(e.img), lo: int64(e.addr(4, 0)) * bs, hi: int64(e.addr(6, 0)) * bs, err: injected}
	f, err := f2fs.Open(r, int64(len(e.img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	err = f.BuildNodeIndex()
	if !errors.Is(err, injected) || errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("first build = %v, want the I/O error and not corruption", err)
	}
	r.armed = false
	if err := f.BuildNodeIndex(); err != nil {
		t.Fatalf("second build = %v, want success (the I/O error must not be cached)", err)
	}
	if got, _ := idxLookup(t, f, 52); len(got) != 1 {
		t.Errorf("lookup(52) after the retry = %+v, want one copy", got)
	}
}

// G2 (kept): a stale copy in a closed node segment whose summary entry names
// another nid is not the block of that nid.
func TestNodeIndexSSAMismatchDropped(t *testing.T) {
	e := newIdxEnv(nil)
	a := e.plant(4, 0, 70, 3)
	f2fstest.SetSSAEntry(e.img, e.o, a, 71)
	e.plant(4, 1, 70, 2)
	f := e.open(t)
	got, _ := idxLookup(t, f, 70)
	if !slices.Equal(vers(got), []uint32{2}) || !got[0].SSA {
		t.Errorf("lookup(70) = %+v, want only the copy whose summary agrees", got)
	}
	if got, _ := idxLookup(t, f, 71); len(got) != 0 {
		t.Errorf("lookup(71) = %+v, want nothing: the footer names 70", got)
	}
	if s := f.NodeIndexStats(); s.SSADropped != 1 {
		t.Errorf("SSADropped = %d, want 1", s.SSADropped)
	}
	if !hasWarning(f.Info(), "1 stale copies were dropped because their summary entry names another node") {
		t.Errorf("the dropped copy is not surfaced: %v", f.Info().Warnings)
	}
}

// A summary block that is not a node summary is not trusted: the segment's copies
// are kept without a confirmation, with a counted warning.
func TestNodeIndexSSAWrongTypeIsNotTrusted(t *testing.T) {
	e := newIdxEnv(nil)
	a := e.plant(4, 0, 70, 3)
	f2fstest.SetSSAType(e.img, e.o, 4, 0)
	f := e.open(t)
	got, _ := idxLookup(t, f, 70)
	if len(got) != 1 || got[0].Addr != a || got[0].SSA {
		t.Errorf("lookup(70) = %+v, want the copy kept without an SSA confirmation", got)
	}
	if s := f.NodeIndexStats(); s.SSAWrongType != 1 {
		t.Errorf("SSAWrongType = %d, want 1", s.SSAWrongType)
	}
	if !hasWarning(f.Info(), "summary block of segment 4 is not a node summary") {
		t.Errorf("no warning: %v", f.Info().Warnings)
	}
}

// An unreadable SIT block hides the type of every segment it covers, so the
// index is undecided (the R5 harsh case: one bad block taints every lookup).
func TestNodeIndexUnreadableSITBlockIsUndecided(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 53, 3)
	sit := int64(e.l.SITBlock(0, false)) * bs
	r := &rangeFault{r: bytes.NewReader(e.img), lo: sit, hi: sit + bs, err: io.ErrUnexpectedEOF}
	f, err := f2fs.Open(r, int64(len(e.img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	for _, nid := range []uint32{53, 999} {
		if _, _, err := f.NodeIndexLookup(nid); !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("lookup(%d) = %v, want ErrUndecided", nid, err)
		}
	}
	if !hasWarning(f.Info(), "SIT block is unreadable, so its type is unknown") {
		t.Errorf("no warning about the unreadable SIT block: %v", f.Info().Warnings)
	}
	if s := f.NodeIndexStats(); s.SegmentsSkipped != 6 || s.SegmentsScanned != 0 {
		t.Errorf("stats = %+v, want all 6 segments of the block skipped and none scanned", s)
	}
}

// Post-checkpoint markers share the entry cap (accepted): when they fill it,
// even a nid with a real copy is ErrLimit, never "not found".
func TestNodeIndexPostCheckpointMarkersCountTowardTheCap(t *testing.T) {
	e := newIdxEnv(nil)
	e.plant(4, 0, 80, 3)
	for j := range uint32(3) {
		e.plant(4, 1+j, 81, uint64(idxVer+j))
	}
	f := e.open(t)
	f.SetNodeIndexCap(3)
	if _, _, err := f.NodeIndexLookup(80); !errors.Is(err, filesys.ErrLimit) {
		t.Errorf("lookup(80) = %v, want ErrLimit", err)
	}
}

// An unreadable summary block loses only the cross-check: the copies of that
// segment stay, unconfirmed, and the other segments are still checked.
func TestNodeIndexSSABlockCorruptKeepsCopiesUnconfirmed(t *testing.T) {
	e := newIdxEnv(nil)
	a := e.plant(4, 0, 90, 3)
	f2fstest.SetSSAEntry(e.img, e.o, a, 91) // would be dropped if the summary were read
	b := e.plant(5, 0, 92, 3)
	f2fstest.SetSSAEntry(e.img, e.o, b, 93) // segment 5's summary is readable: dropped
	ssa := int64(e.l.SSA+4) * bs
	r := &rangeFault{r: bytes.NewReader(e.img), lo: ssa, hi: ssa + bs, err: io.ErrUnexpectedEOF}
	f, err := f2fs.Open(r, int64(len(e.img)))
	if err != nil {
		t.Fatal(err)
	}
	r.armed = true
	got, _ := idxLookup(t, f, 90)
	if len(got) != 1 || got[0].Addr != a || got[0].SSA {
		t.Errorf("lookup(90) = %+v, want the copy kept without an SSA confirmation", got)
	}
	if got, _ := idxLookup(t, f, 92); len(got) != 0 {
		t.Errorf("lookup(92) = %+v, want the mismatching copy of segment 5 dropped", got)
	}
	if !hasWarning(f.Info(), "summary block of segment 4") {
		t.Errorf("no warning about the unreadable summary block: %v", f.Info().Warnings)
	}
	if s := f.NodeIndexStats(); s.SSADropped != 1 || s.SegmentsSkipped != 0 {
		t.Errorf("stats = %+v, want 1 dropped copy and no skipped segment", s)
	}
}
