package f2fstest

import (
	"slices"
	"strings"
)

// Tree reports where BuildTree put things, so a test can patch the image or
// compare what a reader returns.
type Tree struct {
	NID       map[string]uint32   // path -> inode number ("/" is RootIno)
	Addr      map[uint32]uint32   // node id -> block address of its node block
	DirBlocks map[string][]uint32 // directory path -> addresses of its dentry blocks (none for inline directories)
}

// Inode flag bits of i_flags used by the tree builder.
const (
	flagCompr   = 0x4
	flagEncrypt = 0x800
	flagCasefld = 0x40000000
)

type tnode struct {
	f      File
	name   []byte
	nid    uint32
	parent *tnode
	kids   []*tnode
	path   string
}

func (n *tnode) isDir() bool { return n.f.Dir || n.f.Mode&0o170000 == 0o040000 }

// BuildTree lays files out in the image and returns it with a Tree of where
// everything went. The root directory (nid RootIno) is implicit; a File with
// Path "/" configures it. Missing parent directories are created. Inode
// numbers are handed out from RootIno+1 in order of first appearance, then the
// node and data blocks follow. A directory's entries are packed in the order the
// files are listed, after "." and "..", into as many dentry blocks as they
// need (an entry never spans two blocks); a directory with Inline is stored in
// its inode, which panics when its entries do not fit.
func BuildTree(o Options, files []File) ([]byte, *Tree) {
	t := &Tree{NID: map[string]uint32{"/": RootIno}, Addr: map[uint32]uint32{}, DirBlocks: map[string][]uint32{}}
	if len(files) == 0 {
		return build(o), t
	}
	next := uint32(RootIno + 1)
	root := &tnode{f: File{Path: "/", Dir: true}, nid: RootIno, path: "/"}
	byPath := map[string]*tnode{"/": root}
	order := []*tnode{root}
	var ensure func(p string) *tnode
	ensure = func(p string) *tnode {
		if n, ok := byPath[p]; ok {
			if !n.isDir() {
				panic("f2fstest: " + p + " is not a directory")
			}
			return n
		}
		return add(p, File{Path: p, Dir: true, Mode: 0o755}, byPath, &order, &next, ensure)
	}
	for _, f := range files {
		p := cleanPath(f.Path)
		if p == "/" {
			root.f = f
			root.f.Dir = true
			continue
		}
		if _, dup := byPath[p]; dup {
			panic("f2fstest: duplicate path " + p)
		}
		add(p, f, byPath, &order, &next, ensure)
	}

	a := NewAlloc(o, next)
	var nodes []Node
	var data []DataBlock
	for _, n := range order {
		t.NID[n.path] = n.nid
		if n.f.NoInode {
			if len(n.kids) != 0 {
				panic("f2fstest: NoInode with children")
			}
			continue
		}
		ns, ds := emit(o, a, n, t)
		nodes = append(nodes, ns...)
		data = append(data, ds...)
	}
	for _, n := range nodes {
		t.Addr[n.NID] = n.Addr
	}
	o.Nodes = append(slices.Clone(o.Nodes), nodes...)
	o.Data = append(slices.Clone(o.Data), data...)
	return build(o), t
}

func cleanPath(p string) string {
	var parts []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return "/" + strings.Join(parts, "/")
}

func add(p string, f File, byPath map[string]*tnode, order *[]*tnode, next *uint32, ensure func(string) *tnode) *tnode {
	i := strings.LastIndexByte(p, '/')
	parent := ensure(cleanPath(p[:i]))
	n := &tnode{f: f, name: []byte(p[i+1:]), parent: parent, nid: *next, path: p}
	*next++
	if f.RawName != nil {
		n.name = f.RawName
	}
	byPath[p] = n
	*order = append(*order, n)
	parent.kids = append(parent.kids, n)
	return n
}

func modeOf(n *tnode) uint16 {
	m := n.f.Mode
	if m&0o170000 != 0 {
		return uint16(m)
	}
	switch {
	case n.isDir():
		if m == 0 {
			m = 0o755
		}
		return uint16(0o040000 | m)
	case n.f.Symlink != "":
		if m == 0 {
			m = 0o777
		}
		return uint16(0o120000 | m)
	}
	if m == 0 {
		m = 0o644
	}
	return uint16(0o100000 | m)
}

func ftypeOf(n *tnode) uint8 {
	switch modeOf(n) & 0o170000 {
	case 0o100000:
		return FTReg
	case 0o040000:
		return FTDir
	case 0o020000:
		return FTChr
	case 0o060000:
		return FTBlk
	case 0o010000:
		return FTFifo
	case 0o140000:
		return FTSock
	case 0o120000:
		return FTSymlink
	}
	return FTUnknown
}

func blocksOf(content []byte) map[int64][]byte {
	m := map[int64][]byte{}
	for i := 0; i*BlockSize < len(content); i++ {
		m[int64(i)] = content[i*BlockSize : min((i+1)*BlockSize, len(content))]
	}
	return m
}

// emit lays out one inode with its data and returns the node blocks (inode
// first) and data blocks.
func emit(o Options, a *Alloc, n *tnode, t *Tree) ([]Node, []DataBlock) {
	f := n.f
	pino := RootIno
	if n.parent != nil {
		pino = int(n.parent.nid)
	}
	in := Inode{
		NID: n.nid, Mode: modeOf(n), UID: f.UID, GID: f.GID, Links: 1,
		Atime: uint64(f.Times[0]), Ctime: uint64(f.Times[1]), Mtime: uint64(f.Times[2]),
		PIno: uint32(pino), Name: string(n.name),
		Extra: o.ExtraAttr,
	}
	if o.InodeCrtime {
		in.Crtime = uint64(f.Times[3])
	}
	if f.Encrypted {
		in.Flags |= flagEncrypt
	}
	if f.Casefold {
		in.Flags |= flagCasefld
	}
	extra := 0
	if in.Extra {
		extra = 36
	}
	if n.isDir() {
		in.Links = 2
		in.CurDepth = 1
		var ds []Dentry
		for _, k := range n.kids {
			if k.f.Dir {
				in.Links++
			}
			ino := k.nid
			ds = append(ds, Dentry{Name: k.name, Ino: ino, Type: ftypeOf(k), Deleted: k.f.Deleted})
		}
		parent := uint32(pino)
		if n.nid == RootIno {
			parent = RootIno
		}
		if f.Inline {
			in.Dentries = append([]Dentry{{Name: []byte("."), Ino: n.nid, Type: FTDir}, {Name: []byte(".."), Ino: parent, Type: FTDir}}, ds...)
			xw := 50
			if o.InlineXattr && o.ExtraAttr {
				xw = int(f.InlineXattrSize)
				in.InlineXattrSize = f.InlineXattrSize
			}
			in.Size = uint64(max(923-extra/4-xw-1, 0) * 4)
			return []Node{{NID: n.nid, Addr: a.Addr(), Block: InodeBlock(o, in)}}, nil
		}
		blocks := DentryBlocks(true, n.nid, parent, ds)
		fd := FileData{Blocks: map[int64][]byte{}}
		for i, b := range blocks {
			fd.Blocks[int64(i)] = b
		}
		in.Size = uint64(len(blocks)) * BlockSize
		ns, dbs := a.File(o, in, fd)
		for _, d := range dbs {
			t.DirBlocks[n.path] = append(t.DirBlocks[n.path], d.Addr)
		}
		return ns, dbs
	}

	content := f.Data
	if f.Symlink != "" {
		content = []byte(f.Symlink)
	}
	if f.Compressed {
		in.Flags |= flagCompr
	}
	in.Size = uint64(len(content))
	if f.Inline && len(content) <= (923-extra/4-1)*4 {
		in.InlineData = content
		return []Node{{NID: n.nid, Addr: a.Addr(), Block: InodeBlock(o, in)}}, nil
	}
	if len(content) == 0 {
		return []Node{{NID: n.nid, Addr: a.Addr(), Block: InodeBlock(o, in)}}, nil
	}
	return a.File(o, in, FileData{Blocks: blocksOf(content)})
}
