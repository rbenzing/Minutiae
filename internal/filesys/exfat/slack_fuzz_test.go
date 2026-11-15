package exfat_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
)

const fuzzAllocFiles = 256

// FuzzExfatAllocatedRuns opens arbitrary bytes, walks at most 256 regular files
// and checks the AllocatedRunner contract of every file whose runs are valid.
func FuzzExfatAllocatedRuns(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := exfat.Open(bytes.NewReader(b), int64(len(b)))
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
			if filesys.CheckRuns(file.Runs(), file.Size(), info.Size) == nil {
				if err := filesys.CheckAllocatedRuns(file.Runs(), alloc, int64(info.BlockSize), info.Size); err != nil {
					t.Fatalf("%s: runs %v alloc %v: %v", p, file.Runs(), alloc, err)
				}
			}
			return nil
		})
	})
}
