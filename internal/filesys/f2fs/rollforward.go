package f2fs

import (
	"errors"
	"fmt"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Roll-forward (plan 3H): the nodes the kernel appends to the warm-node log after the
// last checkpoint when it fsyncs a file. They are not in the NAT and the live view never
// shows them; this file finds them (the chain) and applies the kernel's replay rules to
// say what a replay would do. It never replays and never changes what the reader serves.
//
// Every layout and replay fact is from memory of the kernel (fs/f2fs/recovery.c,
// include/linux/f2fs_fs.h) and was confirmed on kernel-written images by the 3H probe
// (tools/fixtures/README.md, "F2FS roll-forward probe (3H)").

var (
	_ filesys.RollForwarder = (*FS)(nil)
	_ filesys.Journaler     = (*FS)(nil)
)

const (
	// MethodRollForward is the recovery method of a post-checkpoint file.
	MethodRollForward = "f2fs-rollforward"

	// maxChainNodes bounds the chain (3H-R14): 20 bytes per node in the compact index.
	maxChainNodes = 1 << 20

	// Footer flag bits (from memory: COLD_BIT_SHIFT 0, FSYNC_BIT_SHIFT 1, DENT_BIT_SHIFT 2;
	// the probe saw flag words 0x9, 0x11, 0x19 (cold) and, in the chain, fsync 0x2 and dent
	// 0x4, H2). The offset in the node tree is flag >> offsetBitShift.
	footColdBit  = 0x1
	footFsyncBit = 0x2
	footDentBit  = 0x4

	// xattrNodeOfs is XATTR_NODE_OFFSET, from memory: the footer offset of an xattr node.
	xattrNodeOfs = 0xFFFFFFFF >> offsetBitShift
)

// chainEntry is the compact record of one chain node (3H-R14: 20 bytes).
type chainEntry struct{ block, nid, ino, flag, next uint32 }

// rfChain is the chain of one FS, with the replay facts of every inode number in it.
type rfChain struct {
	blocksRead int
	entries    []chainEntry
	sum        filesys.RollForwardChain // without Files (built by the callers from the maps below)
	live       map[uint32]bool          // ino -> the current NAT maps it
	created    map[uint32]bool          // ino -> not live and an fsync inode node carries the dentry mark
	lastFsync  map[uint32]int           // ino -> index of its last fsync-marked dnode
}

// chainState caches the chain of one FS: a corrupt or unsupported result is cached, an
// I/O error is not.
type chainState struct {
	mu     sync.Mutex
	done   bool
	c      *rfChain
	err    error
	builds int
}

// footerVersion is the cp_ver a chain node's footer must carry (3H-R2, from memory of
// is_recoverable_dnode): the full checkpoint version under CP_NOCRC_RECOVERY_FLAG; under
// CP_CRC_RECOVERY_FLAG the version with the checkpoint's stored checksum in the high half;
// the plain version otherwise. ok is false when the version's own high half is not zero:
// the comparison is then undecided, never a guess.
func (cp *checkpoint) footerVersion() (uint64, bool) {
	if cp.ver>>32 != 0 {
		return 0, false
	}
	switch {
	case cp.flags&cpNoCRCRecoveryFlag != 0:
		return cp.ver, true
	case cp.flags&cpCRCRecoveryFlag != 0:
		return cp.ver | uint64(cp.crc)<<32, true
	}
	return cp.ver, true
}

// nodeKind names the kind of the node at footer offset ofs (3H-R4, from memory of
// IS_DNODE and the node-path numbering): 0 is an inode; 3, 1022 and 2041 are the first
// indirect nodes and the double-indirect node; from 2042 every 1019th is an indirect
// child of the double-indirect node; XATTR_NODE_OFFSET is the xattr node; the rest are
// direct nodes.
func nodeKind(ofs uint32) string {
	const (
		ind1 = 3
		ind2 = ind1 + 1 + addrsPerBlock // 1022
		dind = ind2 + 1 + addrsPerBlock // 2041
		base = dind + 1                 // 2042: the first child of the double-indirect node
	)
	switch {
	case ofs == 0:
		return "inode"
	case ofs == xattrNodeOfs:
		return "xattr"
	case ofs == dind:
		return "double-indirect"
	case ofs == ind1 || ofs == ind2:
		return "indirect"
	case ofs >= base && (ofs-base)%(1+addrsPerBlock) == 0:
		return "indirect"
	}
	return "direct"
}

// isDnode reports whether a node of this kind is an inode, direct or xattr node.
func isDnode(kind string) bool { return kind == "inode" || kind == "direct" || kind == "xattr" }

// deriveChain computes the per-inode replay facts (3H-R4) of a list of chain entries,
// given which inode numbers the current NAT maps.
func deriveChain(entries []chainEntry, live map[uint32]bool) *rfChain {
	c := &rfChain{entries: entries, live: live, created: map[uint32]bool{}, lastFsync: map[uint32]int{}}
	if c.live == nil {
		c.live = map[uint32]bool{}
	}
	for i, e := range entries {
		ofs := e.flag >> offsetBitShift
		if e.flag&footFsyncBit != 0 && isDnode(nodeKind(ofs)) {
			c.lastFsync[e.ino] = i
		}
		if !c.live[e.ino] && e.nid == e.ino && ofs == 0 && e.flag&footFsyncBit != 0 && e.flag&footDentBit != 0 {
			c.created[e.ino] = true
		}
	}
	return c
}

// wouldReplay reports whether the kernel's replay would apply node i: it is a dnode of
// an inode the replay knows (live, or created by an fsync inode node with the dentry
// mark) and lies at or before that inode's last fsync dnode.
func (c *rfChain) wouldReplay(i int) bool {
	if i < 0 || i >= len(c.entries) {
		return false
	}
	e := c.entries[i]
	if !isDnode(nodeKind(e.flag >> offsetBitShift)) {
		return false
	}
	if !c.live[e.ino] && !c.created[e.ino] {
		return false
	}
	last, ok := c.lastFsync[e.ino]
	return ok && i <= last
}

// fileKind classifies an inode number of the chain: post-checkpoint (the replay would
// create it), live-update (the replay would update a file the NAT maps) or not-replayed.
func (c *rfChain) fileKind(ino uint32) string {
	switch {
	case c.live[ino]:
		return "live-update"
	case c.created[ino]:
		return "post-checkpoint"
	}
	return "not-replayed"
}

// chain returns the roll-forward chain, building it on first use.
func (f *FS) chain() (*rfChain, error) {
	s := &f.rf
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return s.c, s.err
	}
	s.builds++
	c, err := f.buildChain()
	if err != nil && !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUnsupported) {
		return nil, err
	}
	s.done, s.c, s.err = true, c, err
	return c, err
}

// buildChain walks the warm-node log of the chosen checkpoint (3H-R2). It adds no
// filesystem warning: findings go to the chain's own warnings.
func (f *FS) buildChain() (*rfChain, error) {
	sb, cp := f.sb, f.cp
	want, ok := cp.footerVersion()
	if !ok {
		return nil, fmt.Errorf("f2fs roll-forward: checkpoint_ver %#x has a non-zero high half, footer versions cannot be compared: %w", cp.ver, filesys.ErrUnsupported)
	}
	cur := cp.curNode[1] // the warm node log
	if cur.seg >= sb.segMain || cur.off >= blocksPerSeg {
		return nil, corrupt("f2fs roll-forward", -1, "warm node cursor (segment %d, offset %d) lies outside the main area of %d segments", cur.seg, cur.off, sb.segMain)
	}
	start := uint64(sb.mainAddr) + uint64(cur.seg)<<segShift + uint64(cur.off)
	mainEnd := sb.mainEnd()
	limit := int(min(uint64(maxChainNodes), uint64(sb.segMain)<<segShift)) // 3H-R14: never more nodes than main-area blocks
	if f.chainCap > 0 && f.chainCap < limit {
		limit = f.chainCap
	}
	natCap := sb.natCapacity()

	c := &rfChain{sum: filesys.RollForwardChain{
		CheckpointVersion: cp.ver, CheckpointPack: cp.pack, FooterVersion: want,
		CRCRecovery: cp.flags&cpCRCRecoveryFlag != 0, NoCRCRecovery: cp.flags&cpNoCRCRecoveryFlag != 0,
		Unmount: cp.flags&cpUmountFlag != 0, Start: start,
	}}
	// The visited set (the cycle check) is a Go map: about 40 bytes per node, so about 40 MB
	// at the 1M-node bound (3H-R14), on top of the 20 MB of chain entries. Both are freed
	// or kept only as the index; a bitset over the main blocks would cost one bit per main
	// block (512 MB for a 2^32-block volume) and is worse for a short chain on a large one.
	visited := map[uint32]struct{}{}
	blk := start
	stop := func(reason string, at uint64, complete bool) {
		c.sum.Stop, c.sum.Complete = fmt.Sprintf("%s@%d", reason, at), complete
	}
	for {
		if blk < uint64(sb.mainAddr) || blk >= mainEnd {
			stop("outside-main", blk, true)
			break
		}
		if _, seen := visited[uint32(blk)]; seen {
			stop("cycle", blk, true)
			c.sum.Warnings = append(c.sum.Warnings, "the chain loops back on itself; the kernel's replay would fail on this looped chain")
			break
		}
		b, err := readBlock(f.data, uint32(blk))
		if err != nil {
			if isIOError(err) {
				return nil, fmt.Errorf("f2fs roll-forward: read of block %d failed: %w", blk, err)
			}
			stop("image-end", blk, false)
			break
		}
		c.blocksRead++
		ft := parseFooter(b)
		if ft.cpVer != want {
			stop("version", blk, true)
			break
		}
		if ft.nid == 0 || ft.ino == 0 || uint64(ft.nid) >= natCap || uint64(ft.ino) >= natCap ||
			ft.nid == sb.nodeIno || ft.nid == sb.metaIno || ft.ino == sb.nodeIno || ft.ino == sb.metaIno {
			stop("footer", blk, false)
			break
		}
		if len(c.entries) >= limit {
			stop("cap", blk, false)
			break
		}
		visited[uint32(blk)] = struct{}{}
		c.entries = append(c.entries, chainEntry{block: uint32(blk), nid: ft.nid, ino: ft.ino, flag: ft.flag, next: ft.nextBlk})
		blk = uint64(ft.nextBlk)
	}
	c.sum.Nodes = len(c.entries)
	if cp.flags&cpUmountFlag != 0 && len(c.entries) > 0 {
		c.sum.Warnings = append(c.sum.Warnings, "an unmount checkpoint is followed by a roll-forward chain")
	}

	// live(X): the CURRENT NAT maps X, decided once per distinct inode number.
	live := map[uint32]bool{}
	seen := map[uint32]bool{}
	for _, e := range c.entries {
		if seen[e.ino] {
			continue
		}
		seen[e.ino] = true
		switch _, err := f.natGet(e.ino); {
		case err == nil:
			live[e.ino] = true
		case errors.Is(err, filesys.ErrNotFound):
		default:
			return nil, err
		}
	}
	d := deriveChain(c.entries, live)
	c.live, c.created, c.lastFsync = d.live, d.created, d.lastFsync
	return c, nil
}
