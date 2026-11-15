package fstest_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// slackFile is a regular file that reports its allocation.
type slackFile struct {
	size  int64
	runs  []filesys.Run
	alloc []filesys.Run
}

func (f *slackFile) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }

func (f *slackFile) Size() int64 { return f.size }

func (f *slackFile) Runs() []filesys.Run { return append([]filesys.Run(nil), f.runs...) }

func (f *slackFile) AllocatedRuns() []filesys.Run { return append([]filesys.Run(nil), f.alloc...) }

// slackFake is a one-file filesystem /a.txt of 700 bytes on a 1 MiB volume with 512-byte units.
type slackFake struct {
	reports bool
	file    *slackFile
}

func (s *slackFake) Info() filesys.Info {
	return filesys.Info{Type: "fake", BlockSize: 512, Size: 1 << 20}
}

func (s *slackFake) Root() filesys.Entry { return filesys.Entry{ID: "root", Type: filesys.TypeDir} }

func (s *slackFake) ReadDir(filesys.Entry) ([]filesys.Entry, error) {
	return []filesys.Entry{{Name: "a.txt", ID: "f1", Type: filesys.TypeFile, Size: 700}}, nil
}

func (s *slackFake) Lookup(string) (filesys.Entry, error) {
	return filesys.Entry{}, filesys.ErrNotFound
}

func (s *slackFake) Open(filesys.Entry) (filesys.File, error) { return s.file, nil }

func (s *slackFake) Unallocated() ([]filesys.Run, error) { return nil, nil }

func (s *slackFake) ReportsSlack() bool { return s.reports }

type slackRecorder struct{ msgs []string }

func (r *slackRecorder) Helper() {}
func (r *slackRecorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func newSlackFake(reports bool, alloc []filesys.Run) *slackFake {
	return &slackFake{reports: reports, file: &slackFile{
		size:  700,
		runs:  []filesys.Run{{Offset: 4096, Length: 700}},
		alloc: alloc,
	}}
}

func TestCheckAllocatedRunsContractReports(t *testing.T) {
	good := []filesys.Run{{Offset: 4096, Length: 1024}}

	var ok slackRecorder
	fstest.CheckAllocatedRunsContract(&ok, newSlackFake(true, good))
	if len(ok.msgs) != 0 {
		t.Errorf("a correct fake gave errors: %v", ok.msgs)
	}

	var short slackRecorder
	fstest.CheckAllocatedRunsContract(&short, newSlackFake(true, good[:0]))
	if len(short.msgs) == 0 || !strings.Contains(strings.Join(short.msgs, "\n"), "/a.txt") {
		t.Errorf("a short allocation list gave %v, want an error naming /a.txt", short.msgs)
	}

	var silent slackRecorder
	fstest.CheckAllocatedRunsContract(&silent, newSlackFake(false, good))
	if len(silent.msgs) == 0 {
		t.Errorf("a filesystem that does not report slack gave no error")
	}
}

// aliasFile hands out its own slice: a caller that changes it changes the next answer.
type aliasFile struct{ *slackFile }

func (f aliasFile) AllocatedRuns() []filesys.Run { return f.alloc }

type aliasFS struct{ *slackFake }

func (a aliasFS) Open(filesys.Entry) (filesys.File, error) { return aliasFile{a.file}, nil }

func TestCheckAllocatedRunsContractReportsAliasing(t *testing.T) {
	var rec slackRecorder
	fstest.CheckAllocatedRunsContract(&rec, aliasFS{newSlackFake(true, []filesys.Run{{Offset: 4096, Length: 1024}})})
	if !strings.Contains(strings.Join(rec.msgs, "\n"), "unaliased") {
		t.Errorf("an aliasing AllocatedRuns gave %v, want an unaliased error", rec.msgs)
	}
}

// dirErrFS fails to list its root; emptyFS lists nothing.
type dirErrFS struct{ *slackFake }

func (dirErrFS) ReadDir(filesys.Entry) ([]filesys.Entry, error) {
	return nil, errors.New("boom: unreadable directory")
}

type emptyFS struct{ *slackFake }

func (emptyFS) ReadDir(filesys.Entry) ([]filesys.Entry, error) { return nil, nil }

func TestCheckAllocatedRunsContractReportsUnreadableDirectory(t *testing.T) {
	var rec slackRecorder
	fstest.CheckAllocatedRunsContract(&rec, dirErrFS{newSlackFake(true, []filesys.Run{{Offset: 4096, Length: 1024}})})
	if !strings.Contains(strings.Join(rec.msgs, "\n"), "walk: /: ") {
		t.Errorf("an unreadable directory gave %v, want a walk error naming it", rec.msgs)
	}
}

func TestCheckAllocatedRunsContractReportsNoFiles(t *testing.T) {
	var rec slackRecorder
	fstest.CheckAllocatedRunsContract(&rec, emptyFS{newSlackFake(true, nil)})
	if !strings.Contains(strings.Join(rec.msgs, "\n"), "no-files") {
		t.Errorf("a tree without regular files gave %v, want a no-files error", rec.msgs)
	}
}

func TestCheckAllocatedRunsContractInlineFileHasNoAllocation(t *testing.T) {
	inline := newSlackFake(true, []filesys.Run{{Offset: 4096, Length: 512}})
	inline.file.runs = nil
	var rec slackRecorder
	fstest.CheckAllocatedRunsContract(&rec, inline)
	if !strings.Contains(strings.Join(rec.msgs, "\n"), "inline-alloc") {
		t.Errorf("an inline file with an allocation gave %v, want an inline-alloc error", rec.msgs)
	}
	ok := newSlackFake(true, nil)
	ok.file.runs = nil
	var rec2 slackRecorder
	fstest.CheckAllocatedRunsContract(&rec2, ok)
	if len(rec2.msgs) != 0 {
		t.Errorf("an inline file without allocation gave %v", rec2.msgs)
	}
}
