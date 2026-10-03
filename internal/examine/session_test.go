package examine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/rbenzing/minutiae/internal/volume"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

const (
	linuxType = "0fc63daf-8483-4772-8e79-3d69d8477de4"
	firstLBA  = 40
	gapLBA    = 8
)

func newCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func mtfsOpts() examine.Options {
	return examine.Options{Drivers: []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: fstest.Open}}}
}

func mtfsImage(files map[string]string) []byte {
	var nodes []fstest.Node
	for p, d := range files {
		nodes = append(nodes, fstest.Node{Path: p, Data: []byte(d)})
	}
	return fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})
}

// disk lays the contents out as consecutive GPT partitions (numbered from 1)
// and returns the disk image.
func disk(contents ...[]byte) []byte {
	type placed struct {
		lba uint64
		b   []byte
	}
	var parts []volumetest.Part
	var ps []placed
	lba := uint64(firstLBA)
	for i, b := range contents {
		sectors := uint64(len(b)+511) / 512
		parts = append(parts, volumetest.Part{
			StartLBA: lba, Sectors: sectors, TypeGUID: linuxType,
			GUID: "00000000-0000-0000-0000-00000000000" + string(rune('1'+i)), Name: "p",
		})
		ps = append(ps, placed{lba, b})
		lba += sectors + gapLBA
	}
	img := volumetest.GPT(512, lba+40, "11111111-2222-3333-4444-555555555555", parts)
	for _, p := range ps {
		copy(img[p.lba*512:], p.b)
	}
	return img
}

// writeSegments splits data into n files in a temp dir and returns their paths.
func writeSegments(t *testing.T, data []byte, n int) []string {
	t.Helper()
	dir := t.TempDir()
	per := (len(data) + n - 1) / n
	var paths []string
	for i := 0; i < n; i++ {
		lo, hi := min(i*per, len(data)), min((i+1)*per, len(data))
		p := filepath.Join(dir, "disk.00"+string(rune('1'+i)))
		if err := os.WriteFile(p, data[lo:hi], 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

func importImage(t *testing.T, c *evidence.Case, data []byte, segments int) []evidence.ManifestRecord {
	t.Helper()
	recs, err := examine.Import(context.Background(), c, "img", writeSegments(t, data, segments), nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return recs
}

func openSession(t *testing.T, c *evidence.Case, ref string) *examine.Session {
	t.Helper()
	s, err := examine.Open(c, ref, mtfsOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenSplitImportReassembles(t *testing.T) {
	c := newCase(t)
	data := disk(mtfsImage(map[string]string{"/a.txt": "hello"}), make([]byte, 4096))
	recs := importImage(t, c, data, 3)
	s := openSession(t, c, recs[0].ID)
	if s.Table.Scheme != "gpt" {
		t.Fatalf("Scheme = %q, want gpt", s.Table.Scheme)
	}
	want, err := volume.Read(bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Table.Partitions) != 2 || len(want.Partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(s.Table.Partitions))
	}
	for i, p := range s.Table.Partitions {
		if p != want.Partitions[i] {
			t.Errorf("partition %d = %+v, want %+v", i, p, want.Partitions[i])
		}
	}
	if s.Image.Format() != "split-raw" || s.Image.Size() != int64(len(data)) || len(s.Segments) != 3 {
		t.Errorf("format %q size %d segments %d", s.Image.Format(), s.Image.Size(), len(s.Segments))
	}
	if s.Parent.ID != recs[0].ID {
		t.Errorf("Parent = %s, want segment 1 %s", s.Parent.ID, recs[0].ID)
	}
	for i, seg := range s.Segments {
		if seg.ID != recs[i].ID {
			t.Errorf("Segments[%d] = %s, want %s", i, seg.ID, recs[i].ID)
		}
	}
	// Referencing any other segment resolves to the same import.
	s2 := openSession(t, c, recs[2].ID)
	if s2.Parent.ID != recs[0].ID || len(s2.Segments) != 3 {
		t.Errorf("open by segment 3: parent %s segments %d", s2.Parent.ID, len(s2.Segments))
	}
	buf := make([]byte, len(data))
	if n, err := s.Image.ReadAt(buf, 0); n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(buf, data) {
		t.Errorf("reassembled image differs (n=%d err=%v)", n, err)
	}
}

func TestOpenByPath(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "x"})), 1)
	s := openSession(t, c, recs[0].Path)
	if s.Parent.ID != recs[0].ID {
		t.Errorf("Parent = %s, want %s", s.Parent.ID, recs[0].ID)
	}
	if _, err := examine.Open(c, "no-such-artifact", mtfsOpts()); !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Errorf("unknown ref error = %v, want ErrUnknownArtifact", err)
	}
}

func TestOpenMissingSegmentIsIntegrityError(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "x"})), 3)
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(recs[1].Path))); err != nil {
		t.Fatal(err)
	}
	if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Open = %v, want ErrIntegrity", err)
	}
}

func TestOpenSegmentGapIsIntegrityError(t *testing.T) {
	c := newCase(t)
	data := disk(mtfsImage(map[string]string{"/a": "x"}))
	acq := evidence.NewAcquisitionID(time.Now())
	for _, seg := range []int{1, 3} {
		_, err := c.Capture("img", acq, "image/s"+string(rune('0'+seg)), evidence.Source{Kind: "import", DeviceID: "img", Segment: seg, Segments: 3},
			func(w io.Writer) error { _, err := w.Write(data); return err })
		if err != nil {
			t.Fatal(err)
		}
	}
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Open = %v, want ErrIntegrity for segments 1,3", err)
	}
}

func TestOpenSizeMismatchIsIntegrityError(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "x"})), 1)
	if err := os.WriteFile(filepath.Join(c.Dir, filepath.FromSlash(recs[0].Path)), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Open = %v, want ErrIntegrity", err)
	}
}

func TestOpenSingleArtifactWithoutSegment(t *testing.T) {
	c := newCase(t)
	data := disk(mtfsImage(map[string]string{"/a": "x"}))
	rec, err := c.Capture("dev", "acq", "partitions/userdata.img", evidence.Source{Kind: "partition", DeviceID: "dev", Partition: "userdata"},
		func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		t.Fatal(err)
	}
	s := openSession(t, c, rec.ID)
	if len(s.Segments) != 1 || s.Parent.ID != rec.ID || s.Table.Scheme != "gpt" {
		t.Errorf("segments %d parent %s scheme %s", len(s.Segments), s.Parent.ID, s.Table.Scheme)
	}
}

func TestInfoReportsPartitionsAndFS(t *testing.T) {
	c := newCase(t)
	data := disk(mtfsImage(map[string]string{"/a": "x"}), make([]byte, 4096))
	recs := importImage(t, c, data, 1)
	s := openSession(t, c, recs[0].ID)
	info := s.Info()
	if info.ParentID != recs[0].ID || info.Path != recs[0].Path || info.SHA256 != recs[0].SHA256 || info.Incomplete {
		t.Errorf("identity fields wrong: %+v", info)
	}
	if info.Format != "raw" || info.Size != int64(len(data)) || info.SectorSize != 512 || info.Scheme != "gpt" || info.DiskGUID == "" {
		t.Errorf("image fields wrong: %+v", info)
	}
	if len(info.Partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(info.Partitions))
	}
	p1, p2 := info.Partitions[0], info.Partitions[1]
	if p1.FSType != "mtfs" || p1.FSInfo == nil || p1.FSInfo.Label != "L" || p1.Error != "" {
		t.Errorf("partition 1 = %+v", p1)
	}
	if p2.FSType != "" || p2.FSInfo != nil || p2.Error == "" {
		t.Errorf("partition 2 = %+v, want unrecognized with an error text", p2)
	}
	if len(info.Unallocated) == 0 {
		t.Error("no unallocated runs")
	}
	if len(info.Metadata) == 0 {
		t.Error("no image metadata")
	}
}

func TestInfoFlagsIncompleteParent(t *testing.T) {
	c := newCase(t)
	data := disk(mtfsImage(map[string]string{"/a": "x"}))
	rec, err := c.Capture("img", "acq", "image/x", evidence.Source{Kind: "import", DeviceID: "img", Segment: 1, Segments: 1},
		func(w io.Writer) error {
			if _, err := w.Write(data); err != nil {
				return err
			}
			return errors.New("interrupted")
		})
	if err == nil || !rec.Incomplete {
		t.Fatalf("setup: err=%v incomplete=%v", err, rec.Incomplete)
	}
	s := openSession(t, c, rec.ID)
	info := s.Info()
	if !info.Incomplete {
		t.Error("Info.Incomplete = false for an incomplete parent")
	}
	found := false
	for _, w := range info.Warnings {
		found = found || strings.Contains(w, "incomplete")
	}
	if !found {
		t.Errorf("no incomplete warning in %v", info.Warnings)
	}
}

func TestFSAutoSelectsSinglePartition(t *testing.T) {
	c := newCase(t)
	one := importImage(t, c, disk(make([]byte, 2048), mtfsImage(map[string]string{"/a.txt": "hi"})), 1)
	s := openSession(t, c, one[0].ID)
	fsys, part, err := s.FS(-1)
	if err != nil {
		t.Fatalf("FS(-1): %v", err)
	}
	if part.Index != 2 || part != s.Table.Partitions[1] {
		t.Errorf("picked %+v, want the MTFS partition %+v", part, s.Table.Partitions[1])
	}
	if _, err := fsys.Lookup("/a.txt"); err != nil {
		t.Errorf("Lookup: %v", err)
	}
	if p, err := s.Partition(-1); err != nil || p != part {
		t.Errorf("Partition(-1) = %+v, %v", p, err)
	}
	if p, err := s.Partition(1); err != nil || p.Index != 1 {
		t.Errorf("Partition(1) = %+v, %v", p, err)
	}
	// Explicit index returns the cached filesystem.
	f2, _, err := s.FS(part.Index)
	if err != nil || f2 != fsys {
		t.Errorf("FS(%d) = %v, %v; want the cached filesystem", part.Index, f2, err)
	}

	two := importImage(t, c, disk(mtfsImage(map[string]string{"/a": "1"}), mtfsImage(map[string]string{"/b": "2"})), 1)
	s2 := openSession(t, c, two[0].ID)
	if _, _, err := s2.FS(-1); err == nil || !strings.Contains(err.Error(), "1, 2") {
		t.Errorf("FS(-1) with two candidates = %v, want an error listing \"1, 2\"", err)
	}
	if _, err := s2.Partition(-1); err == nil || !strings.Contains(err.Error(), "1, 2") {
		t.Errorf("Partition(-1) = %v", err)
	}

	none := importImage(t, c, disk(make([]byte, 2048)), 1)
	s3 := openSession(t, c, none[0].ID)
	if _, _, err := s3.FS(-1); err == nil {
		t.Error("FS(-1) with no recognized filesystem succeeded")
	}
	if _, _, err := s3.FS(1); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("FS(1) on zeros = %v, want ErrUnsupported", err)
	}
	if _, _, err := s3.FS(9); err == nil {
		t.Error("FS(9) on a missing partition succeeded")
	}
}

func TestFSWholeImageWithoutTable(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, mtfsImage(map[string]string{"/a": "x"}), 1)
	s := openSession(t, c, recs[0].ID)
	if s.Table.Scheme != "none" {
		t.Fatalf("Scheme = %q", s.Table.Scheme)
	}
	fsys, part, err := s.FS(-1)
	if err != nil || part.Index != 0 || fsys == nil {
		t.Fatalf("FS(-1) = %v, %+v, %v", fsys, part, err)
	}
}

// panicFS panics in the chosen method.
type panicFS struct {
	filesys.FileSystem
	where string
	calls int
}

func (p *panicFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	if p.where == "ReadDir" {
		panic("boom in ReadDir")
	}
	return p.FileSystem.ReadDir(d)
}

func (p *panicFS) Lookup(path string) (filesys.Entry, error) {
	if p.where == "Lookup" {
		panic("boom in Lookup")
	}
	return p.FileSystem.Lookup(path)
}

func (p *panicFS) Unallocated() ([]filesys.Run, error) {
	if p.where == "Unallocated" {
		panic("boom in Unallocated")
	}
	return p.FileSystem.Unallocated()
}

func (p *panicFS) Info() filesys.Info {
	p.calls++
	if p.where == "Info" && p.calls > 1 { // the first call is the snapshot taken at open
		panic("boom in Info")
	}
	return p.FileSystem.Info()
}

func (p *panicFS) Open(e filesys.Entry) (filesys.File, error) {
	if p.where == "Open" {
		panic("boom in Open")
	}
	f, err := p.FileSystem.Open(e)
	if err != nil {
		return nil, err
	}
	return &panicFile{File: f, where: p.where}, nil
}

type panicFile struct {
	filesys.File
	where string
}

func (f *panicFile) ReadAt(b []byte, off int64) (int, error) {
	if f.where == "ReadAt" {
		panic("boom in ReadAt")
	}
	return f.File.ReadAt(b, off)
}

func (f *panicFile) Size() int64 {
	if f.where == "Size" {
		panic("boom in Size")
	}
	return f.File.Size()
}

func (f *panicFile) Runs() []filesys.Run {
	if f.where == "Runs" {
		panic("boom in Runs")
	}
	return f.File.Runs()
}

func panicSession(t *testing.T, where string) filesys.FileSystem {
	t.Helper()
	c := newCase(t)
	recs := importImage(t, c, disk(mtfsImage(map[string]string{"/a.txt": "content"})), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "panicky", Probe: fstest.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			f, err := fstest.Open(r, size)
			if err != nil {
				return nil, err
			}
			return &panicFS{FileSystem: f, where: where}, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	return fsys
}

func requireCorrupt(t *testing.T, what string, err error) {
	t.Helper()
	var ce *filesys.CorruptError
	switch {
	case !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt):
		t.Errorf("%s error = %v, want *filesys.CorruptError", what, err)
	case !strings.Contains(ce.Reason, "boom"):
		t.Errorf("%s reason %q does not carry the panic value", what, ce.Reason)
	}
}

func TestSessionRecoversFSPanic(t *testing.T) {
	t.Run("ReadDir", func(t *testing.T) {
		fsys := panicSession(t, "ReadDir")
		_, err := fsys.ReadDir(fsys.Root())
		requireCorrupt(t, "ReadDir", err)
	})
	t.Run("Lookup", func(t *testing.T) {
		fsys := panicSession(t, "Lookup")
		_, err := fsys.Lookup("/a.txt")
		requireCorrupt(t, "Lookup", err)
	})
	t.Run("Unallocated", func(t *testing.T) {
		fsys := panicSession(t, "Unallocated")
		_, err := fsys.Unallocated()
		requireCorrupt(t, "Unallocated", err)
	})
	t.Run("Open", func(t *testing.T) {
		fsys := panicSession(t, "Open")
		e, err := fsys.Lookup("/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fsys.Open(e)
		requireCorrupt(t, "Open", err)
	})
	t.Run("ReadAt", func(t *testing.T) {
		fsys := panicSession(t, "ReadAt")
		e, err := fsys.Lookup("/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Fatal(err)
		}
		n, err := f.ReadAt(make([]byte, 4), 0)
		if n != 0 {
			t.Errorf("n = %d", n)
		}
		requireCorrupt(t, "ReadAt", err)
	})
	t.Run("Runs", func(t *testing.T) {
		fsys := panicSession(t, "Runs")
		e, err := fsys.Lookup("/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fsys.Open(e)
		requireCorrupt(t, "Open (Runs panic)", err)
	})
	t.Run("Info", func(t *testing.T) {
		fsys := panicSession(t, "Info")
		info := fsys.Info() // last good snapshot plus a warning, no panic
		warned := false
		for _, w := range info.Warnings {
			warned = warned || strings.Contains(w, "boom in Info")
		}
		if info.Type != "mtfs" || !warned {
			t.Errorf("Info after panic = %+v, want the snapshot with a panic warning", info)
		}
	})
	t.Run("Size", func(t *testing.T) {
		fsys := panicSession(t, "Size")
		e, err := fsys.Lookup("/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fsys.Open(e)
		requireCorrupt(t, "Open (Size panic)", err)
	})
}

func TestLookupByID(t *testing.T) {
	c := newCase(t)
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/dir/live.txt", Data: []byte("live")},
		{Path: "/dir/gone.txt", Data: []byte("gone"), Deleted: true},
	}})
	recs := importImage(t, c, disk(img), 1)
	s := openSession(t, c, recs[0].ID)
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := fsys.Lookup("/dir")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := fsys.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sawDeleted := false
	for _, k := range kids {
		e, p, err := s.Lookup(fsys, "id:"+k.ID)
		if err != nil {
			t.Errorf("Lookup(id:%s): %v", k.ID, err)
			continue
		}
		if e.ID != k.ID || e.Deleted != k.Deleted || p != "/dir/"+k.Name {
			t.Errorf("Lookup(id:%s) = %+v, %q", k.ID, e, p)
		}
		sawDeleted = sawDeleted || (k.Deleted && e.Deleted)
	}
	if !sawDeleted {
		t.Error("deleted entry was not found by id")
	}
	e, p, err := s.Lookup(fsys, "/dir/live.txt")
	if err != nil || p != "/dir/live.txt" || e.Name != "live.txt" {
		t.Errorf("Lookup(path) = %+v, %q, %v", e, p, err)
	}
	if _, _, err := s.Lookup(fsys, "id:nope"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(id:nope) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Lookup(fsys, "/missing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/missing) = %v, want ErrNotFound", err)
	}
	if _, p, err := s.Lookup(fsys, "id:"+fsys.Root().ID); err != nil || p != "/" {
		t.Errorf("Lookup(root id) = %q, %v", p, err)
	}
}

func fileState(t *testing.T, p string) (string, int64) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256hex(b), st.ModTime().UnixNano()
}

func TestExamineNeverModifiesSource(t *testing.T) {
	c := newCase(t)
	img := mtfsImage(map[string]string{"/a.txt": "hello", "/d/b.txt": strings.Repeat("z", 3000)})
	recs := importImage(t, c, disk(img, make([]byte, 1024)), 2)
	type state struct {
		sum string
		mt  int64
	}
	before := map[string]state{}
	var paths []string
	for _, r := range recs {
		p := filepath.Join(c.Dir, filepath.FromSlash(r.Path))
		paths = append(paths, p)
		sum, mt := fileState(t, p)
		before[p] = state{sum, mt}
	}

	s, err := examine.Open(c, recs[0].ID, mtfsOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Info()
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	err = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
		if err != nil || e.Type != filesys.TypeFile {
			return nil
		}
		f, err := fsys.Open(e)
		if err != nil {
			return err
		}
		read++
		_, err = io.Copy(io.Discard, io.NewSectionReader(f, 0, f.Size()))
		return err
	})
	if err != nil || read != 2 {
		t.Fatalf("walk/read: %v (read %d files)", err, read)
	}
	if _, _, err := s.Lookup(fsys, "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		sum, mt := fileState(t, p)
		if sum != before[p].sum || mt != before[p].mt {
			t.Errorf("%s changed: hash equal=%v mtime equal=%v", p, sum == before[p].sum, mt == before[p].mt)
		}
	}
}
