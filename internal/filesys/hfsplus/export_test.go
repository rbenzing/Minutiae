package hfsplus

import (
	"errors"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Test-only accessors, so the external hfsplus_test package can check
// internals without widening the public API.

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// Base returns the byte offset of the volume from the start of the ReaderAt.
func (f *FS) Base() int64 { return f.base }

// ReadVolume reads from the metadata view of the volume, whose offset 0 is
// the start of the volume (Base() bytes into the image).
func (f *FS) ReadVolume(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }

// JournalState names the journal state Open found: "none", "clean",
// "pending" or "unknown".
func (f *FS) JournalState() string {
	switch f.journal {
	case journalClean:
		return "clean"
	case journalPending:
		return "pending"
	case journalUnknown:
		return "unknown"
	}
	return "none"
}

// Tree kinds, B-trees and forks, for tests of the internal layers.
type (
	TreeKind = treeKind
	BTree    = btree
	Fork     = forkMap
	ForkData = forkData
)

const (
	TreeCatalog    = treeCatalog
	TreeExtents    = treeExtents
	TreeAttributes = treeAttributes
)

// NewForkData builds a fork data record: logical size, total blocks and up to
// eight inline extents {start, count}.
func NewForkData(logical uint64, total uint32, exts ...[2]uint32) ForkData {
	fd := forkData{logicalSize: logical, totalBlocks: total}
	for i, e := range exts {
		fd.extents[i] = extent{start: e[0], count: e[1]}
	}
	return fd
}

// Tree opens (or returns the cached) B-tree of a kind.
func (f *FS) Tree(kind TreeKind) (*BTree, error) { return f.tree(kind) }

// BTreeHeader is the validated header of an opened tree.
type BTreeHeader struct {
	NodeSize, Depth                       int
	Root, FirstLeaf, LastLeaf, TotalNodes uint32
	LeafRecords                           uint32
	MaxKeyLength                          uint16
	KeyCompare                            byte
}

func (t *BTree) Header() BTreeHeader {
	return BTreeHeader{t.nodeSize, t.depth, t.root, t.firstLeaf, t.lastLeaf, t.totalNodes, t.leafRecords, t.maxKey, t.keyCompare}
}

// ScanAll walks every leaf record (key and data together) from the first leaf.
func (t *BTree) ScanAll(fn func(rec []byte) (bool, error)) error { return t.scanAll(fn) }

// SplitRecord separates a record into its key bytes and data.
func SplitRecord(rec []byte) (key, data []byte, err error) { return splitRecord(rec) }

// ForkExtents resolves a fork's extents ({start, count} pairs).
func (f *FS) ForkExtents(fileID uint32, resource bool, fd ForkData) ([][2]uint32, bool, error) {
	exts, complete, err := f.forkExtents(fileID, resource, fd)
	out := make([][2]uint32, len(exts))
	for i, e := range exts {
		out[i] = [2]uint32{e.start, e.count}
	}
	return out, complete, err
}

// MapFork builds the fork map of a fork.
func (f *FS) MapFork(fileID uint32, resource bool, fd ForkData) (*Fork, error) {
	return f.fork(fileID, resource, fd)
}

// CatRec is an exported view of a decoded catalog record.
type CatRec struct {
	Type                                         int16
	Flags                                        uint16
	ID, Valence                                  uint32
	Create, ContentMod, AttrMod, Access, Backup  uint32
	Owner, Group                                 uint32
	AdminFlags, OwnerFlags                       uint8
	Mode                                         uint16
	Special                                      uint32
	FileType, FileCreator                        uint32
	FinderFlags                                  uint16
	TextEnc                                      uint32
	DataLogical, RsrcLogical                     uint64
	DataClump, DataBlocks, RsrcClump, RsrcBlocks uint32
	DataExtents, RsrcExtents                     [8][2]uint32
	Parent                                       uint32
	Name                                         []uint16
}

func catRec(r catalogRecord) CatRec {
	c := CatRec{
		Type: r.typ, Flags: r.flags, ID: r.id, Valence: r.valence,
		Create: r.create, ContentMod: r.contentMod, AttrMod: r.attrMod, Access: r.access, Backup: r.backup,
		Owner: r.bsd.owner, Group: r.bsd.group, AdminFlags: r.bsd.adminFlags, OwnerFlags: r.bsd.ownerFlags,
		Mode: r.bsd.mode, Special: r.bsd.special,
		FileType: r.fileType, FileCreator: r.fileCreator, FinderFlags: r.finderFlags, TextEnc: r.textEnc,
		DataLogical: r.data.logicalSize, RsrcLogical: r.rsrc.logicalSize,
		DataClump: r.data.clumpSize, DataBlocks: r.data.totalBlocks, RsrcClump: r.rsrc.clumpSize, RsrcBlocks: r.rsrc.totalBlocks,
		Parent: r.parent, Name: r.name,
	}
	for i := range 8 {
		c.DataExtents[i] = [2]uint32{r.data.extents[i].start, r.data.extents[i].count}
		c.RsrcExtents[i] = [2]uint32{r.rsrc.extents[i].start, r.rsrc.extents[i].count}
	}
	return c
}

// DecodeCatalogRecord decodes the data part of a catalog leaf record. An
// unknown record type is reported as unknown=true with a nil error.
func DecodeCatalogRecord(data []byte) (rec CatRec, unknown bool, err error) {
	r, err := decodeCatalogRecord(data)
	var ue *unknownRecordError
	if errors.As(err, &ue) {
		return CatRec{Type: ue.typ}, true, nil
	}
	return catRec(r), false, err
}

// DecodeCatalogKey decodes a catalog key (the bytes after keyLength).
func DecodeCatalogKey(key []byte) (parent uint32, name []uint16, err error) {
	k, err := parseCatalogKey(key)
	return k.parent, k.name, err
}

func HFSTime(v uint32) filesys.Timestamp { return hfsTime(v) }
func Fold(u []uint16) []uint16           { return fold(u) }
func CompareNames(a, b []uint16, binary bool) int {
	return compareNames(a, b, binary)
}

func CompareKeys(ap uint32, an []uint16, bp uint32, bn []uint16, binary bool) int {
	return compareKeys(catalogKey{ap, an}, catalogKey{bp, bn}, binary)
}

// CatalogThread returns the thread record of an id.
func (f *FS) CatalogThread(cnid uint32) (CatRec, error) {
	r, err := f.catalogThread(cnid)
	return catRec(r), err
}

// FindRecord finds the record keyed (parent, name); the name as stored is returned.
func (f *FS) FindRecord(parent uint32, name string) (CatRec, string, error) {
	k, r, err := f.findRecord(parent, utf16.Encode([]rune(name)))
	s, _ := decodeUnits(k.name)
	return catRec(r), s, err
}

// CatalogScan visits every catalog record in key order until fn returns false.
func (f *FS) CatalogScan(fn func(parent uint32, name string, r CatRec) bool) error {
	return f.catalogScan(catalogKey{}, func(k catalogKey, r catalogRecord) (bool, error) {
		s, _ := decodeUnits(k.name)
		return fn(k.parent, s, catRec(r)), nil
	})
}

// SetDirBudget sets the bytes of catalog nodes the instance may still read for
// directory listings and fallback scans.
func (f *FS) SetDirBudget(n int64) {
	f.dirMu.Lock()
	f.dirBudget = n
	f.dirMu.Unlock()
}

// DirBudget returns the bytes of the directory read budget still available.
func (f *FS) DirBudget() int64 {
	f.dirMu.Lock()
	defer f.dirMu.Unlock()
	return f.dirBudget
}

// SetDirEntryCap lowers the number of entries one listing yields.
func (f *FS) SetDirEntryCap(n int) { f.dirCap = n }

// WithLogical returns fd with its logical size replaced.
func WithLogical(fd ForkData, n uint64) ForkData {
	fd.logicalSize = n
	return fd
}

// SetExtentCap lowers the number of extents kept per fork (0 = the default).
func (f *FS) SetExtentCap(n int)    { f.extentCap = n }
func (f *FS) SetDirRecordCap(n int) { f.recCap = n }

// SetUnallocCap lowers the number of free runs Unallocated reports (0 = the default).
func (f *FS) SetUnallocCap(n int) { f.unallocCap = n }
