package f2fs_test

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// rfFuzzOpts is the volume FuzzRollForward edits.
func rfFuzzOpts() f2fstest.Options {
	return f2fstest.Options{
		Segments: 6, NodeSegs: 2, SSA: true, InodeChksum: true, ExtraAttr: true,
		Unclean: true, CPFlags: f2fstest.CPCRCRecovery,
	}
}

var (
	rfFuzzOnce                                       sync.Once
	rfFuzzVal                                        recFuzzBase
	rfFuzzSmall, rfFuzzSparse, rfFuzzBig, rfFuzzLive uint32 // chain block addresses
)

// fuzzRollForwardBase is the image the fuzzed edits are applied to: a live file with a chain
// update, an inode that nothing replays, and a post-checkpoint small, 1000-block and sparse file.
func fuzzRollForwardBase() *recFuzzBase {
	rfFuzzOnce.Do(func() {
		o := rfFuzzOpts()
		files := []f2fstest.File{
			{Path: "/rf", Dir: true},
			{Path: "/live.bin", Data: stamp(2*bs, "l")},
			{Path: "/rf/small.bin", Data: stamp(3000, "s"), PostCheckpoint: true},
			{Path: "/rf/big.bin", Data: stamp(1000*bs, "b"), PostCheckpoint: true},
			{Path: "/rf/sparse.bin", Data: stamp(5*bs, "p"), PostCheckpoint: true},
		}
		_, probe := f2fstest.BuildTree(o, files)
		live := probe.NID["/live.bin"]
		o.Chain = []f2fstest.ChainNode{
			{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: live, Mode: 0o100644, Links: 1, Size: 9000, PIno: f2fstest.RootIno, Name: "live.bin", Extra: true}), Fsync: true},
			{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 90, Mode: 0o100644, Links: 1, Size: 5, PIno: f2fstest.RootIno, Name: "ghost", Extra: true}), Fsync: true},
		}
		img, tr := f2fstest.BuildTree(o, files)
		// sparse.bin: a hole at logical block 2 (i_addr starts after the 36-byte extra area)
		sp := tr.ChainOf["/rf/sparse.bin"][0]
		le.PutUint32(blk(img, sp)[360+36+4*2:], 0)
		f2fstest.ResealInode(o, img, sp)
		b := recFuzzBase{img: img, tree: tr, layout: f2fstest.Geometry(o)}
		for n := 0; n*bs < len(img); n++ {
			if bytes.ContainsFunc(img[n*bs:min(len(img), (n+1)*bs)], func(r rune) bool { return r != 0 }) {
				b.blocks = append(b.blocks, n)
			}
		}
		rfFuzzVal = b
		rfFuzzSmall, rfFuzzSparse = tr.ChainOf["/rf/small.bin"][0], sp
		rfFuzzBig, rfFuzzLive = tr.ChainOf["/rf/big.bin"][0], tr.ChainAddrs[0]
	})
	return &rfFuzzVal
}

// rfFuzzApply applies the edits and reseals both checkpoint packs, so an edit of the cursor is
// judged on its own merits and not by the checkpoint checksum.
func rfFuzzApply(b *recFuzzBase, in []byte) []byte {
	img := b.apply(in)
	f2fstest.SealCheckpoint(img, 1)
	f2fstest.SealCheckpoint(img, 2)
	return img
}

// FuzzRollForward applies byte edits to a builder image with a live file updated in the chain, an
// inode nothing replays, and post-checkpoint small, 1000-block and sparse files. It never panics;
// RollForward gives a typed error or a chain of at most 1<<20 nodes that visits exactly Nodes
// nodes and only ever replays dnodes; PostCheckpointFiles gives typed errors or candidates that
// pass CheckCandidate and are equal on a second call; and the live view (Unallocated) is the same
// before and after.
//
// Gate run: go test ./internal/filesys/f2fs -run '^$' -fuzz '^FuzzRollForward$' -fuzztime 30s -fuzzminimizetime 0 -parallel 2
func FuzzRollForward(f *testing.F) {
	base := fuzzRollForwardBase()
	l := base.layout
	f.Add([]byte{})
	f.Add(base.edit(int(rfFuzzSmall), 4072+12, 0x7f))     // the footer version of the first file's inode
	f.Add(base.edit(int(rfFuzzBig), 4072+20, 0x09))       // next_blkaddr of a chain inode
	f.Add(base.edit(int(l.CP), 40, 0x01))                 // the warm node segment of the cursor
	f.Add(base.edit(int(l.CP), 70, 0x05))                 // the warm node block offset of the cursor
	f.Add(base.edit(int(rfFuzzSmall), 16, 0xff))          // an inode size
	f.Add(base.edit(int(rfFuzzSparse), 360+36+4*3, 0x55)) // an i_addr slot
	f.Add(base.edit(int(rfFuzzLive), 4072+8, 0x00))       // a footer flag word
	f.Fuzz(func(t *testing.T, in []byte) {
		img := rfFuzzApply(base, in)
		fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return
		}
		size := fsys.Info().Size
		before, errBefore := fsys.Unallocated()

		visited := 0
		var replayedNonDnode bool
		chain, err := fsys.RollForward(func(n filesys.RollForwardNode) bool {
			visited++
			if n.WouldReplay && n.Kind != "inode" && n.Kind != "direct" && n.Kind != "xattr" {
				replayedNonDnode = true
			}
			return true
		})
		if err != nil {
			if !rfTypedError(err) {
				t.Fatalf("RollForward: untyped error %v", err)
			}
		} else if chain.Nodes > f2fs.MaxChainNodes || visited != chain.Nodes || replayedNonDnode {
			t.Fatalf("chain nodes %d, visited %d, replayed a non-dnode %v", chain.Nodes, visited, replayedNonDnode)
		}

		files, err := fsys.PostCheckpointFiles()
		if err != nil {
			if files != nil || !rfTypedError(err) {
				t.Fatalf("PostCheckpointFiles: %v with %d files", err, len(files))
			}
		}
		for _, pf := range files {
			if pf.Err != nil && !rfTypedError(pf.Err) {
				t.Fatalf("%s: untyped error %v", pf.ID, pf.Err)
			}
			if pf.Err != nil && len(pf.Candidates) != 0 {
				t.Fatalf("%s: candidates and an error", pf.ID)
			}
			for _, c := range pf.Candidates {
				covered, cerr := filesys.CheckCandidate(c, size)
				if cerr != nil {
					t.Fatalf("%s: CheckCandidate: %v", pf.ID, cerr)
				}
				if covered > c.Size {
					t.Fatalf("%s: covers %d bytes of a %d-byte file", pf.ID, covered, c.Size)
				}
			}
		}
		again, err2 := fsys.PostCheckpointFiles()
		if (err == nil) != (err2 == nil) || err != nil && err.Error() != err2.Error() || !rfSameFiles(files, again) {
			t.Fatalf("the second PostCheckpointFiles call differs: %v / %v", err, err2)
		}

		after, errAfter := fsys.Unallocated()
		if (errBefore == nil) != (errAfter == nil) || errBefore != nil && errBefore.Error() != errAfter.Error() || !reflect.DeepEqual(before, after) {
			t.Fatalf("Unallocated changed after the roll-forward calls: %v %v / %v %v", before, errBefore, after, errAfter)
		}
	})
}

// rfSameFiles compares two PostCheckpointFiles results, errors by text.
func rfSameFiles(a, b []filesys.PostCheckpointFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if (x.Err == nil) != (y.Err == nil) || x.Err != nil && x.Err.Error() != y.Err.Error() {
			return false
		}
		x.Err, y.Err = nil, nil
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// rfTypedError reports whether err is one of the answers the roll-forward calls may give on
// hostile bytes from a bytes.Reader (which never fails an I/O call).
func rfTypedError(err error) bool {
	return errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrUnsupported)
}

// The unedited base has the files the fuzz edits: if it stopped listing them the fuzz would only
// exercise refusals.
func TestFuzzRollForwardBaseListsItsFiles(t *testing.T) {
	img := rfFuzzApply(fuzzRollForwardBase(), nil)
	fsys := pcOpen(t, img)
	files := pcList(t, fsys)
	want := map[string]int64{"small.bin": 3000, "big.bin": 1000 * bs, "sparse.bin": 5 * bs}
	if len(files) != len(want) {
		t.Fatalf("files = %+v, want %d", files, len(want))
	}
	for _, pf := range files {
		if pf.Err != nil || len(pf.Candidates) != 1 || pf.Size != want[pf.Name] {
			t.Errorf("%s: %+v", pf.Name, pf)
		}
	}
	chain, err := fsys.RollForward(func(filesys.RollForwardNode) bool { return true })
	if err != nil || chain.Nodes != 2+1+2+1 || !chain.Complete && chain.Stop == "" {
		t.Errorf("chain = %+v, %v", chain, err)
	}
	var sparse filesys.Candidate
	for _, pf := range files {
		if pf.Name == "sparse.bin" {
			sparse = pf.Candidates[0]
		}
	}
	if got, _ := hasAssumption(sparse, "stale-chain-stop="); got != "stale-chain-stop=hole@2" {
		t.Errorf("sparse assumptions = %v, want the hole stop", sparse.Assumptions)
	}
}
