package fat

import (
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

// parseCanonical parses a canonical decimal that fits in bits bits: digits
// only, no sign, space or leading zero, so every number has exactly one
// spelling and an Entry ID has exactly one form.
func parseCanonical(s string, bits int) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, bits)
	if err != nil || strconv.FormatUint(n, 10) != s {
		return 0, false
	}
	return n, true
}

// parseDirentID parses "dirent:<cluster>:<index>" (canonical decimals).
func parseDirentID(id string) (dir uint32, idx int, ok bool) {
	rest, ok := strings.CutPrefix(id, direntIDPrefix)
	if !ok {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, 0, false
	}
	n, ok := parseCanonical(a, 32)
	if !ok {
		return 0, 0, false
	}
	// The 31-bit width is a runaway guard on the number's size, not a statement
	// about the directory; maxDirEntries is the real limit.
	i, ok := parseCanonical(b, 31)
	if !ok || i >= maxDirEntries {
		return 0, 0, false
	}
	return uint32(n), int(i), true
}

// parseDirID parses "dir:<first cluster>" (a canonical decimal).
func parseDirID(id string) (uint32, bool) {
	rest, ok := strings.CutPrefix(id, dirIDPrefix)
	if !ok {
		return 0, false
	}
	n, ok := parseCanonical(rest, 32)
	return uint32(n), ok
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
	clusters, truncated, err := f.chainN(first, (f.dirEntryCap*32+cs-1)/cs)
	f.dc = &dirChainEntry{first: first, clusters: clusters, truncated: truncated, err: err}
	return clusters, truncated, err
}

// scanCut says why a scan did not read a directory to its end: the zero value
// is a complete scan. limit is set when a bound of the reader caused it (the
// entry cap, the directory-chain bound, the spent read budget) and not damage.
type scanCut struct {
	reason string
	limit  bool
}

func (c scanCut) isCut() bool { return c.reason != "" }

// errBudget is the error of a directory that cannot be started because the
// directory read budget is spent.
func errBudget(first uint32) error {
	return corrupt("directory", -1, "directory read budget exhausted before the directory at cluster %d was read", first)
}

// scan calls fn with each 32-byte entry (and its index) of the directory that
// starts at first (0 for the fixed FAT12/16 root), up to the first
// end-of-directory entry (0x00, not passed) or the entry cap, until fn returns
// true. It takes the choice of charging the directory read budget and returns
// the cut it reports. The scan is cut when it stopped before an end-of-directory
// entry without fn asking for it: the entry cap, the directory-chain bound, a
// chain error or a failed read, the budget spent after the first entry. Damage
// after the end marker is not a cut. A charged scan whose budget is spent
// before its first entry returns an error (never an empty listing, and not a
// cut: nothing of the directory was read); one cut short after entries were read
// returns them with a nil error and the cut (the budget warning is recorded).
func (f *FS) scan(first uint32, charged bool, fn func(idx int, e []byte) (stop bool)) (read int, cut scanCut, err error) {
	idx := 0
	exhausted := false
	visit := func(chunk []byte) (stop bool) {
		if charged && !f.chargeDir(first, int64(len(chunk))) {
			exhausted = true
			return true
		}
		for i := 0; i+32 <= len(chunk); i += 32 {
			if chunk[i] == 0x00 {
				return true // the end of the directory
			}
			if idx >= f.dirEntryCap {
				f.warn("directory at cluster %d has more than %d entries: the rest is not listed", first, f.dirEntryCap)
				cut = scanCut{reason: fmt.Sprintf("the directory has more than %d entries", f.dirEntryCap), limit: true}
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
	// stopped is the result of a scan whose visit asked to stop: the end marker,
	// fn, the entry cap (cut is set) or the spent budget.
	stopped := func() (int, scanCut, error) {
		if exhausted {
			if idx == 0 {
				return idx, scanCut{}, errBudget(first)
			}
			return idx, scanCut{reason: "the directory read budget is spent", limit: true}, nil
		}
		return idx, cut, nil
	}
	bps := f.b.bytsPerSec
	if first == 0 && f.fatType == 32 {
		return 0, scanCut{}, corrupt("directory", -1, "a directory cannot start at cluster 0 on FAT32")
	}
	if first == 0 {
		start := (int64(f.b.rsvd) + int64(f.b.numFATs)*int64(f.b.fatSz)) * int64(bps) // inside the volume: checked by parseBoot
		total := int64(f.b.rootEnt) * 32
		buf := make([]byte, bps)
		for off := int64(0); off < total; off += int64(bps) {
			chunk := buf[:min(int64(bps), total-off)]
			if err := readFull(f.r, chunk, start+off); err != nil {
				return idx, scanCut{reason: fmt.Sprintf("the root directory cannot be read at offset %d: %v", start+off, err)}, corrupt("root directory", start+off, "read failed: %v", err)
			}
			if visit(chunk) {
				return stopped()
			}
		}
		return idx, scanCut{}, nil
	}
	if charged && !f.chargeDir(first, 0) {
		return 0, scanCut{}, errBudget(first) // the warning is recorded
	}
	clusters, truncated, chainErr := f.dirChain(first)
	cs := f.clusterSize()
	buf := make([]byte, min(cs, dirChunk)) // a power of two that divides cs
	for _, c := range clusters {
		off, ok := f.clusterOffset(c)
		if !ok {
			err := corrupt("directory", -1, "cluster %d has no valid offset", c)
			return idx, scanCut{reason: fmt.Sprintf("cluster %d has no valid offset", c)}, err
		}
		for pos := 0; pos < cs; pos += len(buf) {
			if err := readFull(f.r, buf, off+int64(pos)); err != nil {
				return idx, scanCut{reason: fmt.Sprintf("the directory cannot be read at offset %d: %v", off+int64(pos), err)}, corrupt("directory", off+int64(pos), "read failed: %v", err)
			}
			if visit(buf) {
				return stopped()
			}
		}
	}
	switch {
	case chainErr != nil:
		return idx, scanCut{reason: fmt.Sprintf("the directory chain is damaged after %d entries: %v", idx, chainErr)}, chainErr
	case truncated:
		f.warn("directory at cluster %d is longer than %d entries: the rest is not listed", first, f.dirEntryCap)
		return idx, scanCut{reason: fmt.Sprintf("the directory chain is longer than %d entries", f.dirEntryCap), limit: true}, nil
	}
	return idx, scanCut{}, nil
}

// dirEnd returns the index of the first end-of-directory entry (0x00) of the
// directory that starts at first: the entries from there on are not part of the
// directory, whatever bytes they hold. It is maxDirEntries when the directory
// has no end marker within the entries that can be read, and also when the
// scan was cut or failed (a broken chain: the callers then meet the same
// problem themselves). A cut index is never taken as the end marker: an entry ID
// beyond a cut is undecidable, not beyond the end. The scan is not charged to
// the directory read budget (it reads at most one directory, once: the result is
// remembered).
func (f *FS) dirEnd(first uint32) int {
	f.dmu.Lock()
	n, ok := f.ends[first]
	f.dmu.Unlock()
	if ok {
		return n
	}
	read, cut, err := f.scan(first, false, func(int, []byte) bool { return false })
	if err != nil || cut.isCut() {
		return maxDirEntries
	}
	f.dmu.Lock()
	if f.ends == nil {
		f.ends = map[uint32]int{}
	}
	if len(f.ends) < maxKnownDirs {
		f.ends[first] = read
	}
	f.dmu.Unlock()
	return read
}

func endErr(idx int, first uint32, end int) error {
	return fmt.Errorf("%w: entry %d is at or beyond the end of the directory at cluster %d (entry %d is its end marker)", filesys.ErrNotFound, idx, first, end)
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
		if end := f.dirEnd(first); idx >= end {
			return nil, endErr(idx, first, end)
		}
		start := (int64(f.b.rsvd) + int64(f.b.numFATs)*int64(f.b.fatSz)) * int64(f.b.bytsPerSec)
		if err := readFull(f.r, buf, start+int64(idx)*32); err != nil {
			return nil, corrupt(st, start+int64(idx)*32, "read failed: %v", err)
		}
		return buf, nil
	}
	if end := f.dirEnd(first); idx >= end {
		return nil, endErr(idx, first, end)
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
	aliased bool   // e.Name is the "~raw~" display form (a name that cannot be shown as it is)
	cluster uint32 // first cluster of the entry
	idx     int    // index of the short entry in its directory
}

// makeItem builds the item for a short entry; extra are attrs added after the
// standard ones (each key at most once: lfn=orphan or lfn=recovered, and
// lfn_tail=unterminated). ok is false for entries that are
// not listed (".", "..", the volume label).
func (f *FS) makeItem(e []byte, idx int, dir uint32, deleted bool, units []uint16, have bool, extra ...filesys.KV) (dirItem, bool) {
	attr := e[11]
	if attr&0x18 == attrVolume {
		return dirItem{}, false // the volume label (Info.Label)
	}
	if !deleted && attr&attrDir != 0 && (slices.Equal(e[:11], []byte(".          ")) || slices.Equal(e[:11], []byte("..         "))) {
		return dirItem{}, false
	}
	size := int64(binary.LittleEndian.Uint32(e[28:]))
	cluster := f.firstCluster(e)

	shown, name, raw, aliased := shortName(e[:11], e[12], deleted)
	it := dirItem{short: shown, cluster: cluster, idx: idx}
	if have {
		name, raw, aliased = longName(units)
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
	en.Attrs = append(en.Attrs, extra...)
	en.Times = filesys.Times{
		Created:  dosTimestamp(binary.LittleEndian.Uint16(e[16:]), binary.LittleEndian.Uint16(e[14:]), e[13]),
		Modified: dosTimestamp(binary.LittleEndian.Uint16(e[24:]), binary.LittleEndian.Uint16(e[22:]), 0),
		Accessed: dosTimestamp(binary.LittleEndian.Uint16(e[18:]), 0, 0),
	}
	it.e, it.aliased = en, aliased
	return it, true
}

// listDir lists the directory that starts at first (0: the fixed FAT12/16
// root): live and deleted entries in disk order. When the directory is damaged
// part-way, the entries before the damage are returned with the cut (the damage
// is also an Info warning); only a directory of which nothing can be read is an
// error.
func (f *FS) listDir(first uint32) ([]dirItem, scanCut, error) {
	var (
		items   []dirItem
		lfn     lfnAsm
		pending [][32]byte // deleted long-name entries right before the current one
	)
	read, cut, err := f.scan(first, true, func(idx int, e []byte) bool {
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
				flags []filesys.KV
			)
			if deleted {
				lfn.reset()
				var unterminated bool
				if units, unterminated, have = deletedLFN(pending, e[:11]); have {
					flags = append(flags, filesys.KV{Key: "lfn", Value: "recovered"})
					if unterminated {
						flags = append(flags, filesys.KV{Key: "lfn_tail", Value: "unterminated"})
					}
				}
			} else {
				var orphan bool
				if units, have, orphan = lfn.take(e[:11]); orphan {
					flags = append(flags, filesys.KV{Key: "lfn", Value: "orphan"})
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
			return nil, cut, err
		}
		f.warn("directory at cluster %d is damaged after %d entries: %v", first, read, err)
	}
	f.learnDirs(first, items)
	return items, cut, nil
}

// incompleteLookup is the error of a lookup of comp that found nothing in the
// directory at first, which the scan did not read to the end: the name may be in
// the part that was not read.
func incompleteLookup(first uint32, comp string, cut scanCut) error {
	return cutError(first, fmt.Sprintf("lookup of %q cannot be decided", comp), cut)
}

// cutListing is the error of a listing that was cut: it comes with the entries
// that were read.
func cutListing(first uint32, cut scanCut) error {
	return cutError(first, "listing is incomplete", cut)
}

// cutError wraps filesys.ErrIncomplete together with filesys.ErrLimit (a bound
// of the reader) or a *filesys.CorruptError (damage or a failed read).
func cutError(first uint32, what string, cut scanCut) error {
	if cut.limit {
		return fmt.Errorf("fat: %w: %w: directory at cluster %d: %s: %s", filesys.ErrLimit, filesys.ErrIncomplete, first, what, cut.reason)
	}
	return fmt.Errorf("%w: %w", corrupt("directory", -1, "directory at cluster %d: %s: %s", first, what, cut.reason), filesys.ErrIncomplete)
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
		pc, ok1 := parseCanonical(p, 32)
		ic, ok2 := parseCanonical(i, 31)
		if ok1 && ok2 && f.knownDir(uint32(pc)) && f.isLiveDirAt(uint32(pc), int(ic), first) {
			f.dmu.Lock()
			f.noteDir(first, dirLoc{parent: uint32(pc), idx: int(ic)})
			f.dmu.Unlock()
			return nil
		}
	}
	// Breadth-first search from the root; every listing teaches f.dirs.
	queue := []uint32{f.rootFirst()}
	seen := map[uint32]bool{f.rootFirst(): true}
	var listErr error // the first listing that failed: the subtree behind it is unknown
	for scans := 0; len(queue) > 0; scans++ {
		if scans >= f.locateScans {
			// Cut off: the directory is undecidable, which is not absent.
			f.warn("directory at cluster %d not found within %d directories", first, f.locateScans)
			if f.knownDir(first) {
				return nil
			}
			return fmt.Errorf("%w: the search for the directory at cluster %d stopped after %d directories", filesys.ErrLimit, first, f.locateScans)
		}
		items, cut, err := f.listDir(queue[0])
		if err == nil && cut.isCut() && listErr == nil {
			listErr = cutListing(queue[0], cut) // the subdirectories of the entries read are still searched
		}
		queue = queue[1:]
		if err != nil {
			if listErr == nil {
				listErr = err
			}
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
	if listErr != nil { // unknown is never absent: part of the tree could not be read
		return fmt.Errorf("the search for the directory at cluster %d could not read every directory: %w", first, listErr)
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
// or exceeds 65536 entries, a failed read and the spent budget cut the listing:
// it yields the entries read so far together with an error matching
// filesys.ErrIncomplete (and filesys.ErrLimit for the entry cap, the chain bound
// and the budget; a *filesys.CorruptError for damage), and an Info warning.
// A directory that cannot be started because the per-volume directory read
// budget is spent is a *filesys.CorruptError ("directory read budget
// exhausted"), never an empty listing; one cut short by it is listed as far as
// it was read, with the budget warning and the ErrIncomplete error.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	first, err := f.dirTarget(dir)
	if err != nil {
		return nil, err
	}
	items, cut, err := f.listDir(first)
	if err != nil {
		return nil, err
	}
	out := make([]filesys.Entry, len(items))
	for i := range items {
		out[i] = items[i].e
	}
	if cut.isCut() {
		return out, cutListing(first, cut)
	}
	return out, nil
}

var errNoChild = errors.New("no such entry")

// child finds the live entry named comp in the directory at first. The whole
// directory is scanned (at most two case-insensitive candidates are kept) and
// the answer is decided in one order: (1) an exact match of a real name (the
// long name when it is not a display form, or the 8.3 name); two exact records,
// or an exact record and another record whose "~raw~" display form is comp, are
// ambiguous; (2) the canonical "~raw~" form, standing only for records ReadDir
// shows in that form; two such records are ambiguous; (3) the scan was cut: the
// incomplete-lookup error, because the unread part may hold a record that
// outranks a case-insensitive or alias hit (only an exact match stands on a cut
// scan); (4) one case-insensitive match (the reader's own convention: Unicode
// simple case folding of the long or the 8.3 name; the volume records no rule);
// (5) two or more: ErrAmbiguous; (6) no such entry. "." and ".." never match by
// folding. Unlike exFAT, a long-name set that was lost leaves no undecided
// state: the entry is matched by its 8.3 name only.
func (f *FS) child(first uint32, comp string) (dirItem, error) {
	items, cut, err := f.listDir(first)
	if err != nil {
		return dirItem{}, err
	}
	raw, wantAlias := filesys.DecodeRawNameAlias(comp)
	dots := comp == "." || comp == ".."
	var exact, alias, fold0, fold1 *dirItem
	var nExact, nAlias, nFold int
	for i := range items {
		it := &items[i]
		if it.e.Deleted {
			continue
		}
		switch {
		case !dots && ((!it.aliased && it.e.Name == comp) || it.short == comp):
			if nExact == 0 {
				exact = it
			}
			nExact++
		case wantAlias && it.aliased && slices.Equal(it.e.RawName, raw):
			if nAlias == 0 {
				alias = it
			}
			nAlias++
		case !dots && ((!it.aliased && strings.EqualFold(it.e.Name, comp)) || strings.EqualFold(it.short, comp)):
			switch nFold {
			case 0:
				fold0 = it
			case 1:
				fold1 = it
			}
			nFold++
		}
	}
	switch {
	case nExact > 1:
		return dirItem{}, fmt.Errorf("%w: %q names %d entries of the directory at cluster %d", filesys.ErrAmbiguous, comp, nExact, first)
	case nExact == 1 && nAlias > 0:
		if cut.isCut() {
			return dirItem{}, incompleteLookup(first, comp, cut)
		}
		return dirItem{}, fmt.Errorf("%w: %q is a stored name and also the display form of another entry of the directory at cluster %d", filesys.ErrAmbiguous, comp, first)
	case nExact == 1:
		return *exact, nil
	case nAlias > 1:
		return dirItem{}, fmt.Errorf("%w: %q names %d entries of the directory at cluster %d", filesys.ErrAmbiguous, comp, nAlias, first)
	case cut.isCut(): // the name may be in the part that was not read
		return dirItem{}, incompleteLookup(first, comp, cut)
	case nAlias == 1:
		return *alias, nil
	case nFold == 1:
		return *fold0, nil
	case nFold > 1:
		return dirItem{}, fmt.Errorf("%w: %q matches %d names of the directory at cluster %d case-insensitively: %q, %q and %d more",
			filesys.ErrAmbiguous, comp, nFold, first, fold0.e.Name, fold1.e.Name, nFold-2)
	}
	return dirItem{}, errNoChild
}

// Lookup resolves an absolute slash path through live entries only. Names are
// matched as described at child: long and 8.3 names, case-insensitively.
// Empty components are ignored, "." and ".." are not special. A name that is
// not found in a directory that was cut (see ReadDir) is an error matching
// filesys.ErrIncomplete, never filesys.ErrNotFound.
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

// chargeDir takes bytes (as the entries they hold) from the directory read
// budget; it reports false, with one warning, once the budget is spent. A zero
// charge only asks whether anything is left.
func (f *FS) chargeDir(first uint32, bytes int64) bool {
	f.dmu.Lock()
	entries := bytes / 32
	ok := f.entryBudget > 0 && f.entryBudget >= entries
	if ok {
		f.entryBudget -= entries
	}
	warn := !ok && !f.budgetWarned
	f.budgetWarned = f.budgetWarned || !ok
	f.dmu.Unlock()
	if warn {
		f.warn("directory read budget of %d entries per volume exhausted (at the directory starting at cluster %d); directories are no longer read", maxEntryBudget, first)
	}
	return ok
}
