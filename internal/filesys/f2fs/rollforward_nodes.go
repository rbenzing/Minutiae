package f2fs

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// errRFNotImplemented is what a roll-forward entry point that is not built yet returns:
// unknown must never read as absent while the branch is in progress.
func errRFNotImplemented() error {
	return fmt.Errorf("f2fs roll-forward: not implemented: %w", filesys.ErrUnsupported)
}

// rfBlock reads chain block blk for a report. The index said the block was readable, so a
// failure is a short read (a *CorruptError) or the I/O error, never "absent".
func (f *FS) rfBlock(blk uint32) ([]byte, error) {
	b, err := readBlock(f.data, blk)
	if err != nil {
		return nil, readError("f2fs roll-forward node", int64(blk)*blockSize, err)
	}
	return b, nil
}

// quietParser returns an FS that shares the superblock only, for parsing a chain inode:
// the parser's own warnings go to the FS it is called on, so they are dropped and a
// roll-forward report never adds a filesystem warning. The finding is in the node's
// Inode.Err or Checksum instead.
func (f *FS) quietParser() *FS { return &FS{sb: f.sb} }

// RollForward visits every chain node in order (3H-R4) with fresh slices. The summary
// describes the whole chain whether or not visit stops early. It never replays and never
// changes what the reader serves.
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
	sum.Warnings = slices.Clone(c.sum.Warnings)
	p := f.quietParser()
	for i, e := range c.entries {
		raw, err := f.rfBlock(e.block)
		if err != nil {
			return filesys.RollForwardChain{}, err
		}
		if !visit(c.node(p, i, raw)) {
			break
		}
	}
	return sum, nil
}

// node builds the report of chain entry i from its block.
func (c *rfChain) node(p *FS, i int, raw []byte) filesys.RollForwardNode {
	e := c.entries[i]
	ofs := e.flag >> offsetBitShift
	n := filesys.RollForwardNode{
		Index: i, Block: uint64(e.block), Next: uint64(e.next),
		NID: e.nid, Ino: e.ino, Ofs: ofs, Flag: e.flag, CPVer: c.sum.FooterVersion,
		Kind:  nodeKind(ofs),
		Fsync: e.flag&footFsyncBit != 0, Dentry: e.flag&footDentBit != 0, Cold: e.flag&footColdBit != 0,
		LiveIno: c.live[e.ino], WouldReplay: c.wouldReplay(i),
	}
	switch n.Kind {
	case "inode":
		n.Inode, n.Addrs, n.NIDs = p.chainInode(raw, e)
	case "direct":
		n.Addrs = leWords(raw, 0, addrsPerBlock)
	case "indirect", "double-indirect":
		n.NIDs = leWords(raw, 0, addrsPerBlock)
	}
	return n
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
func (f *FS) chainInode(raw []byte, e chainEntry) (*filesys.RollForwardInode, []uint32, []uint32) {
	if e.nid != e.ino {
		return &filesys.RollForwardInode{Err: fmt.Sprintf("footer nid %d differs from ino %d at offset 0", e.nid, e.ino)}, nil, nil
	}
	in, err := f.parseInodeBlock(raw, e.ino)
	if err != nil {
		return &filesys.RollForwardInode{Err: err.Error()}, nil, nil
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
	return r, leWords(raw, in.addrStart, in.addrSlots), slices.Clone(in.nids[:])
}

// files summarises the chain per inode number, in order of first appearance (3H-R5).
// The inode block of a file is its last inode node at or before its last fsync node
// (the copy the kernel's replay ends with), else its last inode node.
func (c *rfChain) files(f *FS) ([]filesys.RollForwardFile, error) {
	idx := make(map[uint32]int)
	var out []filesys.RollForwardFile
	pick := make(map[uint32]int)
	fallback := make(map[uint32]int)
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
			fallback[e.ino] = i
			if last, ok := c.lastFsync[e.ino]; !ok || i <= last {
				pick[e.ino] = i
			}
		}
	}
	p := f.quietParser()
	for k := range out {
		fl := &out[k]
		i, ok := pick[fl.Ino]
		if !ok {
			if i, ok = fallback[fl.Ino]; !ok {
				continue
			}
		}
		e := c.entries[i]
		fl.InodeBlock = uint64(e.block)
		raw, err := f.rfBlock(e.block)
		if err != nil {
			return nil, err
		}
		// HasInode means a usable inode: an inode node that does not parse leaves it false
		// (InodeBlock still says where it is, and the node's Inode.Err says why).
		in, err := p.parseInodeBlock(raw, fl.Ino)
		if err != nil {
			continue
		}
		fl.HasInode, fl.Pino, fl.Size = true, in.pino, in.size
		if !in.nameBad {
			fl.Name, _ = displayName(in.name, in.encrypted)
		}
	}
	return out, nil
}
