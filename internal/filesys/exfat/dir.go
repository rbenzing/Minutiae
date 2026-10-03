package exfat

import (
	"encoding/base64"
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
	rawPrefix     = "~raw~"     // display form of a name that is not usable UTF-8: prefix + base64url(UTF-16LE)
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
// unreadable or fails its checksum is not used (Lookup falls back to
// strings.EqualFold) and is reported through Info().Warnings.
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

// scanDir calls visit for every File entry set of the directory (the deleted
// ones too with wantDeleted) in on-disk order until visit returns false, or
// the per-directory cap is reached. Damage inside the directory is a warning;
// only a directory of which nothing can be read is an error.
func (f *FS) scanDir(st *dirState, wantDeleted bool, visit func(*setRec) bool) error {
	sp := st.spec
	buf, err := f.readDirData(st)
	if err != nil {
		return err
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
	return nil
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
// "~raw~" + base64url of its UTF-16LE bytes. Lookup accepts both forms.
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
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), raw
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
	// directory without a cluster, by where its entry set lies.
	if isDir && !r.deleted && r.first >= fatReservedClusters {
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
// apply. Damage in the directory (a broken chain, an unreadable cluster) does
// not fail the call: the entries that can be read are returned and the damage
// is an Info warning.
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
	if err := f.scanDir(st, true, func(r *setRec) bool {
		out = append(out, f.makeEntry(st.spec.first, r))
		return true
	}); err != nil {
		return nil, err
	}
	return out, nil
}

var errNoChild = errors.New("no such entry")

// Lookup resolves an absolute slash path. Within each directory the live
// entries are searched in this order of preference over the whole directory:
//
//  1. an exact match of the UTF-16 name;
//  2. the display form of ReadDir, "~raw~"+base64url, so a real name that
//     happens to look like a display form wins over the name that form stands
//     for;
//  3. a case-insensitive match: through the volume's up-case table, or, when
//     there is none, strings.EqualFold when both names are valid UTF-8.
//
// Empty components are ignored, "." and ".." are not special. A directory is
// read up to the per-directory entry cap (maxDirRecords); an entry beyond it is
// reported as not found. Deleted entries are never found.
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

// child finds the live entry of dir named comp (see Lookup for the order).
func (f *FS) child(dir filesys.Entry, comp string) (filesys.Entry, error) {
	st, empty, err := f.dirOf(dir)
	if err != nil {
		return filesys.Entry{}, err
	}
	if empty {
		return filesys.Entry{}, errNoChild
	}
	var exact []uint16 // comp as UTF-16, when it is valid UTF-8
	if utf8.ValidString(comp) {
		exact = utf16.Encode([]rune(comp))
	}
	var alt []uint16 // the name a display form stands for
	if rest, ok := strings.CutPrefix(comp, rawPrefix); ok {
		if b, err := base64.RawURLEncoding.DecodeString(rest); err == nil && len(b)%2 == 0 {
			alt = make([]uint16, len(b)/2)
			for i := range alt {
				alt[i] = binary.LittleEndian.Uint16(b[2*i:])
			}
		}
	}
	var exactRec, altRec, foldRec *setRec
	err = f.scanDir(st, false, func(r *setRec) bool {
		switch {
		case exact != nil && slices.Equal(r.name, exact):
			exactRec = r
			return false
		case altRec == nil && alt != nil && slices.Equal(r.name, alt):
			altRec = r
		case foldRec == nil && exact != nil && f.foldEqual(r.name, exact, comp):
			foldRec = r
		}
		return true
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	for _, r := range []*setRec{exactRec, altRec, foldRec} {
		if r != nil {
			return f.makeEntry(st.spec.first, r), nil
		}
	}
	return filesys.Entry{}, errNoChild
}

// foldEqual reports whether the on-disk name and the query (given both as
// UTF-16 and as a string) are equal ignoring case.
func (f *FS) foldEqual(name, query []uint16, q string) bool {
	if f.upcase != nil {
		return len(name) == len(query) && equalUpper(f, name, query)
	}
	return validUTF16(name) && strings.EqualFold(string(utf16.Decode(name)), q)
}

func equalUpper(f *FS, a, b []uint16) bool {
	for i := range a {
		if f.upcase[a[i]] != f.upcase[b[i]] {
			return false
		}
	}
	return true
}
