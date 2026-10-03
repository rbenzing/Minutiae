package ext4test

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Directory entry file types and flags.
const (
	ftReg     = 1
	ftDir     = 2
	ftSymlink = 7
	ftTail    = 0xDE // file_type of the metadata_csum tail pseudo-entry

	flagIndex   = 0x1000 // EXT4_INDEX_FL: an htree directory
	flagEncrypt = 0x800  // EXT4_ENCRYPT_FL

	direntHeader = 8
	tailLen      = 12 // the metadata_csum tail of a directory block
	dxRootInfo   = 24 // offset of dx_root_info in an htree root block
	dxEntries    = 32 // offset of the dx countlimit in an htree root block
)

// dirChild is one name to put in a directory.
type dirChild struct {
	name     []byte
	inode    uint32
	ftype    byte
	deleted  bool
	newBlock bool
}

// rawEnt is a directory record under construction.
type rawEnt struct {
	dirChild
	recLen int
}

func (e *rawEnt) minLen() int { return (direntHeader + len(e.name) + 3) &^ 3 }

// packRegions lays the entries out in consecutive regions of n usable bytes: a
// region is one directory block (minus its checksum tail) or one part of an
// inline directory. Entries are packed back to back, the last record of a
// region extends to its end, and an entry with newBlock set starts a new
// region. Deleted entries are then unlinked the way the kernel does it: the
// record stays in place, and its rec_len is added to the previous record that
// is still linked (that record's slack now covers it); a deleted record with no
// linked predecessor stays in the chain with its inode zeroed (the name and
// rec_len are kept).
func packRegions(ents []dirChild, n int) [][]byte {
	var regions [][]rawEnt
	var cur []rawEnt
	used := 0
	flush := func() {
		if len(cur) > 0 {
			regions = append(regions, cur)
		}
		cur, used = nil, 0
	}
	for _, c := range ents {
		e := rawEnt{dirChild: c}
		sz := e.minLen()
		if len(c.name) == 0 || len(c.name) > 255 || sz > n {
			panic(fmt.Sprintf("ext4test: cannot place a %d-byte name in a %d-byte directory region", len(c.name), n))
		}
		if (c.newBlock && len(cur) > 0) || used+sz > n {
			flush()
		}
		e.recLen = sz
		cur = append(cur, e)
		used += sz
	}
	flush()

	out := make([][]byte, len(regions))
	for i, r := range regions {
		r[len(r)-1].recLen += n - used0(r)
		buf := make([]byte, n)
		off := 0
		offs := make([]int, len(r))
		for k, e := range r {
			offs[k] = off
			putDirent(buf[off:], e.inode, e.recLen, e.name, e.ftype)
			off += e.recLen
		}
		var chain []int
		for k, e := range r {
			switch {
			case !e.deleted:
				chain = append(chain, k)
			case len(chain) == 0:
				le32(buf, offs[k], 0) // first record: its inode number is lost
				chain = append(chain, k)
			default:
				last := chain[len(chain)-1]
				r[last].recLen += e.recLen
				le16(buf, offs[last]+4, uint16(r[last].recLen))
			}
		}
		out[i] = buf
	}
	return out
}

// used0 is the sum of the minimal lengths of the records in r.
func used0(r []rawEnt) int {
	n := 0
	for i := range r {
		n += r[i].minLen()
	}
	return n
}

func putDirent(b []byte, inode uint32, recLen int, name []byte, ftype byte) {
	le32(b, 0, inode)
	le16(b, 4, uint16(recLen))
	b[6], b[7] = byte(len(name)), ftype
	copy(b[direntHeader:], name)
}

// dirBlockCsum is the kernel's ext4_dirblock_csum: crc32c over the block up to
// its tail, seeded with the filesystem seed folded with the inode number and
// i_generation.
func (b *builder) dirBlockCsum(num, gen uint32, block []byte) uint32 {
	var w [4]byte
	binary.LittleEndian.PutUint32(w[:], num)
	c := rawCRC32C(b.seed, w[:])
	binary.LittleEndian.PutUint32(w[:], gen)
	c = rawCRC32C(c, w[:])
	return rawCRC32C(c, block)
}

// leafBlocks renders regions as full directory blocks, with the metadata_csum
// tail when the feature is on.
func (b *builder) leafBlocks(num, gen uint32, ents []dirChild) []byte {
	reserve := 0
	if b.o.MetadataCsum {
		reserve = tailLen
	}
	var out []byte
	for _, region := range packRegions(ents, b.bs-reserve) {
		blk := make([]byte, b.bs)
		copy(blk, region)
		if reserve > 0 {
			le16(blk, b.bs-reserve+4, tailLen)
			blk[b.bs-reserve+7] = ftTail
			le32(blk, b.bs-4, b.dirBlockCsum(num, gen, blk[:b.bs-tailLen]))
		}
		out = append(out, blk...)
	}
	return out
}

// dirData renders the content of directory f (inode num, parent inode parent)
// holding kids: whole blocks, or for an inline directory the 4-byte parent
// number, the 56 bytes that fit in i_block and the rest for system.data.
func (b *builder) dirData(num, parent uint32, f *File, kids []dirChild) []byte {
	if f.Inline {
		return b.inlineDirData(parent, kids)
	}
	dot := []dirChild{
		{name: []byte("."), inode: num, ftype: ftDir},
		{name: []byte(".."), inode: parent, ftype: ftDir},
	}
	if !f.HTree {
		return b.leafBlocks(num, f.Generation, append(dot, kids...))
	}

	// An htree directory: block 0 is the dx root (".", ".." and the index), the
	// others are leaves. The index is one level deep. The hash values are
	// placeholders (increasing, not real half-MD4 hashes): a reader that scans
	// the leaves linearly never looks at them.
	leaves := b.leafBlocks(num, f.Generation, kids)
	nleaves := len(leaves) / b.bs
	if nleaves == 0 {
		panic("ext4test: an htree directory needs at least one entry")
	}
	limit := (b.bs - dxEntries) / 8
	if b.o.MetadataCsum {
		limit-- // room for the dx_tail
	}
	if nleaves > limit {
		panic(fmt.Sprintf("ext4test: %d htree leaves exceed the root's capacity of %d", nleaves, limit))
	}
	root := make([]byte, b.bs)
	putDirent(root[0:], num, 12, []byte("."), ftDir)
	putDirent(root[12:], parent, b.bs-12, []byte(".."), ftDir)
	root[dxRootInfo+4] = 1 // hash_version: half_md4
	root[dxRootInfo+5] = 8 // info_length
	le16(root, dxEntries, uint16(limit))
	le16(root, dxEntries+2, uint16(nleaves))
	le32(root, dxEntries+4, 1) // block of the first leaf
	for i := 1; i < nleaves; i++ {
		le32(root, dxEntries+8*i, uint32(i)<<24) // placeholder hash
		le32(root, dxEntries+8*i+4, uint32(1+i))
	}
	// The dx_tail checksum (metadata_csum) is left zero: readers skip dx blocks.
	return append(root, leaves...)
}

// inlineDirData lays out an inline directory. Region one is the 56 bytes of
// i_block after the parent inode, region two the system.data value; each is
// filled by whole records and neither is shared by a record.
func (b *builder) inlineDirData(parent uint32, kids []dirChild) []byte {
	if !b.o.InlineData {
		panic("ext4test: an inline directory needs Options.InlineData")
	}
	const first = 56
	data := make([]byte, 60)
	le32(data, 0, parent)

	// Fill region one greedily, then put the remainder in region two.
	var one, two []dirChild
	used := 0
	for _, c := range kids {
		if c.newBlock {
			panic("ext4test: NewBlock does not apply to an inline directory")
		}
	}
	for _, c := range kids {
		sz := (&rawEnt{dirChild: c}).minLen()
		if len(two) == 0 && used+sz <= first {
			one = append(one, c)
			used += sz
			continue
		}
		two = append(two, c)
	}
	if len(one) == 0 {
		// Nothing in region one: it is a single empty record spanning it.
		putDirent(data[4:], 0, first, nil, 0)
	} else {
		copy(data[4:], packRegions(one, first)[0])
	}
	if len(two) > 0 {
		total := 0
		for _, c := range two {
			total += (&rawEnt{dirChild: c}).minLen()
		}
		for _, r := range packRegions(two, total) {
			data = append(data, r...)
		}
	}
	return data
}

// withParents returns files followed by an implicit directory for every
// ancestor path that is not listed, so inode numbers of the listed files do not
// move (see InodeNumber); the implicit ones are numbered after them.
func withParents(files []File) []File {
	all := append([]File(nil), files...)
	have := map[string]bool{"": true}
	for i := range files {
		p := files[i].Path
		if !strings.HasPrefix(p, "/") || len(p) < 2 || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
			panic(fmt.Sprintf("ext4test: bad path %q", p))
		}
		if files[i].Dir {
			have[p] = true
		}
	}
	for i := range files {
		p := files[i].Path
		for k := 1; k < len(p); k++ {
			if p[k] != '/' {
				continue
			}
			if anc := p[:k]; !have[anc] {
				have[anc] = true
				all = append(all, File{Path: anc, Dir: true})
			}
		}
	}
	return all
}

// splitPath returns the parent path ("" for the root) and the last component.
func splitPath(p string) (parent string, name string) {
	k := strings.LastIndexByte(p, '/')
	return p[:k], p[k+1:]
}

// childType is the dirent file_type of f.
func childType(f *File) byte {
	switch {
	case f.Dir:
		return ftDir
	case f.Symlink != "":
		return ftSymlink
	}
	return ftReg
}
