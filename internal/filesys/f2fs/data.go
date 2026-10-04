package f2fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// File data. A regular file's blocks are addressed three ways (kernel
// include/linux/f2fs_fs.h, struct f2fs_inode / direct_node / indirect_node):
//
//   - the inode's own data slots (i_addr[], see inode.addrSlots), which hold
//     block addresses for logical blocks 0.. ;
//   - i_nid[0] and i_nid[1], direct nodes: 1018 addresses each;
//   - i_nid[2] and i_nid[3], indirect nodes: 1018 nids of direct nodes each;
//   - i_nid[4], a double-indirect node: 1018 nids of indirect nodes.
//
// An address of 0 (NULL_ADDR) is a hole; 0xFFFFFFFF (NEW_ADDR) is a block that
// was allocated but never written and also reads as zeros. A nid of 0 is a
// hole covering everything below that node. Any other address must lie in the
// main area.
const (
	nullAddr = 0
	newAddr  = 0xFFFFFFFF

	// addrsPerBlock is ADDRS_PER_BLOCK / NIDS_PER_BLOCK: the 32-bit words of a
	// 4 KiB node before its 24-byte footer.
	addrsPerBlock = (blockSize - 24) / 4 // 1018

	maxFileRuns   = 1 << 20 // runs, and node blocks read, per file
	inlineReserve = 1       // DEF_INLINE_RESERVED_SIZE: the word before inline data
)

// nodeLevels gives the depth below the inode of each i_nid slot: 1 is a
// direct node, 2 an indirect node, 3 a double-indirect node.
var nodeLevels = [nidsPerInode]int{1, 1, 2, 2, 3}

// span is the number of logical blocks a node of the given level maps.
func span(level int) int64 {
	n := int64(1)
	for range level {
		n *= addrsPerBlock
	}
	return n
}

// chain collects the runs of one file in file order.
type chain struct {
	f       *FS
	in      *inode
	size    int64 // bytes the runs must cover
	nb      int64 // logical blocks that cover size
	next    int64 // next logical block to map
	pos     int64 // bytes covered so far
	limit   uint64
	runs    []filesys.Run
	visited map[uint32]struct{}
}

func (c *chain) done() bool { return c.next >= c.nb }

// add maps blocks logical blocks at filesystem byte offset off (-1: a hole),
// trimmed to the size. Runs are merged by hand, in file order: a hole extends a
// preceding hole, a data run one that ends exactly where it begins.
func (c *chain) add(off, blocks int64) error {
	blocks = min(blocks, c.nb-c.next)
	if blocks <= 0 {
		return nil
	}
	rem := c.size - c.pos
	n := rem // the last block of the file is trimmed to the size
	if blocks < (rem+blockSize-1)/blockSize {
		n = blocks * blockSize
	}
	if k := len(c.runs); k > 0 {
		last := &c.runs[k-1]
		if (off < 0 && last.Offset < 0) || (off >= 0 && last.Offset >= 0 && last.Offset+last.Length == off) {
			last.Length += n // <= size: cannot overflow
			c.pos += n
			c.next += blocks
			return nil
		}
	}
	if len(c.runs) >= maxFileRuns {
		return corrupt("f2fs file data", -1, "inode %d maps more than %d runs", c.in.nid, maxFileRuns)
	}
	c.runs = append(c.runs, filesys.Run{Offset: off, Length: n})
	c.pos += n
	c.next += blocks
	return nil
}

// addr maps the next logical block to the data address a.
func (c *chain) addr(a uint32) error {
	if a == nullAddr || a == newAddr {
		return c.add(-1, 1)
	}
	if a < c.f.sb.mainAddr || uint64(a) >= c.limit {
		return corrupt("f2fs file data", int64(a)*blockSize, "inode %d: data block address %d is outside the main area %d..%d", c.in.nid, a, c.f.sb.mainAddr, c.limit)
	}
	return c.add(int64(a)*blockSize, 1)
}

// node maps the blocks below node nid (of the given level) at the current
// position. A zero nid is a hole spanning the node's whole range.
func (c *chain) node(nid uint32, level int) error {
	if c.done() {
		return nil
	}
	if nid == 0 {
		return c.add(-1, span(level))
	}
	if _, dup := c.visited[nid]; dup {
		return corrupt("f2fs file data", -1, "inode %d: node id %d is referenced twice (cycle or shared node)", c.in.nid, nid)
	}
	if len(c.visited) >= maxFileRuns {
		return corrupt("f2fs file data", -1, "inode %d maps more than %d node blocks", c.in.nid, maxFileRuns)
	}
	c.visited[nid] = struct{}{}
	b, err := c.f.node(nid)
	if err != nil {
		if errors.Is(err, filesys.ErrNotFound) {
			return corrupt("f2fs file data", -1, "inode %d: node id %d is free", c.in.nid, nid)
		}
		return err
	}
	for i := 0; i < addrsPerBlock && !c.done(); i++ {
		w := binary.LittleEndian.Uint32(b[4*i:])
		if level == 1 {
			err = c.addr(w)
		} else {
			err = c.node(w, level-1)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// runs returns the filesystem-relative byte runs that cover [0, in.size) of a
// file stored in data blocks: holes (NULL_ADDR, NEW_ADDR, absent nodes) are
// runs with Offset -1, the last run is trimmed to the size, and a size of zero
// has no runs.
//
// When the map is damaged (a bad address, a cycle, a node that is free or
// unreadable, more than maxFileRuns runs, a size beyond what the tree can
// address) it returns the runs mapped before the damage together with a
// *filesys.CorruptError: they are the trusted prefix of the file. A genuine I/O
// error is returned alone, with nil runs.
func (f *FS) runs(in *inode) ([]filesys.Run, error) {
	if in.size == 0 {
		return nil, nil
	}
	c := &chain{
		f: f, in: in, size: in.size,
		nb:      (in.size-1)/blockSize + 1,
		limit:   min(f.sb.mainEnd(), uint64(f.sb.blocks)),
		visited: map[uint32]struct{}{in.nid: {}},
	}
	err := c.walk()
	if err == nil && !c.done() {
		err = corrupt("f2fs file data", -1, "inode %d: size %d is beyond the addressable range of the node tree (%d blocks)", in.nid, in.size, c.next)
	}
	if err != nil {
		if !errors.Is(err, filesys.ErrCorrupt) {
			return nil, err
		}
		return c.runs, err
	}
	return c.runs, nil
}

func (c *chain) walk() error {
	raw := c.in.raw
	for i := 0; i < c.in.addrSlots && !c.done(); i++ {
		if err := c.addr(binary.LittleEndian.Uint32(raw[c.in.addrStart+4*i:])); err != nil {
			return err
		}
	}
	for i, nid := range c.in.nids {
		if err := c.node(nid, nodeLevels[i]); err != nil {
			return err
		}
	}
	return nil
}

// parseDentryID reports whether id is a canonical "dentry:<dir>:<blk>:<slot>"
// ID of a deleted directory entry (decimal numbers, no sign, no leading zero).
func parseDentryID(id string) bool {
	rest, ok := strings.CutPrefix(id, "dentry:")
	if !ok {
		return false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != p {
			return false
		}
	}
	return true
}

// Open opens a regular file or symlink. Only e.ID is used and everything is
// re-read from the volume: Size, Type, Deleted and Attrs on e are ignored. A
// "dentry:" ID (a deleted directory slot) yields filesys.ErrDeleted, an
// "nid:<n>" ID re-reads inode n; directories and special files yield
// filesys.ErrUnsupported, and so does a compressed file. Encrypted content is
// returned as stored (ciphertext). Inline data and inline symlink targets live
// in the inode, so their Runs are nil.
//
// A file whose block map is damaged is opened with the trusted prefix: Runs
// lists only the blocks mapped before the damage (filesys.CheckRunsPrefix
// accepts it, filesys.CheckRuns does not), every read at or beyond the end of
// that prefix returns a *filesys.CorruptError and never zeros, and an Info
// warning is added.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	if parseDentryID(e.ID) {
		return nil, filesys.ErrDeleted
	}
	nid, err := parseNodeID(e.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(nid)
	if err != nil {
		return nil, err
	}
	return f.openInode(in)
}

func (f *FS) openInode(in *inode) (filesys.File, error) {
	switch in.typ() {
	case modeReg, modeSymlink:
	case modeDir:
		return nil, fmt.Errorf("%w: inode %d is a directory", filesys.ErrUnsupported, in.nid)
	default:
		return nil, fmt.Errorf("%w: inode %d is neither a regular file nor a symlink", filesys.ErrUnsupported, in.nid)
	}
	if in.compressed {
		return nil, fmt.Errorf("%w: compressed file (inode %d)", filesys.ErrUnsupported, in.nid)
	}
	if in.inline&inlineData != 0 {
		return f.openInline(in)
	}
	runs, chainErr := f.runs(in)
	if chainErr != nil && !errors.Is(chainErr, filesys.ErrCorrupt) {
		return nil, chainErr
	}
	covered, err := filesys.CheckRunsPrefix(runs, in.size, f.size)
	if err != nil {
		return nil, err
	}
	fl := &file{size: in.size, avail: covered, runs: runs, r: f.data}
	var pos int64
	for _, r := range runs {
		fl.starts = append(fl.starts, pos)
		pos += r.Length
	}
	switch {
	case chainErr != nil:
		fl.tail = chainErr
		f.warn("file inode %d: %v; the %d bytes mapped before it are readable and the rest reads as an error", in.nid, chainErr, covered)
	case covered != in.size:
		fl.tail = corrupt("f2fs file data", -1, "inode %d: runs cover %d of %d bytes", in.nid, covered, in.size)
	}
	return fl, nil
}

// openInline returns the content stored in the inode: it starts one word after
// the first data slot and cannot extend beyond the data slots.
func (f *FS) openInline(in *inode) (filesys.File, error) {
	maxInline := int64(max(in.addrSlots-inlineReserve, 0)) * 4
	if in.size > maxInline {
		return nil, corrupt("f2fs inline data", -1, "inode %d: inline size %d exceeds the %d bytes the inode can hold", in.nid, in.size, maxInline)
	}
	start := in.addrStart + inlineReserve*4
	return &file{size: in.size, avail: in.size, mem: slices.Clone(in.raw[start : start+int(in.size)])}, nil
}

// file is the content of one inode. It is immutable, so concurrent ReadAt
// calls are safe.
type file struct {
	size   int64
	avail  int64         // bytes the runs cover (== size unless the block map is damaged)
	mem    []byte        // inline content (runs is nil), else nil
	runs   []filesys.Run // file order, trimmed to avail
	starts []int64       // starts[i] is the file offset where runs[i] begins
	tail   error         // what reading [avail, size) returns; nil when avail == size
	r      io.ReaderAt   // the filesystem, for non-hole runs
}

// Size is the content length.
func (fl *file) Size() int64 { return fl.size }

// Runs returns a copy of the filesystem-relative runs in file order (nil for
// inline content). They cover [0, Size()) exactly unless the block map is
// damaged; then they cover only the trusted prefix and reads at or beyond its
// end fail with filesys.ErrCorrupt.
func (fl *file) Runs() []filesys.Run { return slices.Clone(fl.runs) }

// ReadAt reads file content. Holes read as zeros; bytes the block map does not
// vouch for are an error (a *filesys.CorruptError), never zeros.
func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("f2fs: negative read offset")
	}
	if off >= fl.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	want, eof := int64(len(p)), false
	if rem := fl.size - off; want > rem {
		want, eof = rem, true
	}
	var n int64
	for n < want {
		pos := off + n
		if fl.mem != nil {
			n += int64(copy(p[n:want], fl.mem[pos:]))
			continue
		}
		if pos >= fl.avail {
			if fl.tail != nil {
				return int(n), fl.tail
			}
			return int(n), io.ErrUnexpectedEOF // unreachable: avail == size without a tail error
		}
		i := sort.Search(len(fl.starts), func(i int) bool { return fl.starts[i] > pos }) - 1
		if i < 0 {
			return int(n), io.ErrUnexpectedEOF // unreachable: starts[0] is 0
		}
		r, within := fl.runs[i], pos-fl.starts[i]
		chunk := min(want-n, r.Length-within)
		if r.Offset < 0 {
			clear(p[n : n+chunk])
		} else if err := readFull(fl.r, p[n:n+chunk], r.Offset+within); err != nil {
			return int(n), readError("f2fs file data", r.Offset+within, err)
		}
		n += chunk
	}
	if eof {
		return int(n), io.EOF
	}
	return int(n), nil
}
