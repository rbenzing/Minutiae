package apfstest

import (
	"sort"
	"unicode/utf8"
)

// File-system record types and constants: the builder's own copy.
const (
	idMask = 0x0fffffffffffffff

	TypeInode       = 3
	TypeXattr       = 4
	TypeSiblingLink = 5
	TypeFileExtent  = 8
	TypeDrec        = 9
	TypeSiblingMap  = 12

	rootParentID = 1
	rootIno      = 2
	privDirIno   = 3
	firstUserIno = 16

	modeDir     = 0o040000
	modeReg     = 0o100000
	modeSymlink = 0o120000
	modeTypeMsk = 0o170000

	bsdCompressed        = 0x20
	inodeHasUncompressed = 0x40000

	xfName    = 4
	xfDstream = 8

	symlinkXattr = "com.apple.fs.symlink"
	xattrStream  = 1
	xattrInline  = 2
)

// FSRecord is one record of a volume's file-system tree: the key is the 8-byte
// header (ID and Type) followed by Key.
type FSRecord struct {
	ID   uint64
	Type uint8
	Key  []byte
	Val  []byte
}

type bInode struct {
	ino, parent, private uint64
	name                 []byte
	mode                 uint32
	uid, gid             uint32
	times                [4]uint64 // create, modify, change, access
	links                int32
	prot, bsd            uint32
	internal             uint64
	uncompressed         uint64
	size                 int64
	hasDstream           bool
	crypto               uint64
	noRecord             bool
	linkOverride         int32
}

func (in *bInode) isDir() bool { return in.mode&modeTypeMsk == modeDir }

// crc32c is the CRC-32C (Castagnoli) with the given initial value and no final
// complement, computed bit by bit.
func crc32c(init uint32, b []byte) uint32 {
	c := init
	for _, x := range b {
		c ^= uint32(x)
		for range 8 {
			if c&1 != 0 {
				c = c>>1 ^ 0x82F63B78
			} else {
				c >>= 1
			}
		}
	}
	return c
}

// NameHash is the 22-bit directory-record name hash: UTF-32 little-endian code
// points of the name (no terminating NUL), CRC-32C with initial value
// 0xFFFFFFFF and no final complement, low 22 bits. Names are not decomposed
// (the builder's tests use ASCII); fold lower-cases ASCII for case-insensitive
// volumes.
func NameHash(name []byte, fold bool) uint32 {
	var u []byte
	for len(name) > 0 {
		r, n := utf8.DecodeRune(name)
		if n == 1 && r == utf8.RuneError {
			r = rune(name[0])
		}
		name = name[n:]
		if fold && r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		u = append(u, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
	}
	return crc32c(0xFFFFFFFF, u) & 0x3fffff
}

// DrecKey returns the bytes after the 8-byte header of a directory-record key
// for name (without NUL): hashed form name_len_and_hash + name, or plain form
// name_len + name; the length includes the terminating NUL. hash is stored as
// given in the hashed form.
func DrecKey(name []byte, hashed bool, hash uint32) []byte {
	nl := len(name) + 1
	if hashed {
		k := make([]byte, 4+nl)
		le.PutUint32(k, uint32(nl)&0x3ff|hash<<10)
		copy(k[4:], name)
		return k
	}
	k := make([]byte, 2+nl)
	le.PutUint16(k, uint16(nl))
	copy(k[2:], name)
	return k
}

// DrecVal encodes a directory-record value: file_id, date_added, flags.
func DrecVal(fileID uint64, typ uint16) []byte {
	v := make([]byte, 18)
	le.PutUint64(v, fileID)
	le.PutUint64(v[8:], DefaultTime)
	le.PutUint16(v[16:], typ)
	return v
}

// XattrKey returns the key bytes after the header of an xattr record.
func XattrKey(name string) []byte {
	k := make([]byte, 2+len(name)+1)
	le.PutUint16(k, uint16(len(name)+1))
	copy(k[2:], name)
	return k
}

// XattrVal encodes an xattr value: inline data, or a stream descriptor.
func XattrVal(data []byte, stream bool) []byte {
	if stream {
		v := make([]byte, 4+48)
		le.PutUint16(v, xattrStream)
		le.PutUint16(v[2:], 48)
		le.PutUint64(v[4:], 0x1234) // xattr_obj_id
		le.PutUint64(v[12:], uint64(len(data)))
		return v
	}
	v := make([]byte, 4+len(data))
	le.PutUint16(v, xattrInline)
	le.PutUint16(v[2:], uint16(len(data)))
	copy(v[4:], data)
	return v
}

// XfBlob encodes extended fields as an xf_blob_t: header, descriptors, then the
// values each padded to 8 bytes.
func XfBlob(fields ...XField) []byte {
	var desc, data []byte
	for _, f := range fields {
		var d [4]byte
		d[0], d[1] = f.Type, f.Flags
		le.PutUint16(d[2:], uint16(len(f.Data)))
		desc = append(desc, d[:]...)
		data = append(data, f.Data...)
		for len(data)%8 != 0 {
			data = append(data, 0)
		}
	}
	b := make([]byte, 4, 4+len(desc)+len(data))
	le.PutUint16(b, uint16(len(fields)))
	le.PutUint16(b[2:], uint16(len(data)))
	b = append(b, desc...)
	return append(b, data...)
}

// XField is one extended field for XfBlob.
type XField struct {
	Type, Flags uint8
	Data        []byte
}

// InodeVal encodes the 92 fixed bytes of a j_inode_val_t followed by xfields
// (the encoded blob, or nil).
func InodeVal(in InodeFields, xf []byte) []byte {
	v := make([]byte, 92, 92+len(xf))
	le.PutUint64(v[0:], in.Parent)
	le.PutUint64(v[8:], in.Private)
	for i, t := range in.Times {
		le.PutUint64(v[16+8*i:], t)
	}
	le.PutUint64(v[48:], in.Internal)
	le.PutUint32(v[56:], uint32(in.Links))
	le.PutUint32(v[60:], in.Prot)
	le.PutUint32(v[68:], in.Bsd)
	le.PutUint32(v[72:], in.UID)
	le.PutUint32(v[76:], in.GID)
	le.PutUint16(v[80:], uint16(in.Mode))
	le.PutUint64(v[84:], in.Uncompressed)
	return append(v, xf...)
}

// InodeFields are the fixed fields of an inode record (Times: create, modify,
// change, access).
type InodeFields struct {
	Parent, Private uint64
	Times           [4]uint64
	Internal        uint64
	Links           int32
	Prot, Bsd       uint32
	UID, GID        uint32
	Mode            uint32
	Uncompressed    uint64
}

// DstreamField returns the DSTREAM extended field (j_dstream_t, 40 bytes).
func DstreamField(size uint64, crypto uint64) XField {
	d := make([]byte, 40)
	le.PutUint64(d[0:], size)
	le.PutUint64(d[8:], size)
	le.PutUint64(d[16:], crypto)
	return XField{Type: xfDstream, Flags: 0x20, Data: d}
}

// NameField returns the NAME extended field.
func NameField(name []byte) XField {
	return XField{Type: xfName, Flags: 2, Data: append(append([]byte(nil), name...), 0)}
}

func direntType(mode uint32) uint16 {
	switch mode & modeTypeMsk {
	case modeDir:
		return 4
	case modeReg:
		return 8
	case modeSymlink:
		return 10
	case 0o010000:
		return 1
	case 0o020000:
		return 2
	case 0o060000:
		return 6
	case 0o140000:
		return 12
	}
	return 0
}

func timesOrDefault(t Times) [4]uint64 {
	out := [4]uint64{t.Create, t.Modify, t.Change, t.Access}
	for i := range out {
		if out[i] == 0 {
			out[i] = DefaultTime
		}
	}
	return out
}

// compile turns the volume's files into sorted fs-tree records. It returns the
// records, the inode number of every path ("/" is the root directory), the
// object counts and the next free object id.
func (v Volume) compile() ([]FSRecord, map[string]uint64, counts, uint64) {
	var (
		recs   []FSRecord
		inodes = map[uint64]*bInode{}
		byPath = map[string]uint64{"/": rootIno}
		cnt    counts
		next   = uint64(firstUserIno)
		kids   = map[uint64]int32{}
		order  []*bInode
	)
	add := func(in *bInode) *bInode {
		inodes[in.ino] = in
		order = append(order, in)
		return in
	}
	add(&bInode{ino: rootIno, parent: rootParentID, name: []byte("root"), mode: modeDir | 0o755, times: timesOrDefault(Times{})})
	add(&bInode{ino: privDirIno, parent: rootParentID, name: []byte("private-dir"), mode: modeDir | 0o755, times: timesOrDefault(Times{})})

	fold := v.CaseInsensitive
	drec := func(parent uint64, name []byte, fileID uint64, typ uint16, bad bool) {
		h := NameHash(name, fold)
		if bad {
			h = ^h & 0x3fffff
		}
		recs = append(recs, FSRecord{ID: parent, Type: TypeDrec, Key: DrecKey(name, v.HashedKeys, h), Val: DrecVal(fileID, typ)})
		kids[parent]++
	}
	// The two directories mkapfs creates, under the root parent.
	drec(rootParentID, []byte("root"), rootIno, 4, false)
	drec(rootParentID, []byte("private-dir"), privDirIno, 4, false)

	alloc := func(explicit uint64) uint64 {
		if explicit != 0 {
			return explicit
		}
		n := next
		next++
		return n
	}
	var ensureDir func(path string) uint64
	ensureDir = func(path string) uint64 {
		if ino, ok := byPath[path]; ok {
			return ino
		}
		_, parent, base := splitPath(path)
		pino := ensureDir(parent)
		in := add(&bInode{ino: alloc(0), parent: pino, name: []byte(base), mode: modeDir | 0o755, times: timesOrDefault(Times{})})
		byPath[path] = in.ino
		drec(pino, []byte(base), in.ino, 4, false)
		cnt.dirs++
		return in.ino
	}

	for _, f := range v.Files {
		clean, parentPath, base := splitPath(f.Path)
		name := []byte(base)
		if f.RawName != nil {
			name = f.RawName
		}
		pino := ensureDir(parentPath)
		if f.LinkTo != "" {
			tclean, _, _ := splitPath(f.LinkTo)
			tino, ok := byPath[tclean]
			if !ok {
				panic("apfstest: hard link target " + f.LinkTo + " does not exist")
			}
			t := inodes[tino]
			t.links++
			fileID := tino
			if f.LinkSibling {
				sib := alloc(0)
				fileID = sib
				recs = append(recs,
					FSRecord{ID: sib, Type: TypeSiblingMap, Val: le8(tino)},
					FSRecord{ID: tino, Type: TypeSiblingLink, Key: le8(sib), Val: siblingLinkVal(pino, name)})
			}
			if f.DrecFileID != 0 {
				fileID = f.DrecFileID
			}
			typ := f.DrecType
			if typ == 0 {
				typ = direntType(t.mode)
			}
			drec(pino, name, fileID, typ, f.BadHash)
			continue
		}
		if f.Dir {
			if ino, ok := byPath[clean]; ok { // an implicit directory: apply the attributes
				in := inodes[ino]
				applyAttrs(in, f)
				continue
			}
		}
		in := &bInode{ino: alloc(f.Ino), parent: pino, name: name, links: 1, noRecord: f.NoInode}
		switch {
		case f.Dir:
			in.mode = modeDir | 0o755
			cnt.dirs++
		case f.Symlink != "":
			in.mode = modeSymlink | 0o777
			cnt.symlinks++
		default:
			in.mode = modeReg | 0o644
			cnt.files++
		}
		in.times = timesOrDefault(f.Times)
		applyAttrs(in, f)
		if f.Data != nil || f.Size > 0 {
			in.hasDstream = true
			in.size = int64(len(f.Data))
			if f.Data == nil {
				in.size = f.Size
			}
		}
		if f.CompressedFlag {
			in.bsd |= bsdCompressed
		}
		if f.UncompressedSize != 0 {
			in.uncompressed = f.UncompressedSize
			in.internal |= inodeHasUncompressed
		}
		add(in)
		byPath[clean] = in.ino
		typ := f.DrecType
		if typ == 0 {
			typ = direntType(in.mode)
		}
		fileID := in.ino
		if f.DrecFileID != 0 {
			fileID = f.DrecFileID
		}
		drec(pino, name, fileID, typ, f.BadHash)
		if f.Symlink != "" {
			recs = append(recs, FSRecord{
				ID: in.ino, Type: TypeXattr, Key: XattrKey(symlinkXattr),
				Val: XattrVal(append([]byte(f.Symlink), 0), f.SymlinkStream),
			})
		}
		for _, x := range f.Xattrs {
			recs = append(recs, FSRecord{ID: in.ino, Type: TypeXattr, Key: XattrKey(x.Name), Val: XattrVal(x.Value, x.Stream)})
		}
	}

	for _, in := range order {
		if in.noRecord {
			continue
		}
		links := in.links
		if in.isDir() {
			links = kids[in.ino]
		}
		if in.linkOverride != 0 {
			links = in.linkOverride
		}
		fields := []XField{NameField(in.name)}
		if in.hasDstream {
			fields = append(fields, DstreamField(uint64(in.size), in.crypto))
		}
		val := InodeVal(InodeFields{
			Parent: in.parent, Private: privateOf(in), Times: in.times, Internal: in.internal,
			Links: links, Prot: in.prot, Bsd: in.bsd, UID: in.uid, GID: in.gid, Mode: in.mode,
			Uncompressed: in.uncompressed,
		}, XfBlob(fields...))
		recs = append(recs, FSRecord{ID: in.ino, Type: TypeInode, Val: val})
	}
	recs = append(recs, v.Extra...)
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].ID&idMask != recs[j].ID&idMask {
			return recs[i].ID&idMask < recs[j].ID&idMask
		}
		return recs[i].Type < recs[j].Type
	})
	if v.Reorder != nil {
		recs = v.Reorder(recs)
	}
	return recs, byPath, cnt, next
}

func le8(n uint64) []byte {
	b := make([]byte, 8)
	le.PutUint64(b, n)
	return b
}

func siblingLinkVal(parent uint64, name []byte) []byte {
	v := make([]byte, 10+len(name)+1)
	le.PutUint64(v, parent)
	le.PutUint16(v[8:], uint16(len(name)+1))
	copy(v[10:], name)
	return v
}

// privateOf is the private_id: the number of the inode unless overridden.
func privateOf(in *bInode) uint64 {
	if in.private != 0 {
		return in.private
	}
	return in.ino
}

func applyAttrs(in *bInode, f File) {
	if f.Mode != 0 {
		if f.Mode&modeTypeMsk != 0 {
			in.mode = f.Mode
		} else {
			in.mode = in.mode&modeTypeMsk | f.Mode
		}
	}
	in.uid, in.gid = f.UID, f.GID
	if f.Times != (Times{}) {
		in.times = timesOrDefault(f.Times)
	}
	in.crypto = f.CryptoID
	in.private = f.PrivateID
	in.prot, in.bsd, in.internal = f.ProtClass, f.BsdFlags, f.InternalFlg
	in.linkOverride = f.Nlink
}
