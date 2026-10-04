package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Catalog record types (the first i16 of a record's data).
const (
	recFolder       = 1
	recFile         = 2
	recFolderThread = 3
	recFileThread   = 4

	folderRecSize = 88
	fileRecSize   = 248
	threadMinSize = 10 // type, reserved, parentID, nodeName length

	hfsEpochOffset = 2082844800 // seconds from 1904-01-01 to 1970-01-01
)

// bsdInfo is HFSPlusBSDInfo (16 bytes at offset 32 of a folder or file record).
type bsdInfo struct {
	owner, group uint32
	adminFlags   uint8
	ownerFlags   uint8
	mode         uint16 // S_IFMT type bits and permissions; 0 on volumes written by classic Mac OS
	special      uint32 // link count of an iNode<N>, the inode number of a hard-link record, a device number
}

// catalogRecord is a decoded catalog data record (folder, file or thread).
// Which fields are set depends on typ.
type catalogRecord struct {
	typ   int16
	flags uint16

	// Folder and file records.
	id          uint32 // folderID or fileID
	valence     uint32 // folder: number of children
	create      uint32 // the five dates, HFS seconds (0 = absent)
	contentMod  uint32
	attrMod     uint32
	access      uint32
	backup      uint32
	bsd         bsdInfo
	fileType    uint32 // file: Finder userInfo fileType (4CC)
	fileCreator uint32 // file: Finder userInfo fileCreator (4CC)
	finderFlags uint16 // file: Finder flags
	textEnc     uint32
	data, rsrc  forkData // file only

	// Thread records.
	parent uint32
	name   []uint16
}

func (r *catalogRecord) isThread() bool { return r.typ == recFolderThread || r.typ == recFileThread }

// unknownRecordError reports a record whose type is not one of the four.
type unknownRecordError struct{ typ int16 }

func (e *unknownRecordError) Error() string {
	return fmt.Sprintf("catalog record of unknown type %d", e.typ)
}

// parseCatalogKey decodes a catalog key (the bytes after keyLength): parentID
// and an HFSUniStr255 name of at most 255 units that must fit the key.
func parseCatalogKey(key []byte) (catalogKey, error) {
	if len(key) < 6 {
		return catalogKey{}, corrupt("catalog key", -1, "key of %d bytes is shorter than the 6-byte minimum", len(key))
	}
	n := int(binary.BigEndian.Uint16(key[4:]))
	if n > maxNameUnits {
		return catalogKey{}, corrupt("catalog key", -1, "name of %d units exceeds %d", n, maxNameUnits)
	}
	if 6+2*n > len(key) {
		return catalogKey{}, corrupt("catalog key", -1, "name of %d units runs past the %d-byte key", n, len(key))
	}
	return catalogKey{parent: binary.BigEndian.Uint32(key), name: readUnits(key[6:], n)}, nil
}

// readUnits decodes n big-endian UTF-16 code units (the caller checked the length).
func readUnits(b []byte, n int) []uint16 {
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return u
}

// decodeCatalogRecord decodes the data part of a catalog leaf record. An
// unknown type is an *unknownRecordError; a record shorter than its type
// needs, or a thread whose name does not fit, is a *filesys.CorruptError.
func decodeCatalogRecord(data []byte) (catalogRecord, error) {
	if len(data) < 2 {
		return catalogRecord{}, corrupt("catalog record", -1, "record of %d bytes has no type", len(data))
	}
	be := binary.BigEndian
	r := catalogRecord{typ: int16(be.Uint16(data))}
	switch r.typ {
	case recFolder, recFile:
		need := folderRecSize
		if r.typ == recFile {
			need = fileRecSize
		}
		if len(data) < need {
			return catalogRecord{}, corrupt("catalog record", -1, "type %d record of %d bytes is shorter than %d", r.typ, len(data), need)
		}
		r.flags = be.Uint16(data[2:])
		r.id = be.Uint32(data[8:])
		r.create, r.contentMod, r.attrMod = be.Uint32(data[12:]), be.Uint32(data[16:]), be.Uint32(data[20:])
		r.access, r.backup = be.Uint32(data[24:]), be.Uint32(data[28:])
		r.bsd = bsdInfo{
			owner:      be.Uint32(data[32:]),
			group:      be.Uint32(data[36:]),
			adminFlags: data[40],
			ownerFlags: data[41],
			mode:       be.Uint16(data[42:]),
			special:    be.Uint32(data[44:]),
		}
		r.textEnc = be.Uint32(data[80:])
		if r.typ == recFolder {
			r.valence = be.Uint32(data[4:])
			return r, nil
		}
		r.fileType, r.fileCreator = be.Uint32(data[48:]), be.Uint32(data[52:])
		r.finderFlags = be.Uint16(data[56:])
		r.data = parseFork(data[88:])
		r.rsrc = parseFork(data[168:])
		return r, nil
	case recFolderThread, recFileThread:
		if len(data) < threadMinSize {
			return catalogRecord{}, corrupt("catalog record", -1, "thread record of %d bytes is shorter than %d", len(data), threadMinSize)
		}
		r.parent = be.Uint32(data[4:])
		n := int(be.Uint16(data[8:]))
		if n > maxNameUnits || threadMinSize+2*n > len(data) {
			return catalogRecord{}, corrupt("catalog record", -1, "thread name of %d units does not fit the %d-byte record", n, len(data))
		}
		r.name = readUnits(data[10:], n)
		return r, nil
	}
	return catalogRecord{}, &unknownRecordError{typ: r.typ}
}

// hfsTime converts HFS+ seconds (since 1904-01-01 UTC) to a timestamp: 0 is
// absent (the zero Timestamp); every other value is UTC.
func hfsTime(v uint32) filesys.Timestamp {
	if v == 0 {
		return filesys.Timestamp{}
	}
	return filesys.Timestamp{T: time.Unix(int64(v)-hfsEpochOffset, 0).UTC(), ZoneKnown: true}
}

// binaryNames reports whether catalog names compare by raw unit value (an
// HFSX case-sensitive volume) rather than after case folding.
func (f *FS) binaryNames() bool { return f.caseSensitive }

// catalogCmp builds the descent comparison for a target catalog key.
func (f *FS) catalogCmp(target catalogKey) keyCmp {
	bin := f.binaryNames()
	return func(key []byte) (int, error) {
		k, err := parseCatalogKey(key)
		if err != nil {
			// A key whose name is damaged still has a readable parent id, and a
			// damaged name sorts after the empty name: that is enough to find a
			// folder's thread record and the start of its children whatever damage
			// lies inside the range. Any other comparison needs the name.
			if len(key) >= 4 && len(target.name) == 0 {
				if p := binary.BigEndian.Uint32(key); p < target.parent {
					return -1, nil
				}
				return 1, nil
			}
			return 0, err
		}
		return compareKeys(k, target, bin), nil
	}
}

// decodeLeaf decodes one catalog leaf record. ok is false for a record of an
// unknown type (a warning is recorded and the caller moves on); malformed
// records are errors.
func (f *FS) decodeLeaf(rec []byte) (k catalogKey, r catalogRecord, ok bool, err error) {
	key, data, err := splitRecord(rec)
	if err != nil {
		return k, r, false, err
	}
	if k, err = parseCatalogKey(key); err != nil {
		return k, r, false, err
	}
	r, err = decodeCatalogRecord(data)
	var ue *unknownRecordError
	if errors.As(err, &ue) {
		f.warn("catalog record of unknown type %d under parent %d skipped", ue.typ, k.parent)
		return k, r, false, nil
	}
	return k, r, err == nil, err
}

// catalogScan calls fn for every decoded catalog record from the first one not
// less than start, in key order, until fn returns false. Records of an unknown
// type are skipped with a warning.
func (f *FS) catalogScan(start catalogKey, fn func(k catalogKey, r catalogRecord) (bool, error)) error {
	t, err := f.tree(treeCatalog)
	if err != nil {
		return err
	}
	return t.scanFrom(f.catalogCmp(start), func(rec []byte) (bool, error) {
		k, r, ok, err := f.decodeLeaf(rec)
		if err != nil || !ok {
			return err == nil, err
		}
		return fn(k, r)
	})
}

// catalogThread returns the thread record of a folder or file id (the record
// keyed (cnid, empty name)); filesys.ErrNotFound when there is none, a
// CorruptError when the record at that key is not a thread.
func (f *FS) catalogThread(cnid uint32) (catalogRecord, error) {
	target := catalogKey{parent: cnid}
	var rec catalogRecord
	found := false
	err := f.catalogScan(target, func(k catalogKey, r catalogRecord) (bool, error) {
		if compareKeys(k, target, f.binaryNames()) == 0 {
			rec, found = r, true
		}
		return false, nil
	})
	if err != nil {
		return rec, err
	}
	if !found {
		return rec, fmt.Errorf("%w: no thread record for id %d", filesys.ErrNotFound, cnid)
	}
	if !rec.isThread() {
		return rec, corrupt("catalog", -1, "the record keyed (%d, empty name) has type %d, not a thread", cnid, rec.typ)
	}
	return rec, nil
}

// maxEqualRun bounds how many records that compare equal to a searched name
// findRecord looks at: a real catalog has one, a forged one may hold more.
const maxEqualRun = 64

// findRecordTree is the first phase of findRecord: the descent, which follows
// the tree's own order (exact for binary HFSX trees, and for case-folding trees
// it finds every name that folds equal under the tree's own order). The records
// that compare equal to the name (several only in a forged or approximately
// folded tree) are examined and an exact spelling is preferred. Thread records
// are never returned. filesys.ErrNotFound when there is none; the key returned
// holds the name as stored. A record already found is still returned when the
// leaf chain is cut after it (the cut has been warned about).
func (f *FS) findRecordTree(parent uint32, name []uint16) (catalogKey, catalogRecord, error) {
	target := catalogKey{parent: parent, name: name}
	binaryMode := f.binaryNames()
	var gotKey catalogKey
	var got catalogRecord
	found, run := false, 0
	err := f.catalogScan(target, func(k catalogKey, r catalogRecord) (bool, error) {
		if compareKeys(k, target, binaryMode) != 0 {
			return false, nil
		}
		if r.isThread() {
			return true, nil
		}
		run++
		if !found {
			gotKey, got, found = k, r, true
		}
		if slices.Equal(k.name, name) {
			gotKey, got = k, r
			return false, nil
		}
		return run < maxEqualRun, nil
	})
	if err != nil && (!found || !errors.Is(err, filesys.ErrCorrupt)) {
		return gotKey, got, err
	}
	if found {
		return gotKey, got, nil
	}
	return gotKey, got, fmt.Errorf("%w: no catalog record for parent %d", filesys.ErrNotFound, parent)
}

// findRecord finds the folder or file record keyed (parent, name); thread
// records are never returned. It is findRecordTree, then, for case-folding
// trees, a linear scan of that parent's children with the approximate fold
// (the fold here only approximates Apple's table, so an exotic name may stay
// unreachable by name): the scan reads catalog nodes and is charged to the
// directory read budget, and a scan that the budget cut short is a
// CorruptError, not a miss. An exact spelling is preferred to a folded one.
// filesys.ErrNotFound when there is none; the key returned holds the name as
// stored.
func (f *FS) findRecord(parent uint32, name []uint16) (catalogKey, catalogRecord, error) {
	gotKey, got, err := f.findRecordTree(parent, name)
	if !errors.Is(err, filesys.ErrNotFound) || f.binaryNames() || len(name) == 0 {
		return gotKey, got, err
	}
	found, exact := false, false
	cutShort, err := f.foldScan(parent, name, func(k catalogKey, r catalogRecord) bool {
		if !found {
			gotKey, got, found = k, r, true
		}
		if slices.Equal(k.name, name) {
			gotKey, got, exact = k, r, true
		}
		return !exact
	})
	if err != nil {
		return gotKey, got, err
	}
	if cutShort && !found {
		return gotKey, got, corrupt("catalog", -1, "the directory read budget ran out while searching folder %d for a name", parent)
	}
	if found {
		return gotKey, got, nil
	}
	return gotKey, got, fmt.Errorf("%w: no catalog record for parent %d", filesys.ErrNotFound, parent)
}

// foldScan visits the folder and file records of parent whose name equals name
// under the approximate case fold, in key order, until visit returns false. The
// leaves read are charged to the directory read budget; cutShort reports that
// the budget ended the scan before the range did. Damaged records are skipped
// with a warning.
func (f *FS) foldScan(parent uint32, name []uint16, visit func(k catalogKey, r catalogRecord) bool) (cutShort bool, err error) {
	t, err := f.tree(treeCatalog)
	if err != nil {
		return false, err
	}
	leaf, pos, err := t.search(f.catalogCmp(catalogKey{parent: parent}))
	if err != nil || leaf == nil {
		return false, err
	}
	want := fold(name)
	err = t.scanHook(leaf, pos, f.chargeLeaf(t), func(rec []byte) (bool, error) {
		k, r, ok, err := f.decodeLeaf(rec)
		if err != nil {
			f.warn("a damaged catalog record was skipped while searching folder %d: %v", parent, err)
			return true, nil
		}
		if !ok {
			return true, nil
		}
		if k.parent != parent {
			return false, nil
		}
		if !r.isThread() && compareUnits(fold(k.name), want) == 0 {
			return visit(k, r), nil
		}
		return true, nil
	})
	if errors.Is(err, errDirBudget) {
		return true, nil
	}
	return false, err
}

// loadLabel sets the volume name from the root folder's thread record (keyed
// (2, empty name)). A volume without one has no label; the reason is a warning.
func (f *FS) loadLabel() {
	th, err := f.catalogThread(rootFolderID)
	switch {
	case err == nil:
		f.label, _ = decodeUnits(th.name)
	case errors.Is(err, filesys.ErrNotFound):
		f.warn("the root folder has no thread record, so the volume has no name")
	default:
		f.warn("the volume name cannot be read from the catalog: %v", err)
	}
}

const rootFolderID = 2
