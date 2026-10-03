package ext4

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Directory constants.
const (
	inodeFlagIndex    = 0x1000     // EXT4_INDEX_FL: an htree directory
	inodeFlagCasefold = 0x40000000 // EXT4_CASEFOLD_FL

	rootInode = 2

	direntHeader = 8  // inode, rec_len, name_len, file_type
	direntMin    = 12 // the smallest valid record
	dirTailLen   = 12 // the metadata_csum tail pseudo-entry
	direntTailFT = 0xDE

	// maxDirBytes is how much of one directory is read.
	maxDirBytes = 64 << 20
	// maxDirRecords bounds the entries (live and deleted) one directory yields,
	// so a hostile directory cannot make ReadDir build an unbounded list.
	maxDirRecords = 1 << 20
	// maxLinkTarget bounds the symlink target read for an Entry (PATH_MAX).
	maxLinkTarget = 4096

	encPrefix = "~enc~" // display form of an encrypted name: prefix + base64url(raw)
	rawPrefix = "~raw~" // display form of a name that is not valid UTF-8
)

// inodeID is the Entry.ID of inode n.
func inodeID(n uint32) string { return "inode:" + strconv.FormatUint(uint64(n), 10) }

// dirRec is one directory record found by a scan. name aliases the block
// buffer; entries copy what they keep.
type dirRec struct {
	inode   uint32
	name    []byte
	ftype   byte
	deleted bool
	blk     int64 // physical block, or -1 for an inline directory
	off     int   // offset in the block, or in the inline data
	csumBad bool  // the block's checksum is bad or missing

	dirInode uint32 // the directory the record was read from
}

// region is one run of directory records: a directory block, or one part of an
// inline directory.
type region struct {
	b       []byte
	limit   int   // records are read below this offset (the checksum tail is excluded)
	blk     int64 // physical block; -1 for inline data
	base    int   // offset of b in the inline data
	slack   bool  // look for deleted records in the slack of each record
	csumBad bool
	what    string // for warnings: "block 12" or "inline data"
}

// dirScan walks the records of one directory.
type dirScan struct {
	f           *FS
	in          *inode
	wantDeleted bool
	visit       func(*dirRec) bool
	index       bool   // the directory has the INDEX flag
	seed        uint32 // checksum seed of the directory's blocks
	count       int
	stop        bool
}

// scanDir calls visit for every live record of directory in and, with
// wantDeleted, for the deleted records found in slack space, in on-disk order,
// until visit returns false. Damage inside a block is a warning and ends the
// read of that block only; a directory whose blocks cannot be mapped is an
// error. Holes in a sparse directory are skipped.
func (f *FS) scanDir(in *inode, wantDeleted bool, visit func(*dirRec) bool) error {
	w := &dirScan{f: f, in: in, wantDeleted: wantDeleted, visit: visit, index: in.flags&inodeFlagIndex != 0}
	if f.sb.metadataCsum() {
		w.seed = f.inodeSeed(in)
	}
	if in.flags&inodeFlagInlineData != 0 {
		return w.inline()
	}
	runs, err := f.dirRuns(in)
	if err != nil {
		return err
	}
	bs := int64(f.sb.blockSize)
	var lblk int64
	for _, r := range runs {
		if r.Offset < 0 {
			f.warn("directory inode %d: no blocks at logical blocks %d-%d (sparse directory)", in.num, lblk, lblk+r.Length/bs-1)
			lblk += r.Length / bs
			continue
		}
		for pos := int64(0); pos+bs <= r.Length && !w.stop; pos += bs {
			buf := make([]byte, bs) // one block: bounded by the block size
			if err := readFull(f.r, buf, r.Offset+pos); err != nil {
				f.warn("directory inode %d: block %d is unreadable: %v", in.num, (r.Offset+pos)/bs, err)
			} else {
				w.block(buf, (r.Offset+pos)/bs, lblk)
			}
			lblk++
		}
	}
	return nil
}

// dirRuns returns the runs of directory in's data, read up to the lesser of
// i_size and maxDirBytes (a warning when truncated) and rounded up to whole
// blocks. A run with Offset -1 is a hole.
func (f *FS) dirRuns(in *inode) ([]filesys.Run, error) {
	size := in.size
	if size > maxDirBytes {
		f.warn("directory inode %d: i_size %d exceeds the 64 MiB directory limit; only the first 64 MiB are read", in.num, in.size)
		size = maxDirBytes
	}
	bs := int64(f.sb.blockSize)
	size = (size + bs - 1) / bs * bs // <= 64 MiB + a block: cannot overflow
	c := *in
	c.size = size
	return f.runs(&c)
}

// inline scans an inline directory: the parent inode number (4 bytes), then the
// records in the rest of i_block, then the records in the system.data value;
// the records of the two parts never share a record.
func (w *dirScan) inline() error {
	data, err := w.f.inlineData(w.in)
	if err != nil {
		return err
	}
	if len(data) < 4 {
		return corrupt("ext4 inline directory", w.in.offset, "inode %d: %d bytes of inline data, no room for the parent inode", w.in.num, len(data))
	}
	first := data[4:min(len(data), inodeBlockLen)]
	w.records(&region{b: first, limit: len(first), blk: -1, base: 4, slack: true, what: "inline data"})
	if len(data) > inodeBlockLen && !w.stop {
		rest := data[inodeBlockLen:]
		w.records(&region{b: rest, limit: len(rest), blk: -1, base: inodeBlockLen, slack: true, what: "inline data"})
	}
	return nil
}

// block checks one directory block and reads its records. blk is the physical
// block, lblk the logical one.
func (w *dirScan) block(buf []byte, blk, lblk int64) {
	g := &region{b: buf, limit: len(buf), blk: blk, slack: true, what: "block " + strconv.FormatInt(blk, 10)}
	switch {
	case w.index && w.isDX(buf, lblk):
		// An htree index block (the root, or an interior node): not a list of
		// entries to look through for deleted ones, and it carries a dx_tail
		// checksum rather than a dirent tail, which is not verified.
		g.slack = false
	case w.f.sb.metadataCsum():
		n := len(buf)
		tail := buf[n-dirTailLen:]
		le := binary.LittleEndian
		if le.Uint32(tail) == 0 && le.Uint16(tail[4:]) == dirTailLen && tail[6] == 0 && tail[7] == direntTailFT {
			g.limit = n - dirTailLen
			if stored, want := le.Uint32(tail[8:]), rawCRC32C(w.seed, buf[:n-dirTailLen]); stored != want {
				g.csumBad = true
				w.f.warn("directory inode %d, %s: checksum mismatch (stored %#08x, computed %#08x)", w.in.num, g.what, stored, want)
			}
		} else {
			g.csumBad = true
			w.f.warn("directory inode %d, %s: checksum mismatch (no checksum tail)", w.in.num, g.what)
		}
	}
	w.records(g)
}

// isDX reports whether buf is a block of an htree index: the root (logical
// block 0: "." and ".." followed by the dx_root_info and the index) or an
// interior node (one empty record spanning the block, then the index). The
// index header (limit, count) is checked against what the block size dictates,
// so a leaf whose entries were all deleted is not mistaken for an index block.
func (w *dirScan) isDX(b []byte, lblk int64) bool {
	le := binary.LittleEndian
	bs := len(b)
	tail := 0
	if w.f.sb.metadataCsum() {
		tail = 1 // the dx_tail takes one slot of the index
	}
	countLimit := func(off int) bool {
		if off+4 > bs {
			return false
		}
		limit, count := int(le.Uint16(b[off:])), int(le.Uint16(b[off+2:]))
		return limit == (bs-off)/8-tail && count >= 1 && count <= limit
	}
	if bs < 40 {
		return false
	}
	if le.Uint32(b) == 0 && w.recLen(le.Uint16(b[4:])) == bs && b[6] == 0 && countLimit(8) {
		return true
	}
	return lblk == 0 &&
		le.Uint16(b[4:]) == 12 && b[6] == 1 && b[8] == '.' &&
		w.recLen(le.Uint16(b[16:])) == bs-12 && b[18] == 2 && b[20] == '.' && b[21] == '.' &&
		le.Uint32(b[24:]) == 0 && b[29] == 8 && countLimit(32)
}

// recLen decodes an on-disk rec_len for this filesystem's block size.
func (w *dirScan) recLen(raw uint16) int { return recLenFromDisk(raw, w.f.sb.blockSize) }

// recLenFromDisk decodes an on-disk rec_len: with 64 KiB blocks the value does
// not fit 16 bits, so 0 and 65535 mean the whole block and the low two bits
// carry bits 16-17 (the kernel's ext4_rec_len_from_disk).
func recLenFromDisk(raw uint16, blockSize int) int {
	if blockSize >= 1<<16 {
		if raw == 0 || raw == 0xFFFF {
			return 1 << 16
		}
		return int(raw&0xFFFC) | int(raw&3)<<16
	}
	return int(raw)
}

// nameLen decodes name_len and file_type of the record at b: name_len is 8 bits
// followed by the file type with the filetype feature, else a 16-bit name_len.
func (w *dirScan) nameLen(b []byte) (n int, ftype byte) {
	if w.f.sb.hasIncompat(incompatFiletype) {
		return int(b[6]), b[7]
	}
	return int(binary.LittleEndian.Uint16(b[6:])), 0
}

// records reads the records of g in order, emitting live ones and, when the
// slack of a record may hold deleted records, those too.
func (w *dirScan) records(g *region) {
	b := g.b
	n := len(b)
	for off := 0; off < g.limit && !w.stop; {
		if off+direntMin > n {
			w.f.warn("directory inode %d, %s: %d trailing bytes at offset %d are too short for a record", w.in.num, g.what, n-off, off)
			return
		}
		inode := binary.LittleEndian.Uint32(b[off:])
		recLen := w.recLen(binary.LittleEndian.Uint16(b[off+4:]))
		if recLen < direntMin || recLen%4 != 0 || off+recLen > n {
			w.f.warn("directory inode %d, %s: invalid rec_len %d at offset %d; the rest of the block is not read", w.in.num, g.what, recLen, off)
			return
		}
		nameLen, ftype := w.nameLen(b[off:])
		if direntHeader+nameLen > recLen {
			w.f.warn("directory inode %d, %s: name_len %d does not fit rec_len %d at offset %d; the record is skipped", w.in.num, g.what, nameLen, recLen, off)
			off += recLen
			continue
		}
		name := b[off+direntHeader : off+direntHeader+nameLen]
		switch {
		case inode != 0 && !isDot(name):
			w.emit(g, &dirRec{inode: inode, name: name, ftype: ftype, off: off})
		case inode == 0 && off == 0 && g.slack && w.wantDeleted && nameLen > 0 && plausibleName(name):
			// The first record of a block, deleted: the kernel zeroed its
			// inode, so only the name, type and place survive. It is reported
			// as a deleted entry of unknown inode (see unknownInodeEntry).
			w.emit(g, &dirRec{name: name, ftype: ftype, deleted: true, off: off})
		}
		// The slack after a live record holds records deleted by merging them
		// into it. A first record with a zeroed inode (the kernel zeroes the
		// inode of a deleted first record) merged its successors the same way.
		if g.slack && w.wantDeleted && !w.stop && (inode != 0 || off == 0) {
			w.slackScan(g, off+(direntHeader+nameLen+3)&^3, off+recLen)
		}
		off += recLen
	}
}

// slackScan looks for deleted records in b[from:to], at 4-byte steps. A
// candidate needs an inode in [1, inodes_count], a name that is non-empty,
// fits in the slack and has no NUL or '/'; its rec_len is ignored (a deleted
// record keeps its old one). After an accepted candidate the scan resumes past
// its minimal length.
func (w *dirScan) slackScan(g *region, from, to int) {
	b := g.b
	for q := from; q+direntHeader < to && !w.stop; {
		inode := binary.LittleEndian.Uint32(b[q:])
		nameLen, ftype := w.nameLen(b[q:])
		end := q + direntHeader + nameLen
		if inode >= 1 && inode <= w.f.sb.inodesCount && nameLen > 0 && end <= to && plausibleName(b[q+direntHeader:end]) {
			w.emit(g, &dirRec{inode: inode, name: b[q+direntHeader : end], ftype: ftype, deleted: true, off: q})
			q += (direntHeader + nameLen + 3) &^ 3
			continue
		}
		q += 4
	}
}

// emit hands one record to the visitor, within the per-directory record cap.
func (w *dirScan) emit(g *region, r *dirRec) {
	if w.count >= w.f.dirRecordCap {
		w.f.warn("directory inode %d: more than %d entries; the rest are not read", w.in.num, w.f.dirRecordCap)
		w.stop = true
		return
	}
	w.count++
	r.blk, r.csumBad, r.dirInode = g.blk, g.csumBad, w.in.num
	if g.blk < 0 {
		r.off += g.base
	}
	if !w.visit(r) {
		w.stop = true
	}
}

func isDot(name []byte) bool {
	return len(name) == 1 && name[0] == '.' || len(name) == 2 && name[0] == '.' && name[1] == '.'
}

// plausibleName reports whether name can be the name of a deleted record: no
// NUL or '/', and not "." or "..".
func plausibleName(name []byte) bool {
	return !bytes.ContainsAny(name, "\x00/") && !isDot(name)
}

// displayName returns the Entry.Name for an on-disk name and the RawName to
// keep. Names in an encrypted directory are ciphertext and always shown as
// "~enc~" + base64url(raw). Other names are shown as they are when they are
// valid UTF-8 without NUL or '/' (RawName is then nil); any other byte string
// is shown losslessly as "~raw~" + base64url(raw). Both forms are accepted by
// Lookup.
func displayName(raw []byte, encrypted bool) (string, []byte) {
	switch {
	case encrypted:
		return encPrefix + base64.RawURLEncoding.EncodeToString(raw), slices.Clone(raw)
	case len(raw) > 0 && utf8.Valid(raw) && !bytes.ContainsAny(raw, "\x00/"):
		return string(raw), nil
	}
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), slices.Clone(raw)
}

// direntType maps a dirent file_type to an entry type.
func direntType(ft byte) filesys.EntryType {
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

// addAttr appends a detail unless the entry already has it.
func addAttr(e *filesys.Entry, key, value string) {
	kv := filesys.KV{Key: key, Value: value}
	if !slices.Contains(e.Attrs, kv) {
		e.Attrs = append(e.Attrs, kv)
	}
}

// dirEntry builds the Entry for a directory record, reading the inode it
// names as it is now. dirEnc marks a record of an encrypted directory.
func (f *FS) dirEntry(r *dirRec, dirEnc bool) filesys.Entry {
	name, raw := displayName(r.name, dirEnc)
	if r.inode == 0 {
		return unknownInodeEntry(r, name, raw, dirEnc)
	}
	in, err := f.inode(r.inode)
	var e filesys.Entry
	if err != nil {
		e = filesys.Entry{Name: name, RawName: raw, ID: inodeID(r.inode), Type: direntType(r.ftype)}
		addAttr(&e, "inode", "unreadable")
	} else {
		e = toEntry(name, raw, in)
		if in.mode&modeTypeMask == modeSymlink && !r.deleted {
			e.LinkTarget = f.linkTarget(in)
		}
	}
	e.Encrypted = e.Encrypted || dirEnc
	if r.deleted {
		e.Deleted = true
		if err == nil && in.links > 0 && in.orphanNext == 0 {
			addAttr(&e, "inode_reused", "true")
		}
		if r.blk >= 0 {
			addAttr(&e, "dirent", fmt.Sprintf("%d:%d", r.blk, r.off))
		} else {
			addAttr(&e, "dirent", fmt.Sprintf("inline:%d", r.off))
		}
	}
	if r.csumBad {
		addAttr(&e, "checksum", "bad")
	}
	return e
}

// unknownInodeEntry is the Entry of a deleted first record of a block, whose
// inode number the kernel zeroed. Nothing is known beyond the name, the
// dirent's file_type (an unknown type without the filetype feature) and where
// the record lies, so the ID is that location ("dirent:<block>:<offset>", or
// "dirent:inline:<dir inode>:<offset>"), attr inode=unknown marks it, and no
// inode-derived field is set.
func unknownInodeEntry(r *dirRec, name string, raw []byte, dirEnc bool) filesys.Entry {
	e := filesys.Entry{Name: name, RawName: raw, Type: direntType(r.ftype), Deleted: true, Encrypted: dirEnc}
	if r.blk >= 0 {
		e.ID = fmt.Sprintf("dirent:%d:%d", r.blk, r.off)
		addAttr(&e, "dirent", fmt.Sprintf("%d:%d", r.blk, r.off))
	} else {
		e.ID = fmt.Sprintf("dirent:inline:%d:%d", r.dirInode, r.off)
		addAttr(&e, "dirent", fmt.Sprintf("inline:%d", r.off))
	}
	addAttr(&e, "inode", "unknown")
	if r.csumBad {
		addAttr(&e, "checksum", "bad")
	}
	return e
}

// linkTarget returns the target of symlink inode in, or "" when it is
// encrypted, too long or unreadable.
func (f *FS) linkTarget(in *inode) string {
	if in.flags&inodeFlagEncrypt != 0 || in.size <= 0 || in.size > maxLinkTarget {
		return ""
	}
	fl, err := f.openInode(in)
	if err != nil {
		return ""
	}
	buf := make([]byte, fl.Size())
	if n, err := fl.ReadAt(buf, 0); n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
		return ""
	}
	return string(buf)
}

// Root returns the root directory (inode 2). If the inode cannot be read the
// entry says so and ReadDir reports the error.
func (f *FS) Root() filesys.Entry {
	in, err := f.inode(rootInode)
	if err != nil {
		e := filesys.Entry{ID: inodeID(rootInode), Type: filesys.TypeDir}
		addAttr(&e, "inode", "unreadable")
		return e
	}
	return toEntry("", nil, in)
}

// dirInode returns the inode of directory entry dir.
func (f *FS) dirInode(dir filesys.Entry) (*inode, error) {
	if dir.Deleted {
		return nil, filesys.ErrDeleted
	}
	n, err := parseInodeID(dir.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(n)
	if err != nil {
		return nil, err
	}
	if in.mode&modeTypeMask != modeDir {
		return nil, fmt.Errorf("%w: inode %d is not a directory", filesys.ErrUnsupported, n)
	}
	return in, nil
}

// ReadDir lists the live entries of dir and the deleted ones found in the
// slack of its blocks (Deleted is set; see dirEntry for the attributes). "."
// and ".." are omitted. Entries are in on-disk order; htree directories are
// read by a linear scan of all blocks. The entry names the inode as it is now,
// so a deleted entry whose inode was reused shows the new inode.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	in, err := f.dirInode(dir)
	if err != nil {
		return nil, err
	}
	enc := in.flags&inodeFlagEncrypt != 0
	var out []filesys.Entry
	if err := f.scanDir(in, true, func(r *dirRec) bool {
		out = append(out, f.dirEntry(r, enc))
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Lookup resolves an absolute slash path. Each component is matched against the
// live entries of its directory by exact bytes, or by the display forms of
// ReadDir ("~enc~" names in encrypted directories, "~raw~" names elsewhere);
// directories with the casefold flag also match with strings.EqualFold, which
// approximates the filesystem's Unicode folding. Empty components are ignored,
// "." and ".." are not special, and symlinks are not followed.
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

// child finds the live entry of dir named comp.
func (f *FS) child(dir filesys.Entry, comp string) (filesys.Entry, error) {
	in, err := f.dirInode(dir)
	if err != nil {
		return filesys.Entry{}, err
	}
	enc := in.flags&inodeFlagEncrypt != 0
	casefold := in.flags&inodeFlagCasefold != 0 && !enc
	exact := []byte(comp)
	var alt []byte // the bytes a display form stands for
	prefix := rawPrefix
	if enc {
		prefix = encPrefix
	}
	if rest, ok := strings.CutPrefix(comp, prefix); ok {
		if b, err := base64.RawURLEncoding.DecodeString(rest); err == nil {
			alt = b
		}
	}
	var found *dirRec
	var rec dirRec
	err = f.scanDir(in, false, func(r *dirRec) bool {
		if bytes.Equal(r.name, exact) || (alt != nil && bytes.Equal(r.name, alt)) ||
			(casefold && strings.EqualFold(string(r.name), comp)) {
			rec = *r
			rec.name = slices.Clone(r.name)
			found = &rec
			return false
		}
		return true
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	if found == nil {
		return filesys.Entry{}, errNoChild
	}
	return f.dirEntry(found, enc), nil
}
