package ext4

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

// Inode flags and limits for file data.
const (
	inodeFlagExtents    = 0x80000
	inodeFlagInlineData = 0x10000000

	extentMagic     = 0xF30A
	extentHeaderLen = 12
	extentEntryLen  = 12
	extentMaxInit   = 32768 // ee_len above this marks an uninitialized extent

	maxExtentDepth = 5       // levels below the root
	maxFileRuns    = 1 << 20 // runs, extents and tree nodes per file
	maxInlineData  = inodeBlockLen + 4096
	fastSymlinkMax = inodeBlockLen // a fast symlink target is shorter than i_block
)

// runList accumulates the byte runs of one file in file order. Runs are
// trimmed to size, adjacent holes and physically adjacent runs are merged, and
// the number of runs is capped.
type runList struct {
	runs []filesys.Run
	size int64 // bytes the runs must cover
	pos  int64 // bytes covered so far
}

func (l *runList) full() bool { return l.pos >= l.size }

// add appends n bytes at filesystem offset off (-1: a hole). The part beyond
// size is dropped.
func (l *runList) add(off, n int64) error {
	if rem := l.size - l.pos; n > rem {
		n = rem
	}
	if n <= 0 {
		return nil
	}
	if k := len(l.runs); k > 0 {
		last := &l.runs[k-1]
		if (off < 0 && last.Offset < 0) || (off >= 0 && last.Offset >= 0 && last.Offset+last.Length == off) {
			last.Length += n // <= size: cannot overflow
			l.pos += n
			return nil
		}
	}
	if len(l.runs) >= maxFileRuns {
		return corrupt("ext4 file data", -1, "file has more than %d runs", maxFileRuns)
	}
	l.runs = append(l.runs, filesys.Run{Offset: off, Length: n})
	l.pos += n
	return nil
}

// addBlocks appends count blocks of bs bytes starting at filesystem block
// phys (negative: a hole). A byte count that overflows is necessarily longer
// than what is left, so it is trimmed to size by add.
func (l *runList) addBlocks(phys, count, bs int64) error {
	n, ok := filesys.MulOK(count, bs)
	if !ok {
		n = l.size - l.pos
	}
	off := int64(-1)
	if phys >= 0 {
		if off, ok = filesys.MulOK(phys, bs); !ok {
			return corrupt("ext4 file data", -1, "block %d offset overflows", phys)
		}
	}
	return l.add(off, n)
}

// finish pads the sparse tail with a hole so the runs cover exactly size.
func (l *runList) finish() error {
	return l.add(-1, l.size-l.pos)
}

// runs returns the filesystem-relative byte runs that cover [0, in.size) of a
// file stored in data blocks (extent tree or block map). Holes and
// uninitialized extents are runs with Offset -1; the last run is trimmed to the
// size. A size of zero has no runs.
func (f *FS) runs(in *inode) ([]filesys.Run, error) {
	runs, err := f.runsPartial(in)
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// runsPartial is runs, but when the map is damaged part-way (a bad extent node
// or indirect block) it returns the runs mapped before the damage together with
// the error. They cover a prefix of the file in file order and are not checked
// against the size.
func (f *FS) runsPartial(in *inode) ([]filesys.Run, error) {
	if in.size == 0 {
		return nil, nil
	}
	rl := &runList{size: in.size}
	var err error
	if in.flags&inodeFlagExtents != 0 {
		err = f.extentRuns(in, rl)
	} else {
		err = f.blockMapRuns(in, rl)
	}
	if err != nil {
		return rl.runs, err
	}
	if err = rl.finish(); err == nil {
		err = filesys.CheckRuns(rl.runs, in.size, f.size)
	}
	if err != nil {
		return nil, err
	}
	return rl.runs, nil
}

// readNode reads one metadata block (an extent node or an indirect block).
func (f *FS) readNode(st string, blk uint64) ([]byte, error) {
	if blk == 0 || blk < uint64(f.sb.firstDataBlock) || blk >= uint64(f.sb.blocksCount) {
		return nil, corrupt(st, -1, "block %d is outside the %d-block filesystem", blk, f.sb.blocksCount)
	}
	off, ok := filesys.MulOK(int64(blk), int64(f.sb.blockSize))
	if !ok {
		return nil, corrupt(st, -1, "block %d offset overflows", blk)
	}
	buf := make([]byte, f.sb.blockSize) // one block: bounded by the block size
	if err := readFull(f.r, buf, off); err != nil {
		return nil, corrupt(st, off, "read of block %d failed: %v", blk, err)
	}
	return buf, nil
}

// extentWalker walks an extent tree in file order.
type extentWalker struct {
	f       *FS
	in      *inode // the file the tree belongs to (for block checksums)
	rl      *runList
	visited map[uint64]struct{} // node blocks read so far: each is read at most once
	entries int                 // entries examined so far
	next    uint64              // first logical block not covered yet
}

// extentRuns collects the runs of a file whose i_block holds an extent root.
func (f *FS) extentRuns(in *inode, rl *runList) error {
	const st = "ext4 extent tree"
	if len(in.block) < extentHeaderLen || binary.LittleEndian.Uint16(in.block[:]) != extentMagic {
		return corrupt(st, in.offset+iBlock, "inode %d: i_block has no extent header", in.num)
	}
	depth := int(binary.LittleEndian.Uint16(in.block[6:]))
	if depth > maxExtentDepth {
		return corrupt(st, in.offset+iBlock, "inode %d: extent tree depth %d exceeds %d", in.num, depth, maxExtentDepth)
	}
	w := &extentWalker{f: f, in: in, rl: rl, visited: map[uint64]struct{}{}}
	return w.node(in.block[:], depth, in.offset+iBlock)
}

// checkDataPointer warns when n data blocks starting at phys overlap the
// filesystem's own metadata (see metaRanges). Reading goes on: a damaged or
// hostile inode may point anywhere, and the examiner decides what that means.
func (f *FS) checkDataPointer(in *inode, phys, n uint64) {
	for _, r := range f.metaRanges {
		if phys < r[1] && phys+n > r[0] {
			f.warn("file data pointer into filesystem metadata (inode %d)", in.num)
			return
		}
	}
}

// checkBlockChecksum verifies the tail checksum of the extent block blk (buf)
// when metadata_csum is on, as the kernel's ext4_extent_block_csum does: crc32c
// over the block up to the tail (12 + 12*eh_max), seeded with the filesystem
// seed folded with the inode number and generation; the tail is the le32 right
// after the last possible entry. A mismatch is a warning only. A block with a
// bad header is left to node, which reports it.
func (w *extentWalker) checkBlockChecksum(buf []byte, blk uint64) {
	if !w.f.sb.metadataCsum() || len(buf) < extentHeaderLen || binary.LittleEndian.Uint16(buf) != extentMagic {
		return
	}
	tail := extentHeaderLen + int(binary.LittleEndian.Uint16(buf[4:]))*extentEntryLen
	if tail+4 > len(buf) {
		w.f.warn("extent block %d of inode %d: no room for the checksum tail (eh_max %d)", blk, w.in.num, binary.LittleEndian.Uint16(buf[4:]))
		return
	}
	stored := binary.LittleEndian.Uint32(buf[tail:])
	if want := rawCRC32C(w.f.inodeSeed(w.in), buf[:tail]); stored != want {
		w.f.warn("extent block %d of inode %d: checksum mismatch (stored %#08x, computed %#08x)", blk, w.in.num, stored, want)
	}
}

// node walks one extent node (buf holds the whole container) whose header must
// declare depth wantDepth; where is its byte offset for error reports.
func (w *extentWalker) node(buf []byte, wantDepth int, where int64) error {
	const st = "ext4 extent node"
	le := binary.LittleEndian
	if len(buf) < extentHeaderLen || le.Uint16(buf) != extentMagic {
		return corrupt(st, where, "bad extent header magic")
	}
	entries, maxEntries, depth := int(le.Uint16(buf[2:])), int(le.Uint16(buf[4:])), int(le.Uint16(buf[6:]))
	switch {
	case depth != wantDepth:
		return corrupt(st, where, "depth %d, want %d", depth, wantDepth)
	case maxEntries > (len(buf)-extentHeaderLen)/extentEntryLen:
		return corrupt(st, where, "capacity of %d entries does not fit the %d-byte node", maxEntries, len(buf))
	case entries > maxEntries:
		return corrupt(st, where, "%d entries exceed the capacity %d", entries, maxEntries)
	}
	bs := int64(w.f.sb.blockSize)
	var prevIdx uint32
	for i := range entries {
		if w.rl.full() {
			return nil
		}
		if w.entries++; w.entries > maxFileRuns {
			return corrupt(st, where, "more than %d extent entries", maxFileRuns)
		}
		e := buf[extentHeaderLen+i*extentEntryLen:][:extentEntryLen]
		if depth > 0 {
			lblk := le.Uint32(e)
			if i > 0 && lblk <= prevIdx {
				return corrupt(st, where, "index entry %d starts at block %d, not after %d", i, lblk, prevIdx)
			}
			prevIdx = lblk
			child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
			if _, dup := w.visited[child]; dup {
				return corrupt(st, where, "extent node block %d is referenced twice (cycle or shared node)", child)
			}
			if len(w.visited) >= maxFileRuns {
				return corrupt(st, where, "more than %d extent nodes", maxFileRuns)
			}
			w.visited[child] = struct{}{}
			cbuf, err := w.f.readNode(st, child)
			if err != nil {
				return err
			}
			w.checkBlockChecksum(cbuf, child)
			if err := w.node(cbuf, depth-1, int64(child)*bs); err != nil {
				return err
			}
			continue
		}

		lblk := uint64(le.Uint32(e))
		n := uint64(le.Uint16(e[4:]))
		uninit := n > extentMaxInit
		if uninit {
			n -= extentMaxInit
		}
		phys := uint64(le.Uint32(e[8:])) | uint64(le.Uint16(e[6:]))<<32
		switch {
		case n == 0:
			return corrupt(st, where, "extent %d has length 0", i)
		case lblk < w.next:
			return corrupt(st, where, "extent %d at block %d overlaps or precedes the previous extent (ends at %d)", i, lblk, w.next)
		case lblk+n > 1<<32:
			return corrupt(st, where, "extent %d (%d+%d) exceeds the 32-bit logical block range", i, lblk, n)
		case phys+n > uint64(w.f.sb.blocksCount):
			return corrupt(st, where, "extent %d maps blocks %d+%d beyond the %d-block filesystem", i, phys, n, w.f.sb.blocksCount)
		}
		w.f.checkDataPointer(w.in, phys, n)
		if lblk > w.next {
			if err := w.rl.addBlocks(-1, int64(lblk-w.next), bs); err != nil {
				return err
			}
		}
		pb := int64(phys)
		if uninit {
			pb = -1 // allocated but never written: reads as zeros
		}
		if err := w.rl.addBlocks(pb, int64(n), bs); err != nil {
			return err
		}
		w.next = lblk + n
	}
	return nil
}

// blockMapWalker walks the direct and indirect block pointers of an ext2/3
// style file in file order.
type blockMapWalker struct {
	f       *FS
	in      *inode
	rl      *runList
	nb      int64 // logical blocks needed to cover the size
	next    int64 // next logical block
	ppb     int64 // pointers per indirect block
	visited map[uint32]struct{}
}

// blockMapRuns collects the runs of a file whose i_block holds block pointers.
func (f *FS) blockMapRuns(in *inode, rl *runList) error {
	bs := int64(f.sb.blockSize)
	nb := in.size / bs
	if in.size%bs != 0 {
		nb++
	}
	w := &blockMapWalker{f: f, in: in, rl: rl, nb: nb, ppb: bs / 4, visited: map[uint32]struct{}{}}
	ptr := func(i int) uint32 { return binary.LittleEndian.Uint32(in.block[i*4:]) }
	for i := range 12 {
		if err := w.walk(ptr(i), 0, in.offset+iBlock); err != nil {
			return err
		}
	}
	for i := range 3 { // indirect, double indirect, triple indirect
		if err := w.walk(ptr(12+i), i+1, in.offset+iBlock); err != nil {
			return err
		}
	}
	return nil
}

// walk handles one pointer. level 0 is a data block; level n an indirect block
// whose pointers are of level n-1. A zero pointer is a hole covering the whole
// span it would map.
func (w *blockMapWalker) walk(ptr uint32, level int, where int64) error {
	const st = "ext4 block map"
	if w.next >= w.nb {
		return nil
	}
	bs := int64(w.f.sb.blockSize)
	if ptr == 0 {
		span := int64(1)
		for range level {
			span *= w.ppb // <= 16384^3: fits
		}
		span = min(span, w.nb-w.next)
		w.next += span
		return w.rl.addBlocks(-1, span, bs)
	}
	if uint64(ptr) >= uint64(w.f.sb.blocksCount) {
		return corrupt(st, where, "block pointer %d is beyond the %d-block filesystem", ptr, w.f.sb.blocksCount)
	}
	if level == 0 {
		w.f.checkDataPointer(w.in, uint64(ptr), 1)
		w.next++
		return w.rl.addBlocks(int64(ptr), 1, bs)
	}
	if _, dup := w.visited[ptr]; dup {
		return corrupt(st, where, "indirect block %d is referenced twice (cycle or shared block)", ptr)
	}
	if len(w.visited) >= maxFileRuns {
		return corrupt(st, where, "more than %d indirect blocks", maxFileRuns)
	}
	w.visited[ptr] = struct{}{}
	buf, err := w.f.readNode(st, uint64(ptr))
	if err != nil {
		return err
	}
	here := int64(ptr) * bs // cannot overflow: ptr < blocksCount, whose byte size fits
	for i := int64(0); i < w.ppb && w.next < w.nb; i++ {
		if err := w.walk(binary.LittleEndian.Uint32(buf[i*4:]), level-1, here); err != nil {
			return err
		}
	}
	return nil
}

// inlineData returns the content of an inline-data inode: the first 60 bytes
// from i_block, then the rest from the system.data extended attribute.
func (f *FS) inlineData(in *inode) ([]byte, error) {
	const st = "ext4 inline data"
	if in.size > maxInlineData {
		return nil, corrupt(st, in.offset, "inode %d: inline size %d exceeds %d", in.num, in.size, maxInlineData)
	}
	size := int(in.size)
	data := make([]byte, 0, size)
	data = append(data, in.block[:min(size, inodeBlockLen)]...)
	if size <= inodeBlockLen {
		return data, nil
	}
	xs, _ := f.xattrs(in) // a damaged table still yields the attributes before the damage
	for _, x := range xs {
		if x.Name != "system.data" {
			continue
		}
		if x.Inum != 0 {
			return nil, corrupt(st, in.offset, "inode %d: system.data lives in an EA inode", in.num)
		}
		if len(x.Value) < size-inodeBlockLen {
			return nil, corrupt(st, in.offset, "inode %d: size %d, but only %d bytes are stored inline", in.num, size, inodeBlockLen+len(x.Value))
		}
		return append(data, x.Value[:size-inodeBlockLen]...), nil
	}
	return nil, corrupt(st, in.offset, "inode %d: size %d needs a system.data attribute, which is missing or unreadable", in.num, size)
}

// parseInodeID parses "inode:<n>" strictly: canonical decimal, n >= 1.
func parseInodeID(id string) (uint32, error) {
	rest, ok := strings.CutPrefix(id, "inode:")
	if ok {
		if n, err := strconv.ParseUint(rest, 10, 32); err == nil && n >= 1 && strconv.FormatUint(n, 10) == rest {
			return uint32(n), nil
		}
	}
	return 0, fmt.Errorf("%w: %q is not an ext4 entry id", filesys.ErrNotFound, id)
}

// Open opens a regular file or symlink. The content is the raw on-disk bytes:
// an encrypted file yields ciphertext. A deleted entry yields
// filesys.ErrDeleted; directories and special files filesys.ErrUnsupported.
// Inline data and fast symlink targets are kept in metadata, so their Runs are
// nil.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	if e.Deleted {
		return nil, filesys.ErrDeleted
	}
	n, err := parseInodeID(e.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(n)
	if err != nil {
		return nil, err
	}
	return f.openInode(in)
}

// openInode opens the content of a regular file or symlink inode.
func (f *FS) openInode(in *inode) (filesys.File, error) {
	n := in.num
	switch in.mode & modeTypeMask {
	case modeReg, modeSymlink:
	case modeDir:
		return nil, fmt.Errorf("%w: inode %d is a directory", filesys.ErrUnsupported, n)
	default:
		return nil, fmt.Errorf("%w: inode %d is neither a regular file nor a symlink", filesys.ErrUnsupported, n)
	}

	switch {
	case in.flags&inodeFlagInlineData != 0:
		data, err := f.inlineData(in)
		if err != nil {
			return nil, err
		}
		return &file{size: in.size, mem: data}, nil
	case in.mode&modeTypeMask == modeSymlink && in.flags&inodeFlagExtents == 0 && in.size < fastSymlinkMax:
		return &file{size: in.size, mem: slices.Clone(in.block[:in.size])}, nil
	}
	runs, err := f.runs(in)
	if err != nil {
		return nil, err
	}
	fl := &file{size: in.size, runs: runs, r: f.data}
	var pos int64
	for _, r := range runs {
		fl.starts = append(fl.starts, pos)
		pos += r.Length
	}
	return fl, nil
}

// file is the content of one inode. It is immutable, so concurrent ReadAt
// calls are safe.
type file struct {
	size   int64
	mem    []byte        // inline content (runs is nil), else nil
	runs   []filesys.Run // covers [0, size)
	starts []int64       // starts[i] is the file offset where runs[i] begins
	r      io.ReaderAt   // the filesystem, for non-hole runs
}

// Size is the content length.
func (fl *file) Size() int64 { return fl.size }

// Runs returns a copy of the filesystem-relative runs (nil for inline content).
func (fl *file) Runs() []filesys.Run { return slices.Clone(fl.runs) }

// ReadAt reads file content; holes and uninitialized extents read as zeros.
func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ext4: negative read offset")
	}
	if off >= fl.size || len(p) == 0 {
		if off >= fl.size {
			return 0, io.EOF
		}
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
		i := sort.Search(len(fl.starts), func(i int) bool { return fl.starts[i] > pos }) - 1
		if i < 0 {
			return int(n), io.ErrUnexpectedEOF // unreachable: starts[0] is 0
		}
		r, within := fl.runs[i], pos-fl.starts[i]
		chunk := min(want-n, r.Length-within)
		if r.Offset < 0 {
			clear(p[n : n+chunk])
		} else if err := readFull(fl.r, p[n:n+chunk], r.Offset+within); err != nil {
			return int(n), err
		}
		n += chunk
	}
	if eof {
		return int(n), io.EOF
	}
	return int(n), nil
}
