package f2fs

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// rfBlock reads chain block blk for a report. The index said the block was readable, so a
// failure is a short read (a *CorruptError) or the I/O error, never "absent".
func (f *FS) rfBlock(blk uint32) ([]byte, error) {
	b, err := readBlock(f.data, blk)
	if err != nil {
		return nil, readError("f2fs roll-forward node", int64(blk)*blockSize, err)
	}
	return b, nil
}

// parseChainInode parses a chain inode node on a throwaway FS that shares the superblock
// only: the parser's warnings go to the FS it is called on, so they are returned here
// instead of reaching the real FS (a roll-forward report adds no filesystem warning).
func (f *FS) parseChainInode(raw []byte, ino uint32) (*inode, []string, error) {
	q := &FS{sb: f.sb}
	in, err := q.parseInodeBlock(raw, ino)
	return in, q.warnings.Snapshot(), err
}

// RollForward visits every chain node in order (3H-R4) with fresh slices. The summary
// describes the whole chain whether or not visit stops early: after a stop the remaining
// blocks are still read, for the parser warnings of their inodes. It never replays and
// never changes what the reader serves.
func (f *FS) RollForward(visit func(filesys.RollForwardNode) bool) (filesys.RollForwardChain, error) {
	c, err := f.chain()
	if err != nil {
		return filesys.RollForwardChain{}, err
	}
	files, err := c.files(f)
	if err != nil {
		return filesys.RollForwardChain{}, err
	}
	sum := c.sum
	sum.Files = files
	var inodeWarns filesys.Warnings
	stopped := false
	for i, e := range c.entries {
		raw, err := f.rfBlock(e.block)
		if err != nil {
			return filesys.RollForwardChain{}, err
		}
		n, ws := c.node(f, i, raw)
		for _, w := range ws {
			inodeWarns.Add("chain inode at block %d (nid %d): %s", e.block, e.nid, w)
		}
		if !stopped && !visit(n) {
			stopped = true
		}
	}
	sum.Warnings = append(slices.Clone(c.sum.Warnings), inodeWarns.Snapshot()...)
	return sum, nil
}

// node builds the report of chain entry i from its block, with the parser warnings of an
// inode node.
func (c *rfChain) node(f *FS, i int, raw []byte) (filesys.RollForwardNode, []string) {
	e := c.entries[i]
	ofs := e.flag >> offsetBitShift
	n := filesys.RollForwardNode{
		Index: i, Block: uint64(e.block), Next: uint64(e.next),
		NID: e.nid, Ino: e.ino, Ofs: ofs, Flag: e.flag, CPVer: c.sum.FooterVersion,
		Kind:  nodeKind(ofs),
		Fsync: e.flag&footFsyncBit != 0, Dentry: e.flag&footDentBit != 0, Cold: e.flag&footColdBit != 0,
		LiveIno: c.live[e.ino], WouldReplay: c.wouldReplay(i),
	}
	var ws []string
	switch n.Kind {
	case "inode":
		n.Inode, n.Addrs, n.NIDs, ws = f.chainInode(raw, e)
	case "direct":
		n.Addrs = leWords(raw, 0, addrsPerBlock)
	case "indirect", "double-indirect":
		n.NIDs = leWords(raw, 0, addrsPerBlock)
	}
	return n, ws
}

// leWords copies n little-endian 32-bit words from raw at off into a fresh slice.
func leWords(raw []byte, off, n int) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(raw[off+4*i:])
	}
	return out
}

// chainInode decodes the inode a chain inode node carries. A parse failure fills Err and
// leaves the rest zero; a bad checksum is reported (Checksum "bad") and the fields stay: it
// is a report, and no map is ever taken from such a copy (3H-R3).
func (f *FS) chainInode(raw []byte, e chainEntry) (*filesys.RollForwardInode, []uint32, []uint32, []string) {
	if e.nid != e.ino {
		return &filesys.RollForwardInode{Err: fmt.Sprintf("footer nid %d differs from ino %d at offset 0", e.nid, e.ino)}, nil, nil, nil
	}
	in, ws, err := f.parseChainInode(raw, e.ino)
	if err != nil {
		return &filesys.RollForwardInode{Err: err.Error()}, nil, nil, ws
	}
	r := &filesys.RollForwardInode{
		Mode: uint32(in.mode), UID: in.uid, GID: in.gid, Links: in.links, Pino: in.pino, Flags: in.flags,
		Size: in.size, Times: in.times, Inline: in.inline,
		Encrypted: in.encrypted, InlineData: in.inline&inlineData != 0, Compressed: in.compressed,
		Checksum: "not-recorded",
	}
	if in.nameBad {
		r.Err = "i_namelen is out of range; the name is unknown"
	} else {
		r.Name, r.RawName = displayName(in.name, in.encrypted)
	}
	if in.csumChecked {
		r.Checksum = "bad"
		if in.csumOK {
			r.Checksum = "ok"
		}
	}
	return r, leWords(raw, in.addrStart, in.addrSlots), slices.Clone(in.nids[:]), ws
}

// goodInode reports whether a parsed chain inode may feed the file summary: a copy whose
// recorded checksum is wrong never does (3H-R3).
func goodInode(in *inode) bool { return !in.csumChecked || in.csumOK }

// files summarises the chain per inode number, in order of first appearance (3H-R5).
// Size and the rest come from the last good inode node at or before the file's last fsync
// node; name and parent from the last good dentry-marked inode node at or before it (what
// the kernel's recover_dentry links), else from the first. A file with no good inode copy
// has HasInode false and a Caveat; InodeBlock then names the last copy considered.
func (c *rfChain) files(f *FS) ([]filesys.RollForwardFile, error) {
	idx := make(map[uint32]int)
	var out []filesys.RollForwardFile
	inodes := make(map[uint32][]int) // ino -> entry indexes of its inode nodes, in order
	for i, e := range c.entries {
		if _, ok := idx[e.ino]; !ok {
			idx[e.ino] = len(out)
			fl := filesys.RollForwardFile{Ino: e.ino, Kind: c.fileKind(e.ino)}
			if fl.Kind == "post-checkpoint" {
				fl.ID = "rollforward:" + strconv.FormatUint(uint64(e.ino), 10)
			}
			if last, ok := c.lastFsync[e.ino]; ok {
				fl.LastFsyncBlock = uint64(c.entries[last].block)
			}
			out = append(out, fl)
		}
		if e.nid == e.ino && e.flag>>offsetBitShift == 0 {
			inodes[e.ino] = append(inodes[e.ino], i)
		}
	}
	for k := range out {
		fl := &out[k]
		cands := inodes[fl.Ino]
		if last, ok := c.lastFsync[fl.Ino]; ok {
			cands = cands[:sortedPrefix(cands, last)]
		}
		if len(cands) == 0 {
			cands = inodes[fl.Ino] // no inode node at or before the last fsync: report the last one
		}
		if len(cands) == 0 {
			continue
		}
		var main, named *inode
		var mainAt int
		for j := len(cands) - 1; j >= 0 && (main == nil || named == nil); j-- {
			e := c.entries[cands[j]]
			if j == len(cands)-1 {
				fl.InodeBlock = uint64(e.block)
			}
			raw, err := f.rfBlock(e.block)
			if err != nil {
				return nil, err
			}
			in, _, perr := f.parseChainInode(raw, fl.Ino)
			if perr != nil || !goodInode(in) {
				continue
			}
			if main == nil {
				main, mainAt = in, cands[j]
			}
			if named == nil && e.flag&footDentBit != 0 {
				named = in
			}
		}
		if main == nil {
			fl.Caveat = "no inode copy with a valid checksum before the last fsync: size, name and parent are unknown"
			continue
		}
		fl.InodeBlock = uint64(c.entries[mainAt].block)
		if named == nil {
			named = main
		}
		fl.HasInode, fl.Size, fl.Pino = true, main.size, named.pino
		if named.nameBad {
			fl.Caveat = "i_namelen is out of range: the name is unknown"
		} else {
			fl.Name, _ = displayName(named.name, named.encrypted)
		}
	}
	return out, nil
}

// sortedPrefix returns how many of the ascending indexes are <= limit.
func sortedPrefix(idx []int, limit int) int {
	n, _ := slices.BinarySearchFunc(idx, limit, func(a, b int) int {
		if a <= b {
			return -1
		}
		return 1
	})
	return n
}
