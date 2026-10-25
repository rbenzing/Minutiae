package fstest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// AllocatedRunsContract asserts the slack contract of a reader on fsys (see CheckAllocatedRunsContract).
func AllocatedRunsContract(t *testing.T, fsys filesys.FileSystem) {
	t.Helper()
	CheckAllocatedRunsContract(t, fsys)
}

// CheckAllocatedRunsContract walks the live tree of fsys and asserts:
//
//   - reports-slack: filesys.ReportsSlack(fsys) is true;
//   - open: every live regular file opens;
//   - has-alloc: filesys.AllocatedRunsOf is true for it;
//   - walk / no-files: an unreadable directory is reported, and a tree with no regular file is an error;
//   - inline-alloc: a file with no runs (inline content) reports no allocation runs either;
//   - contract: when its Runs pass CheckRuns, CheckAllocatedRuns(Runs, AllocatedRuns, Info.BlockSize, Info.Size) is nil;
//   - unaliased: a second AllocatedRuns call returns an equal slice that does not share memory with the first.
func CheckAllocatedRunsContract(r Reporter, fsys filesys.FileSystem) {
	r.Helper()
	if !filesys.ReportsSlack(fsys) {
		r.Errorf("reports-slack: the filesystem does not declare filesys.SlackReporter (or says false)")
	}
	info := fsys.Info()
	files := 0
	err := filesys.Walk(fsys, fsys.Root(), "/", func(path string, e filesys.Entry, err error) error {
		if err != nil {
			r.Errorf("walk: %s: %v", path, err)
			return filesys.SkipDir
		}
		if e.Type != filesys.TypeFile || e.Deleted {
			return nil
		}
		files++
		f, err := fsys.Open(e)
		if err != nil {
			r.Errorf("open: %s: %v", path, err)
			return nil
		}
		first, ok := filesys.AllocatedRunsOf(f)
		if !ok {
			r.Errorf("has-alloc: %s: the file does not implement filesys.AllocatedRunner", path)
			return nil
		}
		if len(f.Runs()) == 0 {
			if len(first) != 0 {
				r.Errorf("inline-alloc: %s: the file has no runs (inline content) but reports %d allocation runs", path, len(first))
			}
			return nil
		}
		if filesys.CheckRuns(f.Runs(), f.Size(), info.Size) != nil {
			return nil // a prefix or invalid list: not the contract's subject
		}
		if err := filesys.CheckAllocatedRuns(f.Runs(), first, int64(info.BlockSize), info.Size); err != nil {
			r.Errorf("contract: %s: %v", path, err)
			return nil
		}
		keep := append([]filesys.Run(nil), first...)
		for i := range first {
			first[i] = filesys.Run{Offset: -7, Length: -7}
		}
		second, _ := filesys.AllocatedRunsOf(f)
		if !reflect.DeepEqual(second, keep) {
			r.Errorf("unaliased: %s: a second AllocatedRuns call gave %v after the first answer was changed, want %v", path, second, keep)
		}
		return nil
	})
	if err != nil && !errors.Is(err, filesys.SkipDir) {
		r.Errorf("walk: %v", err)
	}
	if files == 0 {
		r.Errorf("no-files: the live tree holds no regular file, so nothing was checked (a fixture wiring mistake?)")
	}
}
