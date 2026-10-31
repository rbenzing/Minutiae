package f2fs

import "github.com/rbenzing/minutiae/internal/filesys"

// RollForward is a stub until plan 3H Task 5.
func (f *FS) RollForward(_ func(filesys.RollForwardNode) bool) (filesys.RollForwardChain, error) {
	return filesys.RollForwardChain{}, nil
}
