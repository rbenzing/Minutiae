package apfs

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Open opens a file. Reading file data is added with the data layer; until then
// every file is unsupported, but the entry is validated like any other access:
// a forged ID is ErrNotFound and anything below an encrypted volume is
// ErrEncrypted.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	kind, _, _, _, ok := parseEntryID(e.ID)
	if !ok {
		return nil, notFound("entry ID %q", e.ID)
	}
	if kind != idNode {
		return nil, fmt.Errorf("apfs: %w: %q is a directory", filesys.ErrUnsupported, e.ID)
	}
	v, view, ino, err := f.nodeTarget(e.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(v, view, ino)
	if err != nil {
		return nil, err
	}
	if in.isDir() {
		return nil, fmt.Errorf("apfs: %w: %q is a directory", filesys.ErrUnsupported, e.ID)
	}
	return nil, unsupported("reading file data is not implemented yet")
}

// Unallocated reports the free space; it is added with the space manager.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	return nil, unsupported("free space is not implemented yet")
}
