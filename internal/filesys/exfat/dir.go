package exfat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Directory entry types (the type byte: bit 7 InUse, bit 6 secondary, bit 5
// benign, bits 0-4 the code).
const (
	entrySize = 32

	typeEnd         = 0x00
	typeBitmap      = 0x81
	typeUpcase      = 0x82
	typeLabel       = 0x83
	typeFile        = 0x85
	typeFileDeleted = 0x05
	typeStream      = 0x40 // with InUse: 0xC0; deleted: 0x40
	typeName        = 0x41 // with InUse: 0xC1; deleted: 0x41

	attrDirectory = 0x10

	// maxFileSecondary is the most secondary entries a File set can have: one
	// Stream Extension and 17 Name entries (255 UTF-16 units).
	maxFileSecondary = 18
	namePerEntry     = 15

	// maxDirBytes is how much of one directory is read.
	maxDirBytes = 256 << 20
	// maxDirRecords bounds the entry sets one directory yields (live and
	// deleted for ReadDir, live for Lookup), so a hostile directory cannot make
	// ReadDir build an unbounded list. A directory with more is cut off at the
	// cap with a warning, for Lookup too: an entry beyond it is reported as not
	// found. An entry set takes at most 19 entries, so 1<<18 of them fit
	// within maxDirBytes (152 MiB of 256): the per-directory byte limit binds
	// only for a directory made of junk, and the per-volume budget
	// (maxDirBudget, 1 GiB) after four such directories.
	maxDirRecords = 1 << 18
	// maxUpcaseBytes bounds the up-case table (65536 UTF-16 units).
	maxUpcaseBytes = 128 << 10
	maxLabelUnits  = 11

	dirReadChunk = 4096

	damagedPrefix = "~damaged~" // name of a damaged set that has no name: prefix + entry index
)

// meta flags of the bitmap entry.
const bitmapSecond = 1 // BitmapFlags bit 0: the entry describes the second bitmap

func dirID(first uint32) string { return "dir:" + strconv.FormatUint(uint64(first), 10) }

func direntID(dirFirst uint32, idx int) string {
	return "dirent:" + strconv.FormatUint(uint64(dirFirst), 10) + ":" + strconv.Itoa(idx)
}

// setRec is one File entry set read from a directory.
type setRec struct {
	idx     int // entry index of the File entry in its directory
	deleted bool
	attrs   uint16

	created, modified, accessed     uint32
	createInc, modifyInc            byte
	createOff, modifyOff, accessOff byte

	flags    byte // GeneralSecondaryFlags of the Stream Extension
	name     []uint16
	valid    uint64 // ValidDataLength
	first    uint32
	dataLen  uint64
	checksum bool // the set is inconsistent: bad checksum, secondary count or name entries
	damaged  bool // SecondaryCount above 18, or no Stream Extension: the extent cannot be trusted and Open fails
	nameNote string
}

func (r *setRec) noFatChain() bool { return r.flags&0x02 != 0 }

// loadRootMeta reads the root directory's system entries: the allocation
// bitmap, the up-case table and the volume label.
func (f *FS) loadRootMeta() {
	buf, err := f.readDirData(f.knownDir(f.rootCluster))
	if err != nil {
		f.warn("root directory (first cluster %d) cannot be read: %v", f.rootCluster, err)
		return
	}
	var (
		up       meta
		haveUp   bool
		haveName bool
		le       = binary.LittleEndian
	)
	for i := 0; i+entrySize <= len(buf); i += entrySize {
		e := buf[i : i+entrySize]
		switch e[0] {
		case typeEnd:
			i = len(buf)
		case typeBitmap:
			m := meta{present: true, first: le.Uint32(e[20:]), length: le.Uint64(e[24:]), aux: uint32(e[1])}
			// A TexFAT volume has two bitmaps; the first (flag bit clear) is read.
			if !f.bitmap.present || (f.bitmap.aux&bitmapSecond != 0 && m.aux&bitmapSecond == 0) {
				f.bitmap = m
			}
		case typeUpcase:
			if !haveUp {
				haveUp = true
				up = meta{present: true, first: le.Uint32(e[20:]), length: le.Uint64(e[24:]), aux: le.Uint32(e[4:])}
			}
		case typeLabel:
			if !haveName {
				haveName = true
				n := int(e[1])
				if n > maxLabelUnits {
					f.warn("volume label entry has CharacterCount %d (at most %d); the label is cut", n, maxLabelUnits)
					n = maxLabelUnits
				}
				units := make([]uint16, n)
				for k := range units {
					units[k] = le.Uint16(e[2+2*k:])
				}
				f.label = string(utf16.Decode(units))
			}
		}
	}
	f.upMeta = up
	f.loadUpcase(up)
}

// loadUpcase reads and decodes the up-case table. A table that is too large,
// unreadable or fails its checksum is not used (Lookup then does no case-insensitive
// folding at all, N2) and is reported through Info().Warnings.
func (f *FS) loadUpcase(m meta) {
	if !m.present {
		return
	}
	switch {
	case m.length == 0:
		f.warn("up-case table: the entry has length 0; not used")
		return
	case m.length > maxUpcaseBytes:
		f.warn("up-case table: %d bytes exceed the %d-byte limit; not used", m.length, maxUpcaseBytes)
		return
	case m.length%2 != 0:
		f.warn("up-case table: odd length %d; not used", m.length)
		return
	}
	exts, err := f.metaExtents(m.first, (m.length+uint64(f.cs)-1)/uint64(f.cs))
	if err != nil {
		f.warn("up-case table: cannot be located (first cluster %d): %v; not used", m.first, err)
		return
	}
	data := make([]byte, m.length)
	if err := f.readExtents(exts, data); err != nil {
		f.warn("up-case table: cannot be read: %v; not used", err)
		return
	}
	if got := tableChecksum(data); got != m.aux {
		f.warn("up-case table: checksum mismatch (stored %#08x, computed %#08x); not used", m.aux, got)
		return
	}
	var t [65536]uint16
	for i := range t {
		t[i] = uint16(i)
	}
	idx := 0
	for i := 0; i+2 <= len(data) && idx < len(t); i += 2 {
		v := binary.LittleEndian.Uint16(data[i:])
		if v == 0xFFFF && i+4 <= len(data) { // 0xFFFF, n: the next n units map to themselves
			idx += int(binary.LittleEndian.Uint16(data[i+2:]))
			i += 2
			continue
		}
		t[idx] = v
		idx++
	}
	f.upcase = &t
}

// tableChecksum is the up-case table checksum.
func tableChecksum(data []byte) uint32 {
	var sum uint32
	for _, c := range data {
		sum = (sum&1)<<31 | sum>>1
		sum += uint32(c)
	}
	return sum
}

// upper returns the up-case form of u (u itself without a table).
func (f *FS) upper(u uint16) uint16 {
	if f.upcase == nil {
		return u
	}
	return f.upcase[u]
}

// isSecondary reports whether type byte t is a secondary entry of a set that
// is live (InUse set) or, with deleted, one whose InUse bit was cleared.
func isSecondary(t byte, deleted bool) bool {
	return t&0x40 != 0 && (t&0x80 != 0) != deleted
}

// setChecksum is the entry set checksum over a whole set (the primary entry's
// checksum field excluded).
func setChecksum(set []byte) uint16 { return setSum(set, false) }

// setSum computes the set checksum; with restoreInUse each entry's type byte
// is summed with its InUse bit set, as it was before the set was deleted.
func setSum(set []byte, restoreInUse bool) uint16 {
	var sum uint16
	for i, c := range set {
		switch {
		case i == 2 || i == 3:
			continue
		case restoreInUse && i%entrySize == 0:
			c |= 0x80
		}
		sum = (sum&1)<<15 | sum>>1
		sum += uint16(c)
	}
	return sum
}

// scanDirCut calls visit for every File entry set of the directory (the
// deleted ones too with wantDeleted) in on-disk order until visit returns
// false, or the per-directory cap is reached. Damage inside the directory is a
// warning; only a directory of which nothing can be read is an error. When the
// listing is a prefix (the read ended early or the cap was hit) it also returns
// the reason, so a search can tell "absent" from "unread".
func (f *FS) scanDirCut(st *dirState, wantDeleted bool, visit func(*setRec) bool) (cut error, err error) {
	sp := st.spec
	buf, cut, err := f.readDirDataCut(st)
	if err != nil {
		return nil, err
	}
	var (
		n              = len(buf) / entrySize
		count          int
		skippedDeleted int
		orphans        int
	)
scan:
	for i := 0; i < n; {
		t := buf[i*entrySize]
		switch {
		case t == typeFile || (t == typeFileDeleted && wantDeleted):
			rec, used := f.parseSet(buf, i, 0, sp.first, t == typeFileDeleted)
			if rec == nil {
				if t == typeFileDeleted {
					skippedDeleted++
				}
				i += used
				continue
			}
			if count >= f.dirRecordCap {
				f.warn("directory (first cluster %d): more than %d entries; the rest are not read", sp.first, f.dirRecordCap)
				cut = fmt.Errorf("%w: more than %d entries in the directory", filesys.ErrLimit, f.dirRecordCap)
				break scan
			}
			count++
			i += used
			if !rec.deleted && !rec.damaged && rec.attrs&attrDirectory != 0 && rec.first >= fatReservedClusters {
				f.claim(rec)
			}
			if !visit(rec) {
				break scan
			}
		case t&0xC0 == 0xC0:
			orphans++
			i++
		default:
			i++
		}
	}
	if skippedDeleted > 0 {
		f.warn("directory (first cluster %d): %d deleted entry sets are damaged and were skipped", sp.first, skippedDeleted)
	}
	if orphans > 0 {
		f.warn("directory (first cluster %d): %d secondary entries belong to no File entry and were ignored", sp.first, orphans)
	}
	if cut != nil { // every cut is a prefix of the directory: the typed incomplete-scan error wraps the cause (N6)
		cut = fmt.Errorf("%w: %w", filesys.ErrIncomplete, cut)
	}
	return cut, nil
}

// parseSet reads the File entry set at entry i of buf. It returns the record
// and the number of entries the set occupies, or a nil record when the set
// cannot be told apart from damage (the File entry alone is then skipped).
//
// The set is the File entry followed by up to SecondaryCount secondary
// entries: a Stream Extension, then ceil(NameLength/15) Name entries, then
// possibly other secondary entries. A set whose SecondaryCount, entry types or
// checksum do not fit is still returned, with checksum set, so that it is
// listed and flagged. Deleted sets (InUse cleared on every entry) are read the
// same way; their checksum is accepted either over the stored bytes or over
// the bytes as they were while the set was live.
func (f *FS) parseSet(buf []byte, i, base int, dirFirst uint32, deleted bool) (*setRec, int) {
	n := len(buf) / entrySize
	ent := func(k int) []byte { return buf[k*entrySize : (k+1)*entrySize] }
	le := binary.LittleEndian
	p := ent(i)
	sc := int(p[1])
	idx := base + i // entry index in the directory
	damaged := false
	if sc > maxFileSecondary {
		if deleted {
			return nil, 1
		}
		f.warn("directory (first cluster %d), entry %d: a File entry with SecondaryCount %d (a File set has at most %d secondary entries) is listed as damaged; only the secondary entries present are read", dirFirst, idx, sc, maxFileSecondary)
		sc, damaged = maxFileSecondary, true
	}
	avail := 0 // secondary entries actually present, up to SecondaryCount
	for avail < sc && i+1+avail < n && isSecondary(buf[(i+1+avail)*entrySize], deleted) {
		avail++
	}
	if avail == 0 || ent(i + 1)[0]&0x7F != typeStream {
		if deleted {
			return nil, 1
		}
		f.warn("directory (first cluster %d), entry %d: a File entry without a Stream Extension entry is listed as damaged", dirFirst, idx)
		return &setRec{
			idx: idx, attrs: le.Uint16(p[4:]), checksum: true, damaged: true,
			created: le.Uint32(p[8:]), modified: le.Uint32(p[12:]), accessed: le.Uint32(p[16:]),
			createInc: p[20], modifyInc: p[21], createOff: p[22], modifyOff: p[23], accessOff: p[24],
		}, 1
	}
	st := ent(i + 1)
	r := &setRec{
		idx: idx, deleted: deleted, damaged: damaged,
		attrs:     le.Uint16(p[4:]),
		created:   le.Uint32(p[8:]),
		modified:  le.Uint32(p[12:]),
		accessed:  le.Uint32(p[16:]),
		createInc: p[20], modifyInc: p[21],
		createOff: p[22], modifyOff: p[23], accessOff: p[24],
		flags:   st[1],
		valid:   le.Uint64(st[8:]),
		first:   le.Uint32(st[20:]),
		dataLen: le.Uint64(st[24:]),
	}
	nameLen := int(st[3])
	wantNames := (nameLen + namePerEntry - 1) / namePerEntry
	r.name = make([]uint16, 0, nameLen)
	for k := 0; k < wantNames && 2+k <= avail; k++ {
		ne := ent(i + 2 + k)
		if ne[0]&0x7F != typeName {
			break
		}
		for u := 0; u < namePerEntry && len(r.name) < nameLen; u++ {
			r.name = append(r.name, le.Uint16(ne[2+2*u:]))
		}
	}
	consistent := avail == sc && len(r.name) == nameLen && !damaged
	if nameLen == 0 {
		r.nameNote = "empty"
	} else if len(r.name) < nameLen {
		r.nameNote = "truncated"
	}
	if expected := 1 + wantNames; consistent && sc != expected {
		consistent = sc > expected
		for j := expected + 1; j <= sc && consistent; j++ { // extra secondary entries must not be Stream or Name entries
			if t := ent(i + j)[0] & 0x7F; t == typeStream || t == typeName {
				consistent = false
			}
		}
	}
	if consistent {
		set := buf[i*entrySize : (i+1+sc)*entrySize]
		stored := le.Uint16(p[2:])
		consistent = stored == setSum(set, false) || (deleted && stored == setSum(set, true))
	}
	r.checksum = !consistent || damaged
	return r, 1 + avail
}

// validUTF16 reports whether every surrogate in u is part of a pair.
func validUTF16(u []uint16) bool {
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c >= 0xD800 && c < 0xDC00:
			if i+1 >= len(u) || u[i+1] < 0xDC00 || u[i+1] >= 0xE000 {
				return false
			}
			i++
		case c >= 0xDC00 && c < 0xE000:
			return false
		}
	}
	return true
}

// displayName returns the Entry.Name for an on-disk name and the RawName to
// keep. A valid UTF-16 name without NUL or '/' that is not "." or ".." is shown
// as it is (RawName is then nil); any other name is shown losslessly as
// filesys.RawNameAlias of its UTF-16LE bytes, the one shared spelling. Lookup
// accepts both forms (see Lookup for the order).
func displayName(u []uint16) (string, []byte) {
	if len(u) > 0 && validUTF16(u) && !slices.Contains(u, 0) && !slices.Contains(u, '/') {
		if name := string(utf16.Decode(u)); name != "." && name != ".." {
			return name, nil
		}
	}
	raw := make([]byte, 0, 2*len(u))
	for _, c := range u {
		raw = binary.LittleEndian.AppendUint16(raw, c)
	}
	return filesys.RawNameAlias(raw), raw
}

func addAttr(e *filesys.Entry, key, value string) {
	kv := filesys.KV{Key: key, Value: value}
	if !slices.Contains(e.Attrs, kv) {
		e.Attrs = append(e.Attrs, kv)
	}
}

// decodeTime decodes an exFAT timestamp: ok is false for a value that is not a
// real date (the caller leaves the timestamp absent); a zero value is absent
// without being invalid. inc10 is the 10 ms increment (ignored above 199);
// off is the UTC offset byte: with its valid bit the instant is exact
// (ZoneKnown), otherwise the fields are a local time of unknown zone.
func decodeTime(v uint32, inc10, off byte, hasInc bool) (ts filesys.Timestamp, ok bool) {
	if v == 0 {
		return ts, true
	}
	sec, minute, hour := int(v&0x1F)*2, int(v>>5)&0x3F, int(v>>11)&0x1F
	day, month, year := int(v>>16)&0x1F, int(v>>21)&0xF, 1980+int(v>>25)
	if sec > 58 || minute > 59 || hour > 23 || month < 1 || month > 12 || day < 1 {
		return ts, false
	}
	var ns int
	if hasInc && inc10 <= 199 {
		sec += int(inc10) / 100
		ns = int(inc10) % 100 * 10_000_000
	}
	loc := time.UTC
	if off&0x80 != 0 {
		q := int(off & 0x7F)
		if q >= 0x40 {
			q -= 0x80
		}
		loc = time.FixedZone("", q*15*60)
		ts.ZoneKnown = true
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, ns, loc)
	if t.Day() != day || int(t.Month()) != month { // 30 February and the like
		return filesys.Timestamp{}, false
	}
	ts.T = t
	return ts, true
}

// makeEntry builds the Entry of a set read from the directory whose first
// cluster is dirFirst.
func (f *FS) makeEntry(dirFirst uint32, r *setRec) filesys.Entry {
	name, raw := displayName(r.name)
	if r.damaged && len(r.name) == 0 {
		name, raw = damagedPrefix+strconv.Itoa(r.idx), nil
	}
	e := filesys.Entry{Name: name, RawName: raw, Type: filesys.TypeFile, Deleted: r.deleted}
	isDir := r.attrs&attrDirectory != 0
	if isDir {
		e.Type = filesys.TypeDir
	}
	e.Size = int64(min(r.dataLen, math.MaxInt64))
	// A live directory is identified by its first cluster (so that Walk sees a
	// cross-linked or looping directory twice); everything else, and a
	// directory without a cluster, by where its entry set lies. A damaged set is
	// never claimed, so it is named by its position too: dirOf then reports the
	// damage instead of "no such directory".
	if isDir && !r.deleted && !r.damaged && r.first >= fatReservedClusters {
		e.ID = dirID(r.first)
	} else {
		e.ID = direntID(dirFirst, r.idx)
	}
	addAttr(&e, "dirent", fmt.Sprintf("%d:%d", dirFirst, r.idx))
	addAttr(&e, "first_cluster", strconv.FormatUint(uint64(r.first), 10))
	addAttr(&e, "size", strconv.FormatUint(r.dataLen, 10))
	if r.valid != r.dataLen {
		addAttr(&e, "valid_data_length", strconv.FormatUint(r.valid, 10))
	}
	if r.noFatChain() {
		addAttr(&e, "no_fat_chain", "true")
	}
	var flags []string
	for _, a := range [...]struct {
		bit  uint16
		name string
	}{{0x01, "readonly"}, {0x02, "hidden"}, {0x04, "system"}, {0x20, "archive"}} {
		if r.attrs&a.bit != 0 {
			flags = append(flags, a.name)
		}
	}
	if len(flags) > 0 {
		addAttr(&e, "attributes", strings.Join(flags, ","))
	}
	if r.checksum {
		addAttr(&e, "checksum", "bad")
	}
	if r.damaged {
		addAttr(&e, "set", "damaged")
	}
	if r.nameNote != "" {
		addAttr(&e, "name", r.nameNote)
	}
	var badTimes []string
	for _, t := range [...]struct {
		label  string
		v      uint32
		inc    byte
		off    byte
		hasInc bool
		dst    *filesys.Timestamp
	}{
		{"created", r.created, r.createInc, r.createOff, true, &e.Times.Created},
		{"modified", r.modified, r.modifyInc, r.modifyOff, true, &e.Times.Modified},
		{"accessed", r.accessed, 0, r.accessOff, false, &e.Times.Accessed},
	} {
		ts, ok := decodeTime(t.v, t.inc, t.off, t.hasInc)
		if !ok {
			badTimes = append(badTimes, t.label)
			continue
		}
		*t.dst = ts
	}
	if len(badTimes) > 0 {
		addAttr(&e, "bad_timestamps", strings.Join(badTimes, ","))
	}
	return e
}

// Root returns the root directory.
func (f *FS) Root() filesys.Entry {
	return filesys.Entry{ID: dirID(f.rootCluster), Type: filesys.TypeDir}
}

// ReadDir lists the live and the deleted entry sets of a directory (never
// "." or ".."; exFAT has none) in on-disk order. Each Entry carries the attrs
// dirent (<directory first cluster>:<index of the File entry>), first_cluster
// and size, plus valid_data_length, no_fat_chain and attributes when they
// apply. Damage in the directory (a broken chain, an unreadable cluster, the
// entry cap or the read budget) is an Info warning and ends the listing; the
// listing is then a prefix, so the call returns the entries read together with
// an error wrapping the reason (filesys.ErrLimit for the caps), never an empty
// or partial listing with a nil error. A directory of which nothing can be read
// is an error with no entries.
//
// Only the ID of dir is used; the attributes and size of the Entry are
// informational and never trusted (the directory is found, and its extent read,
// from the disk). A live set with a SecondaryCount above 18 or no Stream
// Extension is listed with attrs checksum=bad and set=damaged (a name when one
// can be read, else "~damaged~<index>"); Open fails on it.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	st, empty, err := f.dirOf(dir)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil
	}
	var out []filesys.Entry
	cut, err := f.scanDirCut(st, true, func(r *setRec) bool {
		out = append(out, f.makeEntry(st.spec.first, r))
		return true
	})
	if err != nil {
		return nil, err
	}
	if cut != nil { // a prefix is never presented as the whole directory (F73); the entries read are returned with the error
		return out, fmt.Errorf("exfat: directory (first cluster %d): %w", st.spec.first, cut)
	}
	return out, nil
}

var errNoChild = errors.New("no such entry")

// Lookup resolves an absolute slash path. Within each directory the live
// entries are searched over the WHOLE directory, and the answer is decided in
// this order:
//
//  1. an exact match of the UTF-16 name; two records with that exact name, or
//     a query that is also the canonical display form of another record, are
//     filesys.ErrAmbiguous (with the scan cut, the latter is the
//     incomplete-scan error);
//  2. the canonical display form of ReadDir ("~raw~" + base64url, one
//     spelling per name), which stands only for records shown in that form;
//     two such records are filesys.ErrAmbiguous;
//  3. the scan was cut (the entry cap, the read budget, an unreadable chunk, a
//     damaged directory chain, the per-directory byte limit): an error
//     wrapping filesys.ErrIncomplete and the cause, never filesys.ErrNotFound;
//  4. a live entry set whose name could not be read in full (no Stream
//     Extension, NameLength 0, fewer name units than NameLength): such a set
//     never matches, and the lookup is filesys.ErrUndecided;
//  5. exactly one case-insensitive match through the volume's up-case table;
//  6. two or more: filesys.ErrAmbiguous naming the first two and the count;
//  7. a volume without a usable up-case table (absent, oversized, unreadable,
//     checksum mismatch) folds nothing: a miss is filesys.ErrUndecided when
//     some stored name has the query's length in UTF-16 units (the table maps
//     one unit to one unit, so names of another length can never match);
//  8. not found.
//
// Empty components are ignored, "." and ".." are not special (and are never
// matched by folding). Deleted entries are never found.
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

// nameUnreadable reports whether the set's name could not be read in full: it
// has no Stream Extension, NameLength 0, or fewer name units than NameLength.
func (r *setRec) nameUnreadable() bool {
	return r.nameNote != "" || (r.damaged && len(r.name) == 0)
}

// incompleteLookup is the error of a lookup in a directory that was not read to
// the end; cut carries filesys.ErrLimit or filesys.ErrCorrupt.
func incompleteLookup(dirFirst uint32, comp string, cut error) error {
	if !errors.Is(cut, filesys.ErrIncomplete) {
		cut = fmt.Errorf("%w: %w", filesys.ErrIncomplete, cut)
	}
	return fmt.Errorf("exfat: directory (first cluster %d): lookup of %q cannot be decided: %w", dirFirst, comp, cut)
}

func undecidedLookup(dirFirst uint32, comp, why string) error {
	return fmt.Errorf("exfat: %w: lookup of %q in directory (first cluster %d): %s", filesys.ErrUndecided, comp, dirFirst, why)
}

// child finds the live entry of dir named comp (see Lookup for the order).
func (f *FS) child(dir filesys.Entry, comp string) (filesys.Entry, error) {
	st, empty, err := f.dirOf(dir)
	if err != nil {
		return filesys.Entry{}, err
	}
	if empty {
		return filesys.Entry{}, errNoChild
	}
	first := st.spec.first
	var exact []uint16 // comp as UTF-16, when it is valid UTF-8
	if utf8.ValidString(comp) {
		exact = utf16.Encode([]rune(comp))
	}
	var alt []uint16 // the name the canonical display form comp stands for
	if raw, ok := filesys.DecodeRawNameAlias(comp); ok && len(raw)%2 == 0 {
		alt = make([]uint16, len(raw)/2)
		for i := range alt {
			alt[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
	}
	var (
		exactRecs, aliasRecs, foldRecs [2]*setRec // at most the first two of each kind
		nExact, nAlias, nFold          int
		unreadable, sameLen            int
	)
	canFold := exact != nil && comp != "." && comp != ".."
	cut, err := f.scanDirCut(st, false, func(r *setRec) bool {
		if r.nameUnreadable() {
			unreadable++
			return true
		}
		switch {
		case exact != nil && slices.Equal(r.name, exact):
			if nExact < len(exactRecs) {
				exactRecs[nExact] = r
			}
			nExact++
		case alt != nil && slices.Equal(r.name, alt) && isRawShown(r):
			if nAlias < len(aliasRecs) {
				aliasRecs[nAlias] = r
			}
			nAlias++
		case canFold && f.foldEqual(r.name, exact):
			if nFold < len(foldRecs) {
				foldRecs[nFold] = r
			}
			nFold++
		case exact != nil && len(r.name) == len(exact):
			sameLen++
		}
		return true
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	switch { // 1: an exact match of a real name
	case nExact > 1:
		return filesys.Entry{}, fmt.Errorf("exfat: %w: %d entries of directory (first cluster %d) are named %q", filesys.ErrAmbiguous, nExact, first, comp)
	case nExact == 1 && alt != nil: // comp is also a display form: another record may be shown as it
		if cut != nil {
			return filesys.Entry{}, incompleteLookup(first, comp, cut)
		}
		if nAlias > 0 {
			return filesys.Entry{}, fmt.Errorf("exfat: %w: %q in directory (first cluster %d) is a stored name and also the display form of another entry", filesys.ErrAmbiguous, comp, first)
		}
		return f.makeEntry(first, exactRecs[0]), nil
	case nExact == 1:
		return f.makeEntry(first, exactRecs[0]), nil
	}
	switch { // 2: the canonical display form
	case nAlias > 1:
		return filesys.Entry{}, fmt.Errorf("exfat: %w: %d entries of directory (first cluster %d) are shown as %q", filesys.ErrAmbiguous, nAlias, first, comp)
	case nAlias == 1:
		return f.makeEntry(first, aliasRecs[0]), nil
	}
	if cut != nil { // 3: not found in a prefix is undecidable, never absent (F73)
		return filesys.Entry{}, incompleteLookup(first, comp, cut)
	}
	if unreadable > 0 { // 4
		return filesys.Entry{}, undecidedLookup(first, comp, fmt.Sprintf("%d live entry set(s) have a name that cannot be read in full", unreadable))
	}
	switch { // 5, 6: the volume's own case folding
	case nFold == 1:
		return f.makeEntry(first, foldRecs[0]), nil
	case nFold > 1:
		n0, _ := displayName(foldRecs[0].name)
		n1, _ := displayName(foldRecs[1].name)
		return filesys.Entry{}, fmt.Errorf("exfat: %w: %q in directory (first cluster %d) matches %d entries ignoring case, the first two are %q and %q", filesys.ErrAmbiguous, comp, first, nFold, n0, n1)
	}
	if f.upcase == nil && sameLen > 0 { // 7: no table, no fold; a name of this length might have matched
		return filesys.Entry{}, undecidedLookup(first, comp, fmt.Sprintf("the volume has no usable up-case table, and %d name(s) have the query's length", sameLen))
	}
	return filesys.Entry{}, errNoChild // 8
}

// isRawShown reports whether ReadDir shows the set in the "~raw~" form.
func isRawShown(r *setRec) bool {
	_, raw := displayName(r.name)
	return raw != nil
}

// foldEqual reports whether the on-disk name and the query are equal ignoring
// case, by the volume's up-case table. Without a usable table nothing is
// folded: exFAT defines the rule on the volume, so any other would be a guess.
func (f *FS) foldEqual(name, query []uint16) bool {
	if f.upcase == nil || len(name) != len(query) {
		return false
	}
	for i := range name {
		if f.upcase[name[i]] != f.upcase[query[i]] {
			return false
		}
	}
	return true
}
