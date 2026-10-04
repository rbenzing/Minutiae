package f2fs

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Directories. An F2FS directory is a multi-level hash table, but every block
// of the directory file is a dentry block, so a directory is read linearly
// through its data runs (the hash levels only matter to lookups by hash). A
// small directory is stored inline in its inode instead.
//
// A dentry block (struct f2fs_dentry_block, kernel include/linux/f2fs_fs.h) is
//
//	dentry_bitmap[SIZE_OF_DENTRY_BITMAP = 27]
//	reserved[SIZE_OF_RESERVED = 3]
//	dentry[NR_DENTRY_IN_BLOCK = 214]   struct f2fs_dir_entry, 11 bytes:
//	                                   hash_code le32, ino le32, name_len le16, file_type u8
//	filename[214][F2FS_SLOT_LEN = 8]
//
// with NR_DENTRY_IN_BLOCK = 4096*8 / ((11+8)*8 + 1) = 214 and SIZE_OF_RESERVED =
// 4096 - (11+8)*214 - 27 = 3. The bitmap uses the kernel's little-endian bit
// order (test_bit_le: slot s is bit s%8 of byte s/8, least significant bit
// first - unlike the most-significant-first version bitmaps). A name of n bytes
// occupies ceil(n/8) consecutive slots, all with their bit set; only the first
// slot's f2fs_dir_entry is filled in. Deleting an entry clears its bits and
// leaves the dentry and the name bytes in place: that is what the deleted-slot
// scan recovers.
//
// The inline dentry area is laid out by the kernel (make_dentry_ptr_inline)
// from the size of the data area of the inode:
//
//	MAX_INLINE_DATA          = 4 * (addrSlots - DEF_INLINE_RESERVED_SIZE(1))
//	NR_INLINE_DENTRY         = MAX_INLINE_DATA * 8 / ((11 + 8) * 8 + 1)
//	INLINE_DENTRY_BITMAP_SIZE = ceil(NR_INLINE_DENTRY / 8)
//	INLINE_RESERVED_SIZE     = MAX_INLINE_DATA - ((11+8) * NR_INLINE_DENTRY + bitmap size)
//
// bitmap at i_addr[extra/4 + 1], dentries after the bitmap and the reserved
// bytes, then the names. addrSlots is this reader's inode.addrSlots: 923 minus
// the extra attribute words minus the inline xattr reservation, which an inode
// with INLINE_DENTRY always has (50 words unless the inode records its own
// size with the flexible inline xattr feature). For a plain inode: 873 slots,
// MAX_INLINE_DATA 3488, NR_INLINE_DENTRY 182, bitmap 23 bytes, 7 reserved.
const (
	dentryBitmapSize = 27
	dentryReserved   = 3
	nrDentryInBlock  = 214
	dentrySize       = 11
	slotLen          = 8
	dentriesOff      = dentryBitmapSize + dentryReserved
	filenamesOff     = dentriesOff + nrDentryInBlock*dentrySize

	// maxDirBytes is how much of one directory is read; the blocks beyond it
	// are not listed (a warning). 64 MiB is 16384 dentry blocks, at most 3.5M
	// slots, which is inside the 4M slot cap the plan sets per directory.
	maxDirBytes  = 64 << 20
	maxDirBlocks = maxDirBytes / blockSize
	// maxDirBudget is how many bytes of dentry blocks one FS instance reads in
	// total, over all listings and lookups (inline directories live in their
	// inode, which is already read, and cost nothing).
	maxDirBudget = 1 << 30
	// maxDirEntries bounds the entries one scan yields, so a hostile directory
	// cannot make ReadDir build an unbounded list (ext4 parity). Deleted entries
	// may use at most half of it, so at least half of it (maxDirEntries/2) is
	// always left for live entries; live entries are not listed first, they are
	// admitted in on-disk order alongside the deleted ones.
	maxDirEntries = 1 << 18

	encPrefix = "~enc~" // display form of an encrypted name: prefix + base64url(raw)
	rawPrefix = "~raw~" // display form of a name that is not plain text: prefix + base64url(raw)

	flagCasefold = 0x40000000 // F2FS_CASEFOLD_FL (FS_CASEFOLD_FL)

	// File types of a dentry (enum F2FS_FT_*): 0 unknown, 1 regular, 2 dir, 3
	// char, 4 block, 5 fifo, 6 socket, 7 symlink.
	ftMax = 7
)

// dentryID is the Entry.ID of a deleted directory slot. A deleted entry is
// identified by where its record lies, never by the inode it names (which is
// reused, or unknown), so the ID alone says the entry is deleted.
func dentryID(dir uint32, blk int64, slot int) string {
	return "dentry:" + strconv.FormatUint(uint64(dir), 10) + ":" + strconv.FormatInt(blk, 10) + ":" + strconv.Itoa(slot)
}

// area is a region that holds dentries: a dentry block or the inline area.
type area struct {
	b       []byte
	bitmap  int // offsets into b
	ents    int
	names   int
	max     int // slots
	inlined bool
}

func (a area) live(i int) bool { return a.b[a.bitmap+i/8]>>(i%8)&1 != 0 }

// dentry is one record found by a scan. name aliases the buffer the scan read
// into; Entries copy what they keep.
type dentry struct {
	ino     uint32
	name    []byte
	ftype   byte
	deleted bool
	blk     int64 // logical block of the directory (0 for an inline directory)
	slot    int
}

// dirScan walks the dentries of one directory.
type dirScan struct {
	f           *FS
	dir         *inode
	enc         bool
	wantDeleted bool
	visit       func(*dentry) bool // false stops the scan
	count       int                // entries yielded, live and deleted
	deleted     int                // of which deleted
	stop        bool
}

// plausibleName reports whether name can be the name of a deleted slot: not
// "." or "..", and no NUL or '/' except in an encrypted directory, where names
// are ciphertext and any byte can occur.
func plausibleName(name []byte, encrypted bool) bool {
	return !isDot(name) && (encrypted || !bytes.ContainsAny(name, "\x00/"))
}

func isDot(name []byte) bool {
	return len(name) == 1 && name[0] == '.' || len(name) == 2 && name[0] == '.' && name[1] == '.'
}

// scanArea reports the records of one dentry area. first is set for the area
// that starts the directory (logical block 0 or the inline area), where the
// "." and ".." records live.
func (w *dirScan) scanArea(a area, blk int64, first bool) {
	f := w.f
	le := binary.LittleEndian
	bad, firstBad := 0, ""
	note := func(format string, args ...any) {
		if bad == 0 {
			firstBad = fmt.Sprintf(format, args...)
		}
		bad++
	}
	for i := 0; i < a.max && !w.stop; {
		p := a.b[a.ents+i*dentrySize:]
		ino, nl, ft := le.Uint32(p[4:]), int(le.Uint16(p[8:])), p[10]
		if a.live(i) {
			slots := (nl + slotLen - 1) / slotLen
			switch {
			case nl < 1 || nl > fNameLen:
				note("slot %d has name_len %d (want 1..%d)", i, nl, fNameLen)
				i++
				continue
			case i+slots > a.max:
				// The name would run past the filename area; what follows is
				// not a record boundary, so the rest of the area is not read.
				note("slot %d has name_len %d, which runs past the %d slots of the area", i, nl, a.max)
				i = a.max
				continue
			case ino == 0 || ft > ftMax:
				note("slot %d has ino %d and file_type %d", i, ino, ft)
				i += slots
				continue
			}
			name := a.b[a.names+i*slotLen : a.names+i*slotLen+nl]
			if first && i < 2 && isDot(name) {
				i += slots
				continue
			}
			if !w.emit(&dentry{ino: ino, name: name, ftype: ft, blk: blk, slot: i}) {
				return
			}
			i += slots
			continue
		}
		// A free slot: a deleted record leaves its dentry and name in place.
		if !w.wantDeleted || ino == 0 || nl < 1 || nl > fNameLen || ft < 1 || ft > ftMax || uint64(ino) >= f.sb.natCapacity() {
			i++
			continue
		}
		slots := (nl + slotLen - 1) / slotLen
		if i+slots > a.max {
			i++
			continue
		}
		name := a.b[a.names+i*slotLen : a.names+i*slotLen+nl]
		if !plausibleName(name, w.enc) {
			i++
			continue
		}
		// Overwritten by a live record: the stale record is gone.
		overlap := false
		for s := i + 1; s < i+slots; s++ {
			if a.live(s) {
				overlap = true
				break
			}
		}
		if overlap {
			i++
			continue
		}
		if !w.emit(&dentry{ino: ino, name: name, ftype: ft, deleted: true, blk: blk, slot: i}) {
			return
		}
		i += slots
	}
	if bad > 0 {
		where := fmt.Sprintf("block %d", blk)
		if a.inlined {
			where = "inline dentries"
		}
		f.warn("directory inode %d, %s: %d damaged dentry slots, not listed (first: %s)", w.dir.nid, where, bad, firstBad)
	}
}

// emit passes r to the visitor, enforcing the per-scan entry cap (limit).
// Deleted records are admitted only up to limit/2 (and while the total is below
// limit); beyond that they are skipped with a warning and the scan goes on. The
// guarantee is: whatever the directory holds, at least limit/2 live entries are
// listed (every live entry is admitted while the total, deleted ones included,
// is below limit, and the deleted ones are at most limit/2 of it). It is not
// that live entries come before deleted ones: records are visited in on-disk
// order. A live entry beyond the cap ends the scan, with a warning.
func (w *dirScan) emit(r *dentry) bool {
	limit := w.f.dirEntryCap()
	if r.deleted {
		if w.deleted >= limit/2 || w.count >= limit {
			w.f.warn("directory inode %d has more than %d deleted entries: the rest of them are not listed", w.dir.nid, limit/2)
			return true
		}
		w.deleted++
	} else if w.count >= limit {
		w.f.warn("directory inode %d has more than %d entries: the rest is not listed", w.dir.nid, limit)
		w.stop = true
		return false
	}
	w.count++
	if !w.visit(r) {
		w.stop = true
		return false
	}
	return true
}

// chargeDir takes n bytes from the per-FS directory read budget.
func (f *FS) chargeDir(n int64) bool {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if f.dirBudget < n {
		return false
	}
	f.dirBudget -= n
	return true
}

// scanDir calls visit for every live record of directory dir and, with
// wantDeleted, for the deleted records recovered from free slots, in on-disk
// order, until visit returns false.
//
// Damage inside a block is a warning and ends the read of the damaged part
// only. A damaged block map keeps the blocks mapped before the damage (a
// warning); holes are skipped; a directory over 64 MiB is cut off (a warning).
// The per-FS directory read budget (1 GiB) is charged per block: when it runs
// out before the first block was read the scan fails with a
// *filesys.CorruptError (never an empty listing), later a warning ends it.
//
// A dentry block belongs to one directory: the first directory that reads a
// block claims it (a per-FS map from block address to directory nid), and a
// block that a later scan of another directory maps, or that the same scan maps
// twice, is skipped with a warning. Without this a hostile image that points
// many i_addr slots (or nested directories) at one dentry block would multiply
// its entries by the number of references. A claim is made only after the block
// is charged to the read budget, so the map holds at most
// maxDirBudget/blockSize addresses.
func (f *FS) scanDir(dir *inode, wantDeleted bool, visit func(*dentry) bool) error {
	_, err := f.scanDirBudget(dir, wantDeleted, visit)
	return err
}

// scanDirBudget is scanDir that also reports whether the scan stopped short
// because the directory read budget ran out after the first block, so a caller
// that needs the whole directory (Lookup) can tell "not found" from "not looked
// at everything".
func (f *FS) scanDirBudget(dir *inode, wantDeleted bool, visit func(*dentry) bool) (budgetCut bool, err error) {
	w := &dirScan{f: f, dir: dir, enc: dir.encrypted, wantDeleted: wantDeleted, visit: visit}
	if dir.inline&inlineDentry != 0 {
		return false, w.inline()
	}
	if dir.size == 0 {
		return false, nil
	}
	nb := (dir.size-1)/blockSize + 1
	if nb > maxDirBlocks {
		f.warn("directory inode %d is %d bytes: only the first 64 MiB are read", dir.nid, dir.size)
		nb = maxDirBlocks
	}
	capped := *dir
	capped.size = nb * blockSize // whole blocks, so every run covers whole blocks
	runs, err := f.runs(&capped)
	if err != nil {
		if !errors.Is(err, filesys.ErrCorrupt) {
			return false, err
		}
		f.warn("directory inode %d: its block map is damaged (%v); only the entries in the blocks mapped before the damage are listed", dir.nid, err)
	}
	buf := make([]byte, blockSize)
	seen := map[int64]struct{}{} // blocks this scan has read (at most maxDirBlocks)
	var blk int64
	read := false
	for _, r := range runs {
		for off := int64(0); off < r.Length && !w.stop; off += blockSize {
			idx := blk
			blk++
			if r.Offset < 0 {
				continue // a hole
			}
			addr := r.Offset + off
			if owner, shared := f.dentryBlockOwner(addr, dir.nid, seen); shared {
				f.warn("dentry block %d shared by directories %d and %d; skipped", addr/blockSize, owner, dir.nid)
				continue
			}
			if !f.chargeDir(blockSize) {
				f.warn("directory read budget of %d bytes per filesystem exhausted (directory inode %d); directories are no longer read in full", int64(maxDirBudget), dir.nid)
				if !read {
					return true, corrupt("f2fs directory", -1, "directory read budget exhausted before directory inode %d was read", dir.nid)
				}
				return true, nil
			}
			read = true
			f.claimDentryBlock(addr, dir.nid)
			seen[addr] = struct{}{}
			if err := readFull(f.data, buf, addr); err != nil {
				if isIOError(err) {
					return false, readError("f2fs directory", addr, err)
				}
				f.warn("directory inode %d: block %d is unreadable (%v); the rest is not listed", dir.nid, idx, err)
				return false, nil
			}
			w.scanArea(area{b: buf, bitmap: 0, ents: dentriesOff, names: filenamesOff, max: nrDentryInBlock}, idx, idx == 0)
		}
		if w.stop {
			break
		}
	}
	return false, nil
}

// dentryBlockOwner reports whether the dentry block at byte address addr must
// not be read for directory nid: this scan already read it (seen; the owner is
// then nid itself) or another directory claimed it.
func (f *FS) dentryBlockOwner(addr int64, nid uint32, seen map[int64]struct{}) (owner uint32, shared bool) {
	if _, dup := seen[addr]; dup {
		return nid, true
	}
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if o, ok := f.dirClaims[addr]; ok && o != nid {
		return o, true
	}
	return 0, false
}

// claimDentryBlock records directory nid as the owner of the dentry block at
// byte address addr; the first claim stays.
func (f *FS) claimDentryBlock(addr int64, nid uint32) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if f.dirClaims == nil {
		f.dirClaims = make(map[int64]uint32)
	}
	if _, ok := f.dirClaims[addr]; !ok {
		f.dirClaims[addr] = nid
	}
}

// inline scans the dentries stored in the inode.
func (w *dirScan) inline() error {
	in := w.dir
	maxData := (in.addrSlots - inlineReserve) * 4
	if maxData <= 0 {
		return corrupt("f2fs inline dentry", -1, "inode %d has no room for inline dentries (%d data slots)", in.nid, in.addrSlots)
	}
	nr := maxData * 8 / ((dentrySize+slotLen)*8 + 1)
	bm := (nr + 7) / 8
	reserved := maxData - ((dentrySize+slotLen)*nr + bm)
	if nr < 1 || reserved < 0 {
		return corrupt("f2fs inline dentry", -1, "inode %d: %d bytes of inline data hold no dentries", in.nid, maxData)
	}
	start := in.addrStart + inlineReserve*4
	b := in.raw[start : start+maxData]
	ents := bm + reserved
	w.scanArea(area{b: b, bitmap: 0, ents: ents, names: ents + dentrySize*nr, max: nr, inlined: true}, 0, true)
	return nil
}

// displayName returns the Entry.Name for an on-disk name and the RawName to
// keep. Names in an encrypted directory are ciphertext and always shown as
// "~enc~" + base64url(raw). Other names are shown as they are when they are
// valid UTF-8 without NUL or '/' and are not "." or ".."; any other byte string
// is shown losslessly as "~raw~" + base64url(raw). Lookup accepts both forms.
func displayName(raw []byte, encrypted bool) (string, []byte) {
	switch {
	case encrypted:
		return encPrefix + base64.RawURLEncoding.EncodeToString(raw), slices.Clone(raw)
	case plainName(raw):
		return string(raw), nil
	}
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), slices.Clone(raw)
}

// plainName reports whether a name of a non-encrypted directory is shown as it
// is (valid UTF-8 without NUL or '/', not empty, not "." or "..").
func plainName(raw []byte) bool {
	return len(raw) > 0 && utf8.Valid(raw) && !bytes.ContainsAny(raw, "\x00/") && !isDot(raw)
}

// dirEntryCap is the per-scan entry cap.
func (f *FS) dirEntryCap() int {
	if f.dirCap > 0 {
		return f.dirCap
	}
	return maxDirEntries
}

// ftypeToEntry maps a dentry file_type to an entry type.
func ftypeToEntry(ft byte) filesys.EntryType {
	switch ft {
	case 1:
		return filesys.TypeFile
	case 2:
		return filesys.TypeDir
	case 7:
		return filesys.TypeSymlink
	}
	return filesys.TypeOther
}

func addAttr(e *filesys.Entry, key, value string) {
	e.Attrs = append(e.Attrs, filesys.KV{Key: key, Value: value})
}

// dirEntry builds the Entry for a dentry of directory dir, reading the inode it
// names as it is now. An inode that cannot be read (free, outside the NAT,
// not an inode, damaged) does not hide the entry: its type comes from the
// dentry's file_type and the attr inode=unreadable says so. Only a genuine I/O
// error is returned.
func (f *FS) dirEntry(d *dentry, dir *inode) (filesys.Entry, error) {
	name, raw := displayName(d.name, dir.encrypted)
	in, err := f.inode(d.ino)
	if err != nil && !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrNotFound) {
		return filesys.Entry{}, err
	}
	var e filesys.Entry
	if err != nil {
		e = filesys.Entry{Name: name, RawName: raw, ID: "nid:" + strconv.FormatUint(uint64(d.ino), 10), Type: ftypeToEntry(d.ftype)}
		if d.deleted {
			addAttr(&e, "inode_unreadable", "true")
		} else {
			addAttr(&e, "inode", "unreadable")
		}
	} else {
		e = toEntry(name, raw, in)
	}
	e.Encrypted = e.Encrypted || dir.encrypted
	if d.deleted {
		// The ID is the slot's location, so Deleted is derivable from it (Open
		// and ReadDir never trust the Entry's own Deleted field); the inode the
		// slot named stays an attribute.
		e.Deleted = true
		e.ID = dentryID(dir.nid, d.blk, d.slot)
		addAttr(&e, "inode", strconv.FormatUint(uint64(d.ino), 10))
		addAttr(&e, "dentry", strconv.FormatInt(d.blk, 10)+":"+strconv.Itoa(d.slot))
		if err == nil && (in.pino != dir.nid || in.nameBad || !bytes.Equal(in.name, d.name)) {
			addAttr(&e, "inode_reused", "true")
		}
	}
	return e, nil
}

// Root returns the root directory. If its inode cannot be read the entry says
// so and ReadDir reports the error.
func (f *FS) Root() filesys.Entry {
	in, err := f.inode(f.sb.rootIno)
	if err != nil {
		e := filesys.Entry{ID: "nid:" + strconv.FormatUint(uint64(f.sb.rootIno), 10), Type: filesys.TypeDir}
		addAttr(&e, "inode", "unreadable")
		return e
	}
	return toEntry("", nil, in)
}

// dirInode returns the inode of directory entry dir. Only dir.ID and the
// on-disk inode are used: a "dentry:" ID is a deleted entry (ErrDeleted), a
// "nid:<n>" ID is re-read, so a forged Deleted, Type or Size on the Entry
// changes nothing and a malformed ID costs nothing.
func (f *FS) dirInode(dir filesys.Entry) (*inode, error) {
	if parseDentryID(dir.ID) {
		return nil, filesys.ErrDeleted
	}
	in, err := f.inodeByID(dir.ID)
	if err != nil {
		return nil, err
	}
	if in.typ() != modeDir {
		return nil, fmt.Errorf("%w: inode %d is not a directory", filesys.ErrUnsupported, in.nid)
	}
	return in, nil
}

// ReadDir lists the live entries of dir and the deleted ones recovered from
// free dentry slots (Deleted is set, the ID is "dentry:<dir ino>:<block>:<slot>"
// and the attrs say which inode the slot named). "." and ".." are omitted.
// Entries are in on-disk order. dir is identified by its ID alone. A deleted
// entry whose inode is still in the NAT is described by that inode, with attr
// inode_reused=true when the inode no longer belongs to this directory and name.
// Damage inside a block is an Info warning, not an error; a directory whose
// read budget is spent before its first block yields a *filesys.CorruptError.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	in, err := f.dirInode(dir)
	if err != nil {
		return nil, err
	}
	var out []filesys.Entry
	var ioErr error
	if err := f.scanDir(in, true, func(d *dentry) bool {
		e, err := f.dirEntry(d, in)
		if err != nil {
			ioErr = err
			return false
		}
		out = append(out, e)
		return true
	}); err != nil {
		return nil, err
	}
	if ioErr != nil {
		return nil, ioErr
	}
	return out, nil
}

// Lookup resolves an absolute slash path. Within each directory the live
// entries are searched in this order of preference over the whole directory:
//
//  1. an exact byte match (never in an encrypted directory, whose names are
//     ciphertext, and never for "." or "..");
//  2. the display form of ReadDir: "~enc~"+base64url in encrypted directories,
//     "~raw~"+base64url elsewhere (decoded strictly: one spelling per name; in
//     a plain directory "~raw~" stands only for the names ReadDir shows that
//     way), so a real name that happens to look like a display form wins over
//     the name that form stands for;
//  3. in a casefold directory (never for "." or ".."), strings.EqualFold
//     when both names are valid UTF-8: an approximation of the filesystem's
//     Unicode case folding, which it does not implement.
//
// Empty components are ignored, "." and ".." are not special, and symlinks are
// not followed. Deleted entries are never found.
func (f *FS) Lookup(p string) (filesys.Entry, error) {
	cur := f.Root()
	for _, comp := range strings.Split(p, "/") {
		if comp == "" {
			continue
		}
		if cur.Type != filesys.TypeDir {
			return filesys.Entry{}, fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
		}
		next, err := f.child(cur, comp)
		if err != nil {
			if errors.Is(err, errNoChild) {
				err = fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
			}
			return filesys.Entry{}, err
		}
		cur = next
	}
	return cur, nil
}

var errNoChild = errors.New("no such entry")

// child finds the live entry of dir named comp (see Lookup for the order).
func (f *FS) child(dir filesys.Entry, comp string) (filesys.Entry, error) {
	in, err := f.dirInode(dir)
	if err != nil {
		return filesys.Entry{}, err
	}
	enc := in.encrypted
	exact := []byte(comp)
	exactOK := !enc && !isDot(exact)
	// "." and ".." are never found, by case folding either.
	casefold := in.flags&flagCasefold != 0 && exactOK && utf8.ValidString(comp)
	var alt []byte // the bytes a display form stands for
	prefix := rawPrefix
	if enc {
		prefix = encPrefix
	}
	if rest, ok := strings.CutPrefix(comp, prefix); ok {
		// Each name has one spelling: the text must be exactly what encoding the
		// decoded bytes gives. (The decoder skips embedded CR/LF and, even strict,
		// would accept them; the round trip rejects those and any other variant.)
		if b, err := base64.RawURLEncoding.Strict().DecodeString(rest); err == nil && base64.RawURLEncoding.EncodeToString(b) == rest {
			alt = b
		}
	}
	var exactRec, altRec, foldRec *dentry
	keep := func(d *dentry) *dentry {
		c := *d
		c.name = slices.Clone(d.name)
		return &c
	}
	budgetCut, err := f.scanDirBudget(in, false, func(d *dentry) bool {
		switch {
		case exactOK && bytes.Equal(d.name, exact):
			exactRec = keep(d)
			return false
		// In a plain directory the "~raw~" form stands only for the names
		// ReadDir shows in that form, never for a plain name.
		case altRec == nil && alt != nil && bytes.Equal(d.name, alt) && (enc || !plainName(d.name)):
			altRec = keep(d)
		case foldRec == nil && casefold && utf8.Valid(d.name) && strings.EqualFold(string(d.name), comp):
			foldRec = keep(d)
		}
		return true
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	for _, d := range []*dentry{exactRec, altRec, foldRec} {
		if d != nil {
			return f.dirEntry(d, in)
		}
	}
	if budgetCut {
		// The directory was not read to the end: "no such entry" would be a claim
		// about the whole directory that was not checked.
		return filesys.Entry{}, corrupt("f2fs directory", -1, "directory read budget exhausted while looking up %q in directory inode %d", comp, in.nid)
	}
	return filesys.Entry{}, errNoChild
}
