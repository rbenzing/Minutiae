package fat

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	dirIDPrefix    = "dir:"    // a live directory: "dir:<first cluster>" (FAT12/16 root: "dir:0")
	direntIDPrefix = "dirent:" // any other entry: "dirent:<first cluster of its directory>:<entry index>"

	dirChunk = 64 << 10 // bytes read from a directory at a time

	// Bounds of the fallback that finds a directory by its first cluster.
	maxKnownDirs   = 1 << 20 // directories remembered
	maxLocateScans = 1 << 16 // directories read by one locate
)

// dirLoc is where the entry of a directory lives: entry idx of the directory
// that starts at cluster parent.
type dirLoc struct {
	parent uint32
	idx    int
}

// dirChainEntry caches the cluster chain of the last directory read.
type dirChainEntry struct {
	first     uint32
	clusters  []uint32
	truncated bool
	err       error
}

func dirID(first uint32) string { return dirIDPrefix + strconv.FormatUint(uint64(first), 10) }

func direntID(dir uint32, idx int) string {
	return direntIDPrefix + strconv.FormatUint(uint64(dir), 10) + ":" + strconv.Itoa(idx)
}

// parseDirentID parses "dirent:<cluster>:<index>".
func parseDirentID(id string) (dir uint32, idx int, ok bool) {
	rest, ok := strings.CutPrefix(id, direntIDPrefix)
	if !ok {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(a, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	i, err := strconv.ParseUint(b, 10, 31)
	if err != nil || i >= maxDirEntries {
		return 0, 0, false
	}
	return uint32(n), int(i), true
}

// parseDirID parses "dir:<first cluster>".
func parseDirID(id string) (uint32, bool) {
	rest, ok := strings.CutPrefix(id, dirIDPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	return uint32(n), err == nil
}

// rootFirst is the first cluster of the root directory: 0 for the fixed FAT12/16
// root, RootClus for FAT32.
func (f *FS) rootFirst() uint32 {
	if f.fatType == 32 {
		return f.b.rootClus
	}
	return 0
}

// Root returns the root directory.
func (f *FS) Root() filesys.Entry {
	return filesys.Entry{ID: dirID(f.rootFirst()), Type: filesys.TypeDir, Mode: 0o755}
}

func (f *FS) clusterSize() int { return f.b.bytsPerSec * f.b.secPerClus }

// dirChain returns the cluster chain of the directory starting at first,
// bounded to what maxDirEntries entries need. The last result is cached.
func (f *FS) dirChain(first uint32) ([]uint32, bool, error) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if c := f.dc; c != nil && c.first == first {
		return c.clusters, c.truncated, c.err
	}
	cs := f.clusterSize()
	clusters, truncated, err := f.chainN(first, (maxDirEntries*32+cs-1)/cs)
	f.dc = &dirChainEntry{first: first, clusters: clusters, truncated: truncated, err: err}
	return clusters, truncated, err
}

// scanDir calls fn with each 32-byte entry (and its index) of the directory that
// starts at first, up to the first end-of-directory entry (0x00, not passed) or
// maxDirEntries entries, until fn returns true. first is 0 for the fixed
// FAT12/16 root. When the directory chain or a read fails part-way, the entries
// before the problem have been passed, read says how many, and the error is
// returned.
func (f *FS) scanDir(first uint32, fn func(idx int, e []byte) (stop bool)) (read int, err error) {
	idx := 0
	visit := func(chunk []byte) (stop bool) {
		if !f.chargeDir(first, int64(len(chunk))) {
			return true
		}
		for i := 0; i+32 <= len(chunk); i += 32 {
			if chunk[i] == 0x00 {
				return true
			}
			if idx >= maxDirEntries {
				f.warn("directory at cluster %d has more than %d entries: the rest is not listed", first, maxDirEntries)
				return true
			}
			cur := idx
			idx++
			if fn(cur, chunk[i:i+32]) {
				return true
			}
		}
		return false
	}
	bps := f.b.bytsPerSec
	if first == 0 && f.fatType == 32 {
		return 0, corrupt("directory", -1, "a directory cannot start at cluster 0 on FAT32")
	}
	if first == 0 {
		start := (int64(f.b.rsvd) + int64(f.b.numFATs)*int64(f.b.fatSz)) * int64(bps) // inside the volume: checked by parseBoot
		total := int64(f.b.rootEnt) * 32
		buf := make([]byte, bps)
		for off := int64(0); off < total; off += int64(bps) {
			chunk := buf[:min(int64(bps), total-off)]
			if err := readFull(f.r, chunk, start+off); err != nil {
				return idx, corrupt("root directory", start+off, "read failed: %v", err)
			}
			if visit(chunk) {
				return idx, nil
			}
		}
		return idx, nil
	}
	if !f.chargeDir(first, 0) {
		return 0, nil // budget spent: nothing more is read (the warning is recorded)
	}
	clusters, truncated, chainErr := f.dirChain(first)
	cs := f.clusterSize()
	buf := make([]byte, min(cs, dirChunk)) // a power of two that divides cs
	for _, c := range clusters {
		off, ok := f.clusterOffset(c)
		if !ok {
			return idx, corrupt("directory", -1, "cluster %d has no valid offset", c)
		}
		for pos := 0; pos < cs; pos += len(buf) {
			if err := readFull(f.r, buf, off+int64(pos)); err != nil {
				return idx, corrupt("directory", off+int64(pos), "read failed: %v", err)
			}
			if visit(buf) {
				return idx, nil
			}
		}
	}
	if truncated && chainErr == nil {
		f.warn("directory at cluster %d is longer than %d entries: the rest is not listed", first, maxDirEntries)
	}
	return idx, chainErr
}

// rawEntry reads entry idx of the directory that starts at first (0: the fixed
// FAT12/16 root).
func (f *FS) rawEntry(first uint32, idx int) ([]byte, error) {
	const st = "directory entry"
	if idx < 0 || idx >= maxDirEntries {
		return nil, corrupt(st, -1, "entry index %d is outside the %d allowed", idx, maxDirEntries)
	}
	buf := make([]byte, 32)
	if first == 0 && f.fatType != 32 {
		if idx >= f.b.rootEnt {
			return nil, corrupt(st, -1, "entry %d lies beyond the %d root directory entries", idx, f.b.rootEnt)
		}
		start := (int64(f.b.rsvd) + int64(f.b.numFATs)*int64(f.b.fatSz)) * int64(f.b.bytsPerSec)
		if err := readFull(f.r, buf, start+int64(idx)*32); err != nil {
			return nil, corrupt(st, start+int64(idx)*32, "read failed: %v", err)
		}
		return buf, nil
	}
	clusters, _, err := f.dirChain(first)
	cs := int64(f.clusterSize())
	k := int64(idx) * 32 / cs
	if k >= int64(len(clusters)) {
		if err != nil {
			return nil, err
		}
		return nil, corrupt(st, -1, "entry %d lies beyond the directory at cluster %d", idx, first)
	}
	off, ok := f.clusterOffset(clusters[k])
	if !ok {
		return nil, corrupt(st, -1, "cluster %d has no valid offset", clusters[k])
	}
	off += int64(idx) * 32 % cs
	if err := readFull(f.r, buf, off); err != nil {
		return nil, corrupt(st, off, "read failed: %v", err)
	}
	return buf, nil
}

// firstCluster is the first cluster of an entry (the high half exists only on FAT32).
func (f *FS) firstCluster(e []byte) uint32 {
	c := uint32(binary.LittleEndian.Uint16(e[26:]))
	if f.fatType == 32 {
		c |= uint32(binary.LittleEndian.Uint16(e[20:])) << 16
	}
	return c
}

// dosTimestamp decodes a FAT date and time (local time, no zone): the date has
// year-1980 in bits 9-15, month in 5-8, day in 0-4; the time has hour in 11-15,
// minute in 5-10 and seconds/2 in 0-4. tenth adds 0-199 x 10 ms. A zero date
// or any impossible field gives an absent timestamp.
func dosTimestamp(date, tm uint16, tenth byte) filesys.Timestamp {
	if date == 0 {
		return filesys.Timestamp{}
	}
	y, mo, d := 1980+int(date>>9), int(date>>5&0xF), int(date&0x1F)
	h, mi, s := int(tm>>11), int(tm>>5&0x3F), int(tm&0x1F)*2
	if mo < 1 || mo > 12 || d < 1 || h > 23 || mi > 59 || s > 58 {
		return filesys.Timestamp{}
	}
	ns := 0
	if tenth <= 199 {
		s += int(tenth / 100)
		ns = int(tenth%100) * 10_000_000
	}
	t := time.Date(y, time.Month(mo), d, h, mi, s, ns, time.UTC)
	if t.Day() != d { // 31 February ...
		return filesys.Timestamp{}
	}
	return filesys.Timestamp{T: t}
}

// dirItem is a listed entry together with what Lookup needs.
type dirItem struct {
	e       filesys.Entry
	short   string // the 8.3 name as shown (with its NTRes case flags)
	cluster uint32 // first cluster of the entry
	idx     int    // index of the short entry in its directory
}

// makeItem builds the item for a short entry; ok is false for entries that are
// not listed (".", "..", the volume label).
func (f *FS) makeItem(e []byte, idx int, dir uint32, deleted bool, units []uint16, have bool, lfnFlags ...string) (dirItem, bool) {
	attr := e[11]
	if attr&0x18 == attrVolume {
		return dirItem{}, false // the volume label (Info.Label)
	}
	if !deleted && attr&attrDir != 0 && (slices.Equal(e[:11], []byte(".          ")) || slices.Equal(e[:11], []byte("..         "))) {
		return dirItem{}, false
	}
	size := int64(binary.LittleEndian.Uint32(e[28:]))
	cluster := f.firstCluster(e)

	shown, name, raw := shortName(e[:11], e[12], deleted)
	it := dirItem{short: shown, cluster: cluster, idx: idx}
	if have {
		name, raw = longName(units)
	}
	en := filesys.Entry{Name: name, RawName: raw, Deleted: deleted}
	switch attr & 0x18 {
	case attrDir:
		en.Type = filesys.TypeDir
	case 0:
		en.Type = filesys.TypeFile
		en.Size = size
	default:
		en.Type = filesys.TypeOther
		en.Size = size
	}
	en.Mode = 0o644
	if en.Type == filesys.TypeDir {
		en.Mode = 0o755
	}
	if attr&attrReadOnly != 0 {
		en.Mode &^= 0o222
	}
	live := en.Type == filesys.TypeDir && !deleted
	if live {
		en.ID = dirID(cluster)
	} else {
		en.ID = direntID(dir, idx)
	}
	en.Attrs = []filesys.KV{
		{Key: "attr", Value: attrString(attr)},
		{Key: "first_cluster", Value: strconv.FormatUint(uint64(cluster), 10)},
		{Key: "short_name", Value: shown},
	}
	if !live {
		en.Attrs = append(en.Attrs, filesys.KV{Key: "size", Value: strconv.FormatInt(size, 10)})
	} else {
		en.Attrs = append(en.Attrs, filesys.KV{Key: "dirent", Value: strconv.FormatUint(uint64(dir), 10) + ":" + strconv.Itoa(idx)})
	}
	for _, fl := range lfnFlags {
		en.Attrs = append(en.Attrs, filesys.KV{Key: "lfn", Value: fl})
	}
	en.Times = filesys.Times{
		Created:  dosTimestamp(binary.LittleEndian.Uint16(e[16:]), binary.LittleEndian.Uint16(e[14:]), e[13]),
		Modified: dosTimestamp(binary.LittleEndian.Uint16(e[24:]), binary.LittleEndian.Uint16(e[22:]), 0),
		Accessed: dosTimestamp(binary.LittleEndian.Uint16(e[18:]), 0, 0),
	}
	it.e = en
	return it, true
}

// listDir lists the directory that starts at first (0: the fixed FAT12/16
// root): live and deleted entries in disk order. When the directory is damaged
// part-way, the entries before the damage are returned and the damage is an
// Info warning; only a directory of which nothing can be read is an error.
func (f *FS) listDir(first uint32) ([]dirItem, error) {
	var (
		items   []dirItem
		lfn     lfnAsm
		pending [][32]byte // deleted long-name entries right before the current one
	)
	read, err := f.scanDir(first, func(idx int, e []byte) bool {
		switch {
		case isLFN(e) && e[0] != 0xE5:
			pending = pending[:0]
			lfn.add(e)
		case isLFN(e):
			if lfn.active { // a live set was interrupted by a deleted entry: it cannot name the next short entry
				lfn.active, lfn.broken = false, true
			}
			if len(pending) == maxLFNEntries {
				pending = slices.Delete(pending, 0, 1)
			}
			pending = append(pending, [32]byte(e))
		default:
			deleted := e[0] == 0xE5
			var (
				units []uint16
				have  bool
				flags []string
			)
			if deleted {
				lfn.reset()
				var unterminated bool
				if units, unterminated, have = deletedLFN(pending, e[:11]); have {
					flags = append(flags, "recovered")
					if unterminated {
						flags = append(flags, "unterminated")
					}
				}
			} else {
				var orphan bool
				if units, have, orphan = lfn.take(e[:11]); orphan {
					flags = append(flags, "orphan")
				}
			}
			pending = pending[:0]
			if it, ok := f.makeItem(e, idx, first, deleted, units, have, flags...); ok {
				items = append(items, it)
			}
		}
		return false
	})
	if err != nil {
		if read == 0 {
			return nil, err
		}
		f.warn("directory at cluster %d is damaged after %d entries: %v", first, read, err)
	}
	f.learnDirs(first, items)
	return items, nil
}

// noteDir records a directory (f.dmu held), within the bound.
func (f *FS) noteDir(first uint32, loc dirLoc) {
	if f.dirs == nil {
		f.dirs = map[uint32]dirLoc{}
	}
	if len(f.dirs) < maxKnownDirs {
		f.dirs[first] = loc
	}
}

// learnDirs remembers where the live directories of a listing are, so that a
// directory ID can be resolved without searching.
func (f *FS) learnDirs(parent uint32, items []dirItem) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	for _, it := range items {
		if it.e.Type != filesys.TypeDir || it.e.Deleted || it.cluster == f.rootFirst() || !f.validCluster(it.cluster) || len(f.dirs) >= maxKnownDirs {
			continue
		}
		if _, ok := f.dirs[it.cluster]; ok {
			continue
		}
		f.noteDir(it.cluster, dirLoc{parent: parent, idx: it.idx})
	}
}

func attrOf(e filesys.Entry, key string) string {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// knownDir reports whether the directory starting at first has been met in a
// listing (the root always is).
func (f *FS) knownDir(first uint32) bool {
	if first == f.rootFirst() {
		return true
	}
	f.dmu.Lock()
	defer f.dmu.Unlock()
	_, ok := f.dirs[first]
	return ok
}

// isLiveDirAt reports whether entry idx of the directory at parent is a live
// directory whose first cluster is first.
func (f *FS) isLiveDirAt(parent uint32, idx int, first uint32) bool {
	e, err := f.rawEntry(parent, idx)
	if err != nil || e[0] == 0x00 || e[0] == 0xE5 || isLFN(e) || e[11]&0x18 != attrDir {
		return false
	}
	return f.firstCluster(e) == first
}

// resolveDir makes sure first is the first cluster of a live directory of this
// volume. Nothing the caller supplies is trusted: the hint (the dirent=P:I
// attribute of the caller's entry, "" for none) is only a place to look, and the
// entry found there must be a live directory with this first cluster in a known
// directory. Otherwise the tree is searched from the root, within bounds.
func (f *FS) resolveDir(first uint32, hint string) error {
	if f.knownDir(first) {
		return nil
	}
	if p, i, ok := strings.Cut(hint, ":"); ok {
		pc, err1 := strconv.ParseUint(p, 10, 32)
		idx, err2 := strconv.Atoi(i)
		if err1 == nil && err2 == nil && f.knownDir(uint32(pc)) && f.isLiveDirAt(uint32(pc), idx, first) {
			f.dmu.Lock()
			f.noteDir(first, dirLoc{parent: uint32(pc), idx: idx})
			f.dmu.Unlock()
			return nil
		}
	}
	// Breadth-first search from the root; every listing teaches f.dirs.
	queue := []uint32{f.rootFirst()}
	seen := map[uint32]bool{f.rootFirst(): true}
	for scans := 0; len(queue) > 0; scans++ {
		if scans >= maxLocateScans {
			f.warn("directory at cluster %d not found within %d directories", first, maxLocateScans)
			break
		}
		items, err := f.listDir(queue[0])
		queue = queue[1:]
		if err != nil {
			continue
		}
		if f.knownDir(first) {
			return nil
		}
		for _, it := range items {
			if it.e.Type == filesys.TypeDir && !it.e.Deleted {
				if !seen[it.cluster] && f.validCluster(it.cluster) {
					seen[it.cluster] = true
					queue = append(queue, it.cluster)
				}
			}
		}
	}
	if f.knownDir(first) {
		return nil
	}
	return fmt.Errorf("%w: no live directory starts at cluster %d", filesys.ErrNotFound, first)
}

// dirTarget returns the first cluster of the directory dir names, re-derived
// from the volume: only dir.ID is used (dir.Attrs supply a hint where to look,
// nothing else; Size, Type, Deleted and the other attributes are ignored).
// "dir:<first>" must be the root, a directory met in a listing, or be found on
// disk; "dirent:<P>:<I>" must name a live directory entry (a deleted one is
// filesys.ErrDeleted, any other kind filesys.ErrUnsupported).
func (f *FS) dirTarget(dir filesys.Entry) (uint32, error) {
	if n, ok := parseDirID(dir.ID); ok {
		if n == 0 && f.fatType == 32 {
			return 0, corrupt("directory", -1, "a directory cannot start at cluster 0 on FAT32")
		}
		if n != f.rootFirst() && !f.validCluster(n) {
			return 0, corrupt("directory", -1, "directory cluster %d is outside the %d clusters", n, f.count)
		}
		if err := f.resolveDir(n, attrOf(dir, "dirent")); err != nil {
			return 0, err
		}
		return n, nil
	}
	p, idx, ok := parseDirentID(dir.ID)
	if !ok {
		return 0, fmt.Errorf("%w: %q is not a FAT directory id", filesys.ErrNotFound, dir.ID)
	}
	e, err := f.liveEntryAt(p, idx)
	if err != nil {
		return 0, err
	}
	if e[11]&0x18 != attrDir {
		return 0, fmt.Errorf("%w: entry %d of the directory at cluster %d is not a directory", filesys.ErrUnsupported, idx, p)
	}
	n := f.firstCluster(e)
	if n == 0 && f.fatType == 32 {
		return 0, corrupt("directory", -1, "a directory cannot start at cluster 0 on FAT32")
	}
	if n != f.rootFirst() && !f.validCluster(n) {
		return 0, corrupt("directory", -1, "directory cluster %d is outside the %d clusters", n, f.count)
	}
	return n, nil
}

// liveEntryAt reads entry idx of the directory at cluster p, which must be a
// directory of this volume, and returns it if it is a live short entry; a
// deleted one is filesys.ErrDeleted.
func (f *FS) liveEntryAt(p uint32, idx int) ([]byte, error) {
	if (p != 0 || f.fatType == 32) && !f.validCluster(p) {
		return nil, fmt.Errorf("%w: cluster %d cannot hold a directory", filesys.ErrNotFound, p)
	}
	if err := f.resolveDir(p, ""); err != nil {
		return nil, err
	}
	e, err := f.rawEntry(p, idx)
	if err != nil {
		return nil, err
	}
	switch {
	case e[0] == 0x00:
		return nil, corrupt("directory entry", -1, "entry %d of the directory at cluster %d is free", idx, p)
	case isLFN(e):
		return nil, corrupt("directory entry", -1, "entry %d of the directory at cluster %d is a long-name entry", idx, p)
	case e[0] == 0xE5:
		return nil, filesys.ErrDeleted
	}
	return e, nil
}

// ReadDir lists the live and deleted entries of dir, in disk order; ".", ".."
// and the volume label are omitted. The directory is identified by dir.ID alone
// and re-read from the volume: Entry fields and Attrs the caller changed have no
// effect (attrs only hint where to look). A directory chain that loops, breaks
// or exceeds 65536 entries yields the entries read so far and an Info warning.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	first, err := f.dirTarget(dir)
	if err != nil {
		return nil, err
	}
	items, err := f.listDir(first)
	if err != nil {
		return nil, err
	}
	out := make([]filesys.Entry, len(items))
	for i := range items {
		out[i] = items[i].e
	}
	return out, nil
}

var errNoChild = errors.New("no such entry")

// child finds the live entry named comp in the directory at first. Preference
// over the whole directory: an exact match of the long or the short name, then
// the "~raw~" display form, then a case-insensitive match (FAT is not case
// sensitive).
func (f *FS) child(first uint32, comp string) (dirItem, error) {
	items, err := f.listDir(first)
	if err != nil {
		return dirItem{}, err
	}
	var alt []byte
	if rest, ok := strings.CutPrefix(comp, rawPrefix); ok {
		if b, err := base64.RawURLEncoding.DecodeString(rest); err == nil {
			alt = b
		}
	}
	var exact, viaRaw, fold *dirItem
	for i := range items {
		it := &items[i]
		if it.e.Deleted {
			continue
		}
		switch {
		case it.e.Name == comp || it.short == comp:
			exact = it
		case viaRaw == nil && alt != nil && it.e.RawName != nil && slices.Equal(it.e.RawName, alt):
			viaRaw = it
		case fold == nil && (strings.EqualFold(it.e.Name, comp) || strings.EqualFold(it.short, comp)):
			fold = it
		}
		if exact != nil {
			break
		}
	}
	for _, it := range []*dirItem{exact, viaRaw, fold} {
		if it != nil {
			return *it, nil
		}
	}
	return dirItem{}, errNoChild
}

// Lookup resolves an absolute slash path through live entries only. Names are
// matched as described at child: long and 8.3 names, case-insensitively.
// Empty components are ignored, "." and ".." are not special.
func (f *FS) Lookup(p string) (filesys.Entry, error) {
	cur, first := f.Root(), f.rootFirst()
	for _, comp := range strings.Split(p, "/") {
		if comp == "" {
			continue
		}
		if cur.Type != filesys.TypeDir {
			return filesys.Entry{}, fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
		}
		it, err := f.child(first, comp)
		if err != nil {
			if errors.Is(err, errNoChild) {
				err = fmt.Errorf("%w: %s", filesys.ErrNotFound, p)
			}
			return filesys.Entry{}, err
		}
		cur, first = it.e, it.cluster
	}
	return cur, nil
}

// chargeDir takes bytes (and the entries they hold) from the directory read
// budget; it reports false, with one warning, once the budget is spent. A zero
// charge only asks whether anything is left.
func (f *FS) chargeDir(first uint32, bytes int64) bool {
	f.dmu.Lock()
	ok := f.dirBudget >= bytes && f.dirBudget > 0 && f.entryBudget >= bytes/32 && f.entryBudget > 0
	if ok {
		f.dirBudget -= bytes
		f.entryBudget -= bytes / 32
	}
	warn := !ok && !f.budgetWarned
	f.budgetWarned = f.budgetWarned || !ok
	f.dmu.Unlock()
	if warn {
		f.warn("directory read budget of %d bytes / %d entries per volume exhausted (at the directory starting at cluster %d); directories are no longer read", maxDirBudget, maxEntryBudget, first)
	}
	return ok
}
