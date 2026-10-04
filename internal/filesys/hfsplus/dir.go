package hfsplus

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	// maxDirBudget is how many bytes of catalog leaf nodes one FS instance reads
	// in total for directory listings and fallback name scans; single-name
	// descents are not charged.
	maxDirBudget = 1 << 30
	// maxDirEntries bounds the entries one listing yields; maxDirRecords bounds
	// the records (listed or damaged) it examines.
	maxDirEntries = 1 << 18
	maxDirRecords = 1 << 20
)

// errDirBudget ends a scan whose leaf reads used up the directory read budget.
var errDirBudget = errors.New("directory read budget exhausted")

// chargeDir takes n bytes from the per-FS directory read budget.
func (f *FS) chargeDir(n int64) bool {
	f.dirMu.Lock()
	defer f.dirMu.Unlock()
	if f.dirBudget < n {
		return false
	}
	f.dirBudget -= n
	return true
}

// chargeLeaf is the scanHook hook that charges each leaf of t to the budget.
func (f *FS) chargeLeaf(t *btree) func(uint32) error {
	return func(uint32) error {
		if !f.chargeDir(int64(t.nodeSize)) {
			f.warn("the directory read budget of %d bytes per filesystem is exhausted; directories are no longer read in full", int64(maxDirBudget))
			return errDirBudget
		}
		return nil
	}
}

// dirRecordCap is the number of catalog records (listed, thread or damaged) one
// listing examines.
func (f *FS) dirRecordCap() int {
	if f.recCap > 0 {
		return f.recCap
	}
	return maxDirRecords
}

func (f *FS) dirEntryCap() int {
	if f.dirCap > 0 {
		return f.dirCap
	}
	return maxDirEntries
}

// folderRecord resolves a folder by id the way ReadDir does: the folder's
// thread record names (parent, name); the record at that key must be a folder
// record carrying the same id. A missing thread wraps filesys.ErrNotFound, a
// thread of a file is filesys.ErrUnsupported (not a directory) and any
// disagreement between thread and record is a CorruptError.
func (f *FS) folderRecord(id uint32) (catalogKey, catalogRecord, error) {
	th, err := f.catalogThread(id)
	if err != nil {
		return catalogKey{}, catalogRecord{}, err
	}
	if th.typ != recFolderThread {
		return catalogKey{}, catalogRecord{}, fmt.Errorf("%w: %s is not a directory", filesys.ErrUnsupported, cnidString(id))
	}
	k, r, err := f.findRecord(th.parent, th.name)
	if errors.Is(err, filesys.ErrNotFound) {
		return k, r, corrupt("catalog", -1, "thread/record mismatch: the thread of folder %d names (%d, %q), which has no folder or file record", id, th.parent, decodeLossy(th.name))
	}
	if err != nil {
		return k, r, err
	}
	if r.typ != recFolder || r.id != id {
		return k, r, corrupt("catalog", -1, "thread/record mismatch: the thread of folder %d names (%d, %q), a type %d record with id %d", id, th.parent, decodeLossy(k.name), r.typ, r.id)
	}
	return k, r, nil
}

func decodeLossy(u []uint16) string {
	s, _ := decodeUnits(u)
	return s
}

// ReadDir lists the folder named by dir.ID alone: the ID is parsed strictly
// ("cnid:<n>", see parseCNID), the folder's thread record and folder record
// must agree, and the children are the folder and file records of the catalog
// leaf range whose key parent is the folder (thread records are skipped). HFS+
// keeps no deleted-entry marker, so no entry is ever Deleted. Damaged records
// are skipped with a warning and a listing cut short by the entry cap, the
// directory read budget or a damaged tree is partial, with a warning; a
// directory whose budget is spent before its first entry is a CorruptError.
// The order is the catalog's.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	id, err := parseCNID(dir.ID)
	if err != nil {
		return nil, err
	}
	_, fr, err := f.folderRecord(id)
	if err != nil {
		return nil, err
	}
	return f.listFolder(id, fr.valence)
}

func (f *FS) listFolder(id, valence uint32) ([]filesys.Entry, error) {
	t, err := f.tree(treeCatalog)
	if err != nil {
		return nil, err
	}
	leaf, pos, err := t.search(f.catalogCmp(catalogKey{parent: id}))
	if err != nil {
		return nil, err
	}
	if leaf == nil {
		return nil, nil
	}
	limit := f.dirEntryCap()
	var out []filesys.Entry
	var ioErr error
	bad, examined := 0, 0
	recCap := f.dirRecordCap()
	seen := map[uint32]struct{}{}
	var firstBad error
	cut := ""
	err = t.scanHook(leaf, pos, f.chargeLeaf(t), func(rec []byte) (bool, error) {
		k, r, ok, err := f.decodeLeaf(rec)
		switch {
		case err != nil:
			bad++
			if firstBad == nil {
				firstBad = err
			}
		case !ok:
			// an unknown record type: decodeLeaf warned about it
		case k.parent != id:
			return false, nil
		case r.isThread():
			// not an entry, but it counts toward the record cap below
		case len(out) >= limit:
			cut = fmt.Sprintf("folder %d has more than %d entries: the rest are not listed", id, limit)
			return false, nil
		default:
			e, err := f.toEntry(k, r)
			if err != nil {
				ioErr = err
				return false, nil
			}
			if _, dup := seen[r.id]; dup {
				f.warn("folder %d lists catalog node id %d more than once (a forged or damaged catalog)", id, r.id)
			}
			seen[r.id] = struct{}{}
			out = append(out, e)
		}
		if examined++; examined >= recCap {
			cut = fmt.Sprintf("folder %d has more than %d catalog records: the rest are not listed", id, recCap)
			return false, nil
		}
		return true, nil
	})
	if ioErr != nil {
		return nil, ioErr
	}
	if bad > 0 {
		f.warn("folder %d: %d damaged catalog records were skipped (first: %v)", id, bad, firstBad)
	}
	switch {
	case errors.Is(err, errDirBudget):
		if len(out) == 0 {
			return nil, corrupt("catalog", -1, "the directory read budget was exhausted before folder %d was read", id)
		}
		cut = fmt.Sprintf("the directory read budget ran out while listing folder %d: the listing is partial", id)
	case errors.Is(err, filesys.ErrCorrupt) && len(out) > 0:
		cut = fmt.Sprintf("folder %d: the catalog is damaged after %d entries (%v): the listing is partial", id, len(out), err)
	case err != nil:
		return nil, err
	}
	if cut != "" {
		f.warn("%s", cut)
	} else if bad == 0 && uint32(len(out)) != valence {
		f.warn("folder %d has %d entries but its record claims a valence of %d", id, len(out), valence)
	}
	return out, nil
}

// Lookup resolves an absolute slash path. Empty components are ignored, "." and
// ".." are not special (a stored "." or ".." is shown in the "~raw~" form, which
// Lookup accepts) and symlinks are not followed. In each folder a component
// matches, in this order of preference: a name spelled exactly as typed (the
// catalog tree decides case: HFS+ and case-folding HFSX volumes ignore it,
// case-sensitive HFSX volumes - keyCompareType 0xBC - do not; an exact spelling
// is preferred to a folded one), then the "~raw~"+base64url display form of a
// name that ReadDir shows that way. Names are compared as stored: no Unicode
// normalization is done, so a composed (NFC) path does not find a name stored
// decomposed (NFD); such an entry is reachable by its listed ID.
func (f *FS) Lookup(p string) (filesys.Entry, error) {
	cur := f.Root()
	for _, comp := range strings.Split(p, "/") {
		if comp == "" {
			continue
		}
		if cur.Type != filesys.TypeDir {
			return filesys.Entry{}, fmt.Errorf("%w: %s", filesys.ErrNotFound, strconv.Quote(p[:min(len(p), 256)]))
		}
		parent, err := parseCNID(cur.ID)
		if err != nil {
			return filesys.Entry{}, err
		}
		next, err := f.child(parent, comp)
		if err != nil {
			if errors.Is(err, filesys.ErrNotFound) {
				err = fmt.Errorf("%w: %s", filesys.ErrNotFound, strconv.Quote(p[:min(len(p), 256)]))
			}
			return filesys.Entry{}, err
		}
		cur = next
	}
	return cur, nil
}

// child finds the entry of folder parent named comp (see Lookup). The order is
// exact > display alias > case fold: the catalog descent finds an exact
// spelling (or a name the tree itself orders as equal); an exact spelling wins
// at once; then a "~raw~" alias, which needs only one more descent; then a name
// the descent found by folding; and only last the linear fold scan, which reads
// the whole folder, is charged to the directory read budget and can fail on a
// damaged leaf chain.
func (f *FS) child(parent uint32, comp string) (filesys.Entry, error) {
	var plain []uint16 // comp as a catalog name, when ReadDir would show such a name as it is
	if utf8.ValidString(comp) {
		if u := utf16.Encode([]rune(comp)); len(u) <= maxNameUnits {
			if _, raw := displayName(u); raw == nil {
				plain = u
			}
		}
	}
	var nearKey catalogKey
	var near catalogRecord
	haveNear := false
	if plain != nil {
		k, r, err := f.findRecordTree(parent, plain)
		switch {
		case err == nil && slices.Equal(k.name, plain):
			return f.toEntry(k, r)
		case err == nil:
			nearKey, near, haveNear = k, r, true
		case !errors.Is(err, filesys.ErrNotFound):
			return filesys.Entry{}, err
		}
	}
	e, ok, err := f.aliasChild(parent, comp)
	if err != nil || ok {
		return e, err
	}
	if haveNear {
		return f.toEntry(nearKey, near)
	}
	if plain != nil {
		k, r, err := f.findRecordFold(parent, plain)
		switch {
		case err == nil:
			return f.toEntry(k, r)
		case !errors.Is(err, filesys.ErrNotFound):
			return filesys.Entry{}, err
		}
	}
	return filesys.Entry{}, fmt.Errorf("%w: no entry named %s", filesys.ErrNotFound, strconv.Quote(comp[:min(len(comp), 64)]))
}

// aliasChild resolves a "~raw~"+base64url display form to the entry whose
// stored name bytes are exactly the decoded ones; ok is false when comp is not
// an alias of a name of this folder.
func (f *FS) aliasChild(parent uint32, comp string) (e filesys.Entry, ok bool, err error) {
	rest, isAlias := strings.CutPrefix(comp, rawPrefix)
	if !isAlias {
		return e, false, nil
	}
	// Each name has one spelling: the text must be exactly what encoding the
	// decoded bytes gives (the decoder skips embedded CR/LF, and a variant with
	// padding or another alphabet is not the display form).
	b, derr := base64.RawURLEncoding.Strict().DecodeString(rest)
	if derr != nil || base64.RawURLEncoding.EncodeToString(b) != rest || len(b)%2 != 0 || len(b)/2 > maxNameUnits {
		return e, false, nil
	}
	u := readUnits(b, len(b)/2)
	k, r, err := f.findRecordTree(parent, u)
	if errors.Is(err, filesys.ErrNotFound) {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	// The alias stands for the exact stored bytes of a name shown in the raw form.
	if display, _ := displayName(k.name); !slices.Equal(k.name, u) || display != comp {
		return e, false, nil
	}
	e, err = f.toEntry(k, r)
	return e, err == nil, err
}

var _ filesys.FileSystem = (*FS)(nil)
