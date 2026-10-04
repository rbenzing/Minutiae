package apfs

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"time"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// File-system record layout (Apple File System Reference; the Format
// reference of the project plan).
const (
	jobjIDMask = 0x0fffffffffffffff // j_key_t: object id in the low 60 bits
	jobjMaxKey = 832                // JOBJ_MAX_KEY_SIZE
	jobjMaxVal = 3808               // JOBJ_MAX_VALUE_SIZE

	fsTypeInode      = 3
	fsTypeXattr      = 4
	fsTypeSiblingMap = 12
	fsTypeDrec       = 9

	inodeFixedSize = 92 // j_inode_val_t before the extended fields

	// internal_flags
	inodeHasUncompressedSize = 0x40000

	// bsd_flags UF_COMPRESSED. Not in the Apple reference: from the BSD
	// headers.
	bsdCompressed = 0x20

	// Mode type bits.
	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeReg      = 0o100000
	modeSymlink  = 0o120000

	// xattr flags and the symlink xattr.
	xattrStream  = 0x1
	xattrInline  = 0x2
	symlinkXattr = "com.apple.fs.symlink"

	// maxXattrNames bounds the xattr names listed in an entry's attributes.
	maxXattrNames = 32

	// maxNameLen is the longest name (255 bytes; name_len counts the NUL).
	maxNameLen = 255

	drecTypeMask = 0x000f
)

// inode is a decoded j_inode_val_t with the facts the reader needs from the
// extended fields and the xattr records of the same object.
type inode struct {
	ino                         uint64
	parentID, privateID         uint64
	create, mod, change, access uint64
	internalFlags               uint64
	links                       int32 // nchildren of a directory, nlink otherwise
	protClass                   uint32
	bsdFlags                    uint32
	uid, gid                    uint32
	mode                        uint16
	uncompressedSize            uint64

	hasDstream bool
	size       int64 // dstream size, clamped to MaxInt64
	cryptoID   uint64

	xattrNames []string // at most maxXattrNames
	xattrCount int      // all xattr records seen
	symlink    []byte   // the embedded symlink target, trailing NUL removed
	symlinkSet bool     // the object has a symlink xattr
	symlinkOK  bool     // ... and its target is embedded (readable)
}

func (in *inode) isDir() bool { return in.mode&modeTypeMask == modeDir }

func (in *inode) compressed() bool { return in.bsdFlags&bsdCompressed != 0 }

// entryType maps the mode's type bits.
func entryType(mode uint16) filesys.EntryType {
	switch mode & modeTypeMask {
	case modeDir:
		return filesys.TypeDir
	case modeReg:
		return filesys.TypeFile
	case modeSymlink:
		return filesys.TypeSymlink
	}
	return filesys.TypeOther
}

// direntType maps a directory record's DT_* type.
func direntType(flags uint16) filesys.EntryType {
	switch flags & drecTypeMask {
	case 4:
		return filesys.TypeDir
	case 8:
		return filesys.TypeFile
	case 10:
		return filesys.TypeSymlink
	}
	return filesys.TypeOther
}

// timestamp converts nanoseconds since 1970 UTC; a value that does not fit an
// int64 is not representable.
func timestamp(ns uint64) (filesys.Timestamp, bool) {
	if ns > math.MaxInt64 {
		return filesys.Timestamp{}, false
	}
	return filesys.Timestamp{T: time.Unix(0, int64(ns)).UTC(), ZoneKnown: true}, true
}

// scanObject visits, in key order, the records of object id whose type is in
// [lo, hi]. Records over the format's size limits are skipped with a warning.
func (f *FS) scanObject(t *tree, id uint64, lo, hi uint8, visit func(typ uint8, key, val []byte) (stop bool, err error)) error {
	f.scans.Add(1)
	prefix := func(key []byte) int {
		k := le.Uint64(key)
		kid, kt := k&jobjIDMask, uint8(k>>60)
		switch {
		case kid < id:
			return -1
		case kid > id:
			return 1
		case kt < lo:
			return -1
		case kt > hi:
			return 1
		}
		return 0
	}
	return t.scan(prefix, func(key, val []byte) (bool, error) {
		if len(key) > jobjMaxKey || len(val) > jobjMaxVal {
			f.warn("file-system record of object %d has a %d-byte key and a %d-byte value, over the format limits: skipped", id, len(key), len(val))
			return false, nil
		}
		return visit(uint8(le.Uint64(key)>>60), key, val)
	})
}

// inode reads the inode ino of volume v as of view (0: the live tree), with the
// names of its xattrs and its symlink target. A missing inode is
// filesys.ErrNotFound; a malformed one a *filesys.CorruptError.
func (f *FS) inode(v *volume, view uint64, ino uint64) (*inode, error) {
	if ino == 0 || ino > maxIno {
		return nil, fmt.Errorf("apfs: inode %d: %w", ino, filesys.ErrNotFound)
	}
	t, err := f.fsTree(v, view)
	if err != nil {
		return nil, err
	}
	var (
		in      *inode
		xnames  []string
		xcount  int
		symlink []byte
		symSet  bool
		symOK   bool
	)
	err = f.scanObject(t, ino, fsTypeInode, fsTypeXattr, func(typ uint8, key, val []byte) (bool, error) {
		switch typ {
		case fsTypeInode:
			if len(key) != 8 {
				f.warn("volume %d inode %d: record key of %d bytes is not an inode key: skipped", v.slot, ino, len(key))
				return false, nil
			}
			if in != nil {
				f.warn("volume %d inode %d: more than one inode record; the first is used", v.slot, ino)
				return false, nil
			}
			d, err := f.decodeInode(v, ino, val)
			if err != nil {
				return false, err
			}
			in = d
		case fsTypeXattr:
			name, ok := xattrName(key)
			if !ok {
				f.warn("volume %d inode %d: xattr record with an unusable name: skipped", v.slot, ino)
				return false, nil
			}
			xcount++
			if len(xnames) < maxXattrNames {
				xnames = append(xnames, displayXattr(name))
			}
			if string(name) == symlinkXattr {
				symSet = true
				if len(val) >= 4 && le.Uint16(val)&xattrInline != 0 {
					n := int(le.Uint16(val[2:]))
					if n <= len(val)-4 {
						symlink = bytes.TrimRight(bytes.Clone(val[4:4+n]), "\x00")
						symOK = true
					}
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("apfs: volume %d inode %d: %w", v.slot, ino, filesys.ErrNotFound)
	}
	in.xattrNames, in.xattrCount = xnames, xcount
	in.symlink, in.symlinkSet, in.symlinkOK = symlink, symSet, symOK
	return in, nil
}

// decodeInode decodes a j_inode_val_t.
func (f *FS) decodeInode(v *volume, ino uint64, val []byte) (*inode, error) {
	if len(val) < inodeFixedSize {
		return nil, corrupt("inode", -1, "volume %d inode %d: value of %d bytes is shorter than the %d fixed bytes", v.slot, ino, len(val), inodeFixedSize)
	}
	in := &inode{
		ino:              ino,
		parentID:         le.Uint64(val[0:]),
		privateID:        le.Uint64(val[8:]),
		create:           le.Uint64(val[16:]),
		mod:              le.Uint64(val[24:]),
		change:           le.Uint64(val[32:]),
		access:           le.Uint64(val[40:]),
		internalFlags:    le.Uint64(val[48:]),
		links:            int32(le.Uint32(val[56:])),
		protClass:        le.Uint32(val[60:]),
		bsdFlags:         le.Uint32(val[68:]),
		uid:              le.Uint32(val[72:]),
		gid:              le.Uint32(val[76:]),
		mode:             le.Uint16(val[80:]),
		uncompressedSize: le.Uint64(val[84:]),
	}
	warn := func(format string, a ...any) {
		f.warn("volume %d inode %d: "+format, append([]any{v.slot, ino}, a...)...)
	}
	for _, x := range parseXfields(val[inodeFixedSize:], warn) {
		if x.typ != xfInodeDstream {
			continue
		}
		if len(x.data) < dstreamMinSize {
			warn("dstream field of %d bytes is too short: ignored", len(x.data))
			continue
		}
		size := le.Uint64(x.data)
		if size > math.MaxInt64 {
			warn("dstream size %d is out of range: clamped", size)
			size = math.MaxInt64
		}
		in.hasDstream, in.size = true, int64(size)
		if len(x.data) >= dstreamCrypto+8 {
			in.cryptoID = le.Uint64(x.data[dstreamCrypto:])
		}
	}
	return in, nil
}

// xattrName returns the name of an xattr key (header, u16 name_len with the
// NUL, name) without the NUL; ok is false when the key is inconsistent.
func xattrName(key []byte) (name []byte, ok bool) {
	if len(key) < 8+2+1 {
		return nil, false
	}
	n := int(le.Uint16(key[8:]))
	if n < 1 || 10+n != len(key) || key[len(key)-1] != 0 {
		return nil, false
	}
	return key[10 : len(key)-1], true
}

// displayXattr shows an xattr name; one that is not valid UTF-8 or holds a NUL
// is shown in the ~raw~ form.
func displayXattr(name []byte) string {
	if utf8.Valid(name) && bytes.IndexByte(name, 0) < 0 {
		return string(name)
	}
	d, _ := displayName(name)
	return d
}

// drec is a decoded directory record.
type drec struct {
	name    []byte // without the NUL, aliases the node
	fileID  uint64
	flags   uint16
	badHash bool // hashed key whose stored hash does not match the (ASCII) name
}

// parseDrecKey returns the name of a directory-record key. Either key form is
// accepted when it is self-consistent: the hashed form (u32 name_len_and_hash
// after the header: low 10 bits the length including the NUL, the rest the
// hash) or the plain form (u16 name_len). The hash is returned for the caller
// to verify (nameHash). why is
// set when the key fits neither form.
func parseDrecKey(key []byte) (name []byte, hash uint32, hashed bool, why string) {
	end := len(key) - 1
	if len(key) >= 8+4+1 {
		nl := int(le.Uint32(key[8:]) & 0x3ff)
		if nl >= 1 && 12+nl == len(key) && key[end] == 0 {
			return key[12:end], le.Uint32(key[8:]) >> 10, true, ""
		}
	}
	if len(key) >= 8+2+1 {
		nl := int(le.Uint16(key[8:]))
		if nl >= 1 && 10+nl == len(key) && key[end] == 0 {
			return key[10:end], 0, false, ""
		}
	}
	return nil, 0, false, "the name length does not fit the key or the name is not NUL-terminated"
}

// scanDir visits the directory records of directory dir of volume v as of
// view, in key order. Records that cannot be used are skipped with a warning.
// The scan is charged to the FS-wide directory budget: when the budget is gone
// before the first record the error is a *filesys.CorruptError, mid-directory
// the scan ends with a warning; a directory of more than maxDirEntries valid
// records is cut with a warning.
func (f *FS) scanDir(v *volume, view uint64, dir uint64, fn func(d *drec) (stop bool)) error {
	t, err := f.fsTree(v, view)
	if err != nil {
		return err
	}
	var seen, valid int
	return f.scanObject(t, dir, fsTypeDrec, fsTypeDrec, func(_ uint8, key, val []byte) (bool, error) {
		seen++
		if f.dirBudget.Add(-int64(len(key)+len(val))) < 0 {
			if seen == 1 {
				return false, corrupt("directory", -1, "volume %d directory %d: the directory read budget of this filesystem is exhausted", v.slot, dir)
			}
			f.warn("volume %d directory %d: the directory read budget is exhausted; the listing is partial", v.slot, dir)
			return true, nil
		}
		name, hash, hashed, why := parseDrecKey(key)
		switch {
		case why != "":
			f.warn("volume %d directory %d: directory record skipped: %s", v.slot, dir, why)
			return false, nil
		case len(name) == 0:
			f.warn("volume %d directory %d: directory record with an empty name skipped", v.slot, dir)
			return false, nil
		case len(name) > maxNameLen:
			f.warn("volume %d directory %d: directory record with a %d-byte name skipped (limit %d)", v.slot, dir, len(name), maxNameLen)
			return false, nil
		case bytes.IndexByte(name, 0) >= 0:
			f.warn("volume %d directory %d: directory record with a NUL inside its name %q skipped", v.slot, dir, name)
			return false, nil
		case len(val) < 18:
			f.warn("volume %d directory %d: directory record %q has a %d-byte value: skipped", v.slot, dir, name, len(val))
			return false, nil
		}
		d := &drec{name: name, fileID: le.Uint64(val), flags: le.Uint16(val[16:])}
		if hashed {
			if want, ok := nameHash(name, v.caseInsen); ok && want != hash {
				d.badHash = true
				f.warn("volume %d directory %d: directory record %q stores the name hash %d, the name hashes to %d: the entry is listed as found", v.slot, dir, name, hash, want)
			}
		}
		if d.fileID == 0 || d.fileID > maxIno {
			f.warn("volume %d directory %d: directory record %q names object %d, which is out of range: skipped", v.slot, dir, name, d.fileID)
			return false, nil
		}
		if valid >= f.maxDirEntries {
			f.warn("volume %d directory %d has more than %d entries; the rest are not listed", v.slot, dir, f.maxDirEntries)
			return true, nil
		}
		valid++
		return fn(d), nil
	})
}

// siblingTarget resolves a sibling id through its SIBLING_MAP record to the
// inode number of the file it names.
func (f *FS) siblingTarget(v *volume, view, sib uint64) (uint64, bool, error) {
	t, err := f.fsTree(v, view)
	if err != nil {
		return 0, false, err
	}
	var target uint64
	err = f.scanObject(t, sib, fsTypeSiblingMap, fsTypeSiblingMap, func(_ uint8, key, val []byte) (bool, error) {
		if len(key) != 8 || len(val) < 8 {
			return false, nil
		}
		target = le.Uint64(val)
		return true, nil
	})
	if err != nil || target == 0 || target > maxIno {
		return 0, false, err
	}
	return target, true, nil
}

// isNotFoundOrCorrupt reports the errors a damaged or absent record gives, as
// opposed to I/O failures.
func isNotFoundOrCorrupt(err error) bool {
	return errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrCorrupt)
}

// castagnoli is the CRC-32C table.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// nameHash is the 22-bit hash a hashed directory-record key stores for name
// (Format reference: the name in NFD, as UTF-32 little-endian code points
// without the NUL, CRC-32C with initial value 0xFFFFFFFF and no final
// complement, low 22 bits; a case-insensitive volume hashes the case-folded
// name). Only pure-ASCII names are supported: NFD is the identity there and
// the fold is lower-casing. ok is false for any other name (it is not
// verified: that needs Unicode normalization and folding tables).
func nameHash(name []byte, foldCase bool) (hash uint32, ok bool) {
	u := make([]byte, 0, 4*len(name))
	for _, c := range name {
		if c >= 0x80 {
			return 0, false
		}
		if foldCase && c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		u = append(u, c, 0, 0, 0)
	}
	// hash/crc32 complements the result; the stored value does not.
	return ^crc32.Checksum(u, castagnoli) & 0x3fffff, true
}
