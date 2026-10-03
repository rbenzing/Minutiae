package ext4

import "github.com/rbenzing/minutiae/internal/filesys"

// Test-only accessors, so the external ext4_test package can check internals
// without widening the public API.

// GroupCount returns the number of block groups read from the descriptor table.
func (f *FS) GroupCount() int { return len(f.groups) }

// Group returns the locations, flags and validity of one group descriptor.
func (f *FS) Group(i int) (blockBitmap, inodeBitmap, inodeTable uint64, flags uint16, bad bool) {
	g := f.groups[i]
	return g.blockBitmap, g.inodeBitmap, g.inodeTable, g.flags, g.bad
}

// CRC16 exposes the group-descriptor crc16.
var CRC16 = crc16

// RawCRC32C exposes the kernel-style crc32c (no final inversion).
var RawCRC32C = rawCRC32C

// Inode is the decoded inode, exposed for tests.
type Inode = inode

// Xattr is one extended attribute.
type Xattr = xattr

// InodeFields is a read-only copy of the decoded inode fields.
type InodeFields struct {
	Num        uint32
	Mode       uint16
	UID, GID   uint32
	Size       int64
	Links      uint16
	Flags      uint32
	FileACL    uint64
	CsumOK     bool
	Generation uint32
	ExtraLen   int
}

// Fields returns the decoded fields of the inode.
func (in *inode) Fields() InodeFields {
	return InodeFields{in.num, in.mode, in.uid, in.gid, in.size, in.links, in.flags, in.fileACL, in.csumOK, in.generation, len(in.extra)}
}

// Times returns the decoded timestamps.
func (in *inode) Times() filesys.Times { return in.times }

// Inode reads inode n.
func (f *FS) Inode(n uint32) (*Inode, error) { return f.inode(n) }

// Xattrs reads the extended attributes of in.
func (f *FS) Xattrs(in *Inode) ([]Xattr, error) { return f.xattrs(in) }

// ToEntry converts an inode to a directory entry.
func ToEntry(name string, raw []byte, in *Inode) filesys.Entry { return toEntry(name, raw, in) }

// ParseXattrs parses an xattr entry table (see parseXattrs).
var ParseXattrs = parseXattrs

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// DirRuns returns the byte runs of directory e's data (holes have Offset -1),
// without the trailing hole that pads the map to the 64 MiB read limit.
func (f *FS) DirRuns(e filesys.Entry) ([]filesys.Run, error) {
	in, err := f.dirInode(e)
	if err != nil {
		return nil, err
	}
	runs, _, err := f.dirRuns(in)
	if n := len(runs); n > 0 && runs[n-1].Offset < 0 {
		runs = runs[:n-1]
	}
	return runs, err
}

// SetDirRecordCap lowers the per-directory entry cap (before any read).
func (f *FS) SetDirRecordCap(n int) { f.dirRecordCap = n }

// RecLenFromDisk exposes the rec_len decoder.
var RecLenFromDisk = recLenFromDisk

// SetSlackScanCap lowers the per-directory cap on slack bytes scanned for
// deleted entries (before any read).
func (f *FS) SetSlackScanCap(n int64) { f.slackScanCap = n }
