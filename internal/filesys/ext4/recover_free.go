package ext4

import (
	"math"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// freeSet is the set of blocks the block bitmaps mark free, as merged filesystem-relative byte runs. It is the one
// place the reader consults free space (3C-R4): pointers are believed only if they come from a map block that is free
// now, because a reallocated map block holds another file's pointers. The DATA runs of a candidate are never checked
// against it; that cut belongs to the caller.
type freeSet struct{ runs []filesys.Run }

// freeSet returns the free set, built once per filesystem. It is built from Unallocated through the quiet view, so the
// warnings Unallocated raises (the same text examine already surfaces from the live path) are not added again
// (3C-R5). A group Unallocated skips (bad descriptor or bitmap) contributes no free blocks, so a map block in it is
// "not free": the conservative direction.
func (f *FS) freeSet() (*freeSet, error) {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	if f.freeS != nil {
		return f.freeS, nil
	}
	runs, err := f.quietView().Unallocated()
	if err != nil {
		return nil, err
	}
	f.freeS = &freeSet{runs: runs}
	return f.freeS, nil
}

// blockFree reports whether the whole of block blk is free.
func (s *freeSet) blockFree(blk uint64, bs int64) bool {
	if bs <= 0 || blk > uint64(math.MaxInt64/bs)-1 {
		return false
	}
	off := int64(blk) * bs
	end := off + bs
	i := sort.Search(len(s.runs), func(i int) bool { return s.runs[i].Offset+s.runs[i].Length > off })
	return i < len(s.runs) && s.runs[i].Offset <= off && s.runs[i].Offset+s.runs[i].Length >= end
}
