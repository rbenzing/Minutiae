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
// after its four prefix units (from memory of TN1150; see Task 4, which
// verifies it).
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
	e := filesys.Entry{
		Name: name, RawName: raw, ID: cnidString(r.id),
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
	if r.typ == recFile {
		if _, err := f.catalogThread(r.id); err != nil {
			switch {
			case errors.Is(err, filesys.ErrNotFound):
				addAttr(&e, "thread", "missing")
			case errors.Is(err, filesys.ErrCorrupt):
				f.warn("the thread record of file %s (id %d) is unusable: %v", strconv.Quote(name), r.id, err)
				addAttr(&e, "thread", "invalid")
			default:
				return filesys.Entry{}, err
			}
		}
	}
	if e.Type == filesys.TypeSymlink && o.link.kind != linkDangling && o.link.kind != linkInvalid {
		e.LinkTarget = f.linkTarget(o, e.Size)
	}
	return e, nil
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

// Unallocated is not implemented yet.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	return nil, fmt.Errorf("%w: unallocated space is not implemented", filesys.ErrUnsupported)
}
