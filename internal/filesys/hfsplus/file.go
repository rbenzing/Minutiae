package hfsplus

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	ufCompressed  = 0x20 // UF_COMPRESSED in the BSD ownerFlags
	maxLinkTarget = 4096 // longest symlink target read for an Entry (PATH_MAX)
)

// object is a catalog file or folder record together with what its content and
// metadata come from: for a resolved hard link the indirect node, else the
// record itself.
type object struct {
	key   catalogKey
	rec   catalogRecord // the record the entry's ID names
	node  catalogRecord // content and metadata source: rec, or the iNode of a resolved file link
	link  linkInfo
	attrs attrInfo // the attributes of node
	cmp   decmpfsHeader
	// compressed: the file is decmpfs-compressed (the UF_COMPRESSED flag or a
	// com.apple.decmpfs attribute).
	compressed bool
}

func (o *object) name() string {
	s, _ := displayName(o.key.name)
	return strconv.Quote(s)
}

// newObject resolves hard links and reads the attributes of a catalog record.
// Only an I/O error is returned: damage is warned about and shown in the entry.
func (f *FS) newObject(k catalogKey, r catalogRecord) (*object, error) {
	o := &object{key: k, rec: r, node: r}
	if r.typ == recFile {
		l, err := f.resolveLink(&r)
		if err != nil {
			return nil, err
		}
		o.link = l
		switch l.kind {
		case linkFile:
			o.node = l.inode
		case linkDangling, linkInvalid:
			f.warn("hard link %s (id %d) to inode %d is unusable: %s", o.name(), r.id, l.inodeNum, l.why)
		}
	}
	a, err := f.attributesOf(o.node.id)
	if err != nil {
		return nil, err
	}
	o.attrs = a
	if r.typ == recFile {
		o.cmp = parseDecmpfs(a.value)
		o.compressed = o.node.bsd.ownerFlags&ufCompressed != 0 || a.decmpfs
		if o.compressed && !o.cmp.ok {
			f.warn("file %s (id %d) is decmpfs-compressed but its decmpfs header is missing or unreadable; its content cannot be decoded", o.name(), o.node.id)
		}
	}
	return o, nil
}

// fileObject finds the file record of a CNID through its thread record. A
// folder is filesys.ErrUnsupported, an unknown id (or a file without a thread
// record, which cannot be searched for) filesys.ErrNotFound, and a thread that
// disagrees with the catalog a CorruptError.
func (f *FS) fileObject(id uint32) (*object, error) {
	th, err := f.catalogThread(id)
	if err != nil {
		if errors.Is(err, filesys.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s has no thread record, so it cannot be found by id", filesys.ErrNotFound, cnidString(id))
		}
		return nil, err
	}
	if th.typ != recFileThread {
		return nil, fmt.Errorf("%w: %s is a directory", filesys.ErrUnsupported, cnidString(id))
	}
	k, r, err := f.findRecord(th.parent, th.name)
	if errors.Is(err, filesys.ErrNotFound) {
		return nil, corrupt("catalog", -1, "thread/record mismatch: the thread of file %d names (%d, %q), which has no folder or file record", id, th.parent, decodeLossy(th.name))
	}
	if err != nil {
		return nil, err
	}
	if r.typ != recFile || r.id != id {
		return nil, corrupt("catalog", -1, "thread/record mismatch: the thread of file %d names (%d, %q), a type %d record with id %d", id, th.parent, decodeLossy(k.name), r.typ, r.id)
	}
	return f.newObject(k, r)
}

// Open opens a regular file or symlink named by e.ID alone: the ID is parsed
// strictly, the file's thread and file record are read, a hard link is
// resolved to its iNode, and everything (size, type, forks, compression) comes
// from the disk, never from the other fields of e. A folder is
// filesys.ErrUnsupported; so are special files (devices, fifos, sockets),
// directory hard links and compressed files that are not zlib-compressed. A
// dangling or invalid hard link is a CorruptError. A file whose fork is
// damaged or shorter than its size opens with the bytes that can be trusted
// (a warning; File.Runs is that prefix and reads at its end fail with an error
// wrapping filesys.ErrCorrupt).
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	id, err := parseCNID(e.ID)
	if err != nil {
		return nil, err
	}
	o, err := f.fileObject(id)
	if err != nil {
		return nil, err
	}
	return f.openObject(o)
}

func (f *FS) openObject(o *object) (filesys.File, error) {
	switch o.link.kind {
	case linkDangling, linkInvalid:
		return nil, corrupt("hard link", -1, "%s (id %d) points at inode %d: %s", o.name(), o.rec.id, o.link.inodeNum, o.link.why)
	case linkDir:
		return nil, fmt.Errorf("%w: directory hard link", filesys.ErrUnsupported)
	}
	if entryType(&o.node) == filesys.TypeOther {
		return nil, fmt.Errorf("%w: special file (mode %#o)", filesys.ErrUnsupported, o.node.bsd.mode)
	}
	if o.compressed {
		if !o.cmp.ok {
			return nil, fmt.Errorf("%w: decmpfs-compressed file (no readable decmpfs header)", filesys.ErrUnsupported)
		}
		switch o.cmp.typ {
		case decmpfsTypeInline:
			return f.openZlibInline(o)
		case decmpfsTypeRsrc:
			return f.openZlibRsrc(o)
		}
		return nil, fmt.Errorf("%w: decmpfs-compressed file (compression type %d)", filesys.ErrUnsupported, o.cmp.typ)
	}
	return f.openFork(o)
}

// dataFile is a file whose content is a data fork.
type dataFile struct {
	fm   *forkMap
	size int64
	runs []filesys.Run
}

func (d *dataFile) ReadAt(p []byte, off int64) (int, error) { return d.fm.ReadAt(p, off) }
func (d *dataFile) Size() int64                             { return d.size }
func (d *dataFile) Runs() []filesys.Run                     { return d.runs }

// openFork opens the data fork of o.node. Runs cover exactly [0, size) when the
// fork maps all of it; when the allocation is damaged or maps fewer bytes than
// the size, they cover the leading bytes that can be trusted (see the
// File.Runs prefix rule) and a warning says so.
func (f *FS) openFork(o *object) (filesys.File, error) {
	fd := o.node.data
	if fd.logicalSize > math.MaxInt64 {
		return nil, corrupt("catalog record", -1, "file %s (id %d) has a data fork size of %d bytes", o.name(), o.node.id, fd.logicalSize)
	}
	size := int64(fd.logicalSize)
	fm, err := f.forkPrefix(o.node.id, false, fd)
	if err != nil && !errors.Is(err, filesys.ErrCorrupt) {
		return nil, err
	}
	mapped, ok := filesys.MulOK(int64(min(fm.blocks, math.MaxInt64/uint64(fm.blockSize))), fm.blockSize)
	if !ok {
		mapped = math.MaxInt64
	}
	switch {
	case err != nil:
		f.warn("file %s (id %d): its data fork is damaged (%v); only the first %d of %d bytes can be trusted", o.name(), o.node.id, err, min(mapped, size), size)
	case mapped < size:
		f.warn("file %s (id %d) has %d bytes but its data fork maps only %d: the rest cannot be trusted", o.name(), o.node.id, size, mapped)
	}
	return &dataFile{fm: fm, size: size, runs: fm.Runs(size)}, nil
}

// linkTarget reads the target of a symlink for its Entry: "" when it is empty,
// compressed, too long (over maxLinkTarget bytes) or unreadable (a warning).
func (f *FS) linkTarget(o *object, size int64) string {
	if size <= 0 || size > maxLinkTarget || o.compressed {
		return ""
	}
	fl, err := f.openFork(o)
	if err != nil {
		f.warn("symlink %s (id %d): the target cannot be read: %v", o.name(), o.node.id, err)
		return ""
	}
	buf := make([]byte, size)
	if n, err := fl.ReadAt(buf, 0); n != len(buf) {
		f.warn("symlink %s (id %d): the target cannot be read: %v", o.name(), o.node.id, err)
		return ""
	}
	return string(buf)
}
