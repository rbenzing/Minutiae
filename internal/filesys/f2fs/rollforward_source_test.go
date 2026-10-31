package f2fs

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The chain node source of the post-checkpoint map (3H Task 6, 3H-R6), on builder images.

// srcImage builds a hand-made chain on a chain-only image and opens it.
func srcImage(t *testing.T, o f2fstest.Options, chain ...f2fstest.ChainNode) (*FS, []byte, *f2fstest.Tree) {
	t.Helper()
	o.NodeSegs, o.Unclean, o.CPFlags = 2, true, f2fstest.CPCRCRecovery
	o.ExtraAttr, o.InodeChksum = true, true
	o.Chain = chain
	img, tree := f2fstest.BuildTree(o, nil)
	return openImg(t, img), img, tree
}

func srcInode(ino uint32, fsync, dent bool) f2fstest.ChainNode {
	o := f2fstest.Options{ExtraAttr: true, InodeChksum: true}
	return f2fstest.ChainNode{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: ino, Mode: 0o100644, Links: 1, Size: 1 << 20, PIno: f2fstest.RootIno, Name: "s", Extra: true}), Fsync: fsync, Dentry: dent}
}

// srcDirect is a direct node whose data slot 0 holds mark, so a served copy is identifiable.
func srcDirect(nid, ino, ofs, mark uint32, fsync bool) f2fstest.ChainNode {
	n := rfDirectNode(nid, ino, ofs, fsync)
	binary.LittleEndian.PutUint32(n.Block, mark)
	return n
}

// markOf is the mark of a served node, 0 for no node.
func markOf(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func TestChainSourceIgnoresNodesAfterLastFsync(t *testing.T) {
	const ino, nid = 91, 92
	// inode(DF) | direct (fsync) | direct copy without fsync, after the last fsync node.
	f, _, _ := srcImage(t, f2fstest.Options{},
		srcInode(ino, true, true), srcDirect(nid, ino, 1, 111, true), srcDirect(nid, ino, 1, 222, false))
	c, err := f.chain()
	if err != nil {
		t.Fatal(err)
	}
	if c.lastFsync[ino] != 1 {
		t.Fatalf("last fsync = %d, want 1", c.lastFsync[ino])
	}
	s := &chainSource{f: f, c: c, limit: c.lastFsync[ino]}
	b, addr, stop, err := s.nodeFor(nid, ino, 1)
	if err != nil || stop != "" || markOf(b) != 111 || addr != c.entries[1].block {
		t.Errorf("nodeFor = mark %d addr %d stop %q err %v, want the copy at index 1 (mark 111)", markOf(b), addr, stop, err)
	}
	// A limit that excludes both copies serves nothing: the later one is out of reach too.
	if _, _, stop, _ := (&chainSource{f: f, c: c, limit: 0}).nodeFor(nid, ino, 1); stop != "node-missing" {
		t.Errorf("limit 0: stop = %q, want node-missing", stop)
	}
}

func TestChainSourceNeverServesIndirect(t *testing.T) {
	const ino, nid = 91, 92
	for _, ofs := range []uint32{3, 1022, 2041, 2042} {
		f, _, _ := srcImage(t, f2fstest.Options{},
			srcInode(ino, true, true), srcDirect(nid, ino, ofs, 7, true))
		c, err := f.chain()
		if err != nil {
			t.Fatal(err)
		}
		s := &chainSource{f: f, c: c, limit: len(c.entries) - 1}
		if b, _, stop, err := s.nodeFor(nid, ino, ofs); stop != "node-missing" || b != nil || err != nil {
			t.Errorf("ofs %d: nodeFor = %d bytes stop %q err %v, want node-missing", ofs, len(b), stop, err)
		}
	}
}

func TestChainSourceRules(t *testing.T) {
	const ino, other, nid = 91, 95, 92
	f, _, _ := srcImage(t, f2fstest.Options{},
		srcInode(ino, true, true),
		srcDirect(nid, other, 1, 1, true), // footer ino of another file
		srcDirect(nid, ino, 2, 2, true),   // another ofs
		srcDirect(nid, ino, 1, 3, true),   // first copy at ofs 1
		srcDirect(nid, ino, 1, 4, true),   // the last copy wins
	)
	c, err := f.chain()
	if err != nil {
		t.Fatal(err)
	}
	before := f.Info().Warnings
	s := &chainSource{f: f, c: c, limit: len(c.entries) - 1}
	for _, tc := range []struct {
		name       string
		nid, owner uint32
		ofs        uint32
		wantStop   string
		wantMark   uint32
		wantEntry  int
	}{
		{"last of two copies", nid, ino, 1, "", 4, 4},
		{"other ofs", nid, ino, 2, "", 2, 2},
		{"ofs nobody wrote", nid, ino, 4, "node-missing", 0, 0},
		{"owner of the other footer", nid, other, 1, "", 1, 1},
		{"owner nobody has", nid, 99, 1, "node-missing", 0, 0},
		{"nid nobody has", 777, ino, 1, "node-missing", 0, 0},
		{"inode node as a direct node", ino, ino, 0, "node-missing", 0, 0},
	} {
		b, addr, stop, err := s.nodeFor(tc.nid, tc.owner, tc.ofs)
		if err != nil || stop != tc.wantStop {
			t.Errorf("%s: stop %q err %v, want %q", tc.name, stop, err, tc.wantStop)
			continue
		}
		if stop == "" && (markOf(b) != tc.wantMark || addr != c.entries[tc.wantEntry].block) {
			t.Errorf("%s: mark %d addr %d, want mark %d at entry %d", tc.name, markOf(b), addr, tc.wantMark, tc.wantEntry)
		}
	}
	if after := f.Info().Warnings; !slices.Equal(before, after) {
		t.Errorf("serving nodes changed the filesystem warnings: %v -> %v", before, after)
	}
}

// A hostile chain holds the indirect node of i_nid[2] (footer ofs 3): the map of the file
// still stops there with node-missing, on the default image (indirect node omitted) and on one
// with a stale copy elsewhere (StaleIndirect). The chain source never serves it.
func TestChainSourceNeverServesTheFilesIndirectNode(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "StaleIndirect"}[stale], func(t *testing.T) {
			o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery}
			const blocks = 923 + 2*1018
			files := []f2fstest.File{{Path: "/ind", Data: make([]byte, (blocks+5)*blockSize), PostCheckpoint: true, StaleIndirect: stale}}
			probe, ptree := f2fstest.BuildTree(o, files)
			ino := ptree.NID["/ind"]
			ind := binary.LittleEndian.Uint32(blockAt(probe, ptree.ChainAddrs[0])[4052+4*2:])
			o.Chain = []f2fstest.ChainNode{rfDirectNode(ind, ino, 3, true)}
			img, _ := f2fstest.BuildTree(o, files)
			f := openImg(t, img)
			c, err := f.chain()
			if err != nil {
				t.Fatal(err)
			}
			if kinds := nodeKind(c.entries[0].flag >> offsetBitShift); c.entries[0].nid != ind || kinds != "indirect" {
				t.Fatalf("hostile node = %+v (%s), want the indirect node %d", c.entries[0], kinds, ind)
			}
			s := &chainSource{f: f, c: c, limit: c.lastFsync[ino]}
			if b, _, stop, err := s.nodeFor(ind, ino, 3); stop != "node-missing" || b != nil || err != nil {
				t.Fatalf("nodeFor(indirect) = %d bytes, stop %q, err %v", len(b), stop, err)
			}
			raw := blockAt(img, c.entries[1].block)
			in, _, err := f.parseChainInode(raw, ino)
			if err != nil {
				t.Fatal(err)
			}
			res, err := f.staleMap(in, s)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("node-missing@%d", ind); res.stop != want || res.covered != blocks*blockSize {
				t.Errorf("map stops %q covering %d, want %s covering %d", res.stop, res.covered, want, blocks*blockSize)
			}
		})
	}
}
