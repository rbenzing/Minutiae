package ext4test

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
)

// Xattr is one extended attribute, with its full name ("user.comment").
type Xattr struct {
	Name  string
	Value []byte
}

// Inode flags.
const (
	flagExtents    = 0x80000
	flagInlineData = 0x10000000
)

const (
	modeReg     = 0x8000
	modeDir     = 0x4000
	modeSymlink = 0xA000

	rootInode = 2

	inodeHeader   = 128
	xattrMagic    = 0xEA020000
	xattrHdrSize  = 32 // block header
	xattrEntryLen = 16 // fixed part of an entry

	extentMagic = 0xF30A
)

// xattrPrefixes maps name prefixes to e_name_index. Longer, more specific
// prefixes come first so "system.posix_acl_access" wins over "system.".
var xattrPrefixes = []struct {
	prefix string
	index  byte
}{
	{"system.posix_acl_access", 2},
	{"system.posix_acl_default", 3},
	{"system.richacl", 8},
	{"user.", 1},
	{"trusted.", 4},
	{"security.", 6},
	{"system.", 7},
	{"encryption.", 9},
}

// InodeNumber returns the inode number the builder gives files[i]: the
// explicitly listed files are numbered consecutively from 11 in slice order
// (inode 2 is the root directory; 1 and 3..10 are reserved and left zero).
// Directories that a path needs but the list does not hold get the numbers
// after the listed files.
func InodeNumber(i int) uint32 { return uint32(firstUserInode + i) }

// placeFiles writes the inodes, the file data and the directories. Everything
// that is not a directory is placed first, in slice order, so file data keeps
// its layout whatever the directory tree looks like; the directory blocks come
// after, then the root's.
func (b *builder) placeFiles(files []File) {
	all := withParents(files)
	nums := map[string]uint32{"": rootInode}
	kids := map[string][]dirChild{}
	for i := range all {
		num := InodeNumber(i)
		g := int(num-1) / b.ipg
		if g >= len(b.groups) {
			panic(fmt.Sprintf("ext4test: %d files do not fit in %d groups of %d inodes", len(all), len(b.groups), b.ipg))
		}
		b.usedInodes[g] = max(b.usedInodes[g], int(num-1)%b.ipg+1)
		if all[i].Dir {
			nums[all[i].Path] = num
		}
	}
	for i := range all {
		parent, name := splitPath(all[i].Path)
		kids[parent] = append(kids[parent], dirChild{
			name: []byte(name), inode: InodeNumber(i), ftype: childType(&all[i]),
			deleted: all[i].Deleted, newBlock: all[i].NewBlock,
		})
	}
	for i := range all {
		if !all[i].Dir {
			b.writeInode(InodeNumber(i), b.renderInode(InodeNumber(i), &all[i]))
		}
	}
	dir := func(num uint32, f File, parentPath string) {
		f.Data = b.dirData(num, nums[parentPath], &f, kids[f.Path])
		b.writeInode(num, b.renderInode(num, &f))
	}
	for i := range all {
		if all[i].Dir {
			parent, _ := splitPath(all[i].Path)
			dir(InodeNumber(i), all[i], parent)
		}
	}
	dir(rootInode, File{Dir: true}, "")
}

// writeInode stores raw as inode num in its group's inode table.
func (b *builder) writeInode(num uint32, raw []byte) {
	g := int(num-1) / b.ipg
	idx := int(num-1) % b.ipg
	copy(b.img[b.groups[g].itable*b.bs+idx*b.isz:], raw)
}

// alloc reserves n contiguous free blocks inside one group's data area and
// returns the first. Blocks are zero (the image starts zeroed).
func (b *builder) alloc(n int) int {
	for _, gr := range b.groups {
		run := 0
		for blk := gr.data; blk < gr.start+b.bpg; blk++ {
			if b.used[blk] {
				run = 0
				continue
			}
			run++
			if run == n {
				first := blk - n + 1
				for k := first; k <= blk; k++ {
					b.used[k] = true
				}
				return first
			}
		}
	}
	panic(fmt.Sprintf("ext4test: no room for %d contiguous blocks", n))
}

// renderInode encodes f as an inode of b.isz bytes, checksum included.
func (b *builder) renderInode(num uint32, f *File) []byte {
	raw := make([]byte, b.isz)
	mode := f.Mode & 0o7777
	switch {
	case f.Dir:
		if f.Mode == 0 {
			mode = 0o755
		}
		mode |= modeDir
	case f.Symlink != "":
		if f.Mode == 0 {
			mode = 0o777
		}
		mode |= modeSymlink
	default:
		if f.Mode == 0 {
			mode = 0o644
		}
		mode |= modeReg
	}
	links := uint16(1)
	if f.Dir {
		links = 2
	}
	var dtime uint32
	if f.Deleted {
		links, dtime = 0, fixedTime
	}
	pieces, isize := b.content(f)
	size := uint64(isize)

	le16(raw, 0x0, uint16(mode))
	le16(raw, 0x2, uint16(f.UID))
	le32(raw, 0x4, uint32(size))
	le32(raw, 0x14, dtime)
	le16(raw, 0x18, uint16(f.GID))
	le16(raw, 0x1A, links)
	le32(raw, 0x6C, uint32(size>>32))
	le16(raw, 0x78, uint16(f.UID>>16))
	le16(raw, 0x7A, uint16(f.GID>>16))
	le32(raw, 0x64, f.Generation)

	var flags uint32
	xattrs := f.Xattrs
	switch {
	case f.Inline:
		if !b.o.InlineData {
			panic("ext4test: Inline needs Options.InlineData")
		}
		flags |= flagInlineData
		n := copy(raw[0x28:0x28+60], f.Data)
		if rest := f.Data[n:]; len(rest) > 0 || b.isz > inodeHeader {
			// The remainder lives in the system.data attribute (always present on
			// real inline inodes, empty when everything fits in i_block).
			xattrs = append([]Xattr{{Name: "system.data", Value: rest}}, xattrs...)
		}
	case f.Symlink != "" && len(f.Symlink) < 60:
		copy(raw[0x28:0x28+60], f.Symlink) // fast symlink
	case b.o.Extents:
		flags |= flagExtents
		b.putExtents(raw[0x28:0x28+60], num, f, pieces)
	default:
		b.putBlockMap(raw[0x28:0x28+60], f, pieces)
	}
	if f.HTree {
		flags |= flagIndex
	}
	if b.o.Encrypt && (f.Path == "/enc" || strings.HasPrefix(f.Path, "/enc/")) {
		flags |= flagEncrypt
	}
	flags |= f.Flags
	le32(raw, 0x20, flags)

	extra := 0
	if b.isz > inodeHeader {
		extra = orDefault(f.ExtraIsize, 32)
		if extra < 4 || extra%4 != 0 || inodeHeader+extra > b.isz {
			panic(fmt.Sprintf("ext4test: extra_isize %d in a %d-byte inode", extra, b.isz))
		}
		le16(raw, 0x80, uint16(extra))
	}
	b.putTimes(raw, extra, f)

	if len(xattrs) > 0 {
		if f.XattrBlock {
			blk := b.xattrBlock(xattrs)
			le32(raw, 0x68, uint32(blk))
			if b.o.Bit64 {
				le16(raw, 0x76, uint16(uint64(blk)>>32))
			}
		} else {
			if extra == 0 {
				panic("ext4test: in-inode xattrs need an inode larger than 128 bytes")
			}
			b.putInodeXattrs(raw[inodeHeader+extra:], xattrs)
		}
	}

	if b.o.MetadataCsum {
		b.putInodeChecksum(raw, num, extra)
	}
	return raw
}

// putTimes encodes the four timestamps. Seconds go in the 32-bit field read as
// signed; what does not fit goes in the two epoch bits of the extra field
// (kernel: ext4_encode_extra_time), with nanoseconds in the upper 30 bits.
// Fields beyond i_extra_isize are not written.
func (b *builder) putTimes(raw []byte, extra int, f *File) {
	type field struct{ lo, ex, end int } // lo offset, extra offset, end of the extra field
	fields := [4]field{
		{0x8, 0x8C, 0x90},  // atime
		{0xC, 0x84, 0x88},  // ctime
		{0x10, 0x88, 0x8C}, // mtime
		{0x90, 0x94, 0x98}, // crtime (no 128-byte form)
	}
	for i, fl := range fields {
		sec, nsec := f.Times[i], f.Nsec[i]
		if sec == 0 && nsec == 0 {
			continue
		}
		if sec < -(1<<31) || sec >= 3<<32+1<<31 {
			panic(fmt.Sprintf("ext4test: time %d is outside the encodable range", sec))
		}
		if nsec > 999999999 {
			panic(fmt.Sprintf("ext4test: nanoseconds %d", nsec))
		}
		haveLo := i != 3 || inodeHeader+extra >= fl.lo+4
		if haveLo {
			le32(raw, fl.lo, uint32(sec))
		}
		if inodeHeader+extra >= fl.end {
			epoch := uint32((sec-int64(int32(sec)))>>32) & 3
			le32(raw, fl.ex, epoch|nsec<<2)
		}
	}
}

// putInodeChecksum stores the metadata_csum inode checksum, following the
// kernel's ext4_inode_csum: crc32c, seeded with the filesystem checksum seed,
// over the le32 inode number, the le32 generation (i_generation, 0x64) and the
// whole inode with i_checksum_lo (0x7C) and, when i_extra_isize reaches it,
// i_checksum_hi (0x82) read as zero. The low 16 bits go to i_checksum_lo, the
// high 16 bits to i_checksum_hi (only when that field exists).
func (b *builder) putInodeChecksum(raw []byte, num uint32, extra int) {
	hasHi := extra >= 4 // 0x82 + 2 <= 128 + extra
	cp := slices.Clone(raw)
	cp[0x7C], cp[0x7D] = 0, 0
	if hasHi {
		cp[0x82], cp[0x83] = 0, 0
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:], num)
	copy(hdr[4:], raw[0x64:0x68])
	c := rawCRC32C(rawCRC32C(b.seed, hdr[:]), cp)
	le16(raw, 0x7C, uint16(c))
	if hasHi {
		le16(raw, 0x82, uint16(c>>16))
	}
}

// xattrEntry encodes one entry header and name; valueOff is relative to the
// value base of the container.
func xattrEntry(x Xattr, valueOff int) []byte {
	var idx byte
	name := x.Name
	for _, p := range xattrPrefixes {
		if strings.HasPrefix(x.Name, p.prefix) {
			idx, name = p.index, strings.TrimPrefix(x.Name, p.prefix)
			break
		}
	}
	if idx == 0 || len(name) > 255 {
		panic(fmt.Sprintf("ext4test: cannot encode xattr name %q", x.Name))
	}
	e := make([]byte, (xattrEntryLen+len(name)+3)&^3)
	e[0], e[1] = byte(len(name)), idx
	le16(e, 2, uint16(valueOff))
	le32(e, 8, uint32(len(x.Value)))
	// e_hash stays 0: the reader does not verify it.
	copy(e[xattrEntryLen:], name)
	return e
}

// encodeXattrs lays entries out from entriesOff and values downward from the
// end of area (value offsets are relative to valueBase). It panics when they
// collide.
func encodeXattrs(area []byte, entriesOff, valueBase int, xs []Xattr) {
	pos, end := entriesOff, len(area)
	for _, x := range xs {
		vlen := (len(x.Value) + 3) &^ 3
		end -= vlen
		e := xattrEntry(x, end-valueBase)
		if pos+len(e)+4 > end { // keep 4 zero bytes as the terminator
			panic("ext4test: xattrs do not fit")
		}
		copy(area[pos:], e)
		copy(area[end:], x.Value)
		pos += len(e)
	}
}

// putInodeXattrs writes the in-inode xattr area: the magic, then entries whose
// value offsets are relative to the first entry (right after the magic).
func (b *builder) putInodeXattrs(area []byte, xs []Xattr) {
	if len(area) < 8 {
		panic("ext4test: no room for in-inode xattrs")
	}
	le32(area, 0, xattrMagic)
	encodeXattrs(area[4:], 0, 0, xs)
}

// xattrBlock allocates and fills an xattr block and returns its number.
func (b *builder) xattrBlock(xs []Xattr) int {
	blk := b.alloc(1)
	buf := b.img[blk*b.bs : (blk+1)*b.bs]
	le32(buf, 0x0, xattrMagic)
	le32(buf, 0x4, 1) // refcount
	le32(buf, 0x8, 1) // blocks
	encodeXattrs(buf, xattrHdrSize, 0, xs)
	if b.o.MetadataCsum {
		// Kernel ext4_xattr_block_csum: crc32c(seed, le64 block) over the block
		// with h_checksum (0x10) zero.
		var nr [8]byte
		binary.LittleEndian.PutUint64(nr[:], uint64(blk))
		le32(buf, 0x10, rawCRC32C(rawCRC32C(b.seed, nr[:]), buf))
	}
	return blk
}
