package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// The reader against the kernel-written crash image and its independent oracle (3H Task 13, spec 16). The oracle
// (testdata/rollforward/f2fs-rollforward.expect.json) never came from Minutiae; every number compared here is read
// from it, none is restated in this file.

func openRF(t testing.TB, img []byte) *f2fs.FS {
	t.Helper()
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// rfByteRuns turns inclusive block ranges into merged filesystem byte runs, cut at size (size <= 0: no cut).
func rfByteRuns(ranges [][2]int64, size int64) []filesys.Run {
	var out []filesys.Run
	left := size
	for _, r := range ranges {
		n := (r[1] - r[0] + 1) * 4096
		if size > 0 {
			n = min(n, left)
		}
		if n <= 0 {
			break
		}
		left -= n
		if k := len(out); k > 0 && out[k-1].Offset+out[k-1].Length == r[0]*4096 {
			out[k-1].Length += n
			continue
		}
		out = append(out, filesys.Run{Offset: r[0] * 4096, Length: n})
	}
	return out
}

func mergeRuns(rs []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range rs {
		if k := len(out); k > 0 && out[k-1].Offset+out[k-1].Length == r.Offset {
			out[k-1].Length += r.Length
			continue
		}
		out = append(out, r)
	}
	return out
}

func trimNode(n filesys.RollForwardNode) [11]any {
	return [11]any{n.Block, n.NID, n.Ino, n.Ofs, n.Kind, n.Flag, n.Fsync, n.Dentry, n.Next, n.LiveIno, n.WouldReplay}
}

func TestRollForwardMatchesOracle(t *testing.T) {
	img, o := loadRFFixture(t, "f2fs-rollforward")
	f := openRF(t, img)
	var nodes []filesys.RollForwardNode
	chain, err := f.RollForward(func(n filesys.RollForwardNode) bool { nodes = append(nodes, n); return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Chain.Nodes) == 0 {
		t.Fatal("the oracle chain is empty")
	}
	if chain.Nodes != len(o.Chain.Nodes) || len(nodes) != len(o.Chain.Nodes) {
		t.Fatalf("chain %d nodes (%d visited), oracle %d", chain.Nodes, len(nodes), len(o.Chain.Nodes))
	}
	if chain.Stop != o.Chain.Stop || chain.Complete != o.Chain.Complete || chain.Start != uint64(o.Chain.Start) {
		t.Errorf("chain stop %q complete %v start %d; oracle %q %v %d", chain.Stop, chain.Complete, chain.Start, o.Chain.Stop, o.Chain.Complete, o.Chain.Start)
	}
	if chain.CheckpointVersion != o.CheckpointVersion || chain.CheckpointPack != o.CheckpointPack ||
		chain.CRCRecovery != o.CRCRecovery || chain.NoCRCRecovery != o.NoCRCRecovery {
		t.Errorf("chain checkpoint (version %d pack %d crc %v nocrc %v) differs from the oracle's (%d %d %v %v)",
			chain.CheckpointVersion, chain.CheckpointPack, chain.CRCRecovery, chain.NoCRCRecovery,
			o.CheckpointVersion, o.CheckpointPack, o.CRCRecovery, o.NoCRCRecovery)
	}
	if want := o.CheckpointFlags&rfxUmount != 0; chain.Unmount != want {
		t.Errorf("chain.Unmount %v, checkpoint flags %#x", chain.Unmount, o.CheckpointFlags)
	}
	if got := fmt.Sprintf("0x%016x", chain.FooterVersion); got != strings.ToLower(o.FooterVersion) {
		t.Errorf("footer version %s, oracle %s", got, o.FooterVersion)
	}
	for i, w := range o.Chain.Nodes {
		g := nodes[i]
		if g.Index != i || g.Block != uint64(w.Block) || g.NID != w.NID || g.Ino != w.Ino || g.Ofs != w.Ofs ||
			g.Kind != w.Kind || g.Flag != w.Flag || g.Fsync != w.Fsync || g.Dentry != w.Dentry || g.Next != uint64(w.Next) ||
			g.LiveIno != w.LiveIno || g.WouldReplay != w.WouldRepl {
			t.Errorf("node %d: got %v, oracle %+v", i, trimNode(g), w)
		}
	}

	byIno := map[uint32]filesys.RollForwardFile{}
	for _, cf := range chain.Files {
		byIno[cf.Ino] = cf
	}
	pcf, err := f.PostCheckpointFiles()
	if err != nil {
		t.Fatal(err)
	}
	pcByIno := map[uint32]filesys.PostCheckpointFile{}
	for _, p := range pcf {
		pcByIno[p.Ino] = p
	}
	wantPC := 0
	for _, of := range o.Files {
		if of.Kind == "absent" {
			// an absent file has no inode and no chain node under any inode number
			if of.Ino != 0 {
				t.Errorf("%s: an absent file with inode %d", of.Path, of.Ino)
			}
			continue
		}
		cf, ok := byIno[of.Ino]
		if !ok || cf.Kind != of.Kind {
			t.Errorf("%s (ino %d): chain file %+v, oracle kind %q", of.Path, of.Ino, cf, of.Kind)
			continue
		}
		if of.Kind != "post-checkpoint" {
			if _, ok := pcByIno[of.Ino]; ok {
				t.Errorf("%s: a %s file is listed as a post-checkpoint file", of.Path, of.Kind)
			}
			continue
		}
		wantPC++
		p, ok := pcByIno[of.Ino]
		if !ok {
			t.Errorf("%s: no PostCheckpointFile for inode %d", of.Path, of.Ino)
			continue
		}
		if p.Pino != of.PIno || p.Name != filepath.Base(of.Path) || p.Size != of.Size {
			t.Errorf("%s: pino %d name %q size %d; oracle %d %q %d", of.Path, p.Pino, p.Name, p.Size, of.PIno, filepath.Base(of.Path), of.Size)
		}
		x := of.Expect
		if x.Outcome == "skip" {
			if p.Err == nil || len(p.Candidates) != 0 {
				t.Errorf("%s: oracle skips (%s) but got %d candidates, err %v", of.Path, x.Skip, len(p.Candidates), p.Err)
				continue
			}
			want := map[string]error{"unsupported": filesys.ErrUnsupported, "corrupt": filesys.ErrCorrupt}[x.Skip]
			if want == nil || !errors.Is(p.Err, want) {
				t.Errorf("%s: skip %q, error %v", of.Path, x.Skip, p.Err)
			}
			continue
		}
		if p.Err != nil || len(p.Candidates) != 1 {
			t.Errorf("%s: %d candidates, err %v; oracle recovers it", of.Path, len(p.Candidates), p.Err)
			continue
		}
		c := p.Candidates[0]
		if want := rfByteRuns(x.Captured, of.Size); !slices.Equal(mergeRuns(c.Runs), want) {
			t.Errorf("%s: runs %v, oracle captured %v", of.Path, mergeRuns(c.Runs), want)
		}
		if c.Method != "f2fs-rollforward" {
			t.Errorf("%s: method %q", of.Path, c.Method)
		}
		hasStop := false
		for _, a := range c.Assumptions {
			if strings.HasPrefix(a, "stale-chain-stop=") {
				hasStop = true
				if want := "stale-chain-stop=" + x.Stop; a != want {
					t.Errorf("%s: assumption %q, want %q", of.Path, a, want)
				}
			}
		}
		if hasStop != (x.Stop != "") {
			t.Errorf("%s: stale-chain-stop present %v, oracle stop %q (assumptions %v)", of.Path, hasStop, x.Stop, c.Assumptions)
		}
		// the map up to the stop never names more than the oracle's whole map
		var capN, wholeN int64
		for _, r := range c.Runs {
			capN += r.Length
		}
		for _, r := range rfByteRuns(of.MapBlocks, 0) {
			wholeN += r.Length
		}
		if capN > wholeN {
			t.Errorf("%s: the candidate names %d bytes, the oracle's whole map %d", of.Path, capN, wholeN)
		}
	}
	if len(pcf) != wantPC {
		t.Errorf("PostCheckpointFiles lists %d, the oracle has %d post-checkpoint files", len(pcf), wantPC)
	}
	for _, cf := range chain.Files {
		if !slices.ContainsFunc(o.Files, func(of rfFile) bool { return of.Ino == cf.Ino }) {
			t.Errorf("chain file inode %d (%s) is not explained by the oracle", cf.Ino, cf.Kind)
		}
	}
}

// 3H-R26: every committed clean F2FS image has an empty, complete chain.
func TestRollForwardCleanFixturesHaveEmptyChains(t *testing.T) {
	paths := fixturePaths(t)
	more, err := filepath.Glob(filepath.Join("testdata", "*-casefold.img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, more...)
	for _, sub := range []string{"recover", "slack"} {
		m, err := filepath.Glob(filepath.Join("testdata", sub, "*.img.gz"))
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, m...)
	}
	if len(paths) < 6 {
		t.Fatalf("only %d clean fixtures found: %v", len(paths), paths)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			f := openRF(t, gunzipFixture(t, p))
			n := 0
			chain, err := f.RollForward(func(filesys.RollForwardNode) bool { n++; return true })
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 || chain.Nodes != 0 || !chain.Complete || len(chain.Files) != 0 {
				t.Errorf("chain of %d nodes (%d visited), complete %v, %d files, stop %q; want empty and complete", chain.Nodes, n, chain.Complete, len(chain.Files), chain.Stop)
			}
			pcf, err := f.PostCheckpointFiles()
			if err != nil || len(pcf) != 0 {
				t.Errorf("PostCheckpointFiles: %d files, %v", len(pcf), err)
			}
		})
	}
}

// The Task 7 assertions on the real crash image: nothing the roll-forward analysis does changes the live view,
// and no post-checkpoint file is a tree entry.
func TestRollForwardNeverAppliedToLiveViewOnTheCrashImage(t *testing.T) {
	img, o := loadRFFixture(t, "f2fs-rollforward")
	var extra []string
	for _, of := range o.Files {
		if of.Kind == "post-checkpoint" || of.Kind == "absent" {
			extra = append(extra, of.Path)
		}
	}
	if len(extra) == 0 {
		t.Fatal("the oracle names no post-checkpoint path")
	}
	a, b := openRF(t, img), openRF(t, img)
	before := liveSnapshot(t, a, extra...)
	useEverything(t, a)
	after := liveSnapshot(t, a, extra...)
	fresh := liveSnapshot(t, b, extra...)
	if d := liveDiff(before, after); len(d) > 0 {
		t.Errorf("the live view changed after the roll-forward calls: %v differ", d)
	}
	if d := liveDiff(after, fresh); len(d) > 0 {
		t.Errorf("the used FS differs from a fresh one: %v differ", d)
	}
	names := map[string]bool{}
	for _, p := range after.Paths {
		names[p] = true
	}
	for _, p := range extra {
		if names[p] {
			t.Errorf("the walk lists %s, a post-checkpoint (or unfsynced) name", p)
		}
		if _, err := a.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%s) = %v, want ErrNotFound", p, err)
		}
	}
	// a live-updated file keeps its checkpoint size, whatever the chain holds
	for _, of := range o.Files {
		if of.Kind != "live-update" {
			continue
		}
		if sz, ok := after.Sizes[of.Path]; !ok || sz == of.Size {
			t.Errorf("%s: live size %d (listed %v) equals the chain's size %d; the live view must show the checkpoint version", of.Path, sz, ok, of.Size)
		}
	}
	if len(after.Info.Warnings) != 0 {
		t.Errorf("Info().Warnings %v; the chain must add none", after.Info.Warnings)
	}
}
