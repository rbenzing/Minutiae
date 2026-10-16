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
	k, r, ok, ue, err := f.decodeLeafKind(rec)
	if ue != nil {
		f.warn("catalog record of unknown type %d under parent %d skipped", ue.typ, k.parent)
	}
	return k, r, ok, err
}

// decodeLeafKind is decodeLeaf without the warning: a record of an unknown type comes back as
// ue (with its key), not ok and without an error, for the caller to decide.
func (f *FS) decodeLeafKind(rec []byte) (k catalogKey, r catalogRecord, ok bool, ue *unknownRecordError, err error) {
	key, data, err := splitRecord(rec)
	if err != nil {
		return k, r, false, nil, err
	}
	if k, err = parseCatalogKey(key); err != nil {
		return k, r, false, nil, err
	}
	r, err = decodeCatalogRecord(data)
	if errors.As(err, &ue) {
		return k, r, false, ue, nil
	}
	return k, r, err == nil, nil, err
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

// unknownSkip is a record of an unknown type a lookup walked past.
type unknownSkip struct {
	k   catalogKey
	typ int16
}

// maxPassedUnknown bounds how many walked-past unknown records a lookup keeps to report together;
// the rest are warned about at once and are never folded into a later warning that names the same
// record (see fileObject), so a record past the cap may be reported twice.
const maxPassedUnknown = 16

// threadUnreadableError reports an unknown-type record AT the thread position (id, empty name):
// the thread exists but cannot be read, which is not the same as no thread. It is a CorruptError.
type threadUnreadableError struct{ *filesys.CorruptError }

func (e *threadUnreadableError) Unwrap() error { return e.CorruptError }

// lookupThread returns the thread record of a folder or file id (the record keyed (cnid, empty
// name)); filesys.ErrNotFound when there is none, a CorruptError when the record at that key is
// not a thread, a *threadUnreadableError when it has an unknown type. The unknown-type records
// the lookup walked past are returned for the caller to report (see catalogThread): none is
// dropped.
func (f *FS) lookupThread(cnid uint32) (catalogRecord, []unknownSkip, error) {
	target := catalogKey{parent: cnid}
	var (
		rec      catalogRecord
		passed   []unknownSkip
		found    bool
		unreadTy int16
		unread   bool
	)
	t, err := f.tree(treeCatalog)
	if err != nil {
		return rec, nil, err
	}
	err = t.scanFrom(f.catalogCmp(target), func(raw []byte) (bool, error) {
		k, r, ok, ue, derr := f.decodeLeafKind(raw)
		if derr != nil {
			return false, derr
		}
		if ue != nil {
			if compareKeys(k, target, f.binaryNames()) == 0 {
				unread, unreadTy = true, ue.typ
				return false, nil
			}
			if len(passed) < maxPassedUnknown {
				passed = append(passed, unknownSkip{k: k, typ: ue.typ})
			} else {
				f.warn("catalog record of unknown type %d under parent %d skipped", ue.typ, k.parent)
			}
			return true, nil
		}
		if !ok {
			return true, nil
		}
		if compareKeys(k, target, f.binaryNames()) == 0 {
			rec, found = r, true
		}
		return false, nil
	})
	switch {
	case err != nil:
		return rec, passed, err
	case unread:
		return rec, passed, &threadUnreadableError{corrupt("catalog", -1, "the thread record of id %d has the unknown type %d and cannot be read", cnid, unreadTy).(*filesys.CorruptError)}
	case !found:
		return rec, passed, fmt.Errorf("%w: no thread record for id %d", filesys.ErrNotFound, cnid)
	case !rec.isThread():
		return rec, passed, corrupt("catalog", -1, "the record keyed (%d, empty name) has type %d, not a thread", cnid, rec.typ)
	}
	return rec, passed, nil
}

func (f *FS) warnSkips(skips []unknownSkip) {
	for _, s := range skips {
		f.warn("catalog record of unknown type %d under parent %d skipped", s.typ, s.k.parent)
	}
}

// catalogThread is lookupThread that reports every unknown-type record it walked past.
func (f *FS) catalogThread(cnid uint32) (catalogRecord, error) {
	rec, passed, err := f.lookupThread(cnid)
	f.warnSkips(passed)
	return rec, err
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
// trees, findRecordFold. filesys.ErrNotFound when there is none; the key
// returned holds the name as stored.
func (f *FS) findRecord(parent uint32, name []uint16) (catalogKey, catalogRecord, error) {
	k, r, err := f.findRecordTree(parent, name)
	if !errors.Is(err, filesys.ErrNotFound) {
		return k, r, err
	}
	return f.findRecordFold(parent, name)
}

// findRecordFold is the second phase of findRecord: for case-folding trees a
// linear scan of that parent's children with the approximate fold (the fold
// here only approximates Apple's table, so an exotic name may stay unreachable
// by name). The scan reads catalog nodes and is charged to the directory read
// budget, and a scan that the budget cut short is a CorruptError, not a miss.
// An exact spelling is preferred to a folded one. On a case-sensitive tree, or
// for an empty name, it finds nothing (filesys.ErrNotFound).
func (f *FS) findRecordFold(parent uint32, name []uint16) (catalogKey, catalogRecord, error) {
	var gotKey catalogKey
	var got catalogRecord
	if f.binaryNames() || len(name) == 0 {
		return gotKey, got, fmt.Errorf("%w: no catalog record for parent %d", filesys.ErrNotFound, parent)
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

// maxIDScanBudget is the bytes of catalog leaves all thread-less scans of one filesystem may read.
const maxIDScanBudget = 256 << 20

// errIDScan ends a scan whose budget or record cap is spent.
var errIDScan = errors.New("id-scan budget exhausted")

// chargeIDScan takes n bytes from the id-scan budget; false when it is spent.
func (f *FS) chargeIDScan(n int64) bool {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if f.idScanBudget < n {
		return false
	}
	f.idScanBudget -= n
	return true
}

func (f *FS) refuseIDScan(id uint32) error {
	f.scanMu.Lock()
	f.idScanRefused++
	n := f.idScanRefused
	f.scanMu.Unlock()
	f.warn("thread-less file opens refused so far: %d (the id-scan budget of %d bytes per filesystem is spent; id %d was not searched)", n, int64(maxIDScanBudget), id)
	return corrupt("catalog", -1, "the id-scan budget ended the search for id %d before the catalog did: the id cannot be proven unique", id)
}

// scanFileByID reads the catalog leaves in key order and returns the one file or
// folder record whose id is id. No record: filesys.ErrNotFound. Two or more
// records with the id, or a record that cannot be decoded (it may be the second
// one): a CorruptError, nothing is opened. A scan the id-scan budget or the
// record cap ends before the catalog does: a CorruptError, counted and warned,
// never a miss.
func (f *FS) scanFileByID(id uint32) (catalogKey, catalogRecord, *unknownSkip, error) {
	var (
		unk      *unknownSkip // the unknown-type record that made the id unprovable
		fk       catalogKey
		fr       catalogRecord
		found    int
		conflict string
		damaged  error
	)
	t, err := f.tree(treeCatalog)
	if err != nil {
		return fk, fr, nil, err
	}
	if t.depth == 0 {
		return fk, fr, nil, fmt.Errorf("%w: %s has no catalog record", filesys.ErrNotFound, cnidString(id))
	}
	leaf, err := t.node(t.firstLeaf)
	if err != nil {
		return fk, fr, nil, err
	}
	charge := func(uint32) error {
		if !f.chargeIDScan(int64(t.nodeSize)) {
			return errIDScan
		}
		return nil
	}
	examined := 0
	err = t.scanHook(leaf, 0, charge, func(rec []byte) (bool, error) {
		if examined++; examined > f.dirRecordCap() {
			return false, errIDScan
		}
		k, r, ok, ue, derr := f.decodeLeafKind(rec)
		if derr != nil {
			damaged = derr // it may be a second owner of the id: stop, uniqueness is unprovable
			return false, nil
		}
		if ue != nil {
			if len(k.name) == 0 && k.parent == id {
				// Keyed (id, empty name): provably the thread position of the id being opened.
				f.warn("catalog record of unknown type %d under parent %d skipped", ue.typ, k.parent)
				return true, nil
			}
			// Any other unknown-type record has an unreadable id (an empty-name file or folder record
			// is valid here): it may be a second owner, so uniqueness is unprovable.
			unk = &unknownSkip{k: k, typ: ue.typ}
			damaged = corrupt("catalog", -1, "uniqueness unprovable: unknown record type %d under parent %d", ue.typ, k.parent)
			return false, nil
		}
		if !ok || r.isThread() || r.id != id {
			return true, nil
		}
		if found++; found == 1 {
			fk, fr = k, r
			return true, nil
		}
		conflict = fmt.Sprintf("(%d, %q)", k.parent, decodeLossy(k.name))
		return false, nil
	})
	switch {
	case errors.Is(err, errIDScan):
		return fk, fr, nil, f.refuseIDScan(id)
	case err != nil:
		return fk, fr, nil, err
	case damaged != nil:
		f.warn("a catalog record that cannot be decoded stopped the search for id %d: %v", id, damaged)
		return fk, fr, unk, corrupt("catalog", -1, "a damaged catalog record may share id %d: the id cannot be proven unique (%v)", id, damaged)
	case found == 0:
		return fk, fr, nil, fmt.Errorf("%w: %s has no catalog record", filesys.ErrNotFound, cnidString(id))
	case found > 1:
		f.warn("catalog node id %d is shared by several records, including %s and (%d, %q): the id cannot be opened", id, conflict, fk.parent, decodeLossy(fk.name))
		return fk, fr, nil, corrupt("catalog", -1, "catalog node id %d is shared by more than one record", id)
	}
	return fk, fr, nil, nil
}
