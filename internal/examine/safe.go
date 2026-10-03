package examine

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// panicError converts a recovered panic value into a *filesys.CorruptError.
// Filesystem parsers read hostile bytes; a bug that panics must not take the
// examination process down.
func panicError(structure, method string, p any) error {
	return &filesys.CorruptError{
		Structure: structure, Offset: -1,
		Reason: fmt.Sprintf("parser panic in %s: %v", method, p),
	}
}

// safeFS wraps a FileSystem so that a panic in any method (or in any method of
// a File it returns) becomes a *filesys.CorruptError.
type safeFS struct {
	fs   filesys.FileSystem
	name string // structure name used in errors
	info filesys.Info
	root filesys.Entry
}

// wrapFS snapshots Info and Root (recovering a panic in either) and returns
// the protected filesystem.
func wrapFS(name string, fsys filesys.FileSystem) (out filesys.FileSystem, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, panicError(name, "Info/Root", p)
		}
	}()
	return &safeFS{fs: fsys, name: name, info: fsys.Info(), root: fsys.Root()}, nil
}

// Info returns the live Info. If the parser panics, the snapshot taken when
// the filesystem was opened is returned with a warning appended.
func (s *safeFS) Info() (info filesys.Info) {
	defer func() {
		if p := recover(); p != nil {
			info = s.info
			info.Warnings = append(append([]string(nil), s.info.Warnings...), panicError(s.name, "Info", p).Error())
		}
	}()
	return s.fs.Info()
}

func (s *safeFS) Root() filesys.Entry { return s.root }

func (s *safeFS) ReadDir(dir filesys.Entry) (es []filesys.Entry, err error) {
	defer func() {
		if p := recover(); p != nil {
			es, err = nil, panicError(s.name, "ReadDir", p)
		}
	}()
	return s.fs.ReadDir(dir)
}

func (s *safeFS) Lookup(path string) (e filesys.Entry, err error) {
	defer func() {
		if p := recover(); p != nil {
			e, err = filesys.Entry{}, panicError(s.name, "Lookup", p)
		}
	}()
	return s.fs.Lookup(path)
}

func (s *safeFS) Open(e filesys.Entry) (f filesys.File, err error) {
	defer func() {
		if p := recover(); p != nil {
			f, err = nil, panicError(s.name, "Open", p)
		}
	}()
	inner, err := s.fs.Open(e)
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, &filesys.CorruptError{Structure: s.name, Offset: -1, Reason: "Open returned no file and no error"}
	}
	// Size and Runs are snapshotted here so a panic in them surfaces as an
	// Open error; ReadAt is protected on every call.
	return &safeFile{f: inner, name: s.name, size: inner.Size(), runs: inner.Runs()}, nil
}

func (s *safeFS) Unallocated() (rs []filesys.Run, err error) {
	defer func() {
		if p := recover(); p != nil {
			rs, err = nil, panicError(s.name, "Unallocated", p)
		}
	}()
	return s.fs.Unallocated()
}

// safeFile is the panic-protected filesys.File.
type safeFile struct {
	f    filesys.File
	name string
	size int64
	runs []filesys.Run
}

func (f *safeFile) ReadAt(p []byte, off int64) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			n, err = 0, panicError(f.name, "ReadAt", r)
		}
	}()
	return f.f.ReadAt(p, off)
}

func (f *safeFile) Size() int64 { return f.size }

func (f *safeFile) Runs() []filesys.Run { return append([]filesys.Run(nil), f.runs...) }
