package hfsplus

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	rawPrefix = "~raw~" // display form of a name that is not plain text: prefix + base64url(raw UTF-16BE)

	// BSD file mode type bits (S_IFMT and values).
	sIFMT  = 0o170000
	sIFREG = 0o100000
	sIFLNK = 0o120000

	// Finder file type of a classic Mac OS symbolic link, used when the BSD
	// info carries no mode.
	ftypeSymlink = 0x736C6E6B // 'slnk'
)

// privateDataName is the name of the hard-link metadata folder in the root,
// after its four prefix units (from memory of TN1150; exercised by builder
// volumes and the populated real fixture).
var privateDataName = unitsOf("HFS+ Private Data")

func unitsOf(s string) []uint16 {
	u := make([]uint16, 0, len(s))
	for _, r := range s {
		u = append(u, uint16(r)) // ASCII only
	}
	return u
}

// isPrivateMetadataName reports whether name is the hard-link metadata folder's:
// four identical NUL, U+2400 or U+200B units followed by "HFS+ Private Data".
func isPrivateMetadataName(name []uint16) bool {
	if len(name) != 4+len(privateDataName) {
		return false
	}
	switch name[0] {
	case 0, 0x2400, 0x200B:
	default:
		return false
	}
	return name[1] == name[0] && name[2] == name[0] && name[3] == name[0] && slices.Equal(name[4:], privateDataName)
}

// parseCNID parses an Entry.ID, which must be exactly "cnid:" and the canonical
// decimal form of a catalog node id that can name a catalog object: the root
// folder (2) or a user id (16 and up, at most 2^32-1). Anything else wraps
// filesys.ErrNotFound, without any read.
func parseCNID(id string) (uint32, error) {
	bad := func() (uint32, error) {
		return 0, fmt.Errorf("%w: %s is not an entry id of this volume", filesys.ErrNotFound, strconv.Quote(id[:min(len(id), 48)]))
	}
	digits, ok := strings.CutPrefix(id, "cnid:")
	if !ok || digits == "" || len(digits) > 10 || (digits[0] == '0' && len(digits) > 1) {
		return bad()
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return bad()
		}
	}
	n, err := strconv.ParseUint(digits, 10, 32)
	if err != nil || (n != rootFolderID && n < firstUserCNID) {
		return bad()
	}
	return uint32(n), nil
}

func cnidString(n uint32) string { return "cnid:" + strconv.FormatUint(uint64(n), 10) }

// conflictPrefix starts the ID of an entry whose catalog node id is claimed by
// another record as well (see conflictID).
const conflictPrefix = "conflict:"

// conflictID is the ID of a catalog record that shares its catalog node id with
// the record the id's thread names: "conflict:<id>:<parent>:<base64url of the
// raw name units>". It names the record by its key, so it can never be
// mistaken for the other record, and it cannot be opened or listed: no read
// could tell which of the two the caller meant by the plain id.
func conflictID(id uint32, k catalogKey) string {
	raw := make([]byte, 2*len(k.name))
	for i, u := range k.name {
		raw[2*i], raw[2*i+1] = byte(u>>8), byte(u)
	}
	return conflictPrefix + strconv.FormatUint(uint64(id), 10) + ":" + strconv.FormatUint(uint64(k.parent), 10) + ":" + base64.RawURLEncoding.EncodeToString(raw)
}

// canonicalUint32 parses a canonical decimal (no sign, space or leading zero).
func canonicalUint32(s string) (uint32, bool) {
	if s == "" || len(s) > 10 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

// refuseConflictID returns nil for an ID that is not a conflict ID. A
// well-formed conflict ID is a CorruptError (no read is made); a malformed one
// wraps filesys.ErrNotFound.
func refuseConflictID(id string) error {
	rest, ok := strings.CutPrefix(id, conflictPrefix)
	if !ok {
		return nil
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) == 3 {
		n, ok1 := canonicalUint32(parts[0])
		p, ok2 := canonicalUint32(parts[1])
		b, derr := base64.RawURLEncoding.Strict().DecodeString(parts[2])
		if ok1 && ok2 && derr == nil && len(b)%2 == 0 && len(b)/2 <= maxNameUnits && base64.RawURLEncoding.EncodeToString(b) == parts[2] && (n == rootFolderID || n >= firstUserCNID) {
			return corrupt("catalog", -1, "the record (%d, %q) shares catalog node id %d with another record, so it cannot be opened or listed by id", p, decodeLossy(readUnits(b, len(b)/2)), n)
		}
	}
	return fmt.Errorf("%w: %s is not an entry id of this volume", filesys.ErrNotFound, strconv.Quote(id[:min(len(id), 48)]))
}

// displayName returns the Entry.Name for an on-disk name and the RawName to
// keep. A name is shown as it is (decoded UTF-16, no normalization) unless it
// is not valid UTF-16, is empty, is "." or "..", or contains '/' or NUL; those
// are shown as "~raw~" + base64url(the UTF-16BE bytes), losslessly, and the
// bytes are the RawName. ':' is shown as stored.
func displayName(units []uint16) (string, []byte) {
	s, ok := decodeUnits(units)
	if ok && len(units) > 0 && s != "." && s != ".." && !strings.ContainsAny(s, "\x00/") {
		return s, nil
	}
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		raw[2*i], raw[2*i+1] = byte(u>>8), byte(u)
	}
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), raw
}

// fourCC formats a Finder type or creator code.
func fourCC(v uint32) string {
	return strconv.Quote(string([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}))
}

func addAttr(e *filesys.Entry, key, value string) {
	e.Attrs = append(e.Attrs, filesys.KV{Key: key, Value: value})
}

func entryType(r *catalogRecord) filesys.EntryType {
	if r.typ == recFolder {
		return filesys.TypeDir
	}
	switch r.bsd.mode & sIFMT {
	case sIFREG:
		return filesys.TypeFile
	case sIFLNK:
		return filesys.TypeSymlink
	case 0:
		if r.fileType == ftypeSymlink {
			return filesys.TypeSymlink
		}
		return filesys.TypeFile
	}
	return filesys.TypeOther
}

// toEntry describes a folder or file record found under key k. Everything
// comes from the records (for a resolved hard link the iNode supplies all but
// the name and the ID), the attributes tree and, for a file, the thread record,
// looked up to flag a legacy volume that has none. Only an I/O error is returned.
func (f *FS) toEntry(k catalogKey, r catalogRecord) (filesys.Entry, error) {
	o, err := f.newObject(k, r)
	if err != nil {
		return filesys.Entry{}, err
	}
	n := &o.node
	name, raw := displayName(k.name)
	th, thErr := f.catalogThread(r.id)
	if thErr != nil && !errors.Is(thErr, filesys.ErrNotFound) && !errors.Is(thErr, filesys.ErrCorrupt) {
		return filesys.Entry{}, thErr
	}
	id := cnidString(r.id)
	conflict := thErr == nil && !f.threadNames(th, k, r)
	if conflict {
		id = conflictID(r.id, k)
		f.warn("catalog record %s under folder %d claims catalog node id %d, which the thread record assigns to the record (%d, %q): the id is shared, so this entry cannot be opened or listed by id", strconv.Quote(name), k.parent, r.id, th.parent, decodeLossy(th.name))
	}
	e := filesys.Entry{
		Name: name, RawName: raw, ID: id,
		Type: entryType(n),
		Mode: uint32(n.bsd.mode), UID: n.bsd.owner, GID: n.bsd.group,
		Times: filesys.Times{
			Created: hfsTime(n.create), Modified: hfsTime(n.contentMod),
			Changed: hfsTime(n.attrMod), Accessed: hfsTime(n.access),
		},
		Encrypted: o.attrs.cprotect,
	}
	if o.link.kind == linkDir {
		e.Type = filesys.TypeOther
	}
	if r.id != rootFolderID && r.id < firstUserCNID {
		f.warn("catalog record %s under folder %d has the reserved id %d and cannot be opened by id", strconv.Quote(name), k.parent, r.id)
	}
	if r.typ == recFolder {
		addAttr(&e, "valence", strconv.FormatUint(uint64(r.valence), 10))
		if k.parent == rootFolderID && isPrivateMetadataName(k.name) {
			addAttr(&e, "private_metadata", "true")
		}
	} else {
		size := n.data.logicalSize
		if o.compressed && o.cmp.ok {
			size = o.cmp.size // the uncompressed size: the data fork of a compressed file is empty
		}
		if size > math.MaxInt64 {
			f.warn("file %s (id %d) records a data fork size of %d bytes, beyond what can be represented", strconv.Quote(name), r.id, size)
			e.Size = math.MaxInt64
		} else {
			e.Size = int64(size)
		}
	}
	if conflict {
		addAttr(&e, "cnid_conflict", "true")
	}
	addAttr(&e, "flags", fmt.Sprintf("0x%04x", n.flags))
	if r.typ == recFile {
		if n.fileType != 0 {
			addAttr(&e, "file_type", fourCC(n.fileType))
		}
		if n.fileCreator != 0 {
			addAttr(&e, "file_creator", fourCC(n.fileCreator))
		}
		if n.rsrc.logicalSize != 0 || n.rsrc.totalBlocks != 0 {
			addAttr(&e, "rsrc_size", strconv.FormatUint(n.rsrc.logicalSize, 10))
			addAttr(&e, "rsrc_blocks", strconv.FormatUint(uint64(n.rsrc.totalBlocks), 10))
		}
	}
	if r.backup != 0 {
		if b := hfsTime(n.backup); !b.T.IsZero() {
			addAttr(&e, "backup_date", b.T.Format(time.RFC3339))
		}
	}
	switch o.link.kind {
	case linkFile:
		addAttr(&e, "hardlink", "file")
		addAttr(&e, "hardlink_inode", strconv.FormatUint(uint64(o.link.inodeNum), 10))
		addAttr(&e, "links", strconv.FormatUint(uint64(o.link.inode.bsd.special), 10))
	case linkDir:
		addAttr(&e, "hardlink", "dir")
		addAttr(&e, "hardlink_inode", strconv.FormatUint(uint64(o.link.inodeNum), 10))
	case linkDangling:
		addAttr(&e, "hardlink", "dangling")
		addAttr(&e, "hardlink_inode", strconv.FormatUint(uint64(o.link.inodeNum), 10))
	case linkInvalid:
		addAttr(&e, "hardlink", "invalid")
		addAttr(&e, "hardlink_inode", strconv.FormatUint(uint64(o.link.inodeNum), 10))
	}
	if o.decmpfsIgnored {
		addAttr(&e, "decmpfs_ignored", "true")
	}
	if o.compressed {
		addAttr(&e, "compressed", o.cmp.label())
		if o.cmp.ok {
			addAttr(&e, "decmpfs_type", strconv.FormatUint(uint64(o.cmp.typ), 10))
			addAttr(&e, "uncompressed_size", strconv.FormatUint(o.cmp.size, 10))
		}
	}
	for _, x := range o.attrs.names {
		addAttr(&e, "xattr", x)
	}
	if o.attrs.more > 0 {
		addAttr(&e, "xattr_more", strconv.Itoa(o.attrs.more))
	}
	if o.attrs.unread {
		addAttr(&e, "attributes", "unreadable")
	}
	if o.attrs.cprotect {
		addAttr(&e, "cprotect", "present")
	}
	if r.typ == recFile && thErr != nil {
		if errors.Is(thErr, filesys.ErrNotFound) {
			addAttr(&e, "thread", "missing")
		} else {
			f.warn("the thread record of file %s (id %d) is unusable: %v", strconv.Quote(name), r.id, thErr)
			addAttr(&e, "thread", "invalid")
		}
	}
	if e.Type == filesys.TypeSymlink && o.link.kind != linkDangling && o.link.kind != linkInvalid {
		e.LinkTarget = f.linkTarget(o, e.Size)
	}
	return e, nil
}

// threadNames reports whether the thread record th of r.id points back at the
// key k the record r was found under (same parent, same name in the catalog's
// own order, same kind). The root and the reserved ids are never in conflict:
// they cannot be opened by id anyway.
func (f *FS) threadNames(th catalogRecord, k catalogKey, r catalogRecord) bool {
	if r.id != rootFolderID && r.id < firstUserCNID {
		return true
	}
	if (r.typ == recFolder) != (th.typ == recFolderThread) {
		return false
	}
	return th.parent == k.parent && (slices.Equal(th.name, k.name) || compareNames(th.name, k.name, f.binaryNames()) == 0)
}

// Root returns the root folder (cnid:2), described by its catalog record when
// that can be read; otherwise a bare entry (ReadDir then reports the error).
func (f *FS) Root() filesys.Entry {
	f.rootMu.Lock()
	cached := f.rootEntry
	f.rootMu.Unlock()
	if cached != nil {
		c := *cached
		c.Attrs = slices.Clone(c.Attrs) // the cached entry must not be reachable through the copy
		return c
	}
	bare := filesys.Entry{ID: cnidString(rootFolderID), Type: filesys.TypeDir}
	k, r, err := f.folderRecord(rootFolderID)
	if err != nil {
		return bare
	}
	e, err := f.toEntry(k, r)
	if err != nil {
		return bare
	}
	e.Name, e.RawName = "", nil // the root has no name of its own (the volume name is Info.Label)
	cp := e
	cp.Attrs = slices.Clone(e.Attrs)
	f.rootMu.Lock()
	f.rootEntry = &cp
	f.rootMu.Unlock()
	return e
}
