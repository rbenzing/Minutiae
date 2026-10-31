package f2fs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Hostile chains (3H Task 7): every bound is a typed error or an exact stop token, no
// candidate covers more than its size, and nothing is allocated from a declared count.

// hostNode sets the footer offset in the node tree of a node block (the flag word is ofs<<3 | cold).
func hostNode(b []byte, ofs uint32) []byte {
	binary.LittleEndian.PutUint32(b[4072+8:], ofs<<3|1)
	return b
}

func hostFile(o f2fstest.Options, in f2fstest.Inode) f2fstest.ChainNode {
	in.NID, in.Name = 91, "h"
	return pcInode(o, in, true, true)
}

// hostCheck asserts what every hostile answer must satisfy: typed errors only, candidates that
// pass CheckCandidate and cover no more than their size.
func hostCheck(t *testing.T, f *f2fs.FS, img []byte) []filesys.PostCheckpointFile {
	t.Helper()
	files, err := f.PostCheckpointFiles()
	if err != nil {
		if !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUnsupported) {
			t.Fatalf("PostCheckpointFiles: untyped error %v", err)
		}
		return nil
	}
	for _, pf := range files {
		if pf.Err != nil && !errors.Is(pf.Err, filesys.ErrCorrupt) && !errors.Is(pf.Err, filesys.ErrUnsupported) {
			t.Errorf("%s: untyped error %v", pf.ID, pf.Err)
		}
		for _, c := range pf.Candidates {
			cov, err := filesys.CheckCandidate(c, int64(len(img)))
			if err != nil || cov > c.Size {
				t.Errorf("%s: covers %d of %d: %v", pf.ID, cov, c.Size, err)
			}
		}
	}
	return files
}

func TestRollForwardHostile(t *testing.T) {
	o := pcOpts()
	l := f2fstest.Geometry(o)
	a := mainAt(40)
	const nSlots = 914 // i_addr slots left after the 36-byte extra header
	slots := func() []uint32 {
		s := make([]uint32, nSlots)
		for i := range s {
			s[i] = a + uint32(i)
		}
		return s
	}
	stats := func(t *testing.T, f *f2fs.FS) f2fs.ChainStats {
		t.Helper()
		st, err := f.ChainStats()
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		return st
	}
	buildT := func(chain ...f2fstest.ChainNode) ([]byte, *f2fstest.Tree, *f2fs.FS) {
		oo := o
		oo.Chain = chain
		img, tr := f2fstest.BuildTree(oo, pcRoot)
		return img, tr, pcOpen(t, img)
	}
	build := func(chain ...f2fstest.ChainNode) ([]byte, *f2fs.FS) {
		img, _, f := buildT(chain...)
		return img, f
	}

	t.Run("next points into the SIT area", func(t *testing.T) {
		n := hostFile(o, f2fstest.Inode{Size: bs, Addrs: []uint32{a}})
		n.Next = &l.SIT
		img, f := build(n)
		st := stats(t, f)
		if st.Stop != fmt.Sprintf("outside-main@%d", l.SIT) || !st.Complete || st.Nodes != 1 {
			t.Errorf("stats = %+v, want outside-main@%d after one node", st, l.SIT)
		}
		hostCheck(t, f, img)
	})
	t.Run("two-node cycle", func(t *testing.T) {
		first := hostFile(o, f2fstest.Inode{Size: bs, Addrs: []uint32{a}})
		second := f2fstest.ChainNode{Block: f2fstest.DirectNodeBlock(92, 91, []uint32{a}), Fsync: true}
		_, tr, _ := buildT(first, second) // the chain's place is fixed by the layout
		start := tr.ChainAddrs[0]
		second.Next = &start
		img, f := build(first, second)
		st := stats(t, f)
		if st.Stop != fmt.Sprintf("cycle@%d", start) || !st.Complete || st.Nodes != 2 {
			t.Errorf("stats = %+v, want cycle@%d after two nodes", st, start)
		}
		hostCheck(t, f, img)
	})
	t.Run("i_size 1<<40", func(t *testing.T) {
		img, f := build(hostFile(o, f2fstest.Inode{Size: 1 << 40, Addrs: []uint32{a}}))
		files := hostCheck(t, f, img)
		if len(files) != 1 || !errors.Is(files[0].Err, filesys.ErrCorrupt) || len(files[0].Candidates) != 0 {
			t.Errorf("files = %+v, want one corrupt refusal", files)
		}
	})
	t.Run("i_nid[0] names the inode itself", func(t *testing.T) {
		img, f := build(hostFile(o, f2fstest.Inode{Size: 1000 * bs, Addrs: slots(), NIDs: [5]uint32{91}}))
		files := hostCheck(t, f, img)
		if len(files) != 1 || files[0].Err != nil || len(files[0].Candidates) != 1 {
			t.Fatalf("files = %+v, want one candidate", files)
		}
		c := files[0].Candidates[0]
		if got, ok := hasAssumption(c, "stale-chain-stop="); !ok || got != "stale-chain-stop=node-cycle@91" {
			t.Errorf("assumptions = %v, want stale-chain-stop=node-cycle@91", c.Assumptions)
		}
		if covered(c) != nSlots*bs {
			t.Errorf("covered %d, want only the nSlots inode slots", covered(c))
		}
	})
	t.Run("direct node addresses outside main", func(t *testing.T) {
		dn := f2fstest.ChainNode{Block: hostNode(f2fstest.DirectNodeBlock(92, 91, []uint32{l.BlockCount + 5, 0xFFFFFFF0}), 1), Fsync: true}
		img, f := build(hostFile(o, f2fstest.Inode{Size: (nSlots + 2) * bs, Addrs: slots(), NIDs: [5]uint32{92}}), dn)
		files := hostCheck(t, f, img)
		if len(files) != 1 {
			t.Fatalf("files = %+v", files)
		}
		if len(files[0].Candidates) != 1 {
			t.Fatalf("files = %+v, want one candidate", files)
		}
		c := files[0].Candidates[0]
		if got, _ := hasAssumption(c, "stale-chain-stop="); got != fmt.Sprintf("stale-chain-stop=address-outside-main@%d", nSlots) || covered(c) != nSlots*bs {
			t.Errorf("stop %q, covered %d; want address-outside-main@%d and only the inode slots", got, covered(c), nSlots)
		}
	})
	t.Run("indirect node in the chain is never served", func(t *testing.T) {
		per := func(from uint32) []uint32 {
			s := make([]uint32, 1018)
			for i := range s {
				s[i] = from + uint32(i)
			}
			return s
		}
		// The indirect node lies before the last fsync dnode, so only the rule that the source serves direct nodes keeps it out.
		chain := []f2fstest.ChainNode{
			hostFile(o, f2fstest.Inode{Size: (nSlots + 2*1018 + 5) * bs, Addrs: slots(), NIDs: [5]uint32{92, 93, 95}}),
			{Block: hostNode(f2fstest.IndirectNodeBlock(95, 91, []uint32{96, 97}), 3), Fsync: true},
			{Block: hostNode(f2fstest.DirectNodeBlock(92, 91, per(a+1000)), 1), Fsync: true},
			{Block: hostNode(f2fstest.DirectNodeBlock(93, 91, per(a+2100)), 2), Fsync: true},
		}
		img, f := build(chain...)
		files := hostCheck(t, f, img)
		if len(files) != 1 || len(files[0].Candidates) != 1 {
			t.Fatalf("files = %+v, want one candidate", files)
		}
		c := files[0].Candidates[0]
		if got, _ := hasAssumption(c, "stale-chain-stop="); got != "stale-chain-stop=node-missing@95" || covered(c) != (nSlots+2*1018)*bs {
			t.Errorf("stop %q, covered %d; want node-missing@95 and the inode slots plus two direct nodes", got, covered(c))
		}
	})
	t.Run("huge footer after a valid inode", func(t *testing.T) {
		bad := f2fstest.ChainNode{Block: f2fstest.DirectNodeBlock(0xFFFFFFF0, 0xFFFFFFF0, nil), Fsync: true}
		img, tr, f := buildT(hostFile(o, f2fstest.Inode{Size: bs, Addrs: []uint32{a}}), bad)
		st := stats(t, f)
		if st.Stop != fmt.Sprintf("footer@%d", tr.ChainAddrs[1]) || st.Complete || st.Nodes != 1 {
			t.Errorf("stats = %+v, want footer@%d, incomplete, one node", st, tr.ChainAddrs[1])
		}
		files := hostCheck(t, f, img)
		if len(files) != 1 || len(files[0].Candidates) != 1 || covered(files[0].Candidates[0]) != bs {
			t.Fatalf("files = %+v, want the valid inode's one-block candidate", files)
		}
		if _, ok := hasAssumption(files[0].Candidates[0], "rollforward-chain-cut=footer@"); !ok {
			t.Errorf("assumptions = %v, want the chain cut named", files[0].Candidates[0].Assumptions)
		}
	})
	t.Run("warm cursor out of range", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			off  int
			val  []byte
		}{
			{"segment", 40, []byte{0xff, 0xff, 0, 0}},
			{"block offset", 70, []byte{0x58, 0x02}}, // 600 >= 512 blocks per segment
		} {
			t.Run(tc.name, func(t *testing.T) {
				oo := o
				oo.Chain = []f2fstest.ChainNode{hostFile(o, f2fstest.Inode{Size: bs, Addrs: []uint32{a}})}
				img, _ := f2fstest.BuildTree(oo, pcRoot)
				for _, pack := range []int{1, 2} {
					hdr := img[(int(l.CP)+(pack-1)*f2fstest.BlocksPerSeg)*bs:]
					if binary.LittleEndian.Uint64(hdr) == 0 {
						continue // an unwritten pack
					}
					copy(hdr[tc.off:], tc.val)
					f2fstest.SealCheckpoint(img, pack)
				}
				f := pcOpen(t, img)
				if _, err := f.ChainStats(); !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "warm node cursor") {
					t.Errorf("chain error = %v, want a corrupt error naming the warm node cursor", err)
				}
				if files, err := f.PostCheckpointFiles(); !errors.Is(err, filesys.ErrCorrupt) || files != nil {
					t.Errorf("PostCheckpointFiles = %v, %v, want nil and ErrCorrupt", files, err)
				}
				if _, err := f.RollForward(func(filesys.RollForwardNode) bool { return true }); !errors.Is(err, filesys.ErrCorrupt) {
					t.Errorf("RollForward error = %v, want ErrCorrupt", err)
				}
			})
		}
	})
	t.Run("double-indirect footer offset", func(t *testing.T) {
		dind := f2fstest.ChainNode{Block: hostNode(f2fstest.IndirectNodeBlock(95, 91, []uint32{96}), 2041), Fsync: true}
		img, f := build(hostFile(o, f2fstest.Inode{Size: 3 * bs, Addrs: []uint32{a, a + 1, a + 2}, NIDs: [5]uint32{0, 0, 0, 0, 95}}), dind)
		var nodes []filesys.RollForwardNode
		if _, err := f.RollForward(func(n filesys.RollForwardNode) bool { nodes = append(nodes, n); return true }); err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 2 || nodes[1].Kind != "double-indirect" || nodes[1].WouldReplay {
			t.Errorf("nodes = %+v, want the second reported double-indirect and never replayed", nodes)
		}
		ws, _ := f.ChainWouldReplay()
		if len(ws) != 2 || !ws[0] || ws[1] {
			t.Errorf("WouldReplay = %v, want [true false]", ws)
		}
		files := hostCheck(t, f, img)
		if len(files) != 1 || len(files[0].Candidates) != 1 || covered(files[0].Candidates[0]) != 3*bs {
			t.Fatalf("files = %+v, want the file's own 3 blocks and nothing served from the double-indirect node", files)
		}
		if b, _ := hasBasis(files[0].Candidates[0], "chain-nodes="); b != "chain-nodes=0" {
			t.Errorf("basis %q: the double-indirect node was served", b)
		}
	})
	t.Run("1500-node chain with the cap at 1000", func(t *testing.T) {
		oo := o
		oo.NodeSegs = 4
		chain := []f2fstest.ChainNode{hostFile(oo, f2fstest.Inode{Size: bs, Addrs: []uint32{a}})}
		for i := 1; i < 1500; i++ {
			chain = append(chain, f2fstest.ChainNode{Block: f2fstest.DirectNodeBlock(uint32(100+i), 91, nil)})
		}
		oo.Chain = chain
		img, _ := f2fstest.BuildTree(oo, pcRoot)
		f := pcOpen(t, img)
		f.SetChainCap(1000)
		st := stats(t, f)
		if st.Nodes != 1000 || st.Complete || !strings.HasPrefix(st.Stop, "cap@") {
			t.Errorf("stats = %+v, want 1000 nodes, an incomplete cap@ stop", st)
		}
		ws, err := f.ChainWouldReplay()
		if err != nil || len(ws) != 1000 {
			t.Errorf("WouldReplay: %d, %v", len(ws), err)
		}
		files := hostCheck(t, f, img)
		if len(files) != 1 || len(files[0].Candidates) != 1 {
			t.Fatalf("files = %+v", files)
		}
		if _, ok := hasAssumption(files[0].Candidates[0], "rollforward-chain-cut=cap@"); !ok {
			t.Errorf("assumptions = %v, want the chain cut named", files[0].Candidates[0].Assumptions)
		}
		j, err := f.Journal()
		if err != nil || !j.NeedsRecovery {
			t.Errorf("Journal = %+v, %v; an incomplete chain needs recovery", j, err)
		}
	})
}

// Huge declared counts cost nothing: an inode that claims i_size 1<<62 and a chain block whose
// footer claims huge node numbers allocate no more than a benign image does.
func TestRollForwardHostileDeclaredCountsAllocateNothing(t *testing.T) {
	o := pcOpts()
	a := mainAt(40)
	img := func(size uint64, footerNID uint32) []byte {
		oo := o
		n := hostFile(oo, f2fstest.Inode{Size: size, Addrs: []uint32{a}})
		oo.Chain = []f2fstest.ChainNode{n, {Block: f2fstest.DirectNodeBlock(footerNID, footerNID, nil)}}
		b, _ := f2fstest.BuildTree(oo, pcRoot)
		return b
	}
	measure := func(b []byte) (allocs float64, bytesPer uint64) {
		run := func() {
			f, err := f2fs.Open(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.PostCheckpointFiles()
		}
		allocs = testing.AllocsPerRun(5, run)
		run() // warm
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		run()
		runtime.ReadMemStats(&m1)
		return allocs, m1.TotalAlloc - m0.TotalAlloc
	}
	benign, benignB := measure(img(bs, 92))
	small, smallB := measure(img(1<<41, 0xFFFFFFF0))
	huge, hugeB := measure(img(1<<62, 0xFFFFFFFF))
	t.Logf("allocations: benign %v, 1<<41 %v, 1<<62 %v; bytes: %d, %d, %d", benign, small, huge, benignB, smallB, hugeB)
	const slackBytes = 64 << 10
	if small > benign || huge > benign || huge > small+5 {
		t.Errorf("allocation counts: benign %v, i_size 1<<41 %v, i_size 1<<62 %v; declared counts must not drive them", benign, small, huge)
	}
	if smallB > benignB+slackBytes || hugeB > benignB+slackBytes {
		t.Errorf("allocated bytes: benign %d, i_size 1<<41 %d, i_size 1<<62 %d; declared counts must not drive them", benignB, smallB, hugeB)
	}
}
