package apfs

import (
	"github.com/rbenzing/minutiae/internal/filesys"
)

// Unallocated reports the free space; it is added with the space manager.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	return nil, unsupported("free space is not implemented yet")
}
