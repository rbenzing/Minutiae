package examine_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
)

func captureSegments(t *testing.T, c *evidence.Case, counts map[int]int) []evidence.ManifestRecord {
	t.Helper()
	data := disk(mtfsImage(map[string]string{"/a": "x"}))
	acq := evidence.NewAcquisitionID(time.Now())
	var recs []evidence.ManifestRecord
	for seg := 1; seg <= len(counts); seg++ {
		rec, err := c.Capture("img", acq, "image/s"+string(rune('0'+seg)),
			evidence.Source{Kind: "import", DeviceID: "img", Segment: seg, Segments: counts[seg]},
			func(w io.Writer) error { _, err := w.Write(data); return err })
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestOpenPartialOrInconsistentImportIsIntegrityError(t *testing.T) {
	t.Run("last segment never imported", func(t *testing.T) {
		c := newCase(t)
		recs := captureSegments(t, c, map[int]int{1: 3, 2: 3})
		_, err := examine.Open(c, recs[0].ID, mtfsOpts())
		if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("Open = %v, want ErrIntegrity naming missing segment 3", err)
		}
	})
	t.Run("disagreeing totals", func(t *testing.T) {
		c := newCase(t)
		recs := captureSegments(t, c, map[int]int{1: 2, 2: 3})
		if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "inconsistent") {
			t.Fatalf("Open = %v, want ErrIntegrity (inconsistent)", err)
		}
	})
	t.Run("no total recorded", func(t *testing.T) {
		c := newCase(t)
		recs := captureSegments(t, c, map[int]int{1: 0})
		if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, evidence.ErrIntegrity) {
			t.Fatalf("Open = %v, want ErrIntegrity", err)
		}
	})
	t.Run("complete", func(t *testing.T) {
		c := newCase(t)
		recs := captureSegments(t, c, map[int]int{1: 2, 2: 2})
		s := openSession(t, c, recs[1].ID)
		if len(s.Segments) != 2 {
			t.Errorf("segments = %d", len(s.Segments))
		}
	})
}

func TestParentSegments(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "x"})), 3)
	s := openSession(t, c, recs[1].ID)
	refs := s.ParentSegments()
	if len(refs) != 3 {
		t.Fatalf("ParentSegments = %+v", refs)
	}
	for i, r := range recs {
		if refs[i].ID != r.ID || refs[i].SHA256 != r.SHA256 {
			t.Errorf("ref %d = %+v, want %s/%s", i, refs[i], r.ID, r.SHA256)
		}
		if r.Source.Segments != 3 {
			t.Errorf("record %d Segments = %d, want 3", i, r.Source.Segments)
		}
	}
	single := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "x"})), 1)
	if got := openSession(t, c, single[0].ID).ParentSegments(); got != nil {
		t.Errorf("single-segment ParentSegments = %+v, want nil", got)
	}
}

// fakeFS is a hand-built tree for exercising Walk-based lookups.
type fakeFS struct {
	root filesys.Entry
	kids map[string][]filesys.Entry
	errs map[string]error
}

func (f *fakeFS) Info() filesys.Info  { return filesys.Info{Type: "fake"} }
func (f *fakeFS) Root() filesys.Entry { return f.root }
func (f *fakeFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	if err := f.errs[d.ID]; err != nil {
		return nil, err
	}
	return f.kids[d.ID], nil
}

func (f *fakeFS) Lookup(string) (filesys.Entry, error)     { return filesys.Entry{}, filesys.ErrNotFound }
func (f *fakeFS) Open(filesys.Entry) (filesys.File, error) { return nil, filesys.ErrUnsupported }
func (f *fakeFS) Unallocated() ([]filesys.Run, error)      { return nil, nil }

func TestLookupByIDReportsUnreadableDirectories(t *testing.T) {
	dir := func(name, id string) filesys.Entry { return filesys.Entry{Name: name, ID: id, Type: filesys.TypeDir} }
	fsys := &fakeFS{
		root: dir("", "r"),
		kids: map[string][]filesys.Entry{
			"r": {dir("a", "d1"), dir("b", "d1"), dir("c", "d2"), {Name: "f", ID: "f1", Type: filesys.TypeFile}},
		},
		errs: map[string]error{"d2": &filesys.CorruptError{Structure: "dir", Offset: 7, Reason: "bad"}},
	}
	var s examine.Session
	// /b repeats directory d1 (a cycle) and /c cannot be read.
	_, _, err := s.Lookup(fsys, "id:zzz")
	if !errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), "not found; 2 directories could not be read (first: ") {
		t.Fatalf("Lookup(id:zzz) = %v", err)
	}
	if !strings.Contains(err.Error(), "/b") && !strings.Contains(err.Error(), "/c") {
		t.Errorf("error does not name a path: %v", err)
	}
	// An id that is present is still found despite the unreadable directories.
	if _, p, err := s.Lookup(fsys, "id:f1"); err != nil || p != "/f" {
		t.Errorf("Lookup(id:f1) = %q, %v", p, err)
	}
	// With no problem the plain not-found error is returned.
	clean := &fakeFS{root: dir("", "r"), kids: map[string][]filesys.Entry{"r": {dir("a", "d1")}}}
	if _, _, err := s.Lookup(clean, "id:zzz"); !errors.Is(err, filesys.ErrNotFound) || strings.Contains(err.Error(), "could not be read") {
		t.Errorf("clean tree error = %v", err)
	}
}

func TestLookupByIDPrefersLiveOverDeleted(t *testing.T) {
	file := func(name string, deleted bool) filesys.Entry {
		return filesys.Entry{Name: name, ID: "x", Type: filesys.TypeFile, Deleted: deleted}
	}
	for name, kids := range map[string][]filesys.Entry{
		"deleted first": {file("a", true), file("b", false)},
		"live first":    {file("a", false), file("b", true)},
	} {
		t.Run(name, func(t *testing.T) {
			fsys := &fakeFS{root: filesys.Entry{ID: "r", Type: filesys.TypeDir}, kids: map[string][]filesys.Entry{"r": kids}}
			var s examine.Session
			e, p, err := s.Lookup(fsys, "id:x")
			if err != nil || e.Deleted {
				t.Fatalf("Lookup = %+v, %q, %v; want the live entry", e, p, err)
			}
		})
	}
	// Only a deleted entry: it is returned, flagged deleted.
	fsys := &fakeFS{root: filesys.Entry{ID: "r", Type: filesys.TypeDir}, kids: map[string][]filesys.Entry{"r": {file("a", true)}}}
	var s examine.Session
	if e, _, err := s.Lookup(fsys, "id:x"); err != nil || !e.Deleted {
		t.Errorf("deleted-only Lookup = %+v, %v", e, err)
	}
}

// ewfFile writes an artifact that starts with the EWF v1 signature.
func ewfArtifact(t *testing.T, c *evidence.Case) evidence.ManifestRecord {
	t.Helper()
	data := append([]byte("EVF\x09\x0d\x0a\xff\x00"), make([]byte, 1024)...)
	rec, err := c.Capture("img", "acq", "image/x.E01", evidence.Source{Kind: "import", DeviceID: "img", Segment: 1, Segments: 1},
		func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

type stubImage struct {
	panicRead bool
	closed    bool
}

func (s *stubImage) ReadAt([]byte, int64) (int, error) {
	if s.panicRead {
		panic("boom in stub ReadAt")
	}
	return 0, io.EOF
}
func (s *stubImage) Size() int64          { return 1 << 20 }
func (s *stubImage) SectorSize() int      { return 512 }
func (s *stubImage) Format() string       { return "ewf" }
func (s *stubImage) Metadata() []image.KV { return nil }
func (s *stubImage) Close() error         { s.closed = true; return nil }

func TestOpenRecoversContainerAndTablePanics(t *testing.T) {
	t.Run("container opener panics", func(t *testing.T) {
		c := newCase(t)
		rec := ewfArtifact(t, c)
		image.RegisterEWF(func([]*os.File) (image.Image, error) { panic("boom in opener") })
		t.Cleanup(func() { image.RegisterEWF(nil) })
		_, err := examine.Open(c, rec.ID, mtfsOpts())
		requireCorrupt(t, "Open", err)
		// Every file handle was released: the artifact can be removed even on Windows.
		if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
			t.Errorf("artifact file still held open: %v", err)
		}
	})
	t.Run("partition table reader panics", func(t *testing.T) {
		c := newCase(t)
		rec := ewfArtifact(t, c)
		stub := &stubImage{panicRead: true}
		image.RegisterEWF(func(files []*os.File) (image.Image, error) {
			for _, f := range files {
				_ = f.Close()
			}
			return stub, nil
		})
		t.Cleanup(func() { image.RegisterEWF(nil) })
		_, err := examine.Open(c, rec.ID, mtfsOpts())
		requireCorrupt(t, "Open", err)
		if !stub.closed {
			t.Error("image was not closed after the panic")
		}
	})
}

func TestInfoShowsProbePanicInPartitionError(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(make([]byte, 2048)), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{
		{Name: "angry", Probe: func(io.ReaderAt, int64) bool { panic("boom in probe") }, Open: fstest.Open},
		{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d", len(info.Partitions))
	}
	p := info.Partitions[0]
	if p.FSType != "" || !strings.Contains(p.Error, "angry probe panicked") || !strings.Contains(p.Error, "boom in probe") {
		t.Errorf("partition = %+v, want the probe panic in Error", p)
	}
}

func TestInfoKeepsProbePanicNoteWhenLaterDriverMatches(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a.txt": "hi"})), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{
		{Name: "angry", Probe: func(io.ReaderAt, int64) bool { panic("boom in probe") }, Open: fstest.Open},
		{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d", len(info.Partitions))
	}
	p := info.Partitions[0]
	if p.FSType != "mtfs" || p.FSInfo == nil {
		t.Fatalf("partition = %+v, want the later driver's filesystem despite the earlier probe panic", p)
	}
	if !strings.Contains(p.Error, "angry probe panicked") || !strings.Contains(p.Error, "boom in probe") {
		t.Errorf("Error = %q, want the earlier driver's probe panic kept as a note", p.Error)
	}
	// The same filesystem must be reachable for ls/extract, not only shown.
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatalf("FS(-1) = %v", err)
	}
	if _, err := fsys.Lookup("/a.txt"); err != nil {
		t.Errorf("Lookup = %v", err)
	}
}

// panicInfoFS is a filesystem whose Info panics, so wrapping it fails.
type panicInfoFS struct{ filesys.FileSystem }

func (panicInfoFS) Info() filesys.Info { panic("boom in info") }

func TestInfoKeepsFSTypeWhenWrappingPanics(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a.txt": "hi"})), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "mtfs", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			f, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return panicInfoFS{f}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := s.Info().Partitions[0]
	if p.FSType != "mtfs" || p.FSInfo != nil || !strings.Contains(p.Error, "boom in info") {
		t.Errorf("partition = %+v, want FSType mtfs with the panic in Error", p)
	}
}

func TestInfoDriversWithDuplicateNamesAreToldApartByPosition(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a.txt": "hi"})), 1)
	var panicProbes int
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{
		{Name: "x", Probe: func(io.ReaderAt, int64) bool { return false }, Open: fstest.Open},
		{Name: "x", Probe: func(io.ReaderAt, int64) bool { panicProbes++; panic("boom in probe") }, Open: fstest.Open},
		{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := s.Info().Partitions[0]
	if p.FSType != "mtfs" || p.FSInfo == nil {
		t.Fatalf("partition = %+v, want the third driver's filesystem", p)
	}
	if n := strings.Count(p.Error, "x probe panicked"); n != 1 {
		t.Errorf("Error = %q has the probe-panic note %d times, want once", p.Error, n)
	}
	if panicProbes != 1 {
		t.Errorf("panicking Probe ran %d times, want 1", panicProbes)
	}
}
