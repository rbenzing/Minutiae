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
// comes from the record; for a file the thread record is looked up to flag a
// legacy volume that has none. Only an I/O error is returned.
func (f *FS) toEntry(k catalogKey, r catalogRecord) (filesys.Entry, error) {
	name, raw := displayName(k.name)
	e := filesys.Entry{
		Name: name, RawName: raw, ID: cnidString(r.id),
		Type: entryType(&r),
		Mode: uint32(r.bsd.mode), UID: r.bsd.owner, GID: r.bsd.group,
		Times: filesys.Times{
			Created: hfsTime(r.create), Modified: hfsTime(r.contentMod),
			Changed: hfsTime(r.attrMod), Accessed: hfsTime(r.access),
		},
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
		if r.data.logicalSize > math.MaxInt64 {
			f.warn("file %s (id %d) records a data fork size of %d bytes, beyond what can be represented", strconv.Quote(name), r.id, r.data.logicalSize)
			e.Size = math.MaxInt64
		} else {
			e.Size = int64(r.data.logicalSize)
		}
	}
	addAttr(&e, "flags", fmt.Sprintf("0x%04x", r.flags))
	if r.typ == recFile {
		if r.fileType != 0 {
			addAttr(&e, "file_type", fourCC(r.fileType))
		}
		if r.fileCreator != 0 {
			addAttr(&e, "file_creator", fourCC(r.fileCreator))
		}
		if r.rsrc.logicalSize != 0 || r.rsrc.totalBlocks != 0 {
			addAttr(&e, "rsrc_size", strconv.FormatUint(r.rsrc.logicalSize, 10))
			addAttr(&e, "rsrc_blocks", strconv.FormatUint(uint64(r.rsrc.totalBlocks), 10))
		}
	}
	if r.backup != 0 {
		if b := hfsTime(r.backup); !b.T.IsZero() {
			addAttr(&e, "backup_date", b.T.Format(time.RFC3339))
		}
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
	return e, nil
}

// Root returns the root folder (cnid:2), described by its catalog record when
// that can be read; otherwise a bare entry (ReadDir then reports the error).
func (f *FS) Root() filesys.Entry {
	f.rootMu.Lock()
	cached := f.rootEntry
	f.rootMu.Unlock()
	if cached != nil {
		return *cached
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
	f.rootMu.Lock()
	f.rootEntry = &e
	f.rootMu.Unlock()
	return e
}

// Open is not implemented yet (file data arrives with the next task).
func (f *FS) Open(_ filesys.Entry) (filesys.File, error) {
	return nil, fmt.Errorf("%w: reading file data is not implemented", filesys.ErrUnsupported)
}

// Unallocated is not implemented yet.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	return nil, fmt.Errorf("%w: unallocated space is not implemented", filesys.ErrUnsupported)
}
