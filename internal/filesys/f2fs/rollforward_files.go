package f2fs

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Post-checkpoint files (plan 3H Task 6): the files the kernel's replay would create, with
// their block maps read from the chain through the 3D stale-map walker. No bytes are read,
// nothing is replayed and nothing reaches the live view (3H-R5, R6, R7).

// maxParseWarnings bounds the inode-parse-warning assumptions of one candidate; the rest are
// counted in one more assumption, never dropped silently.
const maxParseWarnings = 8

// rfIndex is built once per chain from its entries (at most maxChainNodes of them).
type rfIndex struct {
	order  []uint32           // inode numbers in order of first appearance
	inodes map[uint32][]int32 // ino -> entry indexes of its inode nodes (nid == ino, offset 0), ascending
	byNode []int32            // direct-node entry indexes sorted by (nid, ino, footer offset, index)
}

// index builds the chain index on first use.
func (c *rfChain) index() *rfIndex {
	c.ixOnce.Do(func() {
		ix := &rfIndex{inodes: map[uint32][]int32{}}
		seen := map[uint32]struct{}{}
		for i, e := range c.entries {
			if _, ok := seen[e.ino]; !ok {
				seen[e.ino] = struct{}{}
				ix.order = append(ix.order, e.ino)
			}
			if ofs := e.flag >> offsetBitShift; ofs == 0 && e.nid == e.ino {
				ix.inodes[e.ino] = append(ix.inodes[e.ino], int32(i))
			} else if nodeKind(ofs) == "direct" {
				ix.byNode = append(ix.byNode, int32(i))
			}
		}
		slices.SortFunc(ix.byNode, func(a, b int32) int {
			eb := c.entries[b]
			return c.cmpKey(a, eb.nid, eb.ino, eb.flag>>offsetBitShift, b)
		})
		c.ix = ix
	})
	return c.ix
}

// cmpKey orders entry a against the key (nid, ino, footer offset, index); idx < 0 leaves the
// index out of the comparison.
func (c *rfChain) cmpKey(a int32, nid, ino, ofs uint32, idx int32) int {
	e := c.entries[a]
	switch {
	case e.nid != nid:
		return cmpU32(e.nid, nid)
	case e.ino != ino:
		return cmpU32(e.ino, ino)
	case e.flag>>offsetBitShift != ofs:
		return cmpU32(e.flag>>offsetBitShift, ofs)
	case idx >= 0:
		return int(a) - int(idx)
	}
	return 0
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// chainSource serves direct nodes of owner from the chain, the last copy at an index <= limit (3H-R6).
// Only a direct node is ever served: an indirect or double-indirect node is never in the warm log
// (fsync writes only dnodes; the probe confirmed it, H7), so a request for one is node-missing and
// the map ends there as a prefix.
type chainSource struct {
	f     *FS
	c     *rfChain
	limit int
}

// nodeFor implements nodeSource. The match is by the chain's footers alone: nid, owning inode and
// footer offset must all equal the request, and the copy must lie at or before limit.
func (s *chainSource) nodeFor(nid, owner, ofs uint32) ([]byte, uint32, string, error) {
	if nodeKind(ofs) != "direct" || s.limit < 0 {
		return nil, 0, "node-missing", nil
	}
	by := s.c.index().byNode
	// The first position whose key is greater than (nid, owner, ofs, limit); the one before it is the
	// last copy at an index <= limit when its (nid, owner, ofs) match.
	limit := int32(min(s.limit, len(s.c.entries)))
	pos, _ := slices.BinarySearchFunc(by, struct{}{}, func(a int32, _ struct{}) int {
		if s.c.cmpKey(a, nid, owner, ofs, limit) <= 0 {
			return -1
		}
		return 1
	})
	if pos == 0 {
		return nil, 0, "node-missing", nil
	}
	i := by[pos-1]
	if s.c.cmpKey(i, nid, owner, ofs, -1) != 0 {
		return nil, 0, "node-missing", nil
	}
	e := s.c.entries[i]
	b, err := s.f.rfBlock(e.block)
	if err != nil {
		return nil, 0, "", err
	}
	return b, e.block, "", nil
}

// pcMeta is what is known of a post-checkpoint file whether or not a candidate came of it.
type pcMeta struct {
	name string
	pino uint32
	typ  filesys.EntryType
	size int64
}

// pcParent caches the live-parent check of one PostCheckpointFiles call.
type pcParent struct {
	dir *inode
	err error
}

// PostCheckpointFiles lists the files the chain would create (3H-R5), each with its map or the
// typed reason it has none. A damaged chain is the error. It builds fresh slices on every call.
func (f *FS) PostCheckpointFiles() ([]filesys.PostCheckpointFile, error) {
	c, err := f.chain()
	if err != nil {
		return nil, err
	}
	parents := map[uint32]pcParent{}
	var out []filesys.PostCheckpointFile
	for _, ino := range c.index().order {
		if c.fileKind(ino) != "post-checkpoint" {
			continue
		}
		pf := filesys.PostCheckpointFile{ID: "rollforward:" + strconv.FormatUint(uint64(ino), 10), Ino: ino}
		cand, m, err := f.postCheckpointCandidate(c, ino, parents)
		pf.Name, pf.Pino, pf.Type, pf.Size = m.name, m.pino, m.typ, m.size
		switch {
		case err == nil:
			pf.Candidates = []filesys.Candidate{cand}
		case errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrUnsupported):
			pf.Err = err
		default:
			return nil, err // an I/O error is not a property of the file
		}
		out = append(out, pf)
	}
	return out, nil
}

// postCheckpointCandidate builds the claim for inode ino of the chain (3H-R5, R6, R8). The inode is
// the last inode node at or before the file's last fsync node; the name and parent come from the
// last dentry-marked one. A copy whose checksum fails, or whose name cannot be read, yields an
// error and no value: nothing is invented and no older copy takes its place.
func (f *FS) postCheckpointCandidate(c *rfChain, ino uint32, parents map[uint32]pcParent) (filesys.Candidate, pcMeta, error) {
	const what = "f2fs post-checkpoint file"
	var m pcMeta
	last, ok := c.lastFsync[ino]
	cands := c.index().inodes[ino]
	cands = cands[:sortedPrefix32(cands, int32(last))]
	if !ok || len(cands) == 0 {
		return filesys.Candidate{}, m, corrupt(what, -1, "inode %d has no inode node at or before its last fsync node", ino)
	}
	mapIdx := int(cands[len(cands)-1])
	in, mapWarns, err := f.chainInodeAt(c, mapIdx, ino)
	if err != nil {
		return filesys.Candidate{}, m, err
	}
	dentIdx := -1
	for j := len(cands) - 1; j >= 0; j-- {
		if c.entries[cands[j]].flag&footDentBit != 0 {
			dentIdx = int(cands[j])
			break
		}
	}
	if dentIdx < 0 {
		return filesys.Candidate{}, m, corrupt(what, -1, "inode %d has no dentry-marked inode node", ino)
	}
	named, warns := in, mapWarns
	if dentIdx != mapIdx {
		var nw []string
		if named, nw, err = f.chainInodeAt(c, dentIdx, ino); err != nil {
			return filesys.Candidate{}, m, fmt.Errorf("the dentry-marked copy that names the file: %w", err)
		}
		warns = append(slices.Clone(mapWarns), nw...)
	}
	if named.nameBad {
		return filesys.Candidate{}, m, corrupt(what, int64(c.entries[dentIdx].block)*blockSize, "inode %d: i_namelen is out of range, the name is unknown", ino)
	}
	m.name, _ = displayName(named.name, named.encrypted)
	m.pino, m.size = named.pino, in.size

	switch in.typ() {
	case modeReg:
		m.typ = filesys.TypeFile
	case modeSymlink:
		m.typ = filesys.TypeSymlink
	default:
		return filesys.Candidate{}, m, fmt.Errorf("%w: inode %d has mode %#o, only regular files and symlinks are recovered", filesys.ErrUnsupported, ino, in.mode)
	}
	at := int64(c.entries[mapIdx].block) * blockSize
	switch {
	case in.inline&inlineData != 0:
		return filesys.Candidate{}, m, fmt.Errorf("%w: inline data: reported, not recovered (inode %d)", filesys.ErrUnsupported, ino)
	case in.compressed:
		return filesys.Candidate{}, m, fmt.Errorf("%w: compressed file, reported, not recovered (inode %d)", filesys.ErrUnsupported, ino)
	case in.size < 0 || in.size > treeBlocks(in)*blockSize:
		return filesys.Candidate{}, m, corrupt(what, at, "inode %d: size %d is beyond what a node tree can address", ino, in.size)
	case in.size > f.size:
		return filesys.Candidate{}, m, corrupt(what, at, "inode %d: size %d is larger than the %d-byte volume", ino, in.size, f.size)
	}
	dir, err := f.postCheckpointParent(named.pino, parents)
	if err != nil {
		return filesys.Candidate{}, m, err
	}

	res, err := f.staleMap(in, &chainSource{f: f, c: c, limit: last})
	if err != nil {
		return filesys.Candidate{}, m, err
	}
	crc := "off"
	if c.sum.CRCRecovery && !c.sum.NoCRCRecovery {
		crc = "on"
	}
	sum := "not-recorded"
	if in.csumChecked {
		sum = "ok"
	}
	basis := []string{
		"rollforward:" + strconv.FormatUint(uint64(ino), 10), fmt.Sprintf("inode=%d", ino),
		fmt.Sprintf("chain:inode-blk=%d", c.entries[mapIdx].block),
		fmt.Sprintf("chain:dentry-blk=%d", c.entries[dentIdx].block),
		fmt.Sprintf("chain:last-fsync-blk=%d", c.entries[last].block),
		fmt.Sprintf("footer-version=%#x", c.sum.FooterVersion), fmt.Sprintf("checkpoint=%d", c.sum.CheckpointVersion),
		"crc-recovery=" + crc, "inode-checksum=" + sum, "nat=unmapped",
		fmt.Sprintf("parent=%d", named.pino), fmt.Sprintf("chain-nodes=%d", len(res.nodes)),
	}
	assumptions := []string{"not-checkpointed", "replay-not-applied", "content-free-in-checkpoint-view"}
	if res.stop != "" {
		assumptions = append(assumptions, "stale-chain-stop="+res.stop)
	}
	if !c.sum.Complete {
		assumptions = append(assumptions, "rollforward-chain-cut="+c.sum.Stop)
	}
	assumptions = append(assumptions, parseWarningAssumptions(warns)...)
	return filesys.Candidate{
		Method:      MethodRollForward,
		Size:        in.size,
		Runs:        res.runs,
		Basis:       basis,
		Assumptions: assumptions,
		Times:       in.times,
		Mode:        uint32(in.mode),
		Encrypted:   in.encrypted || dir.encrypted,
	}, m, nil
}

// chainInodeAt parses the inode node at chain entry idx with the parser's warnings. A parse failure
// or a recorded checksum that does not match is a *CorruptError: no map is taken from such a copy
// (3H-R3).
func (f *FS) chainInodeAt(c *rfChain, idx int, ino uint32) (*inode, []string, error) {
	e := c.entries[idx]
	raw, err := f.rfBlock(e.block)
	if err != nil {
		return nil, nil, err
	}
	in, ws, err := f.parseChainInode(raw, ino)
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("f2fs post-checkpoint file: inode %d at block %d does not parse: %w", ino, e.block, err)
	case !goodInode(in):
		return nil, nil, corrupt("f2fs post-checkpoint file", int64(e.block)*blockSize, "inode %d at block %d: checksum mismatch, size, name and parent are unknown", ino, e.block)
	}
	return in, ws, nil
}

// postCheckpointParent proves that the parent named by the dentry-marked copy is a live directory
// (3H-R6): the replay links the file into it, so any other parent is reported, not recovered.
func (f *FS) postCheckpointParent(pino uint32, cache map[uint32]pcParent) (*inode, error) {
	if p, ok := cache[pino]; ok {
		return p.dir, p.err
	}
	dir, err := f.inodeByID("nid:" + strconv.FormatUint(uint64(pino), 10))
	switch {
	case err == nil && dir.typ() != modeDir:
		err = fmt.Errorf("%w: parent %d is not a live directory; reported, not recovered", filesys.ErrUnsupported, pino)
	case errors.Is(err, filesys.ErrNotFound):
		err = fmt.Errorf("%w: parent %d is not a live directory; reported, not recovered", filesys.ErrUnsupported, pino)
	case err != nil:
		err = fmt.Errorf("parent %d of the post-checkpoint file: %w", pino, err)
	}
	if err != nil {
		dir = nil
	}
	cache[pino] = pcParent{dir, err}
	return dir, err
}

// parseWarningAssumptions turns the parser's warnings about the inode copies the claim rests on
// into assumptions (the map depends on the slot layout). Each is cut to the candidate string limit
// with an ellipsis; beyond maxParseWarnings one more says how many were left out.
func parseWarningAssumptions(ws []string) []string {
	const prefix = "inode-parse-warning="
	var out []string
	seen := map[string]struct{}{}
	extra := 0
	for _, w := range ws {
		// The dedup is by exact text; which copy a warning came from is in the chain:inode-blk and
		// chain:dentry-blk basis entries.
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		if len(out) >= maxParseWarnings {
			extra++
			continue
		}
		out = append(out, prefix+clipString(strings.Join(strings.Fields(w), " "), 256-len(prefix)))
	}
	if extra > 0 {
		out = append(out, fmt.Sprintf("%s%d more not listed", prefix, extra))
	}
	return out
}

// clipString cuts s to at most n bytes, marking the cut with "...". Only a rune the cut itself split
// is dropped; invalid UTF-8 earlier in the string is the parser's text and stays.
func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-3]
	for i := len(s) - 1; i >= 0 && i > len(s)-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			if !utf8.FullRuneInString(s[i:]) {
				s = s[:i]
			}
			break
		}
	}
	return s + "..."
}

// sortedPrefix32 returns how many of the ascending indexes are <= limit.
func sortedPrefix32(idx []int32, limit int32) int {
	n, _ := slices.BinarySearchFunc(idx, limit, func(a, b int32) int {
		if a <= b {
			return -1
		}
		return 1
	})
	return n
}
