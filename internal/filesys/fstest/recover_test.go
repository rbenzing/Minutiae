package fstest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func asRecoverer(t *testing.T, fsys filesys.FileSystem) filesys.Recoverer {
	t.Helper()
	r, ok := filesys.As[filesys.Recoverer](fsys)
	if !ok {
		t.Fatal("the MTFS reader does not implement filesys.Recoverer")
	}
	return r
}

func entryByName(t *testing.T, fsys filesys.FileSystem, name string) filesys.Entry {
	t.Helper()
	es, err := fsys.ReadDir(fsys.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in the root", name)
	return filesys.Entry{}
}

func TestMTFSFreedBlocksAreUnallocated(t *testing.T) {
	build := func(freed bool) []filesys.Run {
		img := fstest.Build(fstest.BuildSpec{FreeBlocks: 1, Nodes: []fstest.Node{
			{Path: "/a", Data: pattern(2*bs, 1), Deleted: true, Freed: freed},
			{Path: "/b", Data: pattern(2*bs, 2), Deleted: true},
		}})
		rs, err := open(t, img).Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	base := build(false)
	if len(base) != 1 || base[0].Length != bs {
		t.Fatalf("without Freed Unallocated = %v, want only the one free tail block (existing behaviour)", base)
	}
	tail := base[0].Offset
	want := []filesys.Run{{Offset: tail - 4*bs, Length: 2 * bs}, {Offset: tail, Length: bs}}
	if got := build(true); !reflect.DeepEqual(got, want) {
		t.Errorf("with Freed Unallocated = %v, want %v (a's blocks free, b's still allocated)", got, want)
	}
}

func TestMTFSRecoverable(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/live.txt", Data: pattern(2*bs, 1)},
		{Path: "/old", Data: pattern(3*bs-12, 2), Deleted: true, Freed: true, MTime: 1700000000, Recover: []fstest.RecoverMap{
			{Method: "own-runs", Basis: []string{"b1"}},
			{Method: "other-runs", RunsOf: "/live.txt", Size: 700, Basis: []string{"b"}, Assumptions: []string{"a"}, Warnings: []string{"w"}, Mode: 0o600, Encrypted: true},
			{Method: "hostile", Runs: []filesys.Run{{Offset: 1 << 40, Length: -5}, {Offset: -1, Length: 3}}, Size: 9},
		}},
	}})
	fsys := open(t, img)
	rec := asRecoverer(t, fsys)
	old := entryByName(t, fsys, "old")
	got, err := rec.Recoverable(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3: %+v", len(got), got)
	}
	free, _ := fsys.Unallocated() // /old is the only Freed node and the image has no other free block
	if got[0].Method != "own-runs" || got[0].Size != 3*bs-12 || !reflect.DeepEqual(got[0].Runs, []filesys.Run{{Offset: free[0].Offset, Length: 3*bs - 12}}) || !reflect.DeepEqual(got[0].Basis, []string{"b1"}) {
		t.Errorf("own-runs candidate = %+v", got[0])
	}
	if got[0].Times.Modified.T.Unix() != 1700000000 {
		t.Errorf("own-runs candidate Times.Modified = %v, want the stale MTime", got[0].Times.Modified)
	}
	f, err := fsys.Open(entryByName(t, fsys, "live.txt"))
	if err != nil {
		t.Fatal(err)
	}
	wantOther := []filesys.Run{{Offset: f.Runs()[0].Offset, Length: 700}}
	c := got[1]
	if c.Method != "other-runs" || c.Size != 700 || !reflect.DeepEqual(c.Runs, wantOther) || !reflect.DeepEqual(c.Assumptions, []string{"a"}) ||
		!reflect.DeepEqual(c.Warnings, []string{"w"}) || c.Mode != 0o600 || !c.Encrypted {
		t.Errorf("RunsOf candidate = %+v, want runs %v", c, wantOther)
	}
	h := got[2]
	if h.Method != "hostile" || h.Size != 9 || !reflect.DeepEqual(h.Runs, []filesys.Run{{Offset: 1 << 40, Length: -5}, {Offset: -1, Length: 3}}) {
		t.Errorf("hostile candidate = %+v, want the explicit runs verbatim", h)
	}
	if _, err := filesys.CheckCandidate(h, int64(len(img))); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("CheckCandidate(hostile) = %v, want a corrupt-structure error", err)
	}
	// A deleted node with no planted maps has an empty list, not an error.
	img2 := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/x", Data: []byte("x"), Deleted: true}}})
	fs2 := open(t, img2)
	if cs, err := asRecoverer(t, fs2).Recoverable(entryByName(t, fs2, "x")); err != nil || len(cs) != 0 {
		t.Errorf("no planted maps: %v, %v; want an empty list and nil", cs, err)
	}
}

func TestMTFSRecoverableIgnoresForgedEntryFields(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/live.txt", Data: pattern(bs, 1)},
		{Path: "/old", Data: pattern(bs, 2), Deleted: true, Recover: []fstest.RecoverMap{{Method: "m"}}},
	}})
	fsys := open(t, img)
	rec := asRecoverer(t, fsys)
	old := entryByName(t, fsys, "old")
	want, err := rec.Recoverable(old)
	if err != nil || len(want) != 1 {
		t.Fatalf("baseline = %v, %v", want, err)
	}
	forged := []filesys.Entry{
		{ID: old.ID},
		{
			ID: old.ID, Name: "live.txt", RawName: []byte("zz"), Size: 1 << 40, Type: filesys.TypeDir, Deleted: false, Encrypted: true,
			LinkTarget: "/etc/passwd", Attrs: []filesys.KV{{Key: "first_cluster", Value: "9"}},
		},
	}
	for i, e := range forged {
		got, err := rec.Recoverable(e)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("forged %d: %v, %v; want the same candidates as the real entry", i, got, err)
		}
	}
	// The real entry's ID with live.txt's fields on a live id is still not deleted.
	live := entryByName(t, fsys, "live.txt")
	if _, err := rec.Recoverable(filesys.Entry{ID: live.ID, Name: "old", Deleted: true}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live id with Deleted forged: %v, want ErrNotDeleted", err)
	}
}

func TestMTFSRecoverableLiveIsNotDeleted(t *testing.T) {
	fsys := open(t, fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/live.txt", Data: []byte("x")}, {Path: "/d", Dir: true}}}))
	rec := asRecoverer(t, fsys)
	for _, e := range []filesys.Entry{entryByName(t, fsys, "live.txt"), entryByName(t, fsys, "d"), fsys.Root()} {
		if cs, err := rec.Recoverable(e); !errors.Is(err, filesys.ErrNotDeleted) || cs != nil {
			t.Errorf("Recoverable(%q) = %v, %v; want nil, ErrNotDeleted", e.Name, cs, err)
		}
	}
	if _, err := rec.Recoverable(filesys.Entry{ID: "nope"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
}

// withDuplicateID returns img with a copy of the table entry id appended under another name, so the
// id is held by two objects (a hostile image). The padded table length is kept.
func withDuplicateID(t *testing.T, img []byte, id string) []byte {
	t.Helper()
	tlen := int(img[8]) | int(img[9])<<8 | int(img[10])<<16
	var tbl map[string]any
	if err := json.Unmarshal(img[16:16+tlen], &tbl); err != nil {
		t.Fatal(err)
	}
	entries := tbl["entries"].([]any)
	for _, e := range entries {
		m := e.(map[string]any)
		if m["id"] == id {
			cp := map[string]any{}
			for k, v := range m {
				cp[k] = v
			}
			cp["name"] = fmt.Sprint(m["name"], "-twin")
			tbl["entries"] = append(entries, cp)
			out, err := json.Marshal(tbl)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > tlen {
				t.Fatalf("duplicated table (%d bytes) does not fit the padded table (%d)", len(out), tlen)
			}
			out = append(out, bytes.Repeat([]byte(" "), tlen-len(out))...)
			res := bytes.Clone(img)
			copy(res[16:], out)
			return res
		}
	}
	t.Fatalf("no entry with id %q", id)
	return nil
}

func TestMTFSRecoverableDuplicateIDIsCorrupt(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{BlockSize: 1024, Nodes: []fstest.Node{
		{Path: "/old", Data: pattern(bs, 2), Deleted: true, Recover: []fstest.RecoverMap{{Method: "m"}}},
		{Path: "/other", Data: pattern(bs, 3), Deleted: true, Recover: []fstest.RecoverMap{{Method: "m"}}},
	}})
	fsys := open(t, img)
	id := entryByName(t, fsys, "old").ID
	dup := open(t, withDuplicateID(t, img, id))
	rec := asRecoverer(t, dup)
	for _, e := range []filesys.Entry{{ID: id}, {ID: id, Name: "old"}, {ID: id, Name: "old-twin"}} {
		cs, err := rec.Recoverable(e)
		var ce *filesys.CorruptError
		if cs != nil || !errors.Is(err, filesys.ErrCorrupt) || !errors.As(err, &ce) || !strings.Contains(ce.Reason, "duplicate entry id") {
			t.Errorf("Recoverable(%+v) = %v, %v; want a *CorruptError 'duplicate entry id', never a guess", e, cs, err)
		}
	}
	if cs, err := rec.Recoverable(entryByName(t, dup, "other")); err != nil || len(cs) != 1 {
		t.Errorf("the other deleted node: %v, %v; want its candidate", cs, err)
	}
}

func TestOpenDeletedStillErrDeleted(t *testing.T) {
	fsys := open(t, fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/old", Data: pattern(2*bs, 2), Deleted: true, Freed: true, Recover: []fstest.RecoverMap{{Method: "m"}}},
	}}))
	old := entryByName(t, fsys, "old")
	if cs, err := asRecoverer(t, fsys).Recoverable(old); err != nil || len(cs) != 1 {
		t.Fatalf("Recoverable = %v, %v", cs, err)
	}
	for _, deleted := range []bool{true, false} {
		e := old
		e.Deleted = deleted
		if f, err := fsys.Open(e); !errors.Is(err, filesys.ErrDeleted) || f != nil {
			t.Errorf("Open(Deleted=%v) after Recoverable = %v, %v; want ErrDeleted", deleted, f, err)
		}
	}
}

func TestBuildPanicsOnRecoverMapWithoutMethod(t *testing.T) {
	for name, n := range map[string]fstest.Node{
		"no method":            {Path: "/a", Data: []byte("x"), Deleted: true, Recover: []fstest.RecoverMap{{}}},
		"unknown RunsOf":       {Path: "/a", Data: []byte("x"), Deleted: true, Recover: []fstest.RecoverMap{{Method: "m", RunsOf: "/nope"}}},
		"freed but live":       {Path: "/a", Data: []byte("x"), Freed: true},
		"recover but live":     {Path: "/a", Data: []byte("x"), Recover: []fstest.RecoverMap{{Method: "m"}}},
		"second map no method": {Path: "/a", Data: []byte("x"), Deleted: true, Recover: []fstest.RecoverMap{{Method: "m"}, {}}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Build did not panic")
				}
			}()
			fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{n}})
		})
	}
}

// countingReader counts ReadAt calls on the image under a reader.
type countingReader struct {
	r     io.ReaderAt
	reads atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	return c.r.ReadAt(p, off)
}

// contractImage is the image the kit runs against: a deleted freed node with maps, a live node and a
// deleted twin whose id is held twice.
func contractImage(t *testing.T) (img []byte, dupID string) {
	t.Helper()
	img = fstest.Build(fstest.BuildSpec{BlockSize: 2048, FreeBlocks: 2, Nodes: []fstest.Node{
		{Path: "/gone", Data: pattern(2*bs, 4), Deleted: true, Freed: true, Recover: []fstest.RecoverMap{
			{Method: "own-runs", Basis: []string{"table"}, Assumptions: []string{"a1", "a2"}, Warnings: []string{"w1"}},
			{Method: "live-runs", RunsOf: "/live", Size: 100},
		}},
		{Path: "/live", Data: pattern(bs, 5)},
		{Path: "/twin", Data: pattern(bs, 6), Deleted: true, Recover: []fstest.RecoverMap{{Method: "own-runs"}}},
	}})
	fsys := open(t, img)
	dupID = entryByName(t, fsys, "twin").ID
	return withDuplicateID(t, img, dupID), dupID
}

func contractSubject(t *testing.T, fsys filesys.FileSystem, cr *countingReader, dupID string) fstest.RecovererSubject {
	t.Helper()
	dup := filesys.Entry{ID: dupID}
	return fstest.RecovererSubject{
		FS:        fsys,
		Deleted:   entryByName(t, fsys, "gone"),
		Live:      entryByName(t, fsys, "live"),
		ForgedIDs: []string{"", "02", "+2", "-1", "x:2", "2 ", "99999999999999999999999", "0x2", "cnid:2"},
		Reads:     func() int64 { return cr.reads.Load() },
		Duplicate: &dup,
	}
}

func TestRecovererContractOnMTFS(t *testing.T) {
	img, dupID := contractImage(t)
	cr := &countingReader{r: bytes.NewReader(img)}
	fsys, err := fstest.Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	fstest.RecovererContract(t, contractSubject(t, fsys, cr, dupID))

	// Without the optional parts the kit still runs.
	s := contractSubject(t, fsys, cr, dupID)
	s.Reads, s.Duplicate = nil, nil
	fstest.RecovererContract(t, s)
}

type recorder struct{ errs []string }

func (r *recorder) Helper()                   {}
func (r *recorder) Errorf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }
func (r *recorder) reported(tag string) bool {
	for _, e := range r.errs {
		if strings.Contains(e, tag) {
			return true
		}
	}
	return false
}

// badFS wraps a good MTFS reader and bends one behaviour.
type badFS struct {
	filesys.FileSystem
	good  filesys.Recoverer
	recov func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error)
	open  func(b *badFS, e filesys.Entry) (filesys.File, error)
	reads *atomic.Int64
	calls int
	cache []filesys.Candidate
}

func (b *badFS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) { return b.recov(b, e) }

func (b *badFS) Open(e filesys.Entry) (filesys.File, error) {
	if b.open != nil {
		return b.open(b, e)
	}
	return b.FileSystem.Open(e)
}

func TestRecovererContractCatchesBadReaders(t *testing.T) {
	img, dupID := contractImage(t)
	cases := []struct {
		name  string
		tag   string // the rule that must fire
		recov func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error)
		open  func(b *badFS, e filesys.Entry) (filesys.File, error)
	}{
		{name: "invalid candidate", tag: "check-candidate", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			for i := range cs {
				cs[i].Method = "BAD METHOD"
			}
			return cs, err
		}},
		{name: "no candidates for a deleted entry", tag: "no-candidates", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			_, err := b.good.Recoverable(e)
			return nil, err
		}},
		{name: "trusts e.Size", tag: "forged-fields", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if e.Size > 0 && len(cs) > 0 {
				cs[0].Size = e.Size
			}
			return cs, err
		}},
		{name: "trusts e.Type", tag: "forged-fields", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if e.Type == filesys.TypeDir && len(cs) > 0 {
				cs[0].Method = "dir-method"
			}
			return cs, err
		}},
		{name: "trusts e.Name", tag: "forged-fields", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if e.Name == "zz-forged" && len(cs) > 0 {
				cs[0].Basis = []string{"by name"}
			}
			return cs, err
		}},
		{name: "trusts the Deleted flag", tag: "live-not-deleted", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			if e.Deleted {
				return []filesys.Candidate{{Method: "x-map"}}, nil
			}
			return b.good.Recoverable(e)
		}},
		{name: "candidates for a live id", tag: "live-not-deleted", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if errors.Is(err, filesys.ErrNotDeleted) {
				return []filesys.Candidate{{Method: "live-map"}}, nil
			}
			return cs, err
		}},
		{name: "scans before rejecting a forged id", tag: "forged-id-read", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if errors.Is(err, filesys.ErrNotFound) {
				b.reads.Add(1)
			}
			return cs, err
		}},
		{name: "forged id is an empty list", tag: "forged-id-not-found", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if errors.Is(err, filesys.ErrNotFound) {
				return nil, nil
			}
			return cs, err
		}},
		{name: "guesses among duplicate ids", tag: "duplicate-corrupt", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if errors.Is(err, filesys.ErrCorrupt) {
				return []filesys.Candidate{{Method: "guess"}}, nil
			}
			return cs, err
		}},
		{
			name: "Open of a deleted entry succeeds", tag: "open-deleted",
			recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) { return b.good.Recoverable(e) },
			open: func(b *badFS, e filesys.Entry) (filesys.File, error) {
				if f, err := b.FileSystem.Open(e); err == nil {
					return f, nil
				}
				live, err := b.Lookup("/live")
				if err != nil {
					return nil, err
				}
				return b.FileSystem.Open(live)
			},
		},
		{
			name: "trusts the cleared Deleted flag in Open", tag: "open-deleted",
			recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) { return b.good.Recoverable(e) },
			open: func(b *badFS, e filesys.Entry) (filesys.File, error) {
				if e.Deleted {
					return b.FileSystem.Open(e)
				}
				live, err := b.Lookup("/live")
				if err != nil {
					return nil, err
				}
				return b.FileSystem.Open(live)
			},
		},
		{name: "duplicate id answers with candidates and an error", tag: "duplicate-corrupt", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if errors.Is(err, filesys.ErrCorrupt) {
				return []filesys.Candidate{{Method: "guess"}}, err
			}
			return cs, err
		}},
		{name: "more candidates than the cap", tag: "max-candidates", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			for len(cs) > 0 && e.ID == "2" && len(cs) <= filesys.MaxCandidatesPerEntry {
				cs = append(cs, cs[0])
			}
			return cs, err
		}},
		{name: "answers change between calls", tag: "deterministic", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			b.calls++
			if len(cs) > 0 {
				cs[0].Basis = append(cs[0].Basis, fmt.Sprint("call ", b.calls))
			}
			return cs, err
		}},
		{name: "returns its own cache", tag: "aliasing", recov: func(b *badFS, e filesys.Entry) ([]filesys.Candidate, error) {
			cs, err := b.good.Recoverable(e)
			if err == nil && e.ID == "2" {
				if b.cache == nil {
					b.cache = cs
				}
				return b.cache, nil
			}
			return cs, err
		}},
	}
	// The aliasing fake keys on the deleted entry's id; make sure the image gives /gone id 2.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Sanity: the unbent reader passes (the failure below is the bend, not the rig).
			cr := &countingReader{r: bytes.NewReader(img)}
			base, err := fstest.Open(cr, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			b := &badFS{FileSystem: base, good: asRecoverer(t, base), recov: c.recov, open: c.open, reads: &cr.reads}
			s := contractSubject(t, b, cr, dupID)
			if s.Deleted.ID != "2" {
				t.Fatalf("test rig: /gone has id %q, want 2", s.Deleted.ID)
			}
			rec := &recorder{}
			fstest.CheckRecoverer(rec, s)
			if !rec.reported(c.tag) {
				t.Errorf("the kit did not report rule %q for a reader that %s; it reported %q", c.tag, c.name, rec.errs)
			}
		})
	}

	t.Run("unbent reader passes", func(t *testing.T) {
		cr := &countingReader{r: bytes.NewReader(img)}
		base, _ := fstest.Open(cr, int64(len(img)))
		rec := &recorder{}
		fstest.CheckRecoverer(rec, contractSubject(t, base, cr, dupID))
		if len(rec.errs) != 0 {
			t.Errorf("the good reader reported %q", rec.errs)
		}
	})
	t.Run("no Recoverer at all", func(t *testing.T) {
		cr := &countingReader{r: bytes.NewReader(img)}
		base, _ := fstest.Open(cr, int64(len(img)))
		s := contractSubject(t, struct{ filesys.FileSystem }{base}, cr, dupID)
		rec := &recorder{}
		fstest.CheckRecoverer(rec, s)
		if !rec.reported("no-recoverer") {
			t.Errorf("a subject without a Recoverer was not reported: %q", rec.errs)
		}
	})
}

// A map that borrows another node's runs skips that node's holes (a hole is not a place bytes live)
// and, with Size 0, takes the SOURCE node's size, not the deleted node's own.
func TestMTFSRecoverRunsOfSkipsHolesAndDefaultsToSourceSize(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/sparse", Data: pattern(3*bs, 1), Hole: true},
		{Path: "/old", Data: pattern(bs-10, 2), Deleted: true, Recover: []fstest.RecoverMap{
			{Method: "sparse-runs", RunsOf: "/sparse", Size: 3 * bs},
			{Method: "default-size", RunsOf: "/sparse"},
		}},
	}})
	fsys := open(t, img)
	f, err := fsys.Open(entryByName(t, fsys, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	var want []filesys.Run
	for _, r := range f.Runs() {
		if r.Offset >= 0 {
			want = append(want, r)
		}
	}
	if len(want) != len(f.Runs())-1 || len(want) == 0 {
		t.Fatalf("test rig: /sparse runs %v hold no single hole", f.Runs())
	}
	got, err := asRecoverer(t, fsys).Recoverable(entryByName(t, fsys, "old"))
	if err != nil || len(got) != 2 {
		t.Fatalf("Recoverable = %v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0].Runs, want) {
		t.Errorf("sparse-runs = %v, want the source's runs without its hole %v", got[0].Runs, want)
	}
	if got[1].Size != 3*bs {
		t.Errorf("default-size candidate Size = %d, want the source node's size %d", got[1].Size, 3*bs)
	}
}
