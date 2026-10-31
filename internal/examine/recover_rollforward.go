package examine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// Recovery of post-checkpoint files (plan 3H, 3H-R5/R7/R8/R11): the files an F2FS roll-forward chain
// creates after the last checkpoint. The reader lists them with their maps (filesys.RollForwarder);
// everything after that, from the free-space cut to the artifact, is the planner of the deleted-entry
// recovery unchanged. A post-checkpoint file is never an entry of the tree and never presented as live.

// ChainInfo describes the roll-forward chain a roll-forward run read.
type ChainInfo struct {
	Nodes    int
	Stop     string
	Complete bool
}

// pcPathPrefix is the directory shown for a file whose parent has no live path (3H-R7).
const pcPathPrefix = "(post-checkpoint)"

// rfRefPrefix is the reference a roll-forward run accepts: "id:rollforward:<ino>".
const rfRefPrefix = "id:" + rollForwardIDPrefix

const rollForwardIDPrefix = "rollforward:"

// capChainCut caps a candidate read from a chain that stopped with later nodes unknown (3H-R8): a node of
// the file may be among them.
const capChainCut = 20

// rfResolved is what a roll-forward run reads before it audits anything: the targets, how many more met
// the entry cap, and the chain summary.
type rfResolved struct {
	targets []target
	over    int
	chain   ChainInfo
}

// recoveryClass is the class of the artifact a method's candidate becomes.
func recoveryClass(method string) string {
	if strings.HasPrefix(method, evidence.RollForwardMethod) {
		return evidence.ClassPostCheckpointFile
	}
	return evidence.ClassDeletedFile
}

// capRollForward applies the chain-cut cap: a candidate that carries a rollforward-chain-cut= assumption is
// never above capChainCut.
func capRollForward(conf int, assumptions []string) int {
	for _, a := range assumptions {
		if strings.HasPrefix(a, "rollforward-chain-cut=") {
			return min(conf, capChainCut)
		}
	}
	return conf
}

// rollForwarderOf returns the RollForwarder of fsys, or ErrNoRollForward. It runs before any audit entry.
func (s *Session) rollForwarderOf(fsys filesys.FileSystem) (filesys.RollForwarder, error) {
	if sup, ok := fsys.(interface{ SupportsRollForward() bool }); ok {
		if !sup.SupportsRollForward() {
			return nil, ErrNoRollForward
		}
		if r, ok := fsys.(filesys.RollForwarder); ok {
			return r, nil
		}
	}
	if r, ok := filesys.As[filesys.RollForwarder](fsys); ok {
		return r, nil
	}
	return nil, ErrNoRollForward
}

// resolveRollForward reads the chain and selects the targets (pure reads: no audit entry, no write). An
// "id:rollforward:<ino>" reference that is not a post-checkpoint file of the chain is ErrNotFound.
func (s *Session) resolveRollForward(ctx context.Context, o RecoverOptions, fsys filesys.FileSystem, rf filesys.RollForwarder) (*rfResolved, error) {
	p := &planner{s: s, ctx: ctx, o: o, fsys: fsys, rf: rf}
	targets, err := p.enumerateRollForward()
	if err != nil {
		return nil, err
	}
	return &rfResolved{targets: targets, over: p.rfOver, chain: p.rfChain}, nil
}

// errStopWalk ends the directory-path search once every parent is found.
var errStopWalk = errors.New("stop walk")

// enumerateRollForward returns the post-checkpoint files to consider, in chain order, at most
// recoverEntryCap of them (p.rfOver counts the rest), with the FSPath of 3H-R7.
func (p *planner) enumerateRollForward() ([]target, error) {
	chain, err := p.rf.RollForward(func(filesys.RollForwardNode) bool { return false })
	if err != nil {
		return nil, err
	}
	p.rfChain = ChainInfo{Nodes: chain.Nodes, Stop: chain.Stop, Complete: chain.Complete}
	files, err := p.rf.PostCheckpointFiles()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, r := range p.o.Refs {
		wanted[strings.TrimPrefix(r, "id:")] = true
	}
	known := make(map[string]bool, len(files))
	for _, f := range files {
		known[f.ID] = true
	}
	for _, r := range p.o.Refs { // sorted by the caller's order: the first unknown reference is named
		if id := strings.TrimPrefix(r, "id:"); !known[id] {
			return nil, fmt.Errorf("%s: %w", r, filesys.ErrNotFound)
		}
	}
	var picked []filesys.PostCheckpointFile
	for _, f := range files {
		if p.o.All || wanted[f.ID] {
			if len(picked) >= recoverEntryCap {
				p.rfOver++
				continue
			}
			picked = append(picked, f)
		}
	}
	dirs, err := p.liveDirPaths(picked)
	if err != nil {
		return nil, err
	}
	targets := make([]target, 0, len(picked))
	for i := range picked {
		f := &picked[i]
		name := f.Name
		label := name
		if label == "" {
			label = f.ID
		}
		t := target{e: filesys.Entry{ID: f.ID, Name: name, Type: f.Type}, pcf: f}
		if name == "" {
			t.e.Name = "rollforward-" + strconv.FormatUint(uint64(f.Ino), 10) // a local label only
		}
		if dir, ok := dirs[f.Pino]; ok && f.Pino != 0 && f.Name != "" {
			t.path = strings.TrimSuffix(dir, "/") + "/" + f.Name
		} else {
			t.path = pcPathPrefix + "/" + label
			t.parentUnknown = true
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// liveDirPaths finds, with one bounded walk of the live tree, the path of every live directory that is the
// parent of one of files. A directory that is deleted, or that no listing reaches, is simply absent.
func (p *planner) liveDirPaths(files []filesys.PostCheckpointFile) (map[uint32]string, error) {
	want := map[string]uint32{} // entry id -> inode number
	for _, f := range files {
		if f.Pino != 0 {
			want["nid:"+strconv.FormatUint(uint64(f.Pino), 10)] = f.Pino
		}
	}
	out := map[uint32]string{}
	if len(want) == 0 {
		return out, nil
	}
	root := p.fsys.Root()
	if ino, ok := want[root.ID]; ok {
		out[ino] = "/"
	}
	err := filesys.Walk(p.fsys, root, "/", func(wp string, we filesys.Entry, werr error) error {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if werr != nil {
			return nil // an unreadable directory leaves its children's paths unknown
		}
		if filesys.IsSnapshotsDir(we) {
			return filesys.SkipDir
		}
		if ino, ok := want[we.ID]; ok && !we.Deleted && we.Type == filesys.TypeDir {
			if _, dup := out[ino]; !dup {
				out[ino] = wp
			}
			if len(out) == len(want) {
				return errStopWalk
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, err
	}
	return out, nil
}

// candidatesFor is the Recoverer's answer for a deleted entry, or the precomputed post-checkpoint file.
func (p *planner) candidatesFor(t target) ([]filesys.Candidate, error) {
	if t.pcf == nil {
		return p.rec.Recoverable(t.e)
	}
	if t.pcf.Err != nil {
		return nil, t.pcf.Err
	}
	cs := make([]filesys.Candidate, len(t.pcf.Candidates))
	copy(cs, t.pcf.Candidates)
	if t.parentUnknown {
		for i := range cs {
			cs[i].Assumptions = append(slices.Clone(cs[i].Assumptions), "parent-path-unknown")
		}
	}
	return cs, nil
}

// planSource picks where a plan's candidates come from: the Recoverer for deleted entries, or, for a
// roll-forward run, the RollForwarder (resolved here, by pure reads, so that a refused run audits nothing).
func (s *Session) planSource(ctx context.Context, o RecoverOptions, fsys filesys.FileSystem) (filesys.Recoverer, *rfResolved, error) {
	if !o.RollForward {
		rec, err := s.recovererOf(fsys)
		return rec, nil, err
	}
	rf, err := s.rollForwarderOf(fsys)
	if err != nil {
		return nil, nil, err
	}
	rfr, err := s.resolveRollForward(ctx, o, fsys, rf)
	return nil, rfr, err
}
