package f2fs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The roll-forward chain walk and the replay rules (3H Task 4). Images come from the
// f2fstest builder (chain nodes at the warm cursor, no NAT entry, SIT clear).

func rfOpts() f2fstest.Options { return f2fstest.Options{NodeSegs: 2, Unclean: true} }

// rfImage builds the image and opens it; CPFlags default to CPCRCRecovery.
func rfImage(t *testing.T, o f2fstest.Options, files []f2fstest.File, chain ...f2fstest.ChainNode) (*FS, []byte, *f2fstest.Tree) {
	t.Helper()
	if o.CPFlags == 0 {
		o.CPFlags = f2fstest.CPCRCRecovery
	}
	o.Chain = append(slices.Clone(o.Chain), chain...)
	img, tree := f2fstest.BuildTree(o, files)
	return openImg(t, img), img, tree
}

func rfData(blocks int) []byte {
	b := make([]byte, blocks*blockSize)
	for i := range b {
		b[i] = byte(i/blockSize + 1)
	}
	return b
}

// twoFiles: a small file (1 chain node) and a 1000-block file (inode + one direct node).
func twoFiles() []f2fstest.File {
	return []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/rf/a.bin", Data: rfData(3), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: rfData(1000), PostCheckpoint: true},
	}
}

// rfStart is the first node-segment block: where the builder puts the warm cursor of a
// chain-only image.
func rfStart(o f2fstest.Options) uint32 {
	l := f2fstest.Geometry(o)
	return l.Main + (l.MainSegs-uint32(o.NodeSegs))*f2fstest.BlocksPerSeg
}

func rfInodeNode(o f2fstest.Options, ino uint32, fsync, dent bool) f2fstest.ChainNode {
	return f2fstest.ChainNode{
		Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: ino, Mode: 0o100644, Links: 1, PIno: f2fstest.RootIno, Name: "h"}),
		Fsync: fsync, Dentry: dent,
	}
}

func rfDirectNode(nid, ino, ofs uint32, fsync bool) f2fstest.ChainNode {
	b := f2fstest.DirectNodeBlock(nid, ino, nil)
	binary.LittleEndian.PutUint32(b[nodeFooterOff+8:], ofs<<offsetBitShift|footColdBit)
	return f2fstest.ChainNode{Block: b, Fsync: fsync}
}

func u32p(v uint32) *uint32 { return &v }
func u64p(v uint64) *uint64 { return &v }

func patchFooter(img []byte, addr uint32, edit func(*f2fstest.Footer)) {
	ft := parseFooter(blockAt(img, addr))
	w := f2fstest.Footer{NID: ft.nid, Ino: ft.ino, Flag: ft.flag, CPVer: ft.cpVer, NextBlk: ft.nextBlk}
	edit(&w)
	f2fstest.SetFooter(img, addr, w)
}

func TestChainVersionRuleCRC(t *testing.T) {
	f, img, tree := rfImage(t, rfOpts(), twoFiles())
	if len(tree.ChainAddrs) != 3 {
		t.Fatalf("builder chain = %v, want 3 nodes", tree.ChainAddrs)
	}
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("version@%d", tree.ChainAddrs[2]+1); st.Nodes != 3 || st.Stop != want || !st.Complete {
		t.Errorf("stats = %+v, want 3 nodes, stop %s, complete", st, want)
	}
	sum, _ := f.ChainSummary()
	if !sum.CRC || sum.NoCRC || sum.FooterVersion != f2fstest.ChainFooterVersion(img) || sum.FooterVersion>>32 == 0 {
		t.Errorf("summary = %+v, want crc-recovery with footer version %#x", sum, f2fstest.ChainFooterVersion(img))
	}
	if sum.Start != uint64(tree.ChainAddrs[0]) {
		t.Errorf("start = %d, want %d", sum.Start, tree.ChainAddrs[0])
	}

	patchFooter(img, tree.ChainAddrs[1], func(w *f2fstest.Footer) { w.CPVer ^= 1 << 40 })
	st, err = openImg(t, img).ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("version@%d", tree.ChainAddrs[1]); st.Nodes != 1 || st.Stop != want || !st.Complete {
		t.Errorf("after the high-half edit: stats = %+v, want 1 node, stop %s", st, want)
	}
}

func TestChainVersionRuleNoCRC(t *testing.T) {
	o := rfOpts()
	o.CPFlags = f2fstest.CPCRCRecovery | f2fstest.CPNoCRCRecovery
	f, img, tree := rfImage(t, o, twoFiles())
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes != 3 || !st.Complete {
		t.Errorf("stats = %+v, want 3 complete nodes", st)
	}
	sum, _ := f.ChainSummary()
	if !sum.NoCRC || sum.FooterVersion>>32 != 0 {
		t.Errorf("summary = %+v, want the full version without a high half", sum)
	}
	patchFooter(img, tree.ChainAddrs[2], func(w *f2fstest.Footer) { w.CPVer |= 0x1234 << 32 })
	st, err = openImg(t, img).ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("version@%d", tree.ChainAddrs[2]); st.Nodes != 2 || st.Stop != want {
		t.Errorf("a CRC high half under NOCRC: stats = %+v, want 2 nodes, stop %s", st, want)
	}
}

func TestChainStopsAtOlderVersion(t *testing.T) {
	o := rfOpts()
	n := rfInodeNode(o, 20, true, true)
	n.CPVer = u64p(1) // the previous checkpoint's version (the default is 2)
	f, _, tree := rfImage(t, o, nil, n)
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("version@%d", tree.ChainAddrs[0]); st.Nodes != 0 || st.Stop != want || !st.Complete {
		t.Errorf("stats = %+v, want 0 nodes, stop %s, complete", st, want)
	}
}

func TestChainUsesChosenCheckpointOnly(t *testing.T) {
	o := rfOpts()
	o.Pack2Newer = true
	_, img, tree := rfImage(t, o, twoFiles())
	// Pack 1 is the older pack: give it another warm cursor.
	base := int(binary.LittleEndian.Uint32(img[1024+76:]))
	hdr := img[base*blockSize : (base+1)*blockSize]
	binary.LittleEndian.PutUint16(hdr[cpCurNodeBlkoff+2:], binary.LittleEndian.Uint16(hdr[cpCurNodeBlkoff+2:])+1)
	f2fstest.SealCheckpoint(img, 1)
	f := openImg(t, img)
	if f.cp.pack != 2 {
		t.Fatalf("chosen pack = %d, want 2", f.cp.pack)
	}
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := f.ChainSummary()
	if st.Nodes != 3 || sum.Start != uint64(tree.ChainAddrs[0]) {
		t.Errorf("nodes = %d, start = %d, want 3 from the newer pack's cursor %d", st.Nodes, sum.Start, tree.ChainAddrs[0])
	}
}

func TestChainStops(t *testing.T) {
	o := rfOpts()
	start := rfStart(o)
	l := f2fstest.Geometry(o)
	natCap := l.NATCapacity()
	next := func(n f2fstest.ChainNode, to uint32) f2fstest.ChainNode { n.Next = u32p(to); return n }
	cases := []struct {
		name     string
		chain    []f2fstest.ChainNode
		cut      int // keep the image up to start+cut blocks (0 = no cut)
		stop     string
		complete bool
		nodes    int
		warn     bool
	}{
		{
			name: "outside main", chain: []f2fstest.ChainNode{next(rfInodeNode(o, 20, true, true), l.Main-1)},
			stop: fmt.Sprintf("outside-main@%d", l.Main-1), complete: true, nodes: 1,
		},
		{
			name: "next is itself", chain: []f2fstest.ChainNode{next(rfInodeNode(o, 20, true, true), start)},
			stop: fmt.Sprintf("cycle@%d", start), complete: true, nodes: 1, warn: true,
		},
		{
			name: "two node loop", chain: []f2fstest.ChainNode{rfInodeNode(o, 20, true, true), next(rfDirectNode(21, 20, 1, true), start)},
			stop: fmt.Sprintf("cycle@%d", start), complete: true, nodes: 2, warn: true,
		},
		{
			name: "footer nid zero", chain: []f2fstest.ChainNode{rfInodeNode(o, 20, true, true), rfDirectNode(0, 20, 1, true)},
			stop: fmt.Sprintf("footer@%d", start+1), complete: false, nodes: 1,
		},
		{
			name: "footer ino at the NAT capacity", chain: []f2fstest.ChainNode{rfInodeNode(o, 20, true, true), rfDirectNode(21, natCap, 1, true)},
			stop: fmt.Sprintf("footer@%d", start+1), complete: false, nodes: 1,
		},
		{
			name: "image cut", chain: []f2fstest.ChainNode{rfInodeNode(o, 20, true, true), rfDirectNode(21, 20, 1, true), rfDirectNode(22, 20, 2, true)},
			cut: 2, stop: fmt.Sprintf("image-end@%d", start+2), complete: false, nodes: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oo := o
			oo.CPFlags = f2fstest.CPCRCRecovery
			oo.Chain = tc.chain
			img, _ := f2fstest.BuildTree(oo, nil)
			if tc.cut > 0 {
				img = img[:int(start+uint32(tc.cut))*blockSize]
			}
			f := openImg(t, img)
			st, err := f.ChainStats()
			if err != nil {
				t.Fatal(err)
			}
			if st.Stop != tc.stop || st.Complete != tc.complete || st.Nodes != tc.nodes {
				t.Errorf("stats = %+v, want stop %s complete %v nodes %d", st, tc.stop, tc.complete, tc.nodes)
			}
			sum, _ := f.ChainSummary()
			if got := slices.ContainsFunc(sum.Warnings, func(w string) bool { return strings.Contains(w, "looped") }); got != tc.warn {
				t.Errorf("loop warning = %v, want %v (%v)", got, tc.warn, sum.Warnings)
			}
		})
	}
}

func TestChainCapIsIncomplete(t *testing.T) {
	f, _, tree := rfImage(t, rfOpts(), twoFiles())
	f.SetChainCap(2)
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("cap@%d", tree.ChainAddrs[2]); st.Nodes != 2 || st.Stop != want || st.Complete {
		t.Errorf("stats = %+v, want 2 nodes, stop %s, incomplete", st, want)
	}
}

func TestChainCursorOutsideMainIsCorrupt(t *testing.T) {
	for _, cur := range []f2fstest.CurSeg{{Seg: 16}, {Seg: 3, Off: 512}} {
		o := rfOpts()
		o.CurNode[1] = cur
		f, _, _ := rfImage(t, o, nil)
		if _, err := f.chain(); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("cursor %+v: err = %v, want ErrCorrupt", cur, err)
		}
	}
}

func TestChainHighHalfVersionIsUnsupported(t *testing.T) {
	o := rfOpts()
	o.Version = 1<<32 | 5
	f, _, _ := rfImage(t, o, nil)
	if _, err := f.chain(); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestChainEmptyOnCleanImage(t *testing.T) {
	o := rfOpts()
	o.Unclean = false
	f, _, _ := rfImage(t, o, nil)
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := f.ChainSummary()
	if st.Nodes != 0 || !st.Complete || len(sum.Warnings) != 0 || !strings.HasPrefix(st.Stop, "version@") {
		t.Errorf("stats = %+v, warnings %v, want an empty complete chain without warnings", st, sum.Warnings)
	}
}

func TestChainUnmountWithNodesWarns(t *testing.T) {
	o := rfOpts()
	o.Unclean = false
	f, _, _ := rfImage(t, o, twoFiles())
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := f.ChainSummary()
	if st.Nodes != 3 || !sum.Unmount || !slices.ContainsFunc(sum.Warnings, func(w string) bool {
		return strings.Contains(w, "unmount checkpoint is followed by a roll-forward chain")
	}) {
		t.Errorf("nodes = %d, unmount = %v, warnings = %v", st.Nodes, sum.Unmount, sum.Warnings)
	}
}

func TestChainAddsNoFilesystemWarning(t *testing.T) {
	o := rfOpts()
	loop := rfInodeNode(o, 20, true, true)
	loop.Next = u32p(rfStart(o))
	normal, _, _ := rfImage(t, o, twoFiles())
	hostile, _, _ := rfImage(t, o, nil, loop)
	for name, f := range map[string]*FS{"normal": normal, "hostile": hostile} {
		before := f.Info().Warnings
		if _, err := f.chain(); err != nil {
			t.Fatal(err)
		}
		if after := f.Info().Warnings; !slices.Equal(before, after) {
			t.Errorf("%s: Info().Warnings changed by the chain: %v -> %v", name, before, after)
		}
	}
}

func TestChainBuiltOnce(t *testing.T) {
	f, _, _ := rfImage(t, rfOpts(), twoFiles())
	if _, err := f.chain(); err != nil {
		t.Fatal(err)
	}
	st, err := f.ChainStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Builds != 1 || st.Nodes != 3 || st.BlocksRead != st.Nodes+1 {
		t.Errorf("stats = %+v, want 1 build and %d blocks read", st, st.Nodes+1)
	}
}

// failOnce fails the first read that touches the byte at off.
type failOnce struct {
	r     io.ReaderAt
	off   int64
	err   error
	fired bool
}

func (r *failOnce) ReadAt(p []byte, off int64) (int, error) {
	if !r.fired && off <= r.off && r.off < off+int64(len(p)) {
		r.fired = true
		return 0, r.err
	}
	return r.r.ReadAt(p, off)
}

func TestChainIOErrorIsNotCached(t *testing.T) {
	o := rfOpts()
	o.CPFlags = f2fstest.CPCRCRecovery
	img, tree := f2fstest.BuildTree(o, twoFiles())
	boom := errors.New("boom")
	r := &failOnce{r: bytes.NewReader(img), off: int64(tree.ChainAddrs[1]) * blockSize, err: boom}
	f, err := Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.chain(); !errors.Is(err, boom) || errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("first call: err = %v, want the I/O error and not ErrCorrupt", err)
	}
	st, err := f.ChainStats()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if st.Nodes != 3 || st.Builds != 2 {
		t.Errorf("second call: stats = %+v, want 3 nodes after 2 builds", st)
	}
}

func TestChainEntryIs20Bytes(t *testing.T) {
	if n := unsafe.Sizeof(chainEntry{}); n != 20 {
		t.Errorf("sizeof(chainEntry) = %d, want 20", n)
	}
}

func TestNodeKindTable(t *testing.T) {
	cases := []struct {
		ofs  uint32
		want string
	}{
		{0, "inode"},
		{1, "direct"},
		{2, "direct"},
		{3, "indirect"},
		{4, "direct"},
		{1021, "direct"},
		{1022, "indirect"},
		{1023, "direct"},
		{2040, "direct"},
		{2041, "double-indirect"},
		{2042, "indirect"},
		{2043, "direct"},
		{3061, "indirect"},
		{3062, "direct"},
		{xattrNodeOfs, "xattr"},
	}
	for _, tc := range cases {
		if got := nodeKind(tc.ofs); got != tc.want {
			t.Errorf("nodeKind(%d) = %q, want %q", tc.ofs, got, tc.want)
		}
	}
}

func TestWouldReplayRules(t *testing.T) {
	const (
		F = footFsyncBit
		D = footDentBit
	)
	e := func(nid, ino, ofs, marks uint32) chainEntry {
		return chainEntry{nid: nid, ino: ino, flag: ofs<<offsetBitShift | marks | footColdBit}
	}
	cases := []struct {
		name  string
		live  map[uint32]bool
		chain []chainEntry
		want  []bool
		kinds map[uint32]string
	}{
		{"new inode DF then direct F", nil, []chainEntry{e(5, 5, 0, F|D), e(6, 5, 1, F)}, []bool{true, true}, map[uint32]string{5: "post-checkpoint"}},
		{"new direct F then inode DF", nil, []chainEntry{e(6, 5, 1, F), e(5, 5, 0, F|D)}, []bool{true, true}, map[uint32]string{5: "post-checkpoint"}},
		{"new inode F without D", nil, []chainEntry{e(5, 5, 0, F)}, []bool{false}, map[uint32]string{5: "not-replayed"}},
		{"new inode D without F then direct F", nil, []chainEntry{e(5, 5, 0, D), e(6, 5, 1, F)}, []bool{false, false}, map[uint32]string{5: "not-replayed"}},
		{"inode D then inode F|D", nil, []chainEntry{e(5, 5, 0, D), e(5, 5, 0, F|D)}, []bool{true, true}, map[uint32]string{5: "post-checkpoint"}},
		{"direct without F after the last fsync", nil, []chainEntry{e(5, 5, 0, F|D), e(6, 5, 1, F), e(7, 5, 2, 0)}, []bool{true, true, false}, nil},
		{"live inode F then direct F", map[uint32]bool{5: true}, []chainEntry{e(5, 5, 0, F), e(6, 5, 1, F)}, []bool{true, true}, map[uint32]string{5: "live-update"}},
		{"live inode without any fsync node", map[uint32]bool{5: true}, []chainEntry{e(5, 5, 0, 0)}, []bool{false}, map[uint32]string{5: "live-update"}},
		{"indirect node in the middle", nil, []chainEntry{e(5, 5, 0, F|D), e(7, 5, 3, F), e(6, 5, 4, F)}, []bool{true, false, true}, nil},
		{"indirect node last", nil, []chainEntry{e(5, 5, 0, F|D), e(6, 5, 1, F), e(7, 5, 3, F)}, []bool{true, true, false}, nil},
		{"double indirect never replays", nil, []chainEntry{e(5, 5, 0, F|D), e(7, 5, 2041, F), e(6, 5, 1, F)}, []bool{true, false, true}, nil},
		{"xattr node is a dnode", nil, []chainEntry{e(5, 5, 0, F|D), e(8, 5, xattrNodeOfs, F)}, []bool{true, true}, nil},
		{"another inode in between", nil, []chainEntry{e(5, 5, 0, F|D), e(9, 8, 0, F), e(6, 5, 1, F)}, []bool{true, false, true}, map[uint32]string{5: "post-checkpoint", 8: "not-replayed"}},
		{"unknown inode number", nil, nil, nil, map[uint32]string{77: "not-replayed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.chain {
				tc.chain[i].block = uint32(1000 + i)
			}
			c := deriveChain(tc.chain, tc.live)
			for i, want := range tc.want {
				if got := c.wouldReplay(i); got != want {
					t.Errorf("wouldReplay(%d) = %v, want %v", i, got, want)
				}
			}
			for ino, want := range tc.kinds {
				if got := c.fileKind(ino); got != want {
					t.Errorf("fileKind(%d) = %q, want %q", ino, got, want)
				}
			}
			if c.wouldReplay(-1) || c.wouldReplay(len(tc.chain)) {
				t.Error("wouldReplay answered for an index outside the chain")
			}
		})
	}
}

func TestChainLiveInoFromCurrentNAT(t *testing.T) {
	o := rfOpts()
	o.CPFlags = f2fstest.CPCRCRecovery
	files := []f2fstest.File{{Path: "/x", Data: rfData(2)}}
	live := uint32(f2fstest.RootIno + 1) // the first file's inode number
	const stray = 99
	// A live update of /x and an inode the NAT does not map.
	o.Chain = []f2fstest.ChainNode{rfInodeNode(o, live, true, false), rfInodeNode(o, stray, true, true)}
	img, _ := f2fstest.BuildTree(o, files)
	// The NON-current NAT copy maps the stray inode: it must not be consulted.
	l := f2fstest.Geometry(o)
	ent := img[int(l.NATBlock(stray/f2fstest.NATPerBlock, true))*blockSize+(stray%f2fstest.NATPerBlock)*9:]
	ent[0] = 1
	binary.LittleEndian.PutUint32(ent[1:], stray)
	binary.LittleEndian.PutUint32(ent[5:], l.Main)
	f := openImg(t, img)
	c, err := f.chain()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 2 || !c.live[live] || c.live[stray] {
		t.Fatalf("entries = %d, live = %v, want %d live and %d not", len(c.entries), c.live, live, stray)
	}
	if k := c.fileKind(live); k != "live-update" {
		t.Errorf("fileKind(live) = %q, want live-update", k)
	}
	if k := c.fileKind(stray); k != "post-checkpoint" {
		t.Errorf("fileKind(stray) = %q, want post-checkpoint", k)
	}
}

// TestChainNeverHoldsIndirectNode: a file that needs an indirect node has no such node in
// the chain, with the node omitted (the kernel writes none before a checkpoint, probe H7)
// and with a stale copy elsewhere in the log (StaleIndirect). The reader's node-missing
// answer for it is Task 6's (TestPostCheckpointIndirectFileStopsAtNodeMissing and
// TestChainSourceNeverServesIndirect run on both images there).
func TestChainNeverHoldsIndirectNode(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("StaleIndirect=%v", stale), func(t *testing.T) {
			f, _, tree := rfImage(t, rfOpts(), []f2fstest.File{
				{Path: "/rf", Dir: true},
				{Path: "/rf/big.bin", Data: rfData(923 + 2*1018 + 5), PostCheckpoint: true, StaleIndirect: stale},
			})
			c, err := f.chain()
			if err != nil {
				t.Fatal(err)
			}
			in := map[uint32]bool{}
			for _, e := range c.entries {
				if k := nodeKind(e.flag >> offsetBitShift); k == "indirect" || k == "double-indirect" {
					t.Errorf("chain holds a %s node (nid %d)", k, e.nid)
				}
				in[e.nid] = true
			}
			var missing []uint32
			for _, nid := range tree.Nodes["/rf/big.bin"] {
				if !in[nid] {
					missing = append(missing, nid)
				}
			}
			if len(c.entries) != 4 || len(missing) != 1 {
				t.Fatalf("chain nodes = %d, nodes of the file not in the chain = %v, want 4 and one (the indirect node)", len(c.entries), missing)
			}
			if _, err := f.natGet(missing[0]); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("the NAT maps the indirect node %d: %v", missing[0], err)
			}
		})
	}
}
