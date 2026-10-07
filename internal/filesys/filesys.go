// Package filesys defines the read-only filesystem interface shared by every
// filesystem parser (ext4, FAT, exFAT, F2FS, APFS, HFS+), plus the common
// error types, checked integer math, a block cache and a tree walker. It is a
// pure parser layer: it operates on io.ReaderAt, never writes, and imports no
// other Minutiae package.
package filesys

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// FileSystem is a read-only view of one filesystem. All offsets are relative
// to the start of the filesystem (not the image).
type FileSystem interface {
	Info() Info
	Root() Entry
	// ReadDir lists live and deleted entries of dir; "." and ".." are omitted.
	ReadDir(dir Entry) ([]Entry, error)
	// Lookup resolves an absolute slash path ("/a/b"). Only live entries are
	// considered; a missing entry yields ErrNotFound.
	Lookup(path string) (Entry, error)
	// Open opens a regular file or symlink. A deleted entry yields ErrDeleted.
	Open(e Entry) (File, error)
	// Unallocated returns filesystem-relative byte runs, sorted and merged.
	Unallocated() ([]Run, error)
}

// File is the content of one entry.
type File interface {
	io.ReaderAt
	Size() int64
	// Runs lists the filesystem-relative byte runs of the content in file
	// order. Offset -1 marks a sparse hole (Length bytes of zeros). The runs
	// cover exactly [0, Size()): the sum of their lengths, holes included,
	// equals Size(), and the last run is trimmed to the end of the content
	// rather than to the end of its block. Content stored inline in metadata
	// (for example a fast symlink target) has no on-disk run and returns
	// none. CheckRuns verifies this contract.
	//
	// Exception: when the file's allocation is truncated or corrupt (a FAT
	// chain that ends or breaks early, say) Runs may cover only a strict
	// PREFIX of [0, Size()). Every read at or beyond the end of that prefix
	// must then return an error wrapping ErrCorrupt, so a consumer can tell
	// the covered bytes from the missing ones. CheckRunsPrefix validates such
	// a list.
	Runs() []Run
}

// EncryptedFile is implemented by a File that knows its content is stored
// encrypted (for example a file with its own key, which only its extents
// reveal) and is returned as stored. Entry.Encrypted is what a listing can say
// cheaply; this reports what opening the file found.
type EncryptedFile interface {
	File
	Encrypted() bool
}

// FileEncrypted reports whether f reports itself encrypted (see EncryptedFile).
func FileEncrypted(f File) bool {
	e, ok := f.(EncryptedFile)
	return ok && e.Encrypted()
}

// Run is a byte range. In File.Runs an Offset of -1 is a sparse hole.
type Run struct{ Offset, Length int64 }

// Info describes a filesystem.
type Info struct {
	Type        string // "ext4", "fat32", "exfat", "f2fs", "apfs", "hfsplus", "mtfs" ...
	Label, UUID string
	BlockSize   int
	Size        int64    // filesystem size in bytes
	Features    []string // human-readable feature flags
	Encrypted   bool     // any encryption detected
	Volumes     []string // apfs: volume names; others empty
	Warnings    []string // checksum mismatches, truncation, unsupported features seen
}

// EntryType is the kind of a directory entry.
type EntryType int

// Entry kinds.
const (
	TypeOther EntryType = iota
	TypeFile
	TypeDir
	TypeSymlink
)

// String returns "other", "file", "dir" or "symlink".
func (t EntryType) String() string {
	switch t {
	case TypeFile:
		return "file"
	case TypeDir:
		return "dir"
	case TypeSymlink:
		return "symlink"
	default:
		return "other"
	}
}

// Entry is one directory entry.
type Entry struct {
	Name    string // display name; undecodable/encrypted names use "~enc~" + base64url(RawName)
	RawName []byte // on-disk name bytes when they differ from Name
	// ID is a stable fs-specific id: "inode:12", "nid:5", "oid:0x402",
	// "cnid:21", "dirent:<cluster>:<offset>". For a DIRECTORY it must identify
	// the directory itself (its first cluster, inode, ...), not the directory
	// entry that names it, so that Walk detects a cross-linked or
	// self-referencing directory as a cycle on its first revisit.
	ID         string
	Type       EntryType
	Size       int64
	Mode       uint32 // POSIX mode bits where the fs has them
	UID, GID   uint32
	Times      Times
	Deleted    bool // the directory entry is deleted (still visible on disk)
	Encrypted  bool // name and/or content encrypted
	LinkTarget string
	Attrs      []KV // fs-specific details (flags, xattr names, dir-entry location)
}

// KV is a key/value detail attached to an entry.
type KV struct{ Key, Value string }

// Times are the timestamps of an entry; a zero Timestamp.T means absent.
type Times struct{ Modified, Accessed, Changed, Created, Deleted Timestamp }

// Timestamp is a point in time with its zone certainty.
type Timestamp struct {
	T         time.Time // zero = absent
	ZoneKnown bool      // false for FAT local times
}

// Sentinel errors, usable with errors.Is.
var (
	ErrNotFound    = errors.New("not found")
	ErrDeleted     = errors.New("entry is deleted (use image recover)")
	ErrUnsupported = errors.New("unsupported filesystem feature")
	ErrEncrypted   = errors.New("encrypted (decryption is roadmap sub-project 10)")
	ErrCorrupt     = errors.New("corrupt filesystem structure")
	// ErrAmbiguous is returned when a reference (a snapshot name or xid) matches
	// more than one object; the error lists the candidates.
	ErrAmbiguous = errors.New("ambiguous reference")
)

// CorruptError reports a malformed on-disk structure. Offset is the byte
// offset of the structure, or -1 when it is not applicable or unknown.
type CorruptError struct {
	Structure string
	Offset    int64
	Reason    string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("corrupt %s at offset %d: %s", e.Structure, e.Offset, e.Reason)
}

// Is makes errors.Is(err, ErrCorrupt) true for every *CorruptError.
func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

// ErrNeedsVolume is returned by Snapshotter.SnapshotPath for a path that names
// no volume while the filesystem has more than one.
var ErrNeedsVolume = errors.New("the path must name a volume")

// Snapshotter is implemented by a filesystem that keeps snapshots (APFS).
// SnapshotPath maps a path inside a volume ("/Data/docs/a.txt"; "/" alone only
// when there is exactly one volume) to the real path of the same file in the
// named snapshot ("/Data/.snapshots/<snapshot>/docs/a.txt"). The snapshot is
// named by its display name or its "~raw~" alias. An unknown snapshot or volume
// is ErrNotFound (the message lists at most 20 snapshot names, quoted), an
// encrypted volume ErrEncrypted, a path without a volume ErrNeedsVolume. The
// path itself is not looked up.
type Snapshotter interface {
	SnapshotPath(p, snapshot string) (string, error)
}

// IsSnapshotsDir reports whether e is the synthetic directory that holds a
// volume's snapshots (attribute synthetic=snapshots). Recursive operations skip
// it unless the examiner addresses it.
func IsSnapshotsDir(e Entry) bool {
	if e.Type != TypeDir {
		return false
	}
	for _, kv := range e.Attrs {
		if kv.Key == "synthetic" && kv.Value == "snapshots" {
			return true
		}
	}
	return false
}

// SnapshotViewer is implemented by filesystems that keep snapshots: it reports
// whether e (an entry found inside a snapshot view) was read from a snapshot,
// and which one (display name and transaction id). Extraction records it in the
// derivation, so the provenance of snapshot bytes is explicit and not only
// implied by the path.
type SnapshotViewer interface {
	EntrySnapshot(e Entry) (name string, xid uint64, ok bool)
}
