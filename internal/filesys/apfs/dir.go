package apfs

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	// maxDirBudget is how many bytes of directory records one FS instance
	// scans in total (listings and lookups; single-inode reads are not
	// counted). A filesystem that needs more is hostile or enormous: further
	// directories fail, a directory under way is cut with a warning.
	maxDirBudget = 1 << 30

	// maxDirEntries bounds the entries read from one directory.
	maxDirEntries = 1 << 18
)

// Root returns the container root, which lists the volumes.
func (f *FS) Root() filesys.Entry {
	return filesys.Entry{ID: rootID, Type: filesys.TypeDir}
}

// notFound builds an ErrNotFound error.
func notFound(format string, a ...any) error {
	return fmt.Errorf("apfs: %w: %s", filesys.ErrNotFound, fmt.Sprintf(format, a...))
}

// nodeTarget resolves a file-system object ID to its volume without touching
// the disk: a malformed ID, a volume that does not exist and a view that is not
// known are ErrNotFound; an encrypted volume is ErrEncrypted. Nothing but
// e.ID is used, so forged Entry fields change nothing.
func (f *FS) nodeTarget(id string) (v *volume, view, ino uint64, err error) {
	kind, slot, view, ino, ok := parseEntryID(id)
	if !ok || kind != idNode {
		return nil, 0, 0, notFound("entry ID %q", id)
	}
	v = f.slots[slot]
	if v == nil {
		return nil, 0, 0, notFound("entry ID %q names no volume", id)
	}
	if v.encrypted {
		return nil, 0, 0, v.encryptedErr()
	}
	if !v.readable {
		return nil, 0, 0, corrupt("volume", int64(v.paddr)*int64(f.bs), "volume %d is unreadable", v.slot)
	}
	if ok, err := f.viewKnown(v, view); err != nil {
		return nil, 0, 0, err
	} else if !ok {
		return nil, 0, 0, notFound("entry ID %q names no snapshot view", id)
	}
	return v, view, ino, nil
}

// ReadDir lists a directory. The container root lists the volumes (an
// encrypted volume is listed, flagged Encrypted, and cannot be listed itself).
// Only dir.ID is used: everything else is read from the disk.
func (f *FS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	kind, slot, _, _, ok := parseEntryID(dir.ID)
	if !ok {
		return nil, notFound("entry ID %q", dir.ID)
	}
	switch kind {
	case idRoot:
		out := make([]filesys.Entry, 0, len(f.vols))
		for _, v := range f.vols {
			out = append(out, f.volumeEntry(v))
		}
		return out, nil
	case idSnaps:
		if err := f.snapsTarget(slot, dir.ID); err != nil {
			return nil, err
		}
		return f.listSnapshots(f.slots[slot])
	}
	v, view, ino, err := f.nodeTarget(dir.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(v, view, ino)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("apfs: %w: inode %d of volume %d is not a directory", filesys.ErrUnsupported, ino, v.slot)
	}
	var out []filesys.Entry
	var ferr error
	shadow := shadowsSnapshots(view, ino)
	err = f.scanDir(v, view, ino, func(d *drec) bool {
		e, err := f.dirEntry(v, view, d)
		if err != nil {
			ferr = err
			return true
		}
		if shadow && string(d.name) == snapshotsDirName {
			// A real directory of that name: the synthetic one wins, this one is
			// shown (and found) in its ~raw~ form.
			e.Name, e.RawName = rawPrefix+base64.RawURLEncoding.EncodeToString(d.name), slices.Clone(d.name)
		}
		out = append(out, e)
		return false
	})
	if err == nil {
		err = ferr
	}
	if errors.Is(err, errNodeBudget) && len(out) > 0 {
		// The shared budget rule: spent before the first entry the listing is an
		// error, spent mid-directory it is cut short with a warning.
		f.warn("volume %d directory %d: the node read budget is exhausted; the listing is partial (%d entries)", v.slot, ino, len(out))
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if shadow {
		// The synthetic .snapshots directory of the volume, shown even when empty.
		se, serr := f.snapsEntry(v)
		switch {
		case serr == nil:
			out = append(out, se)
		case errors.Is(serr, errNodeBudget) && len(out) > 0:
			// The same rule as for the entries: a partial listing and a warning.
			f.warn("volume %d directory %d: the node read budget is exhausted; the listing is partial (the .snapshots directory is missing)", v.slot, ino)
		default:
			return nil, serr
		}
	}
	return out, nil
}

// shadowsSnapshots reports whether a directory is the live root of a volume,
// where the synthetic .snapshots directory lives.
func shadowsSnapshots(view, ino uint64) bool { return view == 0 && ino == rootIno }

// snapsTarget validates the volume of a snaps:<n> ID without touching the
// disk: a volume that does not exist is ErrNotFound, an encrypted one
// ErrEncrypted.
func (f *FS) snapsTarget(slot int, id string) error {
	v := f.slots[slot]
	switch {
	case v == nil:
		return notFound("entry ID %q names no volume", id)
	case v.encrypted:
		return v.encryptedErr()
	}
	return nil
}

// dirEntry builds the entry for a directory record, reading the inode it names
// (through the sibling map when the id is no inode). An inode that cannot be
// read leaves an entry typed by the record, flagged inode=unreadable.
func (f *FS) dirEntry(v *volume, view uint64, d *drec) (filesys.Entry, error) {
	name, raw := displayName(d.name)
	in, err := f.inode(v, view, d.fileID)
	if errors.Is(err, filesys.ErrNotFound) {
		t, ok, serr := f.siblingTarget(v, view, d.fileID)
		switch {
		case serr != nil && (!isNotFoundOrCorrupt(serr) || errors.Is(serr, errNodeBudget)):
			return filesys.Entry{}, serr
		case ok && t != d.fileID:
			in, err = f.inode(v, view, t)
		}
	}
	if err != nil {
		if !isNotFoundOrCorrupt(err) || errors.Is(err, errNodeBudget) {
			return filesys.Entry{}, err
		}
		f.warn("volume %d: the entry %q names object %d, whose inode cannot be read: %v", v.slot, name, d.fileID, err)
		return filesys.Entry{
			Name: name, RawName: raw, ID: nodeID(v.slot, view, d.fileID), Type: direntType(d.flags),
			Attrs: hashAttr(d, []filesys.KV{{Key: "inode", Value: "unreadable"}}),
		}, nil
	}
	e := f.inodeEntry(v, view, in, name, raw, d.flags)
	e.Attrs = hashAttr(d, e.Attrs)
	return e, nil
}

// inodeEntry describes an inode. flags is the directory record's type, used
// only when the mode carries no type.
func (f *FS) inodeEntry(v *volume, view uint64, in *inode, name string, raw []byte, flags uint16) filesys.Entry {
	e := filesys.Entry{
		Name: name, RawName: raw, ID: nodeID(v.slot, view, in.ino),
		Type: entryType(in.mode), Mode: uint32(in.mode), UID: in.uid, GID: in.gid,
		Times: inodeTimes(in),
	}
	if in.mode&modeTypeMask == 0 {
		e.Type = direntType(flags)
	}
	add := func(k, val string) { e.Attrs = append(e.Attrs, filesys.KV{Key: k, Value: val}) }
	for _, ns := range []uint64{in.create, in.mod, in.change, in.access} {
		if _, ok := timestamp(ns); !ok {
			add("time_ns", "invalid")
			break
		}
	}
	add("parent_id", strconv.FormatUint(in.parentID, 10))
	if in.privateID != in.ino {
		add("private_id", strconv.FormatUint(in.privateID, 10))
	}
	if in.isDir() {
		add("nchildren", strconv.FormatInt(int64(in.links), 10))
	} else {
		add("nlink", strconv.FormatInt(int64(in.links), 10))
	}
	if in.internalFlags != 0 {
		add("internal_flags", fmt.Sprintf("%#x", in.internalFlags))
	}
	if in.bsdFlags != 0 {
		add("bsd_flags", fmt.Sprintf("%#x", in.bsdFlags))
	}
	if in.protClass != 0 {
		add("protection_class", strconv.FormatUint(uint64(in.protClass), 10))
	}
	switch {
	case in.compressed() && in.internalFlags&inodeHasUncompressedSize != 0:
		e.Size = int64(min(in.uncompressedSize, 1<<63-1))
	case in.hasDstream:
		e.Size = in.size
	}
	if in.compressed() {
		add("compressed", in.decmpfs)
	}
	// A file with a key of its own (a dstream crypto id other than 0 and
	// CRYPTO_SW_ID); Open reports it too when only an extent carries the id.
	e.Encrypted = e.Type == filesys.TypeFile && cryptoAnomaly(in.cryptoID)
	if e.Type == filesys.TypeSymlink && in.symlinkSet {
		if in.symlinkOK {
			e.LinkTarget = string(in.symlink)
			e.Size = int64(len(in.symlink))
		} else {
			add("symlink", "unreadable")
		}
	}
	for _, n := range in.xattrNames {
		add("xattr", n)
	}
	if in.xattrCount > len(in.xattrNames) {
		add("xattr_count", strconv.Itoa(in.xattrCount))
	}
	return e
}

// Lookup resolves an absolute slash path; empty components are ignored and
// symlinks are not followed. The first component names a volume (its display
// name, or the "~raw~"+base64url form of its stored name; never case-folded).
// Within a directory the live entries are matched in this order of preference:
//
//  1. the display name exactly;
//  2. the "~raw~"+base64url alias of the stored name (canonical encodings only);
//  3. on a case- or normalization-insensitive volume, strings.EqualFold on
//     valid UTF-8 (an approximation: no normalization or Unicode folding
//     tables; never for "." and "..").
//
// An encrypted volume can be looked up but nothing below it (ErrEncrypted).
func (f *FS) Lookup(p string) (filesys.Entry, error) {
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			comps = append(comps, c)
		}
	}
	if len(comps) == 0 {
		return f.Root(), nil
	}
	v := f.matchVolume(comps[0])
	if v == nil {
		return filesys.Entry{}, notFound("%q", p)
	}
	cur := f.volumeEntry(v)
	if len(comps) == 1 {
		return cur, nil
	}
	if v.encrypted {
		return filesys.Entry{}, v.encryptedErr()
	}
	var (
		dirIno = uint64(rootIno)
		view   uint64
		kind   = idNode
	)
	for i, c := range comps[1:] {
		var next filesys.Entry
		var err error
		if kind == idSnaps {
			next, err = f.snapshotChild(v, c)
		} else {
			next, err = f.child(v, view, dirIno, c)
		}
		if err != nil {
			if errors.Is(err, errNoChild) {
				err = notFound("%q", p)
			}
			return filesys.Entry{}, err
		}
		cur = next
		if i == len(comps)-2 {
			break
		}
		if cur.Type != filesys.TypeDir {
			return filesys.Entry{}, notFound("%q: %q is not a directory", p, cur.Name)
		}
		kind, _, view, dirIno, _ = parseEntryID(cur.ID)
	}
	return cur, nil
}

var errNoChild = errors.New("no such entry")

// matchVolume finds a volume by display name, then by raw alias.
func (f *FS) matchVolume(comp string) *volume {
	for _, v := range f.vols {
		if v.display == comp {
			return v
		}
	}
	if raw, ok := rawAlias(comp); ok {
		for _, v := range f.vols {
			if v.readable && bytes.Equal(v.name, raw) {
				return v
			}
		}
	}
	return nil
}

// child finds the entry of directory dirIno named comp.
func (f *FS) child(v *volume, view, dirIno uint64, comp string) (filesys.Entry, error) {
	shadow := shadowsSnapshots(view, dirIno)
	if shadow && comp == snapshotsDirName {
		return f.snapsEntry(v) // the synthetic directory wins over a real one of that name
	}
	alt, hasAlt := rawAlias(comp)
	fold := v.folds() && utf8.ValidString(comp) && comp != "." && comp != ".."
	var exact, alias, folded *drec
	keep := func(d *drec) *drec {
		c := *d
		c.name = slices.Clone(d.name)
		return &c
	}
	err := f.scanDir(v, view, dirIno, func(d *drec) bool {
		plain := plainName(d.name)
		isAlt := hasAlt && bytes.Equal(d.name, alt)
		if shadow && string(d.name) == snapshotsDirName {
			// Reached only through its ~raw~ alias, never by name or by folding.
			if isAlt {
				exact = keep(d)
				return true
			}
			return false
		}
		switch {
		case plain && string(d.name) == comp, !plain && isAlt:
			exact = keep(d)
			return true
		case alias == nil && plain && isAlt:
			alias = keep(d)
		case folded == nil && fold && plain && strings.EqualFold(string(d.name), comp):
			folded = keep(d)
		}
		return false
	})
	if err != nil {
		return filesys.Entry{}, err
	}
	for _, d := range []*drec{exact, alias, folded} {
		if d == nil {
			continue
		}
		e, err := f.dirEntry(v, view, d)
		if err == nil && shadow && string(d.name) == snapshotsDirName {
			e.Name, e.RawName = rawPrefix+base64.RawURLEncoding.EncodeToString(d.name), slices.Clone(d.name)
		}
		return e, err
	}
	return filesys.Entry{}, errNoChild
}

// hashAttr adds name_hash=bad for a record whose stored name hash is wrong.
func hashAttr(d *drec, attrs []filesys.KV) []filesys.KV {
	if d.badHash {
		attrs = append(attrs, filesys.KV{Key: "name_hash", Value: "bad"})
	}
	return attrs
}
