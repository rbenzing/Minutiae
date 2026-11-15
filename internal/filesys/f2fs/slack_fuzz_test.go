package f2fs_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

const fuzzAllocFiles = 256

// FuzzF2FSAllocatedRuns opens arbitrary bytes, walks at most 256 regular files and checks the AllocatedRunner
// contract of every file whose runs are valid.
func FuzzF2FSAllocatedRuns(f *testing.F) {
	for _, s := range fuzzOpenSeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := f2fs.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		info := fsys.Info()
		files := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
			if err != nil || e.Type != filesys.TypeFile || e.Deleted {
				return nil
			}
			if files++; files > fuzzAllocFiles {
				return errFuzzBudget
			}
			file, err := fsys.Open(e)
			if err != nil {
				return nil
			}
			alloc, ok := filesys.AllocatedRunsOf(file)
			if !ok {
				t.Fatalf("%s: no AllocatedRunner", p)
			}
			if filesys.CheckRuns(file.Runs(), file.Size(), info.Size) == nil && len(file.Runs()) > 0 {
				if err := filesys.CheckAllocatedRuns(file.Runs(), alloc, int64(info.BlockSize), info.Size); err != nil {
					t.Fatalf("%s: runs %v alloc %v: %v", p, file.Runs(), alloc, err)
				}
			}
			return nil
		})
	})
}
