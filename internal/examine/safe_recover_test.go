package examine_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// recImage is an MTFS image with a deleted node that carries maps.
func recImage(t *testing.T) (filesys.FileSystem, filesys.Entry) {
	t.Helper()
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/live", Data: pattern(blk, 1)},
		{Path: "/old", Data: pattern(blk, 2), Deleted: true, Freed: true, Recover: []fstest.RecoverMap{{Method: "own-runs", Basis: []string{"b"}}}},
	}})
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	es, err := fsys.ReadDir(fsys.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Name == "old" {
			return fsys, e
		}
	}
	t.Fatal("no deleted entry")
	return nil, filesys.Entry{}
}

type supports interface{ SupportsRecovery() bool }

func wrap(t *testing.T, fsys filesys.FileSystem) filesys.FileSystem {
	t.Helper()
	w, err := examine.WrapFS("test", fsys)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSafeFSForwardsRecoverer(t *testing.T) {
	fsys, old := recImage(t)
	w := wrap(t, fsys)
	rec, ok := filesys.As[filesys.Recoverer](w)
	if !ok {
		t.Fatal("the wrapper hides the reader's Recoverer")
	}
	got, err := rec.Recoverable(old)
	want, werr := fsys.(filesys.Recoverer).Recoverable(old)
	if err != nil || werr != nil || len(got) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("through the wrapper: %v, %v; direct: %v, %v", got, err, want, werr)
	}
	if s, ok := w.(supports); !ok || !s.SupportsRecovery() {
		t.Error("SupportsRecovery() is not true for a wrapped reader with a Recoverer")
	}
	// sentinel errors pass through unchanged
	if _, err := rec.Recoverable(filesys.Entry{ID: "nope"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
	live, err := fsys.Lookup("/live")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Recoverable(live); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live entry: %v, want ErrNotDeleted", err)
	}
}

func TestSafeFSNoRecovererIsErrNoRecovery(t *testing.T) {
	fsys, old := recImage(t)
	w := wrap(t, struct{ filesys.FileSystem }{fsys}) // the embedding hides Recoverable
	rec, ok := filesys.As[filesys.Recoverer](w)
	if !ok {
		t.Fatal("the wrapper must always answer, with ErrNoRecovery")
	}
	if cs, err := rec.Recoverable(old); !errors.Is(err, filesys.ErrNoRecovery) || cs != nil {
		t.Errorf("Recoverable without support = %v, %v; want nil, ErrNoRecovery", cs, err)
	}
	if s, ok := w.(supports); !ok || s.SupportsRecovery() {
		t.Error("SupportsRecovery() must be false when the wrapped filesystem has no Recoverer")
	}
}

type panicRecFS struct{ filesys.FileSystem }

func (panicRecFS) Recoverable(filesys.Entry) ([]filesys.Candidate, error) { panic("kaboom") }

func TestSafeFSRecoversRecovererPanic(t *testing.T) {
	fsys, old := recImage(t)
	w := wrap(t, panicRecFS{fsys})
	rec := mustAs[filesys.Recoverer](t, w)
	cs, err := rec.Recoverable(old)
	var ce *filesys.CorruptError
	if cs != nil || !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Recoverable that panics = %v, %v; want a *CorruptError", cs, err)
	}
	if s, ok := w.(supports); !ok || !s.SupportsRecovery() {
		t.Error("a panicking Recoverer still supports recovery (it is the call that failed)")
	}
}

// aliasRecFS hands out its own slices every time, as a careless reader could.
type aliasRecFS struct {
	filesys.FileSystem
	cands []filesys.Candidate
}

func (a *aliasRecFS) Recoverable(filesys.Entry) ([]filesys.Candidate, error) { return a.cands, nil }

func TestSafeFSCopiesCandidates(t *testing.T) {
	fsys, old := recImage(t)
	mk := func() []filesys.Candidate {
		return []filesys.Candidate{{
			Method: "m-one", Size: 4, Runs: []filesys.Run{{Offset: 1, Length: 4}}, Basis: []string{"b"}, Assumptions: []string{"a"}, Warnings: []string{"w"},
		}}
	}
	a := &aliasRecFS{FileSystem: fsys, cands: mk()}
	rec := mustAs[filesys.Recoverer](t, wrap(t, a))
	first, err := rec.Recoverable(old)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Method = "scribble"
	first[0].Runs[0] = filesys.Run{Offset: 99, Length: 99}
	first[0].Basis[0], first[0].Assumptions[0], first[0].Warnings[0] = "x", "x", "x"
	second, _ := rec.Recoverable(old)
	if !reflect.DeepEqual(second, mk()) {
		t.Errorf("after the caller changed an answer the next is %+v, want %+v", second, mk())
	}
	if !reflect.DeepEqual(a.cands, mk()) {
		t.Errorf("the reader's own slice was changed through the wrapper: %+v", a.cands)
	}
}

type journalFS struct {
	filesys.FileSystem
	panics bool
}

func (j journalFS) Journal() (filesys.JournalInfo, error) {
	if j.panics {
		panic("journal")
	}
	return filesys.JournalInfo{Type: "jbd2", Features: []string{"f"}}, nil
}

func (j journalFS) JournalTransactions(visit func(filesys.JournalTxn) bool) error {
	if j.panics {
		panic("txns")
	}
	visit(filesys.JournalTxn{Seq: 7, Blocks: []filesys.JournalTag{{FSBlock: 3}}})
	return nil
}

func (j journalFS) JournalBlock(t filesys.JournalTxn, i int) ([]byte, filesys.Run, error) {
	if j.panics {
		panic("block")
	}
	return []byte{byte(t.Seq), byte(i)}, filesys.Run{Offset: 10, Length: 2}, nil
}

func TestSafeFSForwardsJournaler(t *testing.T) {
	fsys, _ := recImage(t)
	j, ok := filesys.As[filesys.Journaler](wrap(t, journalFS{FileSystem: fsys}))
	if !ok {
		t.Fatal("the wrapper hides the Journaler")
	}
	info, err := j.Journal()
	if err != nil || info.Type != "jbd2" || len(info.Features) != 1 {
		t.Errorf("Journal = %+v, %v", info, err)
	}
	var seen []uint32
	if err := j.JournalTransactions(func(tx filesys.JournalTxn) bool { seen = append(seen, tx.Seq); return true }); err != nil || !reflect.DeepEqual(seen, []uint32{7}) {
		t.Errorf("JournalTransactions = %v, %v", seen, err)
	}
	b, r, err := j.JournalBlock(filesys.JournalTxn{Seq: 7}, 2)
	if err != nil || !bytes.Equal(b, []byte{7, 2}) || r != (filesys.Run{Offset: 10, Length: 2}) {
		t.Errorf("JournalBlock = %v, %v, %v", b, r, err)
	}

	// no journal: ErrNoJournal from each method
	nj := mustAs[filesys.Journaler](t, wrap(t, fsys))
	if _, err := nj.Journal(); !errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("Journal without a journal: %v", err)
	}
	if err := nj.JournalTransactions(func(filesys.JournalTxn) bool { return true }); !errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("JournalTransactions without a journal: %v", err)
	}
	if _, _, err := nj.JournalBlock(filesys.JournalTxn{}, 0); !errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("JournalBlock without a journal: %v", err)
	}

	// a panic is a CorruptError, in each method
	pj := mustAs[filesys.Journaler](t, wrap(t, journalFS{FileSystem: fsys, panics: true}))
	if _, err := pj.Journal(); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Journal panic: %v", err)
	}
	if err := pj.JournalTransactions(func(filesys.JournalTxn) bool { return true }); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("JournalTransactions panic: %v", err)
	}
	if _, _, err := pj.JournalBlock(filesys.JournalTxn{}, 0); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("JournalBlock panic: %v", err)
	}
}

// allocFS opens files that know their allocation.
type allocFS struct {
	filesys.FileSystem
	panics bool
}

type allocFile struct {
	filesys.File
	panics bool
}

func (a allocFile) AllocatedRuns() []filesys.Run {
	if a.panics {
		panic("alloc")
	}
	return []filesys.Run{{Offset: 512, Length: 1024}}
}

func (a allocFS) Open(e filesys.Entry) (filesys.File, error) {
	f, err := a.FileSystem.Open(e)
	if err != nil {
		return nil, err
	}
	return allocFile{File: f, panics: a.panics}, nil
}

func TestSafeFileForwardsAllocatedRuns(t *testing.T) {
	fsys, _ := recImage(t)
	live, err := fsys.Lookup("/live")
	if err != nil {
		t.Fatal(err)
	}
	f, err := wrap(t, allocFS{FileSystem: fsys}).Open(live)
	if err != nil {
		t.Fatal(err)
	}
	runs, ok := filesys.AllocatedRunsOf(f)
	want := []filesys.Run{{Offset: 512, Length: 1024}}
	if !ok || !reflect.DeepEqual(runs, want) {
		t.Fatalf("AllocatedRunsOf = %v, %v; want %v", runs, ok, want)
	}
	runs[0].Length = 1 // a copy: the next answer is unchanged
	if again, _ := filesys.AllocatedRunsOf(f); !reflect.DeepEqual(again, want) {
		t.Errorf("AllocatedRuns aliases the snapshot: %v", again)
	}

	// a file without the capability: false, not an empty allocation
	plain, err := wrap(t, fsys).Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if runs, ok := filesys.AllocatedRunsOf(plain); ok || runs != nil {
		t.Errorf("AllocatedRunsOf(plain) = %v, %v; want nil, false", runs, ok)
	}

	// a panic in AllocatedRuns surfaces when the file is opened
	if f, err := wrap(t, allocFS{FileSystem: fsys, panics: true}).Open(live); f != nil || !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open with a panicking AllocatedRuns = %v, %v; want a CorruptError", f, err)
	}
}

func mustAs[T any](t *testing.T, fsys filesys.FileSystem) T {
	t.Helper()
	v, ok := filesys.As[T](fsys)
	if !ok {
		t.Fatalf("filesys.As[%T] found nothing", v)
	}
	return v
}

// sharedJournalFS hands out its own slices (and checks what it is given), as a careless reader could.
type sharedJournalFS struct {
	filesys.FileSystem
	info  filesys.JournalInfo
	txn   filesys.JournalTxn
	block []byte
	got   filesys.JournalTxn // what JournalBlock was given
}

func (j *sharedJournalFS) Journal() (filesys.JournalInfo, error) { return j.info, nil }

func (j *sharedJournalFS) JournalTransactions(visit func(filesys.JournalTxn) bool) error {
	visit(j.txn)
	return nil
}

func (j *sharedJournalFS) JournalBlock(t filesys.JournalTxn, _ int) ([]byte, filesys.Run, error) {
	j.got = t
	if len(t.Blocks) > 0 {
		t.Blocks[0].FSBlock = 999 // a reader that writes into its argument
	}
	return j.block, filesys.Run{Offset: 1, Length: 2}, nil
}

func newSharedJournalFS(fsys filesys.FileSystem) *sharedJournalFS {
	return &sharedJournalFS{
		FileSystem: fsys,
		info:       filesys.JournalInfo{Type: "jbd2", Features: []string{"f1"}, Warnings: []string{"w1"}},
		txn: filesys.JournalTxn{
			Seq: 3, Blocks: []filesys.JournalTag{{FSBlock: 5}}, Revoked: []uint64{6}, Warnings: []string{"tw"},
		},
		block: []byte{1, 2, 3, 4},
	}
}

func TestSafeFSCopiesJournalAnswers(t *testing.T) {
	fsys, _ := recImage(t)
	pristine := newSharedJournalFS(fsys)
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, j filesys.Journaler)
	}{
		{"Journal Features", func(_ *testing.T, j filesys.Journaler) {
			info, _ := j.Journal()
			info.Features[0] = "scribble"
		}},
		{"Journal Warnings", func(_ *testing.T, j filesys.Journaler) {
			info, _ := j.Journal()
			info.Warnings[0] = "scribble"
		}},
		{"visited Blocks", func(_ *testing.T, j filesys.Journaler) {
			_ = j.JournalTransactions(func(tx filesys.JournalTxn) bool { tx.Blocks[0].FSBlock = 77; return true })
		}},
		{"visited Revoked", func(_ *testing.T, j filesys.Journaler) {
			_ = j.JournalTransactions(func(tx filesys.JournalTxn) bool { tx.Revoked[0] = 77; return true })
		}},
		{"visited Warnings", func(_ *testing.T, j filesys.Journaler) {
			_ = j.JournalTransactions(func(tx filesys.JournalTxn) bool { tx.Warnings[0] = "scribble"; return true })
		}},
		{"JournalBlock bytes", func(_ *testing.T, j filesys.Journaler) {
			b, _, _ := j.JournalBlock(filesys.JournalTxn{}, 0)
			b[0] = 0xEE
		}},
		{"JournalBlock input transaction", func(_ *testing.T, j filesys.Journaler) {
			_, _, _ = j.JournalBlock(filesys.JournalTxn{Blocks: []filesys.JournalTag{{FSBlock: 1}}}, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj := newSharedJournalFS(fsys)
			j := mustAs[filesys.Journaler](t, wrap(t, sj))
			tc.damage(t, j)
			if !reflect.DeepEqual(sj.info, pristine.info) || !reflect.DeepEqual(sj.txn, pristine.txn) || !bytes.Equal(sj.block, pristine.block) {
				t.Errorf("the reader's own data was changed through the wrapper: info %+v txn %+v block %v", sj.info, sj.txn, sj.block)
			}
		})
	}
	t.Run("JournalBlock input is the caller's, untouched", func(t *testing.T) {
		sj := newSharedJournalFS(fsys)
		j := mustAs[filesys.Journaler](t, wrap(t, sj))
		in := filesys.JournalTxn{Blocks: []filesys.JournalTag{{FSBlock: 1}}}
		if _, _, err := j.JournalBlock(in, 0); err != nil {
			t.Fatal(err)
		}
		if in.Blocks[0].FSBlock != 1 {
			t.Errorf("a reader that writes into the transaction it is given changed the caller's: %+v", in)
		}
	})
}

// sharedAllocFile returns one shared slice for AllocatedRuns.
type sharedAllocFile struct {
	filesys.File
	shared []filesys.Run
}

func (a sharedAllocFile) AllocatedRuns() []filesys.Run { return a.shared }

type sharedAllocFS struct {
	filesys.FileSystem
	shared []filesys.Run
}

func (a sharedAllocFS) Open(e filesys.Entry) (filesys.File, error) {
	f, err := a.FileSystem.Open(e)
	if err != nil {
		return nil, err
	}
	return sharedAllocFile{File: f, shared: a.shared}, nil
}

func TestSafeFileAllocationIsASnapshotAtOpen(t *testing.T) {
	fsys, _ := recImage(t)
	live, err := fsys.Lookup("/live")
	if err != nil {
		t.Fatal(err)
	}
	shared := []filesys.Run{{Offset: 512, Length: 1024}}
	f, err := wrap(t, sharedAllocFS{FileSystem: fsys, shared: shared}).Open(live)
	if err != nil {
		t.Fatal(err)
	}
	shared[0] = filesys.Run{Offset: -9, Length: -9} // the reader changes its slice after Open
	got, ok := filesys.AllocatedRunsOf(f)
	if want := []filesys.Run{{Offset: 512, Length: 1024}}; !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("AllocatedRunsOf after the reader changed its slice = %v, %v; want the snapshot taken at Open %v", got, ok, want)
	}
}

// panicWrapFS is a Wrapper whose Underlying panics, as a hostile or buggy wrapper could.
type panicWrapFS struct{ filesys.FileSystem }

func (panicWrapFS) Underlying() filesys.FileSystem { panic("underlying") }

func TestSupportsRecoveryRecoversAPanic(t *testing.T) {
	fsys, _ := recImage(t)
	w := wrap(t, panicWrapFS{struct{ filesys.FileSystem }{fsys}})
	s, ok := w.(supports)
	if !ok {
		t.Fatal("the wrapper has no SupportsRecovery")
	}
	if s.SupportsRecovery() {
		t.Error("SupportsRecovery() = true for a filesystem whose wrapper chain panics; want false")
	}
}
