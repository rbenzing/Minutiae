package apfs

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	maxXattrRecords = 1 << 12 // xattr records read for one object
	xattrStreamMin  = 16      // xattr_dstream_t: u64 xattr_obj_id, then j_dstream_t's u64 size
)

// xattrRec is one decoded xattr record of an object. A record that cannot be
// decoded keeps bad: it is listed (Size -1) and fails on open. inline holds the
// value only for the record a caller asked for by name.
type xattrRec struct {
	name      []byte
	inlineLen int
	inline    []byte
	stream    bool
	oid       uint64
	size      uint64
	bad       error
}

func (r *xattrRec) listed() filesys.Xattr {
	x := filesys.Xattr{Name: displayXattr(r.name), Size: -1}
	switch {
	case r.bad != nil:
	case r.stream:
		if r.size <= 1<<63-1 {
			x.Size = int64(r.size)
		}
		x.Storage = filesys.XattrStream
	default:
		x.Size, x.Storage = int64(r.inlineLen), filesys.XattrInline
	}
	return x
}

// decodeXattrVal decodes a j_xattr_val_t: u16 flags, u16 xdata_len, data. The
// stream form's data is xattr_dstream_t: u64 xattr_obj_id, then j_dstream_t,
// whose first field is the u64 size. The inline bytes are NOT copied here.
func decodeXattrVal(val []byte) (r xattrRec, data []byte, err error) {
	if len(val) < 4 {
		return r, nil, fmt.Errorf("a value of %d bytes is shorter than its 4-byte header", len(val))
	}
	flags, n := le.Uint16(val), int(le.Uint16(val[2:]))
	if n > len(val)-4 {
		return r, nil, fmt.Errorf("data length %d does not fit the %d bytes that follow the header", n, len(val)-4)
	}
	data = val[4 : 4+n]
	isStream, isInline := flags&xattrStream != 0, flags&xattrInline != 0
	switch {
	case isStream && isInline:
		return r, nil, errors.New("both the stream and the embedded storage flags are set")
	case isStream:
		if n < xattrStreamMin {
			return r, nil, fmt.Errorf("a stream descriptor of %d bytes is shorter than %d", n, xattrStreamMin)
		}
		r.stream, r.oid, r.size = true, le.Uint64(data), le.Uint64(data[8:])
	case isInline:
		r.inlineLen = n
	default:
		return r, nil, errors.New("neither the stream nor the embedded storage flag is set")
	}
	return r, data, nil
}

func (f *FS) xattrCap() int {
	if f.xattrMax > 0 {
		return f.xattrMax
	}
	return maxXattrRecords
}

// inodeExists reports whether inode ino has an inode record in the tree (a
// cheap probe: one key range, no xattr or extent work).
func (f *FS) inodeExists(t *tree, ino uint64) (bool, error) {
	found := false
	err := f.scanObject(t, ino, fsTypeInode, fsTypeInode, func(uint8, []byte, []byte) (bool, error) {
		found = true
		return true, nil
	})
	return found, err
}

// xattrRecords reads every xattr record of inode ino as of view. match, when
// not empty, is the display name of the one record whose embedded value is
// copied out (no other value is retained). A record with an unusable name is
// skipped with a warning (it has no name to list); a record over the format's
// size limits or with an undecodable value is kept as a damaged record. More
// than the cap is filesys.ErrXattrCap (no partial list). A tree that cannot be
// read is an error, never an empty list.
func (f *FS) xattrRecords(v *volume, view, ino uint64, match string) ([]xattrRec, error) {
	t, err := f.fsTree(v, view)
	if err != nil {
		return nil, err
	}
	if ok, err := f.inodeExists(t, ino); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("apfs: volume %d inode %d: %w", v.slot, ino, filesys.ErrNotFound)
	}
	limit := f.xattrCap()
	var out []xattrRec
	over := false
	err = f.scanObjectLimits(t, ino, fsTypeXattr, fsTypeXattr, nil, false, func(_ uint8, key, val []byte) (bool, error) {
		name, ok := xattrName(key)
		if !ok || len(key) > jobjMaxKey {
			f.warn("volume %d inode %d: xattr record with an unusable name: skipped", v.slot, ino)
			return false, nil
		}
		if len(out) >= limit {
			over = true
			return true, nil
		}
		var (
			r    xattrRec
			data []byte
			derr error
		)
		if len(val) > jobjMaxVal {
			derr = fmt.Errorf("a value of %d bytes is over the format limit of %d", len(val), jobjMaxVal)
		} else {
			r, data, derr = decodeXattrVal(val)
		}
		r.name = bytes.Clone(name)
		if derr != nil {
			r.bad = corrupt("xattr", -1, "volume %d inode %d: xattr %s: %v", v.slot, ino, displayXattr(name), derr)
		} else if match != "" && !r.stream && displayXattr(name) == match {
			r.inline = slices.Clone(data)
		}
		out = append(out, r)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if over {
		return nil, fmt.Errorf("apfs: volume %d inode %d has more than %d xattr records: %w", v.slot, ino, limit, filesys.ErrXattrCap)
	}
	return out, nil
}

// Xattrs lists the extended attributes of e (Entry.ID alone is used). An
// attribute set that cannot be read is an error; an empty list means the entry
// has no attribute.
func (f *FS) Xattrs(e filesys.Entry) ([]filesys.Xattr, error) {
	v, view, ino, err := f.nodeTarget(e.ID)
	if err != nil {
		return nil, err
	}
	recs, err := f.xattrRecords(v, view, ino, "")
	if err != nil {
		return nil, err
	}
	out := make([]filesys.Xattr, len(recs))
	for i := range recs {
		out[i] = recs[i].listed()
	}
	return out, nil
}

// OpenXattr returns the value of the named attribute of e. Embedded values
// come from the record; stream values are read through their own extents (real
// image runs) and are all-or-nothing.
func (f *FS) OpenXattr(e filesys.Entry, name string) (filesys.File, error) {
	v, view, ino, err := f.nodeTarget(e.ID)
	if err != nil {
		return nil, err
	}
	recs, err := f.xattrRecords(v, view, ino, name)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		r := &recs[i]
		if displayXattr(r.name) != name {
			continue
		}
		switch {
		case r.bad != nil:
			return nil, r.bad
		case r.stream:
			sf, err := f.streamFile(v, view, r.oid, ino, r.size, "xattr "+strconv.Quote(name))
			if err != nil {
				return nil, err
			}
			return sf, nil
		}
		n := int64(r.inlineLen)
		return &file{size: n, avail: n, mem: r.inline, inline: true}, nil
	}
	return nil, notFound("inode %d of volume %d has no extended attribute %q", ino, v.slot, name)
}

// streamFile maps a stream value (xattr or symlink target) whose extents are
// keyed by oid and which belongs to inode owner; messages name the owner, never
// only the stream's own object id. The whole value must be mapped and
// hole-free; a size over the container, a damaged map or a hole is a
// CorruptError, never a prefix or zeros.
func (f *FS) streamFile(v *volume, view, oid, owner, size uint64, what string) (*file, error) {
	if size > uint64(f.size) {
		return nil, corrupt(what, -1, "volume %d inode %d: a stream of %d bytes cannot fit the %d-byte container", v.slot, owner, size, f.size)
	}
	if size == 0 {
		return &file{r: f.data}, nil
	}
	if oid == 0 || oid > maxIno {
		return nil, corrupt(what, -1, "volume %d inode %d: the stream object id %d is not valid", v.slot, owner, oid)
	}
	n := int64(size)
	m, err := f.extentMap(v, view, oid, owner, n)
	if err != nil {
		return nil, err
	}
	if m.tail != nil {
		return nil, m.tail
	}
	for _, r := range m.runs {
		if r.Offset < 0 {
			return nil, corrupt(what, -1, "volume %d inode %d: the stream has a hole in its %d bytes", v.slot, owner, n)
		}
	}
	if m.cryptoSeen {
		f.warn("volume %d inode %d: %s has extents with a key of their own: the bytes are returned as stored (ciphertext)", v.slot, owner, what)
	}
	return f.newFile(m, n, m.cryptoSeen), nil
}
