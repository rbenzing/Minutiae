package f2fs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Post-checkpoint files and their maps (3H Task 6), on builder images.

func pcOpts() f2fstest.Options {
	return f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery, ExtraAttr: true, InodeChksum: true}
}

func pcOpen(t *testing.T, img []byte) *f2fs.FS {
	t.Helper()
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pcList(t *testing.T, f *f2fs.FS) []filesys.PostCheckpointFile {
	t.Helper()
	files, err := f.PostCheckpointFiles()
	if err != nil {
		t.Fatalf("PostCheckpointFiles: %v", err)
	}
	return files
}

// pcOne is the only file listed, with its only candidate.
func pcOne(t *testing.T, f *f2fs.FS) (filesys.PostCheckpointFile, filesys.Candidate) {
	t.Helper()
	files := pcList(t, f)
	if len(files) != 1 || files[0].Err != nil || len(files[0].Candidates) != 1 {
		t.Fatalf("files = %+v, want one file with one candidate", files)
	}
	return files[0], files[0].Candidates[0]
}

func hasAssumption(c filesys.Candidate, prefix string) (string, bool) {
	for _, a := range c.Assumptions {
		if strings.HasPrefix(a, prefix) {
			return a, true
		}
	}
	return "", false
}

func hasBasis(c filesys.Candidate, prefix string) (string, bool) {
	for _, a := range c.Basis {
		if strings.HasPrefix(a, prefix) {
			return a, true
		}
	}
	return "", false
}

func covered(c filesys.Candidate) int64 {
	var n int64
	for _, r := range c.Runs {
		n += r.Length
	}
	return n
}

func TestPostCheckpointSmallFile(t *testing.T) {
	img, tree := f2fstest.BuildTree(pcOpts(), []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/rf/a.bin", Data: rfStamp(3), PostCheckpoint: true},
	})
	f := pcOpen(t, img)
	pf, c := pcOne(t, f)
	ino := tree.NID["/rf/a.bin"]
	if pf.ID != fmt.Sprintf("rollforward:%d", ino) || pf.Ino != ino || pf.Name != "a.bin" || pf.Pino != tree.NID["/rf"] ||
		pf.Type != filesys.TypeFile || pf.Size != 3*bs {
		t.Errorf("file = %+v", pf)
	}
	if c.Method != "f2fs-rollforward" || c.Size != 3*bs || c.Mode&0o170000 != 0o100000 || c.Encrypted {
		t.Errorf("candidate = %+v", c)
	}
	if got := readRuns(img, c.Runs); !bytes.Equal(got, rfStamp(3)) {
		t.Errorf("runs read %d bytes that differ from the data", len(got))
	}
	if cov, err := filesys.CheckCandidate(c, int64(len(img))); err != nil || cov != 3*bs {
		t.Errorf("CheckCandidate = %d, %v", cov, err)
	}
	chain := tree.ChainOf["/rf/a.bin"]
	wantBasis := []string{
		pf.ID, fmt.Sprintf("inode=%d", ino), fmt.Sprintf("chain:inode-blk=%d", chain[0]), fmt.Sprintf("chain:dentry-blk=%d", chain[0]),
		fmt.Sprintf("chain:last-fsync-blk=%d", chain[0]), "footer-version=0x", "checkpoint=", "crc-recovery=on", "inode-checksum=ok",
		"nat=unmapped", fmt.Sprintf("parent=%d", tree.NID["/rf"]), "chain-nodes=0",
	}
	if len(c.Basis) != len(wantBasis) {
		t.Fatalf("basis = %v, want %d entries", c.Basis, len(wantBasis))
	}
	for i, w := range wantBasis {
		if !strings.HasPrefix(c.Basis[i], w) {
			t.Errorf("basis[%d] = %q, want prefix %q", i, c.Basis[i], w)
		}
	}
	if want := []string{"not-checkpointed", "replay-not-applied", "content-free-in-checkpoint-view"}; !slices.Equal(c.Assumptions, want) {
		t.Errorf("assumptions = %v, want %v", c.Assumptions, want)
	}
}

func TestPostCheckpointDirectNodeFile(t *testing.T) {
	img, tree := f2fstest.BuildTree(pcOpts(), []f2fstest.File{{Path: "/big.bin", Data: rfStamp(1000), PostCheckpoint: true}})
	_, c := pcOne(t, pcOpen(t, img))
	if c.Size != 1000*bs || covered(c) != 1000*bs || len(c.Assumptions) != 3 {
		t.Fatalf("candidate = size %d covered %d assumptions %v", c.Size, covered(c), c.Assumptions)
	}
	want := make([]filesys.Run, 0, 1000)
	for _, a := range tree.Data["/big.bin"] {
		want = append(want, filesys.Run{Offset: int64(a) * bs, Length: bs})
	}
	if !bytes.Equal(readRuns(img, c.Runs), readRuns(img, want)) {
		t.Error("runs do not read the file data")
	}
	if b, _ := hasBasis(c, "chain-nodes="); b != "chain-nodes=1" {
		t.Errorf("chain-nodes basis = %q, want 1 direct node used", b)
	}
}

// A file that needs an indirect node is cut there: fsync writes only dnodes, so the indirect
// node is never in the chain (3H-R6), with or without a stale copy elsewhere in the image.
func TestPostCheckpointIndirectFileStopsAtNodeMissing(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("StaleIndirect=%v", stale), func(t *testing.T) {
			o := pcOpts()
			o.ExtraAttr, o.InodeChksum = false, false
			const blocks = 923 + 2*1018
			data := rfStamp(blocks + 5)
			img, tree := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/ind", Data: data, PostCheckpoint: true, StaleIndirect: stale}})
			f := pcOpen(t, img)
			_, c := pcOne(t, f)
			ind := binary.LittleEndian.Uint32(blk(img, tree.ChainAddrs[0])[4052+4*2:])
			if c.Size != int64(len(data)) || covered(c) != blocks*bs || !bytes.Equal(readRuns(img, c.Runs), data[:blocks*bs]) {
				t.Errorf("candidate size %d covers %d bytes, want %d of %d", c.Size, covered(c), blocks*bs, len(data))
			}
			if got, ok := hasAssumption(c, "stale-chain-stop="); !ok || got != fmt.Sprintf("stale-chain-stop=node-missing@%d", ind) {
				t.Errorf("assumptions = %v, want stale-chain-stop=node-missing@%d", c.Assumptions, ind)
			}
			_, nodes := rfWalk(t, f)
			for _, n := range nodes {
				if n.Kind == "indirect" || n.Kind == "double-indirect" {
					t.Errorf("the chain holds an %s node: %+v", n.Kind, n)
				}
			}
		})
	}
}

// pcInode is a regular-file inode node of a hand-made chain; a Mode set by the caller is kept.
func pcInode(o f2fstest.Options, in f2fstest.Inode, fsync, dent bool) f2fstest.ChainNode {
	if in.Mode == 0 {
		in.Mode = 0o100644
	}
	in.Links, in.Extra = 1, true
	if in.PIno == 0 {
		in.PIno = f2fstest.RootIno
	}
	return f2fstest.ChainNode{Block: f2fstest.InodeBlock(o, in), Fsync: fsync, Dentry: dent}
}

func pcChain(t *testing.T, nodes ...f2fstest.ChainNode) (*f2fs.FS, []byte, *f2fstest.Tree) {
	t.Helper()
	o := pcOpts()
	o.Chain = nodes
	img, tree := f2fstest.BuildTree(o, pcRoot)
	return pcOpen(t, img), img, tree
}

// mainAt is a block address inside the main area, clear of the builder's own blocks.
func mainAt(n uint32) uint32 { return f2fstest.Geometry(pcOpts()).Main + n }

func TestPostCheckpointHoleStops(t *testing.T) {
	a := mainAt(40)
	f, _, _ := pcChain(t, pcInode(pcOpts(), f2fstest.Inode{NID: 91, Size: 3 * bs, Name: "h", Addrs: []uint32{a, a + 1, 0}}, true, true))
	_, c := pcOne(t, f)
	if covered(c) != 2*bs || c.Size != 3*bs {
		t.Errorf("covered %d of %d, want 2 blocks of 3", covered(c), c.Size)
	}
	if got, ok := hasAssumption(c, "stale-chain-stop="); !ok || got != "stale-chain-stop=hole@2" {
		t.Errorf("assumptions = %v, want stale-chain-stop=hole@2", c.Assumptions)
	}
}

func TestPostCheckpointTwoFsyncsUsesLastInode(t *testing.T) {
	o := pcOpts()
	a, b := mainAt(40), mainAt(80)
	f, _, tree := pcChain(t,
		pcInode(o, f2fstest.Inode{NID: 91, Size: 2 * bs, Name: "first", Addrs: []uint32{a, a + 1}}, true, true),
		pcInode(o, f2fstest.Inode{NID: 91, Size: 4 * bs, Name: "second", Addrs: []uint32{b, b + 1, b + 2, b + 3}}, true, false))
	pf, c := pcOne(t, f)
	if pf.Size != 4*bs || c.Size != 4*bs || !slices.Equal(c.Runs, []filesys.Run{{Offset: int64(b) * bs, Length: 4 * bs}}) {
		t.Errorf("size %d runs %v, want the second inode's 4-block map at %d", c.Size, c.Runs, b)
	}
	// The name is the last dentry-marked copy's (what the kernel links), not the last copy's.
	if pf.Name != "first" {
		t.Errorf("name = %q, want first", pf.Name)
	}
	ca := tree.ChainAddrs
	if x, _ := hasBasis(c, "chain:inode-blk="); x != fmt.Sprintf("chain:inode-blk=%d", ca[1]) {
		t.Errorf("inode basis = %q, want block %d", x, ca[1])
	}
	if x, _ := hasBasis(c, "chain:dentry-blk="); x != fmt.Sprintf("chain:dentry-blk=%d", ca[0]) {
		t.Errorf("dentry basis = %q, want block %d", x, ca[0])
	}
}

func TestPostCheckpointFilesOnlyCreatedInodes(t *testing.T) {
	fx := newRFFix(t)
	files := pcList(t, fx.open(t))
	aIno, bIno := fx.tree.NID["/rf/a.bin"], fx.tree.NID["/rf/b.bin"]
	if len(files) != 2 || files[0].Ino != aIno || files[1].Ino != bIno {
		t.Fatalf("files = %+v, want a.bin then b.bin only", files)
	}
	for _, pf := range files {
		if pf.Err != nil || len(pf.Candidates) != 1 {
			t.Errorf("%s: err %v, %d candidates", pf.Name, pf.Err, len(pf.Candidates))
		}
		if pf.Ino == fx.live || pf.Ino == fx.ghost {
			t.Errorf("listed %d: a live update or an unreplayed inode", pf.Ino)
		}
	}
}

func TestPostCheckpointRefusals(t *testing.T) {
	o := pcOpts()
	a := mainAt(40)
	ok := func(in f2fstest.Inode) f2fstest.ChainNode {
		in.NID, in.Name = 91, "r"
		if in.Size == 0 && in.InlineData == nil {
			in.Size, in.Addrs = bs, []uint32{a}
		}
		return pcInode(o, in, true, true)
	}
	tests := []struct {
		name    string
		node    f2fstest.ChainNode
		corrupt bool // flip a byte under the inode checksum
		want    error
		text    string
	}{
		{name: "inline data", node: ok(f2fstest.Inode{InlineData: []byte("hi"), Size: 2}), want: filesys.ErrUnsupported},
		{name: "compressed", node: ok(f2fstest.Inode{Flags: 0x4}), want: filesys.ErrUnsupported},
		{name: "parent is not a live directory", node: ok(f2fstest.Inode{PIno: 4000}), want: filesys.ErrUnsupported, text: "4000"},
		{name: "directory mode", node: ok(f2fstest.Inode{Mode: 0o040755}), want: filesys.ErrUnsupported},
		{name: "special file mode", node: ok(f2fstest.Inode{Mode: 0o020644}), want: filesys.ErrUnsupported},
		{name: "bad checksum on the inode the map needs", node: ok(f2fstest.Inode{}), corrupt: true, want: filesys.ErrCorrupt},
		{name: "size beyond the volume", node: ok(f2fstest.Inode{Size: 1 << 40, Addrs: []uint32{a}}), want: filesys.ErrCorrupt},
		{name: "size beyond any node tree", node: ok(f2fstest.Inode{Size: 1 << 60, Addrs: []uint32{a}}), want: filesys.ErrCorrupt},
		{name: "i_namelen 300", node: ok(f2fstest.Inode{RawOverrides: map[int][]byte{88: {0x2c, 0x01, 0, 0}}}), want: filesys.ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, img, tree := pcChain(t, tc.node)
			if tc.corrupt {
				f = rfCorrupt(t, img, tree.ChainAddrs[0])
			}
			files := pcList(t, f)
			if len(files) != 1 {
				t.Fatalf("files = %+v, want one entry carrying the refusal", files)
			}
			pf := files[0]
			if !errors.Is(pf.Err, tc.want) || len(pf.Candidates) != 0 || pf.ID != "rollforward:91" || pf.Ino != 91 {
				t.Errorf("file = %+v, want Err %v, no candidate", pf, tc.want)
			}
			if tc.text != "" && (pf.Err == nil || !strings.Contains(pf.Err.Error(), tc.text)) {
				t.Errorf("error %v does not mention %q", pf.Err, tc.text)
			}
		})
	}

	t.Run("size 0 is a candidate with no runs", func(t *testing.T) {
		f, _, _ := pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Name: "z"}, true, true))
		pf, c := pcOne(t, f)
		if pf.Size != 0 || c.Size != 0 || len(c.Runs) != 0 {
			t.Errorf("file %+v candidate %+v, want size 0 and no runs", pf, c)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f, _, _ := pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Mode: 0o120777, Name: "l", Size: 5, Addrs: []uint32{a}}, true, true))
		pf, c := pcOne(t, f)
		if pf.Type != filesys.TypeSymlink || c.Mode&0o170000 != 0o120000 {
			t.Errorf("type %v mode %o, want a symlink", pf.Type, c.Mode)
		}
	})
}

// A file with no good inode copy or an unreadable name shows no invented value: no name,
// size or parent (Task 5 carry-over).
func TestPostCheckpointRefusalInventsNothing(t *testing.T) {
	o := pcOpts()
	_, img, tree := pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Size: 7 * bs, Name: "secret", Addrs: []uint32{mainAt(40)}}, true, true))
	f := rfCorrupt(t, img, tree.ChainAddrs[0])
	pf := pcFirst(t, f)
	if pf.Name != "" || pf.Size != 0 || pf.Pino != 0 || len(pf.Candidates) != 0 || !errors.Is(pf.Err, filesys.ErrCorrupt) {
		t.Errorf("bad-checksum file = %+v, want only ID and Ino and a corrupt error", pf)
	}
	f, _, _ = pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Size: bs, Name: "n", Addrs: []uint32{mainAt(40)}, RawOverrides: map[int][]byte{88: {0x2c, 0x01, 0, 0}}}, true, true))
	pf = pcFirst(t, f)
	if pf.Name != "" || len(pf.Candidates) != 0 || !errors.Is(pf.Err, filesys.ErrCorrupt) || !strings.Contains(pf.Err.Error(), "name") {
		t.Errorf("unreadable-name file = %+v, want no name, no candidate and a corrupt error naming the name", pf)
	}
}

// A later inode copy that fails its checksum is not replaced by an older one: the map needs
// the last copy at or before the last fsync node (3H-R5).
func TestPostCheckpointBadLastCopyIsNotSkipped(t *testing.T) {
	o := pcOpts()
	a := mainAt(40)
	_, img, tree := pcChain(t,
		pcInode(o, f2fstest.Inode{NID: 91, Size: bs, Name: "old", Addrs: []uint32{a}}, true, true),
		pcInode(o, f2fstest.Inode{NID: 91, Size: 2 * bs, Name: "old", Addrs: []uint32{a, a + 1}}, true, false))
	f := rfCorrupt(t, img, tree.ChainAddrs[1])
	pf := pcFirst(t, f)
	if !errors.Is(pf.Err, filesys.ErrCorrupt) || len(pf.Candidates) != 0 {
		t.Errorf("file = %+v, want a corrupt refusal, not the older copy's map", pf)
	}
}

// The parser's warnings about the inode copy the map comes from are assumptions of the
// candidate, because the map depends on the slot layout (Task 5 carry-over).
func TestPostCheckpointCarriesInodeParseWarnings(t *testing.T) {
	o := pcOpts()
	a := mainAt(40)
	f, _, _ := pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Size: bs, Name: "w", Addrs: []uint32{a}, ExtraIsize: 40}, true, true))
	_, c := pcOne(t, f)
	got, found := hasAssumption(c, "inode-parse-warning=")
	if !found || !strings.Contains(got, "i_extra_isize") {
		t.Errorf("assumptions = %v, want an inode-parse-warning= naming i_extra_isize", c.Assumptions)
	}
	if _, err := filesys.CheckCandidate(c, 1<<30); err != nil {
		t.Errorf("CheckCandidate: %v", err)
	}
	// A clean inode carries none.
	f, _, _ = pcChain(t, pcInode(o, f2fstest.Inode{NID: 91, Size: bs, Name: "w", Addrs: []uint32{a}}, true, true))
	if _, c = pcOne(t, f); len(c.Assumptions) != 3 {
		t.Errorf("clean inode assumptions = %v, want only the three fixed ones", c.Assumptions)
	}
}

func TestPostCheckpointMissingExtraHeaderWarns(t *testing.T) {
	// A flexible-xattr volume whose chain inode has no extra attribute header.
	o := pcOpts()
	o.InlineXattr = true
	in := f2fstest.Inode{NID: 91, Mode: 0o100644, Links: 1, Size: bs, PIno: f2fstest.RootIno, Name: "x", Addrs: []uint32{mainAt(40)}}
	o.Chain = []f2fstest.ChainNode{{Block: f2fstest.InodeBlock(o, in), Fsync: true, Dentry: true}}
	img, _ := f2fstest.BuildTree(o, pcRoot)
	_, c := pcOne(t, pcOpen(t, img))
	if got, ok := hasAssumption(c, "inode-parse-warning="); !ok || !strings.Contains(got, "extra") {
		t.Errorf("assumptions = %v, want an inode-parse-warning= about the missing extra attribute header", c.Assumptions)
	}
}

func TestPostCheckpointChainCutAssumption(t *testing.T) {
	img, tree := f2fstest.BuildTree(pcOpts(), []f2fstest.File{
		{Path: "/a.bin", Data: rfStamp(2), PostCheckpoint: true},
		{Path: "/b.bin", Data: rfStamp(2), PostCheckpoint: true},
	})
	f := pcOpen(t, img)
	f.SetChainCap(1)
	pf, c := pcOne(t, f)
	if pf.Name != "a.bin" {
		t.Errorf("file = %+v, want a.bin only", pf)
	}
	want := fmt.Sprintf("rollforward-chain-cut=cap@%d", tree.ChainAddrs[1])
	if got, ok := hasAssumption(c, "rollforward-chain-cut="); !ok || got != want {
		t.Errorf("assumptions = %v, want %s", c.Assumptions, want)
	}
	// An uncut chain carries no such assumption.
	for _, pf := range pcList(t, pcOpen(t, img)) {
		if _, ok := hasAssumption(pf.Candidates[0], "rollforward-chain-cut="); ok {
			t.Errorf("%s: an uncut chain carries a cut assumption", pf.Name)
		}
	}
}

func TestPostCheckpointEncryptedIsFlagged(t *testing.T) {
	img, _ := f2fstest.BuildTree(pcOpts(), []f2fstest.File{
		{Path: "/p", Dir: true},
		{Path: "/p/own.bin", Data: rfStamp(1), PostCheckpoint: true, Encrypted: true},
		{Path: "/e", Dir: true, Encrypted: true},
		{Path: "/e/kid.bin", Data: rfStamp(1), PostCheckpoint: true},
		{Path: "/plain.bin", Data: rfStamp(1), PostCheckpoint: true},
	})
	files := pcList(t, pcOpen(t, img))
	if len(files) != 3 {
		t.Fatalf("files = %+v", files)
	}
	want := []bool{true, true, false} // own flag, encrypted parent, neither
	for i, pf := range files {
		if pf.Err != nil || len(pf.Candidates) != 1 || pf.Candidates[0].Encrypted != want[i] {
			t.Errorf("file %d (%q): err %v, candidates %+v, want Encrypted=%v", i, pf.Name, pf.Err, pf.Candidates, want[i])
		}
	}
}

func TestPostCheckpointIDIsNotATreeEntry(t *testing.T) {
	img, _ := f2fstest.BuildTree(pcOpts(), []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/rf/a.bin", Data: rfStamp(1), PostCheckpoint: true},
	})
	f := pcOpen(t, img)
	pf, _ := pcOne(t, f)
	if _, err := f.Open(filesys.Entry{ID: pf.ID, Name: "a.bin"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Open(%s) = %v, want ErrNotFound", pf.ID, err)
	}
	if _, err := f.Lookup("/rf/a.bin"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup = %v, want ErrNotFound", err)
	}
	dir, err := f.Lookup("/rf")
	if err != nil {
		t.Fatal(err)
	}
	es, err := f.ReadDir(dir)
	if err != nil || len(es) != 0 {
		t.Errorf("ReadDir(/rf) = %v, %v, want nothing", es, err)
	}
}

func TestPostCheckpointDeterministic(t *testing.T) {
	fx := newRFFix(t)
	f := fx.open(t)
	first := pcList(t, f)
	snapshot := fmt.Sprintf("%+v", first)
	for i := range first {
		for j := range first[i].Candidates {
			c := &first[i].Candidates[j]
			for k := range c.Runs {
				c.Runs[k].Offset = -7
			}
			for k := range c.Basis {
				c.Basis[k] = "mutated"
			}
			for k := range c.Assumptions {
				c.Assumptions[k] = "mutated"
			}
		}
	}
	second := pcList(t, f)
	if got := fmt.Sprintf("%+v", second); got != snapshot {
		t.Errorf("a second call differs after the first result was mutated:\n%s\n%s", got, snapshot)
	}
	if third := pcList(t, pcOpen(t, fx.img)); fmt.Sprintf("%+v", third) != snapshot {
		t.Error("a fresh FS lists different files")
	}
}

// pcFirst is the first file listed.
func pcFirst(t *testing.T, f *f2fs.FS) filesys.PostCheckpointFile {
	t.Helper()
	files := pcList(t, f)
	if len(files) == 0 {
		t.Fatal("PostCheckpointFiles lists nothing")
	}
	return files[0]
}

// pcRoot gives a chain-only image its root directory, the live parent of hand-made inodes.
var pcRoot = []f2fstest.File{{Path: "/", Dir: true}}
