package ext4

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Test-only accessors, so the external ext4_test package can check internals
// without widening the public API.

// Encoding returns s_encoding and s_encoding_flags.
func (f *FS) Encoding() (encoding, flags uint16) { return f.sb.encoding, f.sb.encFlags }

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

// RawXattrs reads the extended attributes of in (the production Xattrs takes an Entry).
func (f *FS) RawXattrs(in *Inode) ([]Xattr, error) { return f.xattrs(in) }

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

// SlackWork reports the slack scan work done so far: candidate checks plus bytes
// indexed for the name check (a deterministic stand-in for time).
func (f *FS) SlackWork() int64 { return f.slackWork.Load() }

// CutIfShort runs the "block map ends before i_size" check of the directory scan on inode n, after a first cut
// of the kind given (none when firstCut is empty), and returns the cut reason the scan holds afterwards.
func (f *FS) CutIfShort(n uint32, lblk, isizeBlocks int64, firstCut string) (string, error) {
	in, err := f.inode(n)
	if err != nil {
		return "", err
	}
	w := &dirScan{f: f, in: in}
	if firstCut != "" {
		w.cutAt(false, "%s", firstCut)
	}
	w.cutIfShort(lblk, isizeBlocks)
	return w.cut.reason, nil
}

// MaxEAValue is the cap on the size of a value held in an EA inode.
const MaxEAValue = maxEAValue

// EAInodeValue opens the value of the EA-inode attribute name of inode n as a file, and says whether the value
// failed its hash.
func (f *FS) EAInodeValue(n uint32, name string) (filesys.File, bool, error) {
	in, err := f.inode(n)
	if err != nil {
		return nil, false, err
	}
	xs, err := f.xattrs(in)
	if err != nil && len(xs) == 0 {
		return nil, false, err
	}
	for _, x := range xs {
		if x.Name == name {
			fl, bad, err := f.eaInodeValue(in, x)
			if err != nil {
				return nil, bad, err
			}
			return fl, bad, nil
		}
	}
	return nil, false, fmt.Errorf("inode %d has no attribute %q", n, name)
}

// SetXattrCap lowers the per-entry cap on listed attributes (before any read).
func (f *FS) SetXattrCap(n int) { f.xattrCap = n }

// SetEAHashCap lowers the bound on EA-inode value bytes hashed (before any read).
func (f *FS) SetEAHashCap(n int64) { f.eaHashCap = n }

// MaxEAHashBytes is the default bound on EA-inode value bytes hashed per filesystem.
const MaxEAHashBytes = maxEAHashBytes

// SetLocateCaps lowers the directory and run caps of the deleted-ID directory index (before any resolution); a
// value of 0 or less leaves that cap as it is.
func (f *FS) SetLocateCaps(dirs, runs int) {
	if dirs > 0 {
		f.locateDirsCap = dirs
	}
	if runs > 0 {
		f.locateRunsCap = runs
	}
}

// LocateWork reports the work the deleted-ID resolution has done: directory index units plus one per re-read.
func (f *FS) LocateWork() int64 { return f.locateWork.Load() }

// ResolveDeleted resolves a deleted-entry ID from disk: the directory that holds the record, the inode the record
// names (0 when unknown), and the logical block of the record's block (-1 for an inline directory).
func (f *FS) ResolveDeleted(id string) (dirInode, recInode uint32, lblk int64, err error) {
	d, ok := f.parseDeletedID(id)
	if !ok {
		return 0, 0, 0, fmt.Errorf("%w: %q is not a canonical deleted-entry id", filesys.ErrNotFound, id)
	}
	r, err := f.resolveDeleted(d)
	if err != nil {
		return 0, 0, 0, err
	}
	return r.dir.num, r.rec.inode, r.lblk, nil
}

// GroupInodeBitmapCsum reads group g's inode bitmap and returns the checksum the descriptor stores, the one computed
// over the bitmap (masked to 16 bits when the descriptor has no high half), and whether the descriptor has the high
// half. It panics on a read failure.
func (f *FS) GroupInodeBitmapCsum(g int) (stored, computed uint32, hasHi bool) {
	d := &f.groups[g]
	nb := (f.sb.inodesPerGroup + 7) / 8
	bm := make([]byte, nb)
	if err := readFull(f.r, bm, int64(d.inodeBitmap)*int64(f.sb.blockSize)); err != nil {
		panic(err)
	}
	computed = rawCRC32C(f.sb.csumSeed, bm[:f.sb.inodesPerGroup/8])
	if !d.inodeBitmapCsumHi {
		computed &= 0xFFFF
	}
	return d.inodeBitmapCsum, computed, d.inodeBitmapCsumHi
}

// InodeAllocated reports the allocation state of inode n: "used", "free", or "unknown: <reason>".
func (f *FS) InodeAllocated(n uint32) (state string, err error) {
	st, reason, err := f.inodeState(n)
	if err != nil {
		return "", err
	}
	switch st {
	case allocFree:
		return "free", nil
	case allocUsed:
		return "used", nil
	}
	return "unknown: " + reason, nil
}

// StaleInode resolves the deleted-entry id and applies the stale-inode acceptance rules (3C-R7); it returns the
// accepted inode's number.
func (f *FS) StaleInode(id string) (num uint32, err error) {
	st, err := f.staleOf(id)
	if err != nil {
		return 0, err
	}
	return st.in.num, nil
}

// StaleInodeDetail is StaleInode that also returns the accepted inode's flags and the quiet-decode notes.
func (f *FS) StaleInodeDetail(id string) (num, flags uint32, notes []string, err error) {
	st, err := f.staleOf(id)
	if err != nil {
		return 0, 0, nil, err
	}
	return st.in.num, st.in.flags, st.notes, nil
}

func (f *FS) staleOf(id string) (*staleInode, error) {
	d, ok := f.parseDeletedID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a canonical deleted-entry id", filesys.ErrNotFound, id)
	}
	r, err := f.resolveDeleted(d)
	if err != nil {
		return nil, err
	}
	return f.staleInodeFor(r)
}
