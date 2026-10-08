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
	// maxDirRecords bounds the entries one directory yields (live and deleted
	// for ReadDir, live for Lookup), so a hostile directory cannot make ReadDir
	// build an unbounded list. A directory with more entries is cut off at the
	// cap with a warning, for Lookup too: an entry beyond the cap is reported as
	// not found.
	maxDirRecords = 1 << 18
	// maxSlackScan bounds the slack bytes one directory has searched for deleted
	// entries. Slack is normally a few bytes per record, so a directory that
	// exceeds it is hostile or badly damaged; the rest is not searched.
	maxSlackScan = 16 << 20
	// maxLinkTarget bounds the symlink target read for an Entry (PATH_MAX).
	maxLinkTarget = 4096

	encPrefix = "~enc~" // display form of an encrypted name: prefix + base64url(raw)
	rawPrefix = "~raw~" // display form of a name that is not valid UTF-8
)

// inodeID is the Entry.ID of inode n.
func inodeID(n uint32) string { return "inode:" + strconv.FormatUint(uint64(n), 10) }

// direntPrefix starts the ID of a deleted directory entry. Such an entry is
// identified by where its record lies, never by the inode it names (the inode
// is reused, or unknown), so the ID alone says the entry is deleted.
const direntPrefix = "dirent:"

// direntID is the Entry.ID of the deleted record r: "dirent:<physical
// block>:<offset>", or "dirent:inline:<dir inode>:<offset>" for an inline
// directory.
func direntID(r *dirRec) string {
	if r.blk >= 0 {
		return fmt.Sprintf("dirent:%d:%d", r.blk, r.off)
	}
	return fmt.Sprintf("dirent:inline:%d:%d", r.dirInode, r.off)
}

// isDirentID reports whether id names a deleted directory entry.
func isDirentID(id string) bool { return strings.HasPrefix(id, direntPrefix) }

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
	beyond  bool  // the block lies beyond the directory's i_size
	wasLive bool  // a live-looking record beyond i_size, reported as deleted

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
	beyond  bool
	first   bool   // logical block 0: the "." and ".." records belong here
	what    string // for warnings: "block 12" or "inline data"
}

// dirScan walks the records of one directory.
type dirScan struct {
	f            *FS
	in           *inode
	wantDeleted  bool
	visit        func(*dirRec) bool
	index        bool  // the directory has the INDEX flag
	enc          bool  // the directory is encrypted: names are ciphertext
	isizeBlocks  int64 // blocks that i_size covers; blocks past them are "beyond"
	warnedBeyond bool
	seed         uint32 // checksum seed of the directory's blocks
	count        int
	stop         bool
	slackBytes   int64    // slack bytes searched for deleted records so far
	badIdx       []uint32 // see plausibleSlack
	badFor       *region  // the region badIdx describes
}

// scanDir calls visit for every live record of directory in and, with
// wantDeleted, for the deleted records found in slack space, in on-disk order,
// until visit returns false. Damage inside a block is a warning and ends the
// read of that block only. If the block map is damaged part-way, the blocks
// mapped before the damage are still read and the rest is a warning; holes in a
// sparse directory are skipped; blocks mapped beyond i_size are read too, with a
// warning, and their records are reported as deleted (flagged beyond_isize),
// never as live. Only an inline directory whose data cannot
// be read is an error.
func (f *FS) scanDir(in *inode, wantDeleted bool, visit func(*dirRec) bool) error {
	w := &dirScan{f: f, in: in, wantDeleted: wantDeleted, visit: visit, index: in.flags&inodeFlagIndex != 0, enc: in.flags&inodeFlagEncrypt != 0}
	if f.sb.metadataCsum() {
		w.seed = f.inodeSeed(in)
	}
	if in.flags&inodeFlagInlineData != 0 {
		return w.inline()
	}
	runs, isizeBlocks, err := f.dirRuns(in)
	if err != nil {
		f.warn("directory inode %d: its block map is damaged (%v); only the entries in the blocks mapped before the damage are listed", in.num, err)
	}
	w.isizeBlocks = isizeBlocks
	bs := int64(f.sb.blockSize)
	var lblk int64
	for _, r := range runs {
		if w.stop {
			break
		}
		if r.Offset < 0 {
			if lblk < isizeBlocks {
				f.warn("directory inode %d: no blocks at logical blocks %d-%d (sparse directory)", in.num, lblk, min(lblk+r.Length/bs, isizeBlocks)-1)
			}
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

// dirRuns maps directory in's blocks: the runs (Offset -1 is a hole), the
// number of blocks i_size covers (at most 64 MiB, a warning when i_size is
// larger), and an error if the map is damaged, in which case the runs mapped
// before the damage are returned. The map is read up to 64 MiB whatever i_size
// says, so blocks mapped beyond i_size are seen; with a zero i_size every
// mapped block is beyond it (a warning).
func (f *FS) dirRuns(in *inode) ([]filesys.Run, int64, error) {
	size := in.size
	if size > maxDirBytes {
		f.warn("directory inode %d: i_size %d exceeds the 64 MiB directory limit; only the first 64 MiB are read", in.num, in.size)
		size = maxDirBytes
	}
	bs := int64(f.sb.blockSize)
	isizeBlocks := (size + bs - 1) / bs // <= 64 MiB: cannot overflow
	c := *in
	c.size = maxDirBytes
	runs, err := f.runsPartial(&c)
	if in.size == 0 && slices.ContainsFunc(runs, func(r filesys.Run) bool { return r.Offset >= 0 }) {
		f.warn("directory inode %d has i_size 0 but maps blocks; they are scanned and their entries treated as lying beyond i_size", in.num)
	}
	return runs, isizeBlocks, err
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
	g := &region{b: buf, limit: len(buf), blk: blk, slack: true, what: "block " + strconv.FormatInt(blk, 10), beyond: lblk >= w.isizeBlocks, first: lblk == 0}
	if g.beyond && !w.warnedBeyond {
		w.warnedBeyond = true
		w.f.warn("directory inode %d: blocks are mapped beyond i_size (from logical block %d, i_size %d); they are scanned and their entries flagged beyond_isize", w.in.num, lblk, w.in.size)
	}
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
		// With metadata_csum the index gives up one slot to the dx_tail, but an
		// index made before the feature was enabled has the full capacity.
		return (limit == (bs-off)/8-tail || limit == (bs-off)/8) && count >= 1 && count <= limit
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
	rec := -1 // index of the current record in the region
	for off := 0; off < g.limit && !w.stop; {
		rec++
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
		case inode != 0 && (!g.first || rec >= 2):
			// "." and ".." are the first two records of the first block and are
			// never listed; anywhere else they are not a directory's own.
			w.f.warn("directory inode %d, %s: a \".\" or \"..\" record at offset %d is not among the first two records of the directory; it is not listed", w.in.num, g.what, off)
		case inode == 0 && off == 0 && g.slack && w.wantDeleted && nameLen > 0 && plausibleName(name, w.enc):
			// The first record of a block, deleted: the kernel zeroed its
			// inode, so only the name, type and place survive. It is reported
			// as a deleted entry of unknown inode (see unknownInodeEntry).
			w.emit(g, &dirRec{name: name, ftype: ftype, deleted: true, off: off})
		}
		// The slack after a record holds records deleted by merging them into
		// it. That includes a record with a zeroed inode (the kernel zeroes the
		// inode of a deleted first record, which later deletions merge into).
		if g.slack && w.wantDeleted && !w.stop {
			w.slackScan(g, off+(direntHeader+nameLen+3)&^3, off+recLen)
		}
		off += recLen
	}
}

// slackScan looks for deleted records in b[from:to], at 4-byte steps. A
// candidate needs an inode in [1, inodes_count], a name that is non-empty,
// fits in the slack and has no NUL or '/'; its rec_len is ignored (a deleted
// record keeps its old one). After an accepted candidate the scan resumes past
// its minimal length. A directory's slack is searched up to the FS's slack cap;
// the rest is a warning.
func (w *dirScan) slackScan(g *region, from, to int) {
	if from+direntHeader >= to {
		return
	}
	remaining := w.f.slackScanCap - w.slackBytes
	if remaining <= 0 || int64(to-from) > remaining {
		w.f.warn("directory inode %d: more than %d bytes of slack space searched for deleted entries; the slack beyond that is not searched", w.in.num, w.f.slackScanCap)
		if remaining <= 0 {
			return
		}
		to = from + int(remaining) // remaining < to-from, which is an int
	}
	w.slackBytes += int64(to - from)
	var checks int64
	defer func() { w.f.slackWork.Add(checks) }()
	b := g.b
	for q := from; q+direntHeader < to && !w.stop; {
		checks++
		inode := binary.LittleEndian.Uint32(b[q:])
		nameLen, ftype := w.nameLen(b[q:])
		end := q + direntHeader + nameLen
		if inode >= 1 && inode <= w.f.sb.inodesCount && nameLen > 0 && end <= to && w.plausibleSlack(g, q+direntHeader, end) {
			w.emit(g, &dirRec{inode: inode, name: b[q+direntHeader : end], ftype: ftype, deleted: true, off: q})
			q += (direntHeader + nameLen + 3) &^ 3
			continue
		}
		q += 4
	}
}

// plausibleSlack is plausibleName for g.b[lo:hi] in constant time: the first
// call on a region indexes it once (for each offset, the next NUL or '/'), so
// a hostile slack cannot make every 4-byte step rescan up to 64 KiB.
func (w *dirScan) plausibleSlack(g *region, lo, hi int) bool {
	if isDot(g.b[lo:hi]) {
		return false
	}
	if w.enc {
		return true
	}
	if w.badFor != g {
		w.badIdx = slices.Grow(w.badIdx[:0], len(g.b)+1)[:len(g.b)+1]
		next := uint32(len(g.b))
		w.badIdx[len(g.b)] = next
		for i := len(g.b) - 1; i >= 0; i-- {
			if c := g.b[i]; c == 0 || c == '/' {
				next = uint32(i)
			}
			w.badIdx[i] = next
		}
		w.badFor = g
		w.f.slackWork.Add(int64(len(g.b)))
	}
	return int(w.badIdx[lo]) >= hi
}

// emit hands one record to the visitor, within the per-directory record cap. A
// live-looking record in a block beyond i_size is handed over as a deleted one
// (wasLive), and only to a scan that wants deleted entries.
func (w *dirScan) emit(g *region, r *dirRec) {
	if g.beyond && !r.deleted {
		// A record in a block beyond i_size is not part of the directory: it is
		// reported, as deleted, only to a scan that wants deleted entries.
		if !w.wantDeleted {
			return
		}
		r.deleted, r.wasLive = true, true
	}
	if w.count >= w.f.dirRecordCap {
		w.f.warn("directory inode %d: more than %d entries; the rest are not read", w.in.num, w.f.dirRecordCap)
		w.stop = true
		return
	}
	w.count++
	r.blk, r.csumBad, r.beyond, r.dirInode = g.blk, g.csumBad, g.beyond, w.in.num
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

// plausibleName reports whether name can be the name of a deleted record: not
// "." or "..", and no NUL or '/' - except in an encrypted directory, where
// names are ciphertext and any byte can occur.
func plausibleName(name []byte, encrypted bool) bool {
	return !isDot(name) && (encrypted || !bytes.ContainsAny(name, "\x00/"))
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
// links caches symlink targets by inode for the duration of one ReadDir (may be
// nil).
func (f *FS) dirEntry(r *dirRec, dirEnc bool, links map[uint32]string) filesys.Entry {
	name, raw := displayName(r.name, dirEnc)
	if r.inode == 0 {
		return unknownInodeEntry(r, name, raw, dirEnc)
	}
	in, err := f.inode(r.inode)
	var e filesys.Entry
	if err != nil {
		e = filesys.Entry{Name: name, RawName: raw, ID: inodeID(r.inode), Type: direntType(r.ftype)}
		if r.deleted {
			addAttr(&e, "inode_unreadable", "true")
		} else {
			addAttr(&e, "inode", "unreadable")
		}
	} else {
		e = toEntry(name, raw, in)
		if in.mode&modeTypeMask == modeSymlink && !r.deleted {
			t, ok := links[r.inode]
			if !ok {
				t = f.linkTarget(in)
				if links != nil {
					links[r.inode] = t
				}
			}
			e.LinkTarget = t
		}
		if !r.deleted && in.links == 0 && !in.times.Deleted.T.IsZero() {
			addAttr(&e, "inode_freed", "true")
			f.warn("directory inode %d: the entry %q names inode %d, which has been freed (links_count 0, dtime set)", r.dirInode, name, r.inode)
		}
	}
	if r.beyond {
		addAttr(&e, "beyond_isize", "true")
	}
	e.Encrypted = e.Encrypted || dirEnc
	if r.deleted {
		// The ID is the record's location, so Deleted is derivable from it
		// (Open and ReadDir never trust the Entry's own Deleted field); the
		// inode the record names, as of the scan, stays an attribute.
		e.Deleted = true
		e.ID = direntID(r)
		addAttr(&e, "inode", strconv.FormatUint(uint64(r.inode), 10))
		if err == nil && in.links > 0 && in.orphanNext == 0 && !r.wasLive {
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
	e.ID = direntID(r)
	if r.blk >= 0 {
		addAttr(&e, "dirent", fmt.Sprintf("%d:%d", r.blk, r.off))
	} else {
		addAttr(&e, "dirent", fmt.Sprintf("inline:%d", r.off))
	}
	addAttr(&e, "inode", "unknown")
	if r.beyond {
		addAttr(&e, "beyond_isize", "true")
	}
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

// dirInode returns the inode of directory entry dir. Only dir.ID and the
// on-disk inode are used: a "dirent:" ID is a deleted entry (ErrDeleted), an
// "inode:<n>" ID is re-read, so a forged Deleted, Type or Size on the Entry
// changes nothing.
func (f *FS) dirInode(dir filesys.Entry) (*inode, error) {
	if isDirentID(dir.ID) {
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
// slack of its blocks (Deleted is set; see dirEntry for the attributes). A
// deleted entry's ID is the location of its record ("dirent:<block>:<offset>",
// "dirent:inline:<dir inode>:<offset>"), with attr inode=<n> when the record
// still names an inode; live entries are "inode:<n>". dir is identified by its
// ID alone. "."
// and ".." are omitted. Entries are in on-disk order; htree directories are
// read by a linear scan of all blocks. The entry names the inode as it is now,
// so a deleted entry whose inode was reused shows the new inode. A deleted
// entry that is a stale copy of a live one (same inode and name bytes, as an
// htree leaf split leaves behind) stays listed with attr stale_copy=true. An entry found in a
// block mapped beyond i_size is not live: it is reported Deleted with attr
// beyond_isize=true (Lookup ignores it). A live entry whose inode has been freed
// (links_count 0, dtime set) has attr inode_freed=true. A damaged block map does not fail the call: the entries that can be read are
// returned and the damage is an Info warning.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	in, err := f.dirInode(dir)
	if err != nil {
		return nil, err
	}
	enc := in.flags&inodeFlagEncrypt != 0
	type key struct {
		inode uint32
		name  string
	}
	var out []filesys.Entry
	live := map[key]struct{}{}
	var deleted []key   // keys of the deleted entries with a known inode...
	var deletedAt []int // ...and where they are in out
	links := map[uint32]string{}
	if err := f.scanDir(in, true, func(r *dirRec) bool {
		out = append(out, f.dirEntry(r, enc, links))
		switch {
		case !r.deleted:
			live[key{r.inode, string(r.name)}] = struct{}{}
		case r.inode != 0:
			deleted = append(deleted, key{r.inode, string(r.name)})
			deletedAt = append(deletedAt, len(out)-1)
		}
		return true
	}); err != nil {
		return nil, err
	}
	for i, k := range deleted {
		if _, ok := live[k]; ok {
			addAttr(&out[deletedAt[i]], "stale_copy", "true")
		}
	}
	return out, nil
}

// Lookup resolves an absolute slash path. Within each directory the live
// entries are searched in this order of preference over the whole directory:
//
//  1. an exact byte match (never in an encrypted directory, whose names are
//     ciphertext: there only the "~enc~" form matches);
//  2. the display form of ReadDir: "~enc~"+base64url in encrypted directories,
//     "~raw~"+base64url elsewhere, so a real name that happens to look like a
//     display form wins over the name that form stands for;
//  3. in a casefold directory, strings.EqualFold when both names are valid
//     UTF-8 (an approximation of the filesystem's Unicode folding).
//
// Empty components are ignored, "." and ".." are not special, and symlinks are
// not followed. A directory is read up to the per-directory entry cap
// (maxDirRecords); an entry beyond it is reported as not found.
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
	enc := in.flags&inodeFlagEncrypt != 0
	casefold := in.flags&inodeFlagCasefold != 0 && !enc && utf8.ValidString(comp)
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
	var exactRec, altRec, foldRec *dirRec
	keep := func(r *dirRec) *dirRec {
		c := *r
		c.name = slices.Clone(r.name)
		return &c
	}
	err = f.scanDir(in, false, func(r *dirRec) bool {
		switch {
		case !enc && bytes.Equal(r.name, exact):
			exactRec = keep(r)
			return false
		case altRec == nil && alt != nil && bytes.Equal(r.name, alt):
			altRec = keep(r)
		case foldRec == nil && casefold && utf8.Valid(r.name) && strings.EqualFold(string(r.name), comp):
			foldRec = keep(r)
		}
		return true
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	for _, r := range []*dirRec{exactRec, altRec, foldRec} {
		if r != nil {
			return f.dirEntry(r, enc, nil), nil
		}
	}
	return filesys.Entry{}, errNoChild
}
