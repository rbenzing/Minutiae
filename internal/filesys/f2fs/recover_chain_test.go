package f2fs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The stale map walker and the index node source (3D Task 5). staleMap is
// driven with a fake nodeSource; indexSource runs on builder images.

const chainVer = 20

type chainEnv struct {
	o   f2fstest.Options
	l   f2fstest.Layout
	img []byte
}

// newChainEnv: 10 main segments, the last two node segments with summaries,
// checkpoint version chainVer. Data addresses start at data(0).
func newChainEnv() *chainEnv {
	o := f2fstest.Options{Segments: 10, NodeSegs: 2, SSA: true, Version: chainVer}
	return &chainEnv{o: o, l: f2fstest.Geometry(o), img: f2fstest.Build(o, nil)}
}

func (e *chainEnv) data(n uint32) uint32 { return e.l.Main + 16 + n }
func (e *chainEnv) nodeAddr(seg, j uint32) uint32 {
	return e.l.Main + seg*f2fstest.BlocksPerSeg + j
}

// plant leaves a stale direct-node-like block (words) of nid, owned by ino,
// at footer offset ofs and version ver, in node segment seg (8 or 9), slot j.
func (e *chainEnv) plant(seg, j, nid, ino, ofs uint32, ver uint64, words ...uint32) uint32 {
	a := e.nodeAddr(seg, j)
	copy(blockAt(e.img, a), f2fstest.DirectNodeBlock(nid, ino, words))
	f2fstest.SetFooter(e.img, a, f2fstest.Footer{NID: nid, Ino: ino, Flag: ofs << 3, CPVer: ver})
	f2fstest.SetSSAEntry(e.img, e.o, a, nid)
	return a
}

func (e *chainEnv) src(t *testing.T, below uint32) (*FS, *indexSource) {
	t.Helper()
	f := openImg(t, e.img)
	idx, err := f.nodeIndex()
	if err != nil {
		t.Fatalf("nodeIndex: %v", err)
	}
	return f, &indexSource{f: f, idx: idx, below: below}
}

// fakeSource serves node blocks by nid and records the requests.
type fakeNode struct {
	b    []byte
	addr uint32
	stop string
	err  error
}

type fakeSource struct {
	nodes map[uint32]fakeNode
	seen  [][3]uint32 // nid, owner, ofs
}

func (s *fakeSource) nodeFor(nid, owner, ofs uint32) ([]byte, uint32, string, error) {
	s.seen = append(s.seen, [3]uint32{nid, owner, ofs})
	n, ok := s.nodes[nid]
	if !ok {
		return nil, 0, "node-missing", nil
	}
	return n.b, n.addr, n.stop, n.err
}

func words(ws ...uint32) []byte { return f2fstest.DirectNodeBlock(0, 0, ws) }

// mkInode is a parsed-inode stand-in: the walker uses only nid, size, the
// data slots and i_nid.
func mkInode(nid uint32, size int64, addrs []uint32, nids [nidsPerInode]uint32) *inode {
	raw := make([]byte, blockSize)
	for i, a := range addrs {
		binary.LittleEndian.PutUint32(raw[iAddr+4*i:], a)
	}
	return &inode{nid: nid, size: size, raw: raw, addrStart: iAddr, addrSlots: addrsPerIno, nids: nids}
}

func seq(from uint32, n int) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = from + uint32(i)
	}
	return out
}

func TestStaleMapAddrSlots(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	a0, a3 := e.data(0), e.data(50)
	size := int64(5*blockSize - 100)
	in := mkInode(20, size, []uint32{a0, a0 + 1, a0 + 2, a3, a3 + 1}, [5]uint32{})
	r, err := f.staleMap(in, &fakeSource{})
	if err != nil {
		t.Fatal(err)
	}
	want := []filesys.Run{{Offset: int64(a0) * blockSize, Length: 3 * blockSize}, {Offset: int64(a3) * blockSize, Length: size - 3*blockSize}}
	if !slices.Equal(r.runs, want) || r.stop != "" || r.covered != size {
		t.Errorf("staleMap = %+v, want runs %v, no stop, covered %d", r, want, size)
	}
}

func TestStaleMapDirectAndIndirect(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	base := e.data(0)
	size := int64(923+2*addrsPerBlock+5)*blockSize - 7
	in := mkInode(20, size, seq(base, 923), [5]uint32{101, 102, 103, 0, 0})
	src := &fakeSource{nodes: map[uint32]fakeNode{
		101: {b: words(seq(base+923, addrsPerBlock)...), addr: 9001},
		102: {b: words(seq(base+923+addrsPerBlock, addrsPerBlock)...), addr: 9002},
		103: {b: words(104), addr: 9003},
		104: {b: words(seq(base+923+2*addrsPerBlock, 5)...), addr: 9004},
	}}
	r, err := f.staleMap(in, src)
	if err != nil {
		t.Fatal(err)
	}
	if r.stop != "" || r.covered != size {
		t.Fatalf("stop %q covered %d, want none and %d", r.stop, r.covered, size)
	}
	if len(r.runs) != 1 || r.runs[0].Offset != int64(base)*blockSize || r.runs[0].Length != size {
		t.Errorf("runs = %v, want one run of %d bytes at block %d", r.runs, size, base)
	}
	if !slices.Equal(r.nodes, []uint32{9001, 9002, 9003, 9004}) {
		t.Errorf("nodes = %v, want the four node addresses in walk order", r.nodes)
	}
	var ofs []uint32
	for _, s := range src.seen {
		if s[1] != 20 {
			t.Errorf("request %v: owner %d, want 20", s, s[1])
		}
		ofs = append(ofs, s[2])
	}
	if !slices.Equal(ofs, []uint32{1, 2, 3, 4}) {
		t.Errorf("required offsets = %v, want [1 2 3 4]", ofs)
	}
}

func TestNodeOffsetTable(t *testing.T) {
	for _, c := range []struct {
		slot, i, k int
		want       uint32
	}{
		{0, -1, -1, 1},
		{1, -1, -1, 2},
		{2, -1, -1, 3},
		{2, 0, -1, 4},
		{2, 7, -1, 11},
		{3, -1, -1, 1022},
		{3, 0, -1, 1023},
		{3, 5, -1, 1028},
		{4, -1, -1, 2041},
		{4, 0, -1, 2042},
		{4, 1, -1, 2042 + 1019},
		{4, 0, 0, 2043},
		{4, 1, 2, 2043 + 1019 + 2},
	} {
		if got := nodeOffset(c.slot, c.i, c.k); got != c.want {
			t.Errorf("nodeOffset(%d, %d, %d) = %d, want %d", c.slot, c.i, c.k, got, c.want)
		}
	}
}

func TestStaleMapStops(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	a := e.data(0)
	full := seq(a, 923)
	bigSize := int64(923+2*addrsPerBlock)*blockSize + 5
	stopNode := func(stop string) *fakeSource {
		return &fakeSource{nodes: map[uint32]fakeNode{40: {stop: stop}}}
	}
	prefix923 := []filesys.Run{{Offset: int64(a) * blockSize, Length: 923 * blockSize}}
	one := []filesys.Run{{Offset: int64(a) * blockSize, Length: blockSize}}
	cases := []struct {
		name      string
		in        *inode
		src       *fakeSource
		wantStop  string
		wantRuns  []filesys.Run
		wantCover int64
	}{
		{"hole in i_addr", mkInode(20, 3*blockSize, []uint32{a, 0, a + 1}, [5]uint32{}), &fakeSource{}, "hole@1", one, blockSize},
		{"NEW_ADDR", mkInode(20, 3*blockSize, []uint32{a, newAddr, a + 1}, [5]uint32{}), &fakeSource{}, "hole@1", one, blockSize},
		{"nid 0 at i_nid[0]", mkInode(20, bigSize, full, [5]uint32{}), &fakeSource{}, "hole@923", prefix923, 923 * blockSize},
		{"node missing", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-missing"), "node-missing@40", prefix923, 923 * blockSize},
		{"node owner", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-owner"), "node-owner@40", prefix923, 923 * blockSize},
		{"node ofs", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-ofs"), "node-ofs@40", prefix923, 923 * blockSize},
		{"node tie", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-tie"), "node-tie@40", prefix923, 923 * blockSize},
		{"node evicted", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-evicted"), "node-evicted@40", prefix923, 923 * blockSize},
		{"node not node", mkInode(20, bigSize, full, [5]uint32{40}), stopNode("node-not-node"), "node-not-node@40", prefix923, 923 * blockSize},
		{"address below the main area", mkInode(20, 3*blockSize, []uint32{a, 1, a + 1}, [5]uint32{}), &fakeSource{}, "address-outside-main@1", one, blockSize},
		{"address past the volume", mkInode(20, 2*blockSize, []uint32{a, 0x7FFFFFFF}, [5]uint32{}), &fakeSource{}, "address-outside-main@1", one, blockSize},
		{
			"address repeated at logical block 4", mkInode(20, 8*blockSize, []uint32{a, a + 1, a + 2, a + 3, a + 1, a + 9}, [5]uint32{}), &fakeSource{}, "address-repeated@4",
			[]filesys.Run{{Offset: int64(a) * blockSize, Length: 4 * blockSize}},
			4 * blockSize,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := f.staleMap(c.in, c.src)
			if err != nil {
				t.Fatal(err)
			}
			if r.stop != c.wantStop {
				t.Errorf("stop = %q, want %q", r.stop, c.wantStop)
			}
			if !slices.Equal(r.runs, c.wantRuns) {
				t.Errorf("runs = %v, want %v", r.runs, c.wantRuns)
			}
			if r.covered != c.wantCover || r.covered >= c.in.size {
				t.Errorf("covered = %d, want %d (< size %d)", r.covered, c.wantCover, c.in.size)
			}
		})
	}

	t.Run("cycle", func(t *testing.T) {
		in := mkInode(20, bigSize, full, [5]uint32{101, 102, 50})
		src := &fakeSource{nodes: map[uint32]fakeNode{
			101: {b: words(seq(a+923, addrsPerBlock)...), addr: 9001},
			102: {b: words(seq(a+923+addrsPerBlock, addrsPerBlock)...), addr: 9002},
			50:  {b: words(50), addr: 9050},
		}}
		r, err := f.staleMap(in, src)
		if err != nil {
			t.Fatal(err)
		}
		if r.stop != "node-cycle@50" || r.covered != int64(923+2*addrsPerBlock)*blockSize {
			t.Errorf("stop %q covered %d, want node-cycle@50 after the direct nodes", r.stop, r.covered)
		}
	})
	t.Run("run cap", func(t *testing.T) {
		in := mkInode(20, 6*blockSize, []uint32{a, a + 2, a + 4, a + 6}, [5]uint32{})
		r, err := f.staleMapLimit(in, &fakeSource{}, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := []filesys.Run{{Offset: int64(a) * blockSize, Length: blockSize}, {Offset: int64(a+2) * blockSize, Length: blockSize}}
		if r.stop != "run-cap@2" || !slices.Equal(r.runs, want) || r.covered != 2*blockSize {
			t.Errorf("stop %q runs %v covered %d, want run-cap@2 with two runs", r.stop, r.runs, r.covered)
		}
	})
	t.Run("node cap", func(t *testing.T) {
		in := mkInode(20, bigSize, full, [5]uint32{101, 102})
		src := &fakeSource{nodes: map[uint32]fakeNode{
			101: {b: words(seq(a+923, addrsPerBlock)...), addr: 9001},
			102: {b: words(seq(a+923+addrsPerBlock, addrsPerBlock)...), addr: 9002},
		}}
		r, err := f.staleMapLimit(in, src, 1)
		if err != nil {
			t.Fatal(err)
		}
		if r.stop != "run-cap@1941" || len(r.nodes) != 1 {
			t.Errorf("stop %q nodes %v, want run-cap@1941 after one node block", r.stop, r.nodes)
		}
	})
	t.Run("source error is returned alone", func(t *testing.T) {
		boom := errors.New("disk on fire")
		in := mkInode(20, bigSize, full, [5]uint32{40})
		r, err := f.staleMap(in, &fakeSource{nodes: map[uint32]fakeNode{40: {err: boom}}})
		if !errors.Is(err, boom) || len(r.runs) != 0 {
			t.Errorf("staleMap = %+v, %v; want the error alone", r, err)
		}
	})
	t.Run("negative size is corrupt", func(t *testing.T) {
		_, err := f.staleMap(mkInode(20, -1, nil, [5]uint32{}), &fakeSource{})
		if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	t.Run("size 0 has no runs", func(t *testing.T) {
		r, err := f.staleMap(mkInode(20, 0, nil, [5]uint32{}), &fakeSource{})
		if err != nil || len(r.runs) != 0 || r.stop != "" || r.covered != 0 {
			t.Errorf("staleMap = %+v, %v", r, err)
		}
	})
}

func TestStaleMapAllocatesNothingFromSize(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	run := func(size int64) float64 {
		in := mkInode(20, size, nil, [5]uint32{})
		return testing.AllocsPerRun(20, func() {
			r, err := f.staleMap(in, &fakeSource{})
			if err != nil || r.stop != "hole@0" {
				t.Fatalf("staleMap = %+v, %v", r, err)
			}
		})
	}
	small, huge := run(blockSize), run(1<<62)
	if huge > small+1 || huge > 12 {
		t.Errorf("allocations: %v for a 4 KiB size, %v for 1<<62; want a small constant independent of size", small, huge)
	}
}

func TestIndexSourceEvictedStops(t *testing.T) {
	e := newChainEnv()
	for v := range 8 { // eight foreign copies fill the cap with the highest versions
		e.plant(8, uint32(v), 30, 99, 1, uint64(11+v))
	}
	e.plant(9, 0, 30, 20, 1, 3) // the owner's copy is the lowest: evicted
	_, src := e.src(t, chainVer)
	_, _, stop, err := src.nodeFor(30, 20, 1)
	if err != nil || stop != "node-evicted" {
		t.Errorf("nodeFor = stop %q, err %v; want node-evicted", stop, err)
	}
}

func TestIndexSourceRules(t *testing.T) {
	check := func(t *testing.T, src *indexSource, nid, owner, ofs uint32, wantStop string) ([]byte, uint32) {
		t.Helper()
		b, addr, stop, err := src.nodeFor(nid, owner, ofs)
		if err != nil || stop != wantStop {
			t.Fatalf("nodeFor(%d, %d, %d) = stop %q, err %v; want stop %q", nid, owner, ofs, stop, err, wantStop)
		}
		return b, addr
	}
	t.Run("nothing is missing", func(t *testing.T) {
		_, src := newChainEnv().src(t, chainVer)
		check(t, src, 30, 20, 1, "node-missing")
	})
	t.Run("foreign owner", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 99, 1, 5)
		_, src := e.src(t, chainVer)
		check(t, src, 30, 20, 1, "node-owner")
	})
	t.Run("wrong offset", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 5, 5)
		_, src := e.src(t, chainVer)
		check(t, src, 30, 20, 1, "node-ofs")
	})
	t.Run("offset 0 is an inode not a node", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 0, 5)
		_, src := e.src(t, chainVer)
		check(t, src, 30, 20, 1, "node-not-node")
	})
	t.Run("the highest version below wins", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 1, 3, 1000)
		a7 := e.plant(8, 1, 30, 20, 1, 7, 2000)
		e.plant(9, 0, 30, 20, 1, 5, 3000)
		_, src := e.src(t, chainVer)
		b, addr := check(t, src, 30, 20, 1, "")
		if addr != a7 || len(b) < 4 || binary.LittleEndian.Uint32(b) != 2000 {
			t.Errorf("took block %d (%d bytes), want the version 7 copy at %d with word 2000", addr, len(b), a7)
		}
	})
	t.Run("versions at or above below are ignored", func(t *testing.T) {
		e := newChainEnv()
		a5 := e.plant(8, 0, 30, 20, 1, 5, 1)
		e.plant(8, 1, 30, 20, 1, 15, 2)
		_, src := e.src(t, 10)
		if _, addr := check(t, src, 30, 20, 1, ""); addr != a5 {
			t.Errorf("took block %d, want the version 5 copy at %d", addr, a5)
		}
		e2 := newChainEnv()
		e2.plant(8, 0, 30, 20, 1, 15, 2)
		_, src2 := e2.src(t, 10)
		check(t, src2, 30, 20, 1, "node-missing")
	})
	t.Run("a tie with different bytes", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 1, 7, 1)
		e.plant(9, 0, 30, 20, 1, 7, 2)
		_, src := e.src(t, chainVer)
		check(t, src, 30, 20, 1, "node-tie")
	})
	t.Run("a tie with identical bytes", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 1, 7, 1)
		e.plant(9, 0, 30, 20, 1, 7, 1)
		_, src := e.src(t, chainVer)
		b, addr := check(t, src, 30, 20, 1, "")
		if len(b) != blockSize || addr == 0 || !bytes.Equal(b, blockAt(e.img, addr)) {
			t.Errorf("took %d bytes at %d, want a whole copy", len(b), addr)
		}
	})
	t.Run("a lower tie does not matter", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 1, 3, 1)
		e.plant(9, 0, 30, 20, 1, 3, 2)
		a9 := e.plant(9, 1, 30, 20, 1, 7, 3)
		_, src := e.src(t, chainVer)
		if _, addr := check(t, src, 30, 20, 1, ""); addr != a9 {
			t.Errorf("took %d, want %d", addr, a9)
		}
	})
	t.Run("an index error is returned", func(t *testing.T) {
		e := newChainEnv()
		e.plant(8, 0, 30, 20, 1, 3)
		e.plant(8, 1, 31, 20, 1, 3)
		f := openImg(t, e.img)
		f.indexCap = 1
		idx, err := f.nodeIndex()
		if err != nil {
			t.Fatal(err)
		}
		_, _, stop, err := (&indexSource{f: f, idx: idx, below: chainVer}).nodeFor(30, 20, 1)
		if !errors.Is(err, filesys.ErrLimit) || stop != "" {
			t.Errorf("nodeFor = stop %q, err %v; want ErrLimit", stop, err)
		}
	})
}

func TestNodeOffsetLastChildren(t *testing.T) {
	last := addrsPerBlock - 1
	for _, c := range []struct {
		slot, i, k int
		want       uint32
	}{
		{2, last, -1, 4 + 1017},
		{3, last, -1, 1023 + 1017},
		{4, last, -1, 2042 + 1017*1019},
		{4, last, last, 2043 + 1017*1019 + 1017},
		{4, 0, last, 2043 + 1017},
	} {
		if got := nodeOffset(c.slot, c.i, c.k); got != c.want {
			t.Errorf("nodeOffset(%d, %d, %d) = %d, want %d", c.slot, c.i, c.k, got, c.want)
		}
	}
	if got := nodeOffset(3, last, -1); got >= nodeOffset(4, -1, -1) {
		t.Errorf("the last child of i_nid[3] (%d) collides with i_nid[4] (%d)", got, nodeOffset(4, -1, -1))
	}
}

func TestStaleMapTreeEnd(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	a := e.data(0)
	next := a + 1000
	direct := func(addr uint32) fakeNode {
		n := fakeNode{b: words(next, next+2), addr: addr}
		next += 4
		return n
	}
	src := &fakeSource{nodes: map[uint32]fakeNode{
		11: direct(9011), 12: direct(9012),
		21: {b: words(22, 23), addr: 9021}, 22: direct(9022), 23: direct(9023),
		31: {b: words(32, 33), addr: 9031}, 32: direct(9032), 33: direct(9033),
		41: {b: words(42, 43), addr: 9041},
		42: {b: words(44, 45), addr: 9042}, 43: {b: words(46, 47), addr: 9043},
		44: direct(9044), 45: direct(9045), 46: direct(9046), 47: direct(9047),
	}}
	size := int64(923+20+3) * blockSize
	in := mkInode(20, size, seq(a, 923), [5]uint32{11, 12, 21, 31, 41})
	r, err := f.staleMapFan(in, src, maxFileRuns, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.stop != "tree-end@943" || r.covered != 943*blockSize || len(r.nodes) != 15 {
		t.Errorf("stop %q covered %d nodes %d, want tree-end@943, %d bytes, 15 nodes", r.stop, r.covered, len(r.nodes), 943*blockSize)
	}
}

type failReader struct{ err error }

func (r failReader) ReadAt(_ []byte, _ int64) (int, error) { return 0, r.err }

func TestIndexSourceReadFailureIsNotMissing(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		corrupt bool
	}{
		{"short read", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"io error", errors.New("disk on fire"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newChainEnv()
			e.plant(8, 0, 30, 20, 1, 5, 1)
			f, src := e.src(t, chainVer)
			f.data = failReader{c.err}
			_, _, stop, err := src.nodeFor(30, 20, 1)
			if stop != "" || err == nil || errors.Is(err, filesys.ErrCorrupt) != c.corrupt {
				t.Errorf("nodeFor = stop %q, err %v; want an error (corrupt %v) and no stop", stop, err, c.corrupt)
			}
		})
	}
}

func TestStaleMapShortNodeBlockIsAnError(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	a := e.data(0)
	in := mkInode(20, int64(923+10)*blockSize, seq(a, 923), [5]uint32{40})
	r, err := f.staleMap(in, &fakeSource{nodes: map[uint32]fakeNode{40: {b: make([]byte, 100), addr: 9040}}})
	if !errors.Is(err, filesys.ErrCorrupt) || len(r.runs) != 0 || r.stop != "" {
		t.Errorf("staleMap = %+v, %v; want a CorruptError alone", r, err)
	}
}

func TestStaleMapRepeatedCutTrimsNodes(t *testing.T) {
	e := newChainEnv()
	f := openImg(t, e.img)
	a := e.data(0)
	in := mkInode(20, int64(923+addrsPerBlock+10)*blockSize, seq(a, 923), [5]uint32{101, 102})
	src := &fakeSource{nodes: map[uint32]fakeNode{
		101: {b: words(seq(a+923, addrsPerBlock)...), addr: 9001},
		102: {b: words(seq(a, 10)...), addr: 9002}, // repeats the first run
	}}
	r, err := f.staleMap(in, src)
	if err != nil {
		t.Fatal(err)
	}
	if r.stop != "address-repeated@1941" || r.covered != 1941*blockSize || !slices.Equal(r.nodes, []uint32{9001}) {
		t.Errorf("stop %q covered %d nodes %v, want address-repeated@1941 and only node 9001", r.stop, r.covered, r.nodes)
	}
}
