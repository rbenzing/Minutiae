package f2fstest

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// Roll-forward chain support: node blocks that the kernel writes into the warm
// node log after the last checkpoint (fsync without a checkpoint). Flag values
// are from memory of include/linux/f2fs_fs.h and were confirmed by the 3H probe
// (tools/fixtures/README.md, "F2FS roll-forward probe (3H)"): footer cp_ver =
// checkpoint_ver | crc<<32 on a volume with CP_CRC_RECOVERY_FLAG, every chain node
// carries the cold bit, an fsynced inode is 0x7 (cold|fsync|dent), a direct node
// 0x3|ofs<<3, and no NAT entry or SIT bit exists for them.
const (
	CPNoCRCRecovery = 0x200 // CP_NOCRC_RECOVERY_FLAG (0x20 is CP_FASTBOOT_FLAG)
	CPCRCRecovery   = 0x40  // CP_CRC_RECOVERY_FLAG
	FlagCold        = 0x1   // footer flag bits: COLD_BIT_SHIFT 0
	FlagFsync       = 0x2   // FSYNC_BIT_SHIFT 1
	FlagDent        = 0x4   // DENT_BIT_SHIFT 2
)

// ChainNode is one block of the log. The builder places the elements at
// consecutive blocks from the warm cursor (CurNode[1]; when it is zero the
// builder sets it to the block after the last node it placed in the node
// segments, in both packs), sets each footer's FSYNC/DENT bits from the fields
// (keeping the block's own ofs and cold bit), cp_ver to ChainFooterVersion unless
// CPVer is set, and next_blkaddr to the next element's address (the last: the
// block after it) unless Next is set. No NAT entry, SIT bit clear; an inode's
// checksum is resealed after the footer is written.
//
// A live file's chain is its inode node, plus dnodes only for blocks appended
// beyond the inode's own address slots (probe H8): an append inside the slots
// writes the inode alone.
type ChainNode struct {
	Block         []byte
	Fsync, Dentry bool
	Addr          uint32  // 0 = the next block of the log
	CPVer         *uint64 // nil = what the kernel writes
	Next          *uint32 // nil = the next element
}

// ChainFooterVersion is the footer cp_ver the kernel's rule expects for img: the
// newer pack's checkpoint_ver, with the stored checksum in the high half when
// that pack has CPCRCRecovery and not CPNoCRCRecovery. The builder decodes the
// superblock and the pack header itself.
func ChainFooterVersion(img []byte) uint64 {
	le := binary.LittleEndian
	base := int(le.Uint32(img[sbOffset+76:]))
	var best []byte
	for _, p := range []int{base, base + BlocksPerSeg} {
		h := img[p*BlockSize : (p+1)*BlockSize]
		if best == nil || le.Uint64(h) > le.Uint64(best) {
			best = h
		}
	}
	ver, flags := le.Uint64(best), le.Uint32(best[132:])
	if flags&CPCRCRecovery != 0 && flags&CPNoCRCRecovery == 0 {
		return ver | uint64(le.Uint32(best[le.Uint32(best[164:]):]))<<32
	}
	return ver
}

// chainCursor sets CurNode[1] when it is zero to the block after the last node
// block o places in the node segments (the first node-segment block when it
// places none). It panics when a chain is given without NodeSegs.
func chainCursor(o Options) Options {
	if len(o.Chain) == 0 {
		return o
	}
	if o.NodeSegs <= 0 {
		panic("f2fstest: a Chain needs NodeSegs > 0")
	}
	if o.CurNode[1] != (CurSeg{}) {
		return o
	}
	l := Geometry(o)
	first := l.Main + (l.MainSegs-uint32(o.NodeSegs))*BlocksPerSeg
	next := first
	for _, ns := range [][]Node{o.Nodes, o.StaleNodes} {
		for _, n := range ns {
			if n.Block != nil && n.Addr >= first && n.Addr < l.BlockCount {
				next = max(next, n.Addr+1)
			}
		}
	}
	seg, off := mainSeg(l, next)
	o.CurNode[1] = CurSeg{Seg: seg, Off: uint16(off)}
	return o
}

// chainAddrs returns the block address of every element of o.Chain: the warm
// cursor plus the index, or the element's own Addr.
func chainAddrs(o Options) []uint32 {
	if len(o.Chain) == 0 {
		return nil
	}
	o = chainCursor(o)
	l := Geometry(o)
	start := l.Main + o.CurNode[1].Seg*BlocksPerSeg + uint32(o.CurNode[1].Off)
	addrs := make([]uint32, len(o.Chain))
	for i, c := range o.Chain {
		addrs[i] = start + uint32(i)
		if c.Addr != 0 {
			addrs[i] = c.Addr
		}
	}
	checkChainCollisions(o, addrs)
	return addrs
}

// checkChainCollisions panics when a chain block address is also the address of
// a node (live or stale), of a data block or of another chain element: the later
// write would silently replace the earlier block.
func checkChainCollisions(o Options, addrs []uint32) {
	taken := make(map[uint32]string)
	for _, ns := range [][]Node{o.Nodes, o.StaleNodes} {
		for _, n := range ns {
			if n.Block != nil && n.Addr != 0 {
				taken[n.Addr] = "a node"
			}
		}
	}
	for _, d := range o.Data {
		taken[d.Addr] = "a data block"
	}
	for i, a := range addrs {
		if what, ok := taken[a]; ok {
			panic(fmt.Sprintf("f2fstest: chain element %d at block %d collides with %s", i, a, what))
		}
		taken[a] = "another chain element"
	}
}

// writeChain writes the chain after both checkpoint packs are sealed (the footer
// version needs the sealed checksum). It writes no NAT entry and no SIT bit.
func writeChain(img []byte, o Options, l Layout) {
	addrs := chainAddrs(o)
	if len(addrs) == 0 {
		return
	}
	ver := ChainFooterVersion(img)
	le := binary.LittleEndian
	for i, c := range o.Chain {
		if len(c.Block) != BlockSize {
			panic("f2fstest: chain node block must be 4096 bytes")
		}
		if addrs[i] < l.Main || addrs[i] >= l.BlockCount {
			panic("f2fstest: chain node outside the main area")
		}
		b := slices.Clone(c.Block)
		flag := le.Uint32(b[footerOff+8:]) &^ (FlagFsync | FlagDent)
		if c.Fsync {
			flag |= FlagFsync
		}
		if c.Dentry {
			flag |= FlagDent
		}
		le.PutUint32(b[footerOff+8:], flag)
		v := ver
		if c.CPVer != nil {
			v = *c.CPVer
		}
		le.PutUint64(b[footerOff+12:], v)
		next := addrs[i] + 1
		if c.Next != nil {
			next = *c.Next
		} else if i+1 < len(addrs) {
			next = addrs[i+1]
		}
		le.PutUint32(b[footerOff+20:], next)
		if flag>>3 == 0 && le.Uint32(b[footerOff:]) == le.Uint32(b[footerOff+4:]) {
			resealBlock(o, b) // an inode: its checksum covers the footer
		}
		copy(img[int(addrs[i])*BlockSize:], b)
	}
}
