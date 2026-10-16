package hfsplus

import (
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const nameResourceFork = "com.apple.ResourceFork"

// valueFile is an attribute value held in memory (no image runs).
type valueFile struct{ data []byte }

func (v *valueFile) Size() int64         { return int64(len(v.data)) }
func (v *valueFile) Runs() []filesys.Run { return nil }
func (v *valueFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative offset %d", off)
	}
	if off >= int64(len(v.data)) {
		return 0, io.EOF
	}
	n := copy(p, v.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// xattrObject resolves e.ID (strictly) to its file object.
func (f *FS) xattrObject(e filesys.Entry) (*object, error) {
	if err := refuseConflictID(e.ID); err != nil {
		return nil, err
	}
	id, err := parseCNID(e.ID)
	if err != nil {
		return nil, err
	}
	return f.fileObject(id)
}

// Xattrs lists the extended attributes of a file. Only two values are
// extractable: the inline com.apple.decmpfs attribute and the resource fork,
// which Apple's userland presents as the attribute com.apple.ResourceFork. The
// attributes-file layouts for other values are builder-only (see
// docs/limitations.md), so their sizes are unknown (-1) and OpenXattr refuses
// them. More attribute names than the listing cap, or an attributes tree that
// could not be read completely, is filesys.ErrXattrCap / ErrCorrupt, never a
// shortened list. A record stored under another file id is no record of this
// file, so an empty list means "none under this id"; detectable damage is
// ErrCorrupt (TestXattrsRecordMovedToAnotherIDGivesAnEmptyList).
func (f *FS) Xattrs(e filesys.Entry) ([]filesys.Xattr, error) {
	o, err := f.xattrObject(e)
	if err != nil {
		return nil, err
	}
	if o.attrs.more > 0 {
		return nil, fmt.Errorf("hfsplus: %s (id %d) has more than %d extended attributes: %w", o.name(), o.node.id, maxAttrShown, filesys.ErrXattrCap)
	}
	if o.attrs.unread || o.attrs.bad > 0 {
		// attributesOf skips damaged records with a warning; a listing that
		// silently omitted them would call a damaged attribute absent.
		return nil, corrupt("attributes B-tree", -1, "the attributes of id %d cannot be listed completely (%d damaged records)", o.node.id, o.attrs.bad)
	}
	var out []filesys.Xattr
	for _, n := range o.attrs.names {
		x := filesys.Xattr{Name: n, Size: -1}
		if n == nameDecmpfs && o.attrs.value != nil {
			x.Size, x.Storage = int64(len(o.attrs.value)), filesys.XattrInline
		}
		out = append(out, x)
	}
	if o.node.rsrc.logicalSize > 0 {
		if o.node.rsrc.logicalSize > math.MaxInt64 {
			return nil, corrupt("catalog record", -1, "file %s (id %d) has a resource fork size of %d bytes", o.name(), o.node.id, o.node.rsrc.logicalSize)
		}
		out = append(out, filesys.Xattr{Name: nameResourceFork, Size: int64(o.node.rsrc.logicalSize), Storage: filesys.XattrFork})
	}
	return out, nil
}

// OpenXattr returns the raw value of com.apple.decmpfs or the resource fork
// (com.apple.ResourceFork), byte for byte. Any other attribute that exists is
// filesys.ErrUnsupported (its value is not extracted), one that does not is
// filesys.ErrNotFound.
func (f *FS) OpenXattr(e filesys.Entry, name string) (filesys.File, error) {
	o, err := f.xattrObject(e)
	if err != nil {
		return nil, err
	}
	switch name {
	case nameDecmpfs:
		switch {
		case !o.attrs.decmpfs:
			return nil, fmt.Errorf("hfsplus: %w: %s has no %s attribute", filesys.ErrNotFound, o.name(), name)
		case o.attrs.value == nil:
			return nil, fmt.Errorf("hfsplus: %w: %s of %s is stored as a fork-data attribute or is over %d bytes", filesys.ErrUnsupported, name, o.name(), maxAttrValue)
		}
		return &valueFile{data: slices.Clone(o.attrs.value)}, nil
	case nameResourceFork:
		if o.node.rsrc.logicalSize == 0 {
			return nil, fmt.Errorf("hfsplus: %w: %s has no resource fork", filesys.ErrNotFound, o.name())
		}
		return f.openResourceFork(o)
	}
	if slices.Contains(o.attrs.names, name) {
		return nil, fmt.Errorf("hfsplus: %w: the value of attribute %q of %s is not extracted (the attributes-file layouts are builder-only)", filesys.ErrUnsupported, name, o.name())
	}
	return nil, fmt.Errorf("hfsplus: %w: %s has no attribute %q", filesys.ErrNotFound, o.name(), name)
}
