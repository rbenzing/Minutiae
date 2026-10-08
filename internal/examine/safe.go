package examine

import (
	"fmt"
	"slices"

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
	sf := &safeFile{f: inner, name: s.name, size: inner.Size(), runs: inner.Runs(), enc: filesys.FileEncrypted(inner)}
	if ar, ok := filesys.AllocatedRunsOf(inner); ok {
		sf.alloc, sf.hasAlloc = slices.Clone(ar), true
	}
	return sf, nil
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
	enc  bool // the file reported itself encrypted when it was opened

	// allocation snapshot (filesys.AllocatedRunner), taken at Open inside the recover
	alloc    []filesys.Run
	hasAlloc bool
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

// Encrypted forwards what the file reported when it was opened
// (filesys.EncryptedFile).
func (f *safeFile) Encrypted() bool { return f.enc }

// SnapshotPath implements filesys.Snapshotter for every wrapped filesystem: it
// forwards to the filesystem when that keeps snapshots (a panic becomes a
// *filesys.CorruptError) and is an ErrUnsupported error otherwise.
func (s *safeFS) SnapshotPath(p, snapshot string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", panicError(s.name, "SnapshotPath", r)
		}
	}()
	sn, ok := s.fs.(filesys.Snapshotter)
	if !ok {
		return "", errNoSnapshots()
	}
	return sn.SnapshotPath(p, snapshot)
}

// EntrySnapshot implements filesys.SnapshotViewer for every wrapped
// filesystem: false when it keeps no snapshots (a panic is not a snapshot).
func (s *safeFS) EntrySnapshot(e filesys.Entry) (name string, xid uint64, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			name, xid, ok = "", 0, false
		}
	}()
	sv, has := s.fs.(filesys.SnapshotViewer)
	if !has {
		return "", 0, false
	}
	return sv.EntrySnapshot(e)
}

// AllocatedRuns implements filesys.AllocatedRunner with the allocation the file reported when it was
// opened (a copy); HasAllocatedRuns says whether it reported one at all.
func (f *safeFile) AllocatedRuns() []filesys.Run { return slices.Clone(f.alloc) }

// HasAllocatedRuns reports whether the wrapped file knows its allocation.
func (f *safeFile) HasAllocatedRuns() bool { return f.hasAlloc }

// SupportsRecovery reports whether the wrapped filesystem (or one it wraps) implements
// filesys.Recoverer. Recoverable is always present on this wrapper, so callers ask this first.
func (s *safeFS) SupportsRecovery() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_, ok = filesys.As[filesys.Recoverer](s.fs)
	return ok
}

// Recoverable implements filesys.Recoverer: it forwards to the wrapped filesystem's Recoverer (a panic
// becomes a *filesys.CorruptError) and hands out a deep copy, so a caller cannot change the reader's
// own slices. A filesystem without recovery is filesys.ErrNoRecovery.
func (s *safeFS) Recoverable(e filesys.Entry) (cs []filesys.Candidate, err error) {
	defer func() {
		if p := recover(); p != nil {
			cs, err = nil, panicError(s.name, "Recoverable", p)
		}
	}()
	r, ok := filesys.As[filesys.Recoverer](s.fs)
	if !ok {
		return nil, filesys.ErrNoRecovery
	}
	got, err := r.Recoverable(e)
	return copyCandidates(got), err
}

func copyCandidates(in []filesys.Candidate) []filesys.Candidate {
	if in == nil {
		return nil
	}
	out := make([]filesys.Candidate, len(in))
	for i, c := range in {
		out[i] = c
		out[i].Runs = slices.Clone(c.Runs)
		out[i].Basis = slices.Clone(c.Basis)
		out[i].Assumptions = slices.Clone(c.Assumptions)
		out[i].Warnings = slices.Clone(c.Warnings)
	}
	return out
}

// Journal implements filesys.Journaler by forwarding (filesys.ErrNoJournal when the wrapped filesystem
// keeps none; a panic is a *filesys.CorruptError).
func (s *safeFS) Journal() (info filesys.JournalInfo, err error) {
	defer func() {
		if p := recover(); p != nil {
			info, err = filesys.JournalInfo{}, panicError(s.name, "Journal", p)
		}
	}()
	j, ok := filesys.As[filesys.Journaler](s.fs)
	if !ok {
		return filesys.JournalInfo{}, filesys.ErrNoJournal
	}
	info, err = j.Journal()
	info.Features = slices.Clone(info.Features)
	info.Warnings = slices.Clone(info.Warnings)
	return info, err
}

// JournalTransactions implements filesys.Journaler; each transaction handed to visit is a copy.
func (s *safeFS) JournalTransactions(visit func(filesys.JournalTxn) bool) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = panicError(s.name, "JournalTransactions", p)
		}
	}()
	j, ok := filesys.As[filesys.Journaler](s.fs)
	if !ok {
		return filesys.ErrNoJournal
	}
	return j.JournalTransactions(func(t filesys.JournalTxn) bool { return visit(copyTxn(t)) })
}

// JournalBlock implements filesys.Journaler.
func (s *safeFS) JournalBlock(t filesys.JournalTxn, i int) (b []byte, r filesys.Run, err error) {
	defer func() {
		if p := recover(); p != nil {
			b, r, err = nil, filesys.Run{}, panicError(s.name, "JournalBlock", p)
		}
	}()
	j, ok := filesys.As[filesys.Journaler](s.fs)
	if !ok {
		return nil, filesys.Run{}, filesys.ErrNoJournal
	}
	b, r, err = j.JournalBlock(copyTxn(t), i)
	return slices.Clone(b), r, err
}

func copyTxn(t filesys.JournalTxn) filesys.JournalTxn {
	t.Blocks = slices.Clone(t.Blocks)
	t.Revoked = slices.Clone(t.Revoked)
	t.Warnings = slices.Clone(t.Warnings)
	return t
}
