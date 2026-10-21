package ext4

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// maxXattrsPerEntry bounds the attributes one entry lists: more is filesys.ErrXattrCap, never a partial list.
// It equals the APFS reader's bound.
const maxXattrsPerEntry = 4096

var _ filesys.XattrReader = (*FS)(nil)

// xattrOwner re-reads the inode an entry names. Only e.ID is used: a forged Size, Type or Attrs on e changes
// nothing, and a "dirent:" ID (a deleted entry) is filesys.ErrDeleted.
func (f *FS) xattrOwner(e filesys.Entry) (*inode, error) {
	if isDirentID(e.ID) {
		return nil, filesys.ErrDeleted
	}
	n, err := parseInodeID(e.ID)
	if err != nil {
		return nil, err
	}
	return f.rawInode(n)
}

// xattrDisplayName is the name of an attribute as listed: the stored name (prefix and name), or its
// filesys.RawNameAlias when that is not valid UTF-8 or holds a NUL. An ext4 name always has a prefix, so it is
// never empty and never starts with the alias prefix.
func xattrDisplayName(name string) string {
	if utf8.ValidString(name) && !strings.ContainsRune(name, 0) {
		return name
	}
	return filesys.RawNameAlias([]byte(name))
}

// Xattrs lists the attributes of the inode e names: the in-inode area first, then the xattr block. A value in the
// inode or the block is XattrInline; a value in an EA inode is XattrStream with the size the EA inode really has,
// and the mark XattrHashBad when its value fails the stored hash (XattrHashUnchecked when the hash work bound is
// spent). An attribute whose EA inode is damaged is listed with Size -1 and fails in OpenXattr with ErrCorrupt.
// A table that cannot be read is an error, never a partial or an empty list; more than the cap is
// filesys.ErrXattrCap.
func (f *FS) Xattrs(e filesys.Entry) ([]filesys.Xattr, error) {
	in, err := f.xattrOwner(e)
	if err != nil {
		return nil, err
	}
	xs, _, err := f.xattrsChecked(in)
	if err != nil {
		return nil, err // the attributes before the damage are dropped: a prefix is not the set
	}
	if len(xs) > f.xattrCap {
		return nil, fmt.Errorf("ext4: inode %d has %d extended attributes, more than the %d listed: %w", in.num, len(xs), f.xattrCap, filesys.ErrXattrCap)
	}
	out := make([]filesys.Xattr, 0, len(xs))
	for _, x := range xs {
		out = append(out, f.listXattr(in, x))
	}
	return out, nil
}

func (f *FS) listXattr(in *inode, x xattr) filesys.Xattr {
	l := filesys.Xattr{Name: xattrDisplayName(x.Name), Size: int64(len(x.Value)), Storage: filesys.XattrInline}
	if x.Inum == 0 {
		return l
	}
	l.Storage, l.Size = filesys.XattrStream, -1
	if x.Name == "system.data" {
		return l // inline file content in an EA inode: not decoded (OpenXattr is ErrUnsupported)
	}
	fl, bad, err := f.eaInodeValue(in, x)
	switch {
	case errors.Is(err, filesys.ErrLimit):
		l.Integrity = filesys.XattrHashUnchecked
	case err != nil:
	default:
		l.Size = fl.Size()
		l.Integrity = filesys.XattrHashOK // the value was hashed and the hash matched
		if bad {
			l.Integrity = filesys.XattrHashBad
		}
	}
	return l
}

// OpenXattr returns the value of the attribute of e called name (the display name): a value in the inode or the
// block as a File with no runs, a value in an EA inode as a File of its real image runs. An unknown name is
// filesys.ErrNotFound, a name that two attributes share is filesys.ErrAmbiguous (never the first), a damaged EA
// inode is filesys.ErrCorrupt, a system.data value held in an EA inode is filesys.ErrUnsupported, and a table
// that cannot be read is its error: a value is never offered from a damaged set.
func (f *FS) OpenXattr(e filesys.Entry, name string) (filesys.File, error) {
	in, err := f.xattrOwner(e)
	if err != nil {
		return nil, err
	}
	xs, _, err := f.xattrsChecked(in)
	if err != nil {
		return nil, err
	}
	var hit *xattr
	n := 0
	for i := range xs {
		if xattrDisplayName(xs[i].Name) == name {
			hit = &xs[i]
			n++
		}
	}
	switch {
	case n == 0:
		return nil, fmt.Errorf("%w: inode %d has no extended attribute %q", filesys.ErrNotFound, in.num, name)
	case n > 1:
		return nil, fmt.Errorf("%w: inode %d has %d extended attributes called %q", filesys.ErrAmbiguous, in.num, n, name)
	case hit.Inum == 0:
		return &file{size: int64(len(hit.Value)), mem: hit.Value}, nil
	case hit.Name == "system.data":
		return nil, fmt.Errorf("%w: ext4: inode %d: system.data is held in EA inode %d, a layout this reader does not decode", filesys.ErrUnsupported, in.num, hit.Inum)
	}
	fl, _, err := f.eaInodeValue(in, *hit)
	if err != nil {
		return nil, err
	}
	return fl, nil
}
