package filesys

import (
	"encoding/base64"
	"fmt"
)

// RawNamePrefix starts the display form of a name that cannot be shown as it is.
const RawNamePrefix = "~raw~"

// RawNameAlias is the one display form of a stored name that is not usable as
// text: RawNamePrefix + base64.RawURLEncoding(raw), the URL-safe alphabet,
// unpadded. Entry names and extended-attribute names of every reader use it.
func RawNameAlias(raw []byte) string {
	return RawNamePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Xattr describes one extended attribute of an entry, as listed by an
// XattrReader. The value is read with OpenXattr.
//
// Name is the display name; a name that is not valid UTF-8 or holds a NUL is
// its RawNameAlias (for example the byte 0xff is "~raw~_w" and the bytes
// fb ff fe are "~raw~-__-"). Size is the value length in bytes, -1 when the
// reader cannot say without reading the value. Storage is XattrInline,
// XattrStream, XattrFork or "" when unknown.
type Xattr struct {
	Name    string
	Size    int64
	Storage string
}

// Storage kinds of an Xattr.
const (
	XattrInline = "inline" // the value is part of the metadata record
	XattrStream = "stream" // the value has extents of its own: File.Runs are real image runs
	XattrFork   = "fork"   // a resource fork presented as an attribute
)

// ErrXattrCap is returned instead of a partial list when an object has more
// extended attributes than the reader will list (a bound, never a silent cut).
var ErrXattrCap = fmt.Errorf("%w: more extended attributes than the reader will list", ErrUnsupported)

// XattrReader is implemented by a filesystem that can return extended-attribute
// values (APFS xattrs; HFS+ the decmpfs attribute and the resource fork).
// Xattrs lists the attributes of e (Entry.ID alone identifies it: no other
// field of e is used); an attribute whose record is damaged is listed with
// Size -1 and fails in OpenXattr with ErrCorrupt. OpenXattr returns the value
// as a File: a value stored in the metadata has no Runs; a value with extents
// of its own reports its real image Runs (the File.Runs contract applies). An
// unknown name is ErrNotFound; a value the reader does not extract is
// ErrUnsupported; the value is never offered as a prefix of a damaged one.
// Xattrs returns an error, never an empty list, when the attribute set cannot
// be read; an empty list means the entry has none.
// Discover it with As[XattrReader](fsys).
type XattrReader interface {
	Xattrs(e Entry) ([]Xattr, error)
	OpenXattr(e Entry, name string) (File, error)
}
