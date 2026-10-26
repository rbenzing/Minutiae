package examine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// ---- helpers -------------------------------------------------------------------------------------------

const slackTestCS = 1024 // bytes per cluster of the test images (2 sectors)

func slackFill(n int64) []byte { return bytes.Repeat([]byte{'x'}, int(n)) }

// slackFAT is a FAT16 image with the location of its entries.
type slackFAT struct {
	img   []byte
	g     fattest.Geometry
	built *fattest.Built
}

func newSlackFAT(files []fattest.File, secPerClus int) *slackFAT {
	o := fattest.Options{Type: 16, SecPerClus: secPerClus}
	img, built := fattest.BuildLayout(o, files)
	return &slackFAT{img: img, g: built.Geometry, built: built}
}

func (f *slackFAT) cs() int64 { return int64(f.g.SecPerClus * f.g.SectorSize) }

// tail returns the image offset and length of the tail of the last cluster of the file at path.
func (f *slackFAT) tail(path string, deleted bool, size int64) (off, n int64) {
	e := f.built.Find(path, deleted)
	last := e.Clusters[len(e.Clusters)-1]
	r := size % f.cs()
	return f.g.ClusterOffset(last) + r, f.cs() - r
}

// patchTail writes a non-uniform pattern (at most 64 bytes) at the start of the tail and returns the tail.
func (f *slackFAT) patchTail(path string, deleted bool, size int64) (off, n int64) {
	off, n = f.tail(path, deleted, size)
	for i := range min(n, 64) {
		f.img[off+i] = byte(0xA0 + i%7)
	}
	return off, n
}

func newSlackCase(t *testing.T) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "C", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func slackSessionOn(t *testing.T, c *evidence.Case, img []byte, opts Options) *Session {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.bin")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := Import(context.Background(), c, "img", []string{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(c, recs[0].ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func slackSession(t *testing.T, img []byte, opts Options) *Session {
	t.Helper()
	return slackSessionOn(t, newSlackCase(t), img, opts)
}

// slackPlanOf plans partition -1 of s with the given options.
func slackPlanOf(t *testing.T, s *Session, o SlackOptions) *slackPlan {
	t.Helper()
	fsys, part, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBudget(evidence.MaxRecoveredRuns, o.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.planSlack(context.Background(), o, fsys, part, b)
	if err != nil {
		t.Fatalf("planSlack: %v", err)
	}
	checkAddsUp(t, p)
	return p
}

// checkAddsUp is the 3F counting identity: every file the walk saw ends in exactly one counted outcome.
func checkAddsUp(t *testing.T, p *slackPlan) {
	t.Helper()
	if got := p.fileOutcomes(); got != p.files {
		t.Errorf("files = %d but exported %d + uniform %d + skipped %v (volume-level %d) = %d", p.files, len(p.file), p.uniformSegs, p.skipped, p.volCounted, got)
	}
}

func fatDriver(wrap func(*fat.FS) filesys.FileSystem) Options {
	return Options{Drivers: []detect.Driver{{
		Name:  "slackfake",
		Probe: fat.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			f, err := fat.Open(r, size)
			if err != nil {
				return nil, err
			}
			return wrap(f), nil
		},
	}}}
}

// slackFakeFS wraps a real FAT reader and lets a test make its answers lie.
type slackFakeFS struct {
	filesys.FileSystem
	readDir func(dir filesys.Entry, kids []filesys.Entry, err error) ([]filesys.Entry, error)
	open    func(e filesys.Entry, f filesys.File) (filesys.File, error)
	unalloc func([]filesys.Run) []filesys.Run
	info    func(filesys.Info) filesys.Info
}

func (f *slackFakeFS) ReadDir(dir filesys.Entry) ([]filesys.Entry, error) {
	kids, err := f.FileSystem.ReadDir(dir)
	if f.readDir != nil {
		return f.readDir(dir, kids, err)
	}
	return kids, err
}

func (f *slackFakeFS) Open(e filesys.Entry) (filesys.File, error) {
	inner, err := f.FileSystem.Open(e)
	if err != nil || f.open == nil {
		return inner, err
	}
	return f.open(e, inner)
}

func (f *slackFakeFS) Unallocated() ([]filesys.Run, error) {
	r, err := f.FileSystem.Unallocated()
	if f.unalloc != nil {
		r = f.unalloc(r)
	}
	return r, err
}

func (f *slackFakeFS) Info() filesys.Info {
	in := f.FileSystem.Info()
	if f.info != nil {
		in = f.info(in)
	}
	return in
}

func fakeOpts(f *slackFakeFS) Options {
	return fatDriver(func(r *fat.FS) filesys.FileSystem { f.FileSystem = r; return f })
}

// Files with lying answers. Embedding the filesys.File interface hides every optional method of the real file.
type allocFile struct {
	filesys.File
	alloc []filesys.Run
}

func (f allocFile) AllocatedRuns() []filesys.Run { return slices.Clone(f.alloc) }

type panicAllocFile struct{ filesys.File }

func (panicAllocFile) AllocatedRuns() []filesys.Run { panic("boom") }

type encFile struct {
	filesys.File
	alloc []filesys.Run
}

func (f encFile) AllocatedRuns() []filesys.Run { return slices.Clone(f.alloc) }
func (encFile) Encrypted() bool                { return true }

func (f *slackFAT) segIn(p *slackPlan, path string) (SlackSegment, bool) {
	for _, s := range p.file {
		if s.FSPath == path {
			return s, true
		}
	}
	return SlackSegment{}, false
}

func overlapsAny(p *slackPlan, off, n int64) bool {
	for _, s := range slices.Concat(p.file, p.vol) {
		if s.ImageOffset < off+n && off < s.ImageOffset+s.Length {
			return true
		}
	}
	return false
}

func withObserver(t *testing.T) *[][2]int64 {
	t.Helper()
	var mu sync.Mutex
	var reads [][2]int64
	slackReadObserver = func(off int64, n int) {
		mu.Lock()
		reads = append(reads, [2]int64{off, int64(n)})
		mu.Unlock()
	}
	t.Cleanup(func() { slackReadObserver = nil })
	return &reads
}

func setCap[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// ---- file slack ----------------------------------------------------------------------------------------

func TestPlanSlackFATTails(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(1)},
		{Path: "B.TXT", Data: slackFill(cs - 5)},
		{Path: "C.TXT", Data: slackFill(cs)},
		{Path: "D", Dir: true},
		{Path: "D/E.TXT", Data: slackFill(cs + 1)},
		{Path: "Z.TXT"},
	}, 2)
	sizes := map[string]int64{"A.TXT": 1, "B.TXT": cs - 5, "D/E.TXT": cs + 1}
	type want struct{ off, n int64 }
	wants := map[string]want{}
	for p, sz := range sizes {
		off, n := f.patchTail(p, false, sz)
		wants[p] = want{off, n}
	}
	s := slackSession(t, f.img, Options{})
	fsys, part, _ := s.FS(-1)
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})

	order := []string{"A.TXT", "B.TXT", "D/E.TXT"}
	if len(p.file) != 3 {
		t.Fatalf("segments = %+v", p.file)
	}
	for i, path := range order {
		e, err := fsys.Lookup("/" + path)
		if err != nil {
			t.Fatal(err)
		}
		seg, w := p.file[i], wants[path]
		if seg.FSPath != "/"+path || seg.FSID != e.ID || seg.FileSize != sizes[path] || seg.ImageOffset != part.Start+w.off || seg.Length != w.n || seg.Uniform || seg.PathEscaped {
			t.Errorf("segment %d = %+v, want %s id %s size %d at %d+%d", i, seg, path, e.ID, sizes[path], part.Start+w.off, w.n)
		}
	}
	if p.skipped["exact-fit"] != 1 || p.skipped["no-runs"] != 1 || p.files != 5 {
		t.Errorf("files %d skipped %v", p.files, p.skipped)
	}
	if p.unit != cs || p.fsType != "fat16" {
		t.Errorf("unit %d type %q", p.unit, p.fsType)
	}
}

func TestPlanSlackUniformCounted(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(1)},
		{Path: "B.TXT", Data: slackFill(cs - 5)},
		{Path: "C.TXT", Data: slackFill(cs + 7)},
	}, 2)
	s := slackSession(t, f.img, Options{})
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.uniformSegs != 3 || p.uniformBytes != (cs-1)+5+(cs-7) {
		t.Fatalf("segs %d uniform %d/%d", len(p.file), p.uniformSegs, p.uniformBytes)
	}
	if p.omitted != nil {
		t.Errorf("a complete plan keeps no omitted ranges: %+v", p.omitted)
	}
	q := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true, IncludeZero: true})
	if len(q.file) != 3 || q.uniformSegs != 0 {
		t.Fatalf("include-zero: segs %d uniform %d", len(q.file), q.uniformSegs)
	}
	for _, seg := range q.file {
		if !seg.Uniform || seg.Fill != 0 {
			t.Errorf("segment %+v should be uniform zero", seg)
		}
	}
}

func TestPlanSlackUniformAnyByteValue(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	off, n := f.tail("A.TXT", false, 10)
	for i := range n {
		f.img[off+i] = 0xFF
	}
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.uniformSegs != 1 {
		t.Fatalf("a 0xFF tail is uniform: segs %d uniform %d", len(p.file), p.uniformSegs)
	}
	q := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true, IncludeZero: true})
	if len(q.file) != 1 || !q.file[0].Uniform || q.file[0].Fill != 0xFF {
		t.Fatalf("segments %+v", q.file)
	}
}

func TestPlanSlackSkipsDeletedEntries(t *testing.T) {
	f := newSlackFAT([]fattest.File{
		{Path: "LIVE.TXT", Data: slackFill(10)},
		{Path: "DEL.TXT", Data: slackFill(10), Deleted: true},
	}, 2)
	f.patchTail("LIVE.TXT", false, 10)
	delOff, delN := f.patchTail("DEL.TXT", true, 10)
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 1 || p.file[0].FSPath != "/LIVE.TXT" || p.files != 1 {
		t.Fatalf("plan = %+v files %d", p.file, p.files)
	}
	if overlapsAny(p, delOff, delN) {
		t.Error("the tail of a deleted file was planned")
	}
}

func TestPlanSlackPrefixFileHasNoSegment(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{{Path: "F.BIN", Data: slackFill(3*cs - 10)}}, 2)
	e := f.built.Find("F.BIN", false)
	fo1, _ := f.patchTail("F.BIN", false, 3*cs-10)
	fatSetEOC(f, e.Clusters[1])
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if p.skipped["prefix"] != 1 || len(p.file) != 0 {
		t.Fatalf("skipped %v segs %+v", p.skipped, p.file)
	}
	for _, c := range e.Clusters {
		if overlapsAny(p, f.g.ClusterOffset(c), cs) {
			t.Errorf("cluster %d of the prefix file was planned", c)
		}
	}
	_ = fo1
}

func fatSetEOC(f *slackFAT, c uint32) { fattest.SetFATEntry(f.img, f.g, c, 0xFFFF) }

func TestPlanSlackSharedUnitIsDropped(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "X.BIN", Data: slackFill(3*cs - 10)},
		{Path: "Y.BIN", Data: slackFill(100)},
	}, 2)
	x := f.built.Find("X.BIN", false)
	xLast := x.Clusters[len(x.Clusters)-1]
	f.patchTail("X.BIN", false, 3*cs-10)
	first := xLast
	fattest.PatchEntry(f.img, f.g, f.built.Find("Y.BIN", false), &first, nil)
	// Y's 100 bytes now sit in X's last cluster, whose tail starts at offset 100 of that cluster.
	for i := range 20 {
		f.img[f.g.ClusterOffset(xLast)+100+int64(i)] = byte(1 + i)
	}
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if overlapsAny(p, f.g.ClusterOffset(xLast), cs) {
		t.Errorf("a cross-linked unit was exported: %+v", p.file)
	}
	if p.skipped["shared-unit"] < 1 {
		t.Errorf("skipped = %v", p.skipped)
	}
	named := false
	for _, w := range p.warnings {
		if strings.Contains(w.Path, "X.BIN") || strings.Contains(w.Path, "Y.BIN") {
			named = true
		}
	}
	if !named {
		t.Errorf("no warning names the path: %+v", p.warnings)
	}
}

func TestPlanSlackEqualSizeCrossLinkIsDropped(t *testing.T) {
	cs := int64(slackTestCS)
	sz := 2*cs - 10
	f := newSlackFAT([]fattest.File{
		{Path: "X.BIN", Data: slackFill(sz)},
		{Path: "Y.BIN", Data: slackFill(sz)},
	}, 2)
	x := f.built.Find("X.BIN", false)
	f.patchTail("X.BIN", false, sz)
	first := x.Clusters[0]
	fattest.PatchEntry(f.img, f.g, f.built.Find("Y.BIN", false), &first, nil)
	reads := withObserver(t)
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.skipped["shared-unit"] != 2 {
		t.Fatalf("segs %+v skipped %v", p.file, p.skipped)
	}
	for _, r := range *reads {
		if r[1] <= uniformChunk && r[0] >= f.g.ClusterOffset(x.Clusters[0]) && r[0] < f.g.ClusterOffset(x.Clusters[1])+cs && r[1] != 0 && r[1] < cs {
			t.Errorf("a shared tail was read: %v", r)
		}
	}
}

func TestPlanSlackSegmentOverlapsOtherFileUnits(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "X.BIN", Data: slackFill(cs - 10)}, // one cluster
		{Path: "Y.BIN", Data: slackFill(3*cs - 10)},
	}, 2)
	x := f.built.Find("X.BIN", false)
	y := f.built.Find("Y.BIN", false)
	xOff := f.g.ClusterOffset(x.Clusters[0])
	f.patchTail("X.BIN", false, cs-10)
	yo, yn := f.patchTail("Y.BIN", false, 3*cs-10)
	fake := &slackFakeFS{open: func(e filesys.Entry, in filesys.File) (filesys.File, error) {
		if e.Name != "Y.BIN" {
			return in, nil
		}
		// Y's own tail stays; X's cluster is a middle unit of Y's chain.
		runs := []filesys.Run{{Offset: f.g.ClusterOffset(y.Clusters[0]), Length: cs}, {Offset: xOff, Length: cs}, {Offset: f.g.ClusterOffset(y.Clusters[2]), Length: cs - 10}}
		return multiRunFile{File: in, runs: runs, alloc: filesys.UnitRuns(runs, cs)}, nil
	}}
	s := slackSession(t, f.img, fakeOpts(fake))
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	if overlapsAny(p, xOff, cs) {
		t.Errorf("X's segment overlaps a unit of Y: %+v", p.file)
	}
	if p.skipped["shared-unit"] != 1 {
		t.Errorf("skipped = %v", p.skipped)
	}
	if len(p.file) != 1 || p.file[0].ImageOffset != yo || p.file[0].Length != yn {
		t.Errorf("Y's own tail should remain: %+v", p.file)
	}
}

// multiRunFile reports other runs and allocation than the wrapped file.
type multiRunFile struct {
	filesys.File
	runs, alloc []filesys.Run
}

func (f multiRunFile) Runs() []filesys.Run          { return slices.Clone(f.runs) }
func (f multiRunFile) AllocatedRuns() []filesys.Run { return slices.Clone(f.alloc) }
func (f multiRunFile) Size() int64 {
	var n int64
	for _, r := range f.runs {
		n += r.Length
	}
	return n
}

func TestPlanSlackFreeSpaceIsNeverExported(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	off, n := f.patchTail("A.TXT", false, 10)
	fake := &slackFakeFS{unalloc: func(r []filesys.Run) []filesys.Run {
		return append(slices.Clone(r), filesys.Run{Offset: off - 10, Length: 10 + n})
	}}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.skipped["free-space"] != 1 || len(p.warnings) == 0 {
		t.Fatalf("segs %+v skipped %v warnings %+v", p.file, p.skipped, p.warnings)
	}
}

func TestPlanSlackAllocMismatch(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(2*cs - 10)}}, 2)
	f.patchTail("A.TXT", false, 2*cs-10)
	fake := &slackFakeFS{open: func(_ filesys.Entry, in filesys.File) (filesys.File, error) {
		a := filesys.UnitRuns(in.Runs(), cs)
		a[0].Length -= cs
		return allocFile{File: in, alloc: a}, nil
	}}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.skipped["alloc-mismatch"] != 1 || len(p.warnings) == 0 {
		t.Fatalf("segs %+v skipped %v", p.file, p.skipped)
	}
}

// noAllocFile hides every optional method of the file.
type noAllocFile struct{ f filesys.File }

func (n noAllocFile) ReadAt(p []byte, off int64) (int, error) { return n.f.ReadAt(p, off) }
func (n noAllocFile) Size() int64                             { return n.f.Size() }
func (n noAllocFile) Runs() []filesys.Run                     { return n.f.Runs() }

func TestPlanSlackNoAllocation(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}, {Path: "Z.TXT"}}, 2)
	f.patchTail("A.TXT", false, 10)
	fake := &slackFakeFS{open: func(_ filesys.Entry, in filesys.File) (filesys.File, error) { return noAllocFile{in}, nil }}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.skipped["no-allocation"] != 1 || p.skipped["no-runs"] != 1 {
		t.Fatalf("segs %+v skipped %v", p.file, p.skipped)
	}
}

func TestPlanSlackEncryptedNotExported(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}, {Path: "B.TXT", Data: slackFill(10)}}, 2)
	f.patchTail("A.TXT", false, 10)
	f.patchTail("B.TXT", false, 10)
	fake := &slackFakeFS{
		open: func(e filesys.Entry, in filesys.File) (filesys.File, error) {
			if e.Name == "A.TXT" {
				return encFile{File: in, alloc: filesys.UnitRuns(in.Runs(), slackTestCS)}, nil
			}
			return in, nil
		},
		readDir: func(_ filesys.Entry, kids []filesys.Entry, err error) ([]filesys.Entry, error) {
			for i := range kids {
				if kids[i].Name == "B.TXT" {
					kids[i].Encrypted = true // the listing says so
				}
			}
			return kids, err
		},
	}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if len(p.file) != 0 || p.skipped["encrypted"] != 2 {
		t.Fatalf("segs %+v skipped %v", p.file, p.skipped)
	}
}

func TestPlanSlackEscapesInvalidNames(t *testing.T) {
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(10)}, {Path: "B.TXT", Data: slackFill(10)}, {Path: "C.TXT", Data: slackFill(10)},
	}, 2)
	for _, n := range []string{"A.TXT", "B.TXT", "C.TXT"} {
		f.patchTail(n, false, 10)
	}
	names := map[string]string{"A.TXT": "a\xFF.txt", "B.TXT": `b\c.txt`, "C.TXT": "d\\e\xFF\x00"}
	fake := &slackFakeFS{readDir: func(_ filesys.Entry, kids []filesys.Entry, err error) ([]filesys.Entry, error) {
		for i := range kids {
			if n, ok := names[kids[i].Name]; ok {
				kids[i].Name = n
			}
		}
		return kids, err
	}}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	want := map[string]bool{`/a\xFF.txt`: true, `/b\c.txt`: false, `/d\\e\xFF\x00`: true}
	if len(p.file) != 3 {
		t.Fatalf("segments = %+v", p.file)
	}
	for _, seg := range p.file {
		esc, ok := want[seg.FSPath]
		if !ok || esc != seg.PathEscaped {
			t.Errorf("segment %q escaped=%v unexpected", seg.FSPath, seg.PathEscaped)
		}
	}
}

func TestEscapePathLabel(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/é.txt":  "/plain/é.txt",
		"/a\xFFb":       `/a\xFFb`,
		"/a\x00b":       `/a\x00b`,
		"/a\\b\xC3":     `/a\\b\xC3`,
		"/\xEF\xBF\xBD": "/\xEF\xBF\xBD",
	} {
		got, esc := escapePathLabel(in)
		if got != want || esc != (got != in) {
			t.Errorf("escapePathLabel(%q) = %q,%v want %q", in, got, esc, want)
		}
	}
	// The longest label is four times the longest walk path (every byte escaped).
	long := strings.Repeat("\xFF", filesys.MaxWalkPath)
	if lab, _ := escapePathLabel(long); len(lab) != 4*filesys.MaxWalkPath {
		t.Errorf("label length %d", len(lab))
	}
}

// 3F-C1: the evidence format must accept the longest label the planner can produce.
func TestSlackLabelBoundMatchesEvidenceFormat(t *testing.T) {
	label, _ := escapePathLabel(strings.Repeat("\xFF", filesys.MaxWalkPath))
	var buf bytes.Buffer
	l := evidence.SlackMapLine{Offset: 0, Length: 1, ImageOffset: 0, FSPath: label, PathEscaped: true, FSID: "id", FileSize: 1, Exported: true}
	if err := evidence.WriteSlackMapLine(&buf, evidence.MethodSlackFile, l); err != nil {
		t.Fatalf("the longest planned label is refused by the slack map: %v", err)
	}
	var got []evidence.SlackMapLine
	if err := evidence.ReadSlackRunMap(&buf, evidence.MethodSlackFile, func(l evidence.SlackMapLine) error { got = append(got, l); return nil }); err != nil || len(got) != 1 || got[0].FSPath != label {
		t.Fatalf("read back: %v %d", err, len(got))
	}
}

// The omitted list is bounded by the reader's cap, so an incomplete map stays readable (T7 carry-over).
func TestPlanSlackOmittedCapIsTheReaderCap(t *testing.T) {
	if slackOmittedCap != evidence.MaxRecoveredRuns || slackSegmentCap != evidence.MaxRecoveredRuns {
		t.Fatalf("caps %d/%d, reader cap %d", slackOmittedCap, slackSegmentCap, evidence.MaxRecoveredRuns)
	}
}

func TestPlanSlackOpenPanicIsCounted(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}, {Path: "B.TXT", Data: slackFill(10)}}, 2)
	f.patchTail("A.TXT", false, 10)
	f.patchTail("B.TXT", false, 10)
	fake := &slackFakeFS{open: func(e filesys.Entry, in filesys.File) (filesys.File, error) {
		if e.Name == "A.TXT" {
			return panicAllocFile{in}, nil
		}
		return in, nil
	}}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if p.skipped["open-error"] != 1 || len(p.file) != 1 || p.file[0].FSPath != "/B.TXT" {
		t.Fatalf("skipped %v segs %+v", p.skipped, p.file)
	}
}

func TestPlanSlackHardLinkOnce(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	f.patchTail("A.TXT", false, 10)
	fake := &slackFakeFS{readDir: func(_ filesys.Entry, kids []filesys.Entry, err error) ([]filesys.Entry, error) {
		if len(kids) == 1 && kids[0].Name == "A.TXT" {
			link := kids[0]
			link.Name = "LINK.TXT"
			kids = append(kids, link)
		}
		return kids, err
	}}
	p := slackPlanOf(t, slackSession(t, f.img, fakeOpts(fake)), SlackOptions{Partition: -1, Files: true})
	if p.files != 2 || len(p.file) != 1 || p.skipped["duplicate-id"] != 1 || p.file[0].FSPath != "/A.TXT" {
		t.Fatalf("files %d segs %+v skipped %v", p.files, p.file, p.skipped)
	}
}

// ---- volume slack --------------------------------------------------------------------------------------

func pat(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%7)
	}
	return b
}

func TestVolumeSlackChunks(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	fsEnd := int64(len(f.img))
	img := slices.Concat(f.img, pat(4096, 0x30), make([]byte, 8192), pat(100, 0x50))
	s := slackSession(t, img, Options{})
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Volume: true})
	want := []SlackSegment{{ImageOffset: fsEnd, Length: 4096}, {ImageOffset: fsEnd + 12288, Length: 100}}
	if !reflect.DeepEqual(p.vol, want) {
		t.Fatalf("vol = %+v want %+v", p.vol, want)
	}
	if p.volUniformChunks != 2 || p.volUniformBytes != 8192 || p.omitted != nil || p.volOmitted != nil {
		t.Errorf("uniform %d/%d omitted %v", p.volUniformChunks, p.volUniformBytes, p.volOmitted)
	}
	// two adjacent non-uniform chunks merge
	img2 := slices.Concat(f.img, pat(4096, 0x30), pat(4096, 0x40))
	p2 := slackPlanOf(t, slackSession(t, img2, Options{}), SlackOptions{Partition: -1, Volume: true})
	if len(p2.vol) != 1 || p2.vol[0].Length != 8192 || p2.vol[0].Uniform {
		t.Errorf("merge: %+v", p2.vol)
	}
	// include-zero exports the zero run as one uniform segment
	p3 := slackPlanOf(t, s, SlackOptions{Partition: -1, Volume: true, IncludeZero: true})
	if len(p3.vol) != 3 || !p3.vol[1].Uniform || p3.vol[1].Length != 8192 || p3.vol[1].ImageOffset != fsEnd+4096 || p3.volUniformChunks != 0 {
		t.Errorf("include-zero: %+v", p3.vol)
	}
}

func TestVolumeSlackMergesOnlySameFill(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	img := slices.Concat(f.img, make([]byte, 4096), bytes.Repeat([]byte{0xFF}, 4096))
	p := slackPlanOf(t, slackSession(t, img, Options{}), SlackOptions{Partition: -1, Volume: true, IncludeZero: true})
	if len(p.vol) != 2 || p.vol[0].Fill != 0x00 || p.vol[1].Fill != 0xFF || !p.vol[0].Uniform || !p.vol[1].Uniform {
		t.Fatalf("vol = %+v", p.vol)
	}
	q := slackPlanOf(t, slackSession(t, img, Options{}), SlackOptions{Partition: -1, Volume: true})
	if len(q.vol) != 0 || q.volUniformChunks != 2 || q.volUniformBytes != 8192 {
		t.Fatalf("counted: %+v %d/%d", q.vol, q.volUniformChunks, q.volUniformBytes)
	}
}

func TestVolumeSlackClipsToImage(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	fsEnd := int64(len(f.img))
	img := slices.Concat(f.img, pat(4096, 0x30))
	s := slackSession(t, img, Options{})
	fsys, part, _ := s.FS(-1)
	part.Length += 1 << 20 // the partition claims a megabyte the image does not hold
	b, _ := newBudget(evidence.MaxRecoveredRuns, 0)
	p, err := s.planSlack(context.Background(), SlackOptions{Volume: true}, fsys, part, b)
	if err != nil {
		t.Fatal(err)
	}
	if p.skipped["beyond-image"] != 1 || len(p.vol) != 1 || p.vol[0].ImageOffset != fsEnd || p.vol[0].Length != 4096 {
		t.Fatalf("skipped %v vol %+v", p.skipped, p.vol)
	}
	checkAddsUp(t, p)

	// A filesystem that fills its partition has no volume slack and nothing is counted.
	q := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Volume: true})
	if len(q.vol) != 0 || len(q.skipped) != 0 {
		t.Errorf("plan = %+v %v", q.vol, q.skipped)
	}
}

func TestVolumeSlackSizeBeyondPartitionIsCounted(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	img := slices.Concat(f.img, pat(4096, 0x30))
	fake := &slackFakeFS{info: func(in filesys.Info) filesys.Info { in.Size = int64(len(img)) + 4096; return in }}
	p := slackPlanOf(t, slackSession(t, img, fakeOpts(fake)), SlackOptions{Partition: -1, Volume: true})
	if p.skipped["size-beyond-partition"] != 1 || len(p.vol) != 0 || len(p.warnings) != 1 {
		t.Fatalf("skipped %v vol %+v warnings %+v", p.skipped, p.vol, p.warnings)
	}
	fake2 := &slackFakeFS{info: func(in filesys.Info) filesys.Info { in.Size = 0; return in }}
	q := slackPlanOf(t, slackSession(t, img, fakeOpts(fake2)), SlackOptions{Partition: -1, Volume: true})
	if q.skipped["size-unknown"] != 1 || len(q.vol) != 0 {
		t.Fatalf("skipped %v", q.skipped)
	}
}

func TestPlanSlackMemoryIndependentOfVolumeTail(t *testing.T) {
	f := newSlackFAT([]fattest.File{{Path: "A.TXT", Data: slackFill(10)}}, 2)
	fsEnd := int64(len(f.img))
	const tail = 64 << 20
	img := slices.Concat(f.img, make([]byte, tail))
	s := slackSession(t, img, Options{})
	reads := withObserver(t)
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Volume: true})
	if p.volUniformChunks != tail/SlackVolumeChunk || len(p.vol) != 0 {
		t.Fatalf("chunks %d vol %v", p.volUniformChunks, p.vol)
	}
	var covered int64
	next := fsEnd
	for _, r := range *reads {
		if r[1] > uniformChunk {
			t.Fatalf("a read of %d bytes exceeds the %d-byte buffer", r[1], uniformChunk)
		}
		if r[0] >= fsEnd {
			if r[0] != next {
				t.Fatalf("the volume tail is not read once, in order: read at %d, expected %d", r[0], next)
			}
			next += r[1]
			covered += r[1]
		}
	}
	if covered != tail {
		t.Errorf("volume reads cover %d of %d bytes", covered, tail)
	}
}

// ---- limits, walk errors, determinism ------------------------------------------------------------------

func threeTails(t *testing.T) (*slackFAT, *Session) {
	t.Helper()
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(1)},
		{Path: "B.TXT", Data: slackFill(cs - 5)},
		{Path: "C.TXT", Data: slackFill(cs + 3)},
	}, 2)
	f.patchTail("A.TXT", false, 1)
	f.patchTail("B.TXT", false, cs-5)
	f.patchTail("C.TXT", false, cs+3)
	return f, slackSession(t, f.img, Options{})
}

func TestPlanSlackLimits(t *testing.T) {
	cs := int64(slackTestCS)
	t.Run("max-bytes", func(t *testing.T) {
		_, s := threeTails(t)
		p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true, MaxBytes: cs}) // A's tail is cs-1, B's 5
		if p.limit != "max-bytes" || len(p.file) != 1 || p.file[0].FSPath != "/A.TXT" || p.skipped["limit"] != 2 {
			t.Fatalf("limit %q segs %+v skipped %v", p.limit, p.file, p.skipped)
		}
		if !p.fileIncomplete() {
			t.Error("a limit makes the plan incomplete")
		}
	})
	t.Run("max-segments", func(t *testing.T) {
		_, s := threeTails(t)
		setCap(t, &slackSegmentCap, 1)
		p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
		if p.limit != "max-segments" || len(p.file) != 1 || p.skipped["limit"] != 2 {
			t.Fatalf("limit %q segs %+v skipped %v", p.limit, p.file, p.skipped)
		}
	})
	t.Run("max-path-bytes", func(t *testing.T) {
		_, s := threeTails(t)
		setCap(t, &slackPathBytesCap, 4)
		p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
		if p.limit != "max-path-bytes" || len(p.file) != 0 || p.skipped["limit"] != 3 {
			t.Fatalf("limit %q segs %+v skipped %v", p.limit, p.file, p.skipped)
		}
	})
	t.Run("a limit stops the volume part and counts it", func(t *testing.T) {
		f, _ := threeTails(t)
		s := slackSession(t, slices.Concat(f.img, pat(4096, 1)), Options{})
		p := slackPlanOf(t, s, SlackOptions{Partition: -1, MaxBytes: 1000})
		if p.limit != "max-bytes" || len(p.vol) != 0 || !p.volumeIncomplete() {
			t.Fatalf("limit %q vol %+v", p.limit, p.vol)
		}
	})
}

func TestPlanSlackScanBudget(t *testing.T) {
	cs := int64(slackTestCS)
	f, _ := threeTails(t)
	// A's tail is cs-1 bytes: a budget of one tail leaves no room for the second.
	s := slackSession(t, f.img, Options{Scan: ScanBudget{Bytes: cs - 1}})
	reads := withObserver(t)
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	if p.limit != "max-scan-bytes" || len(p.file) != 1 || p.skipped["limit"] != 2 {
		t.Fatalf("limit %q segs %+v skipped %v", p.limit, p.file, p.skipped)
	}
	if len(*reads) != 1 {
		t.Errorf("reads = %v, want one tail read", *reads)
	}
	// The same budget stops the volume tail.
	img := slices.Concat(f.img, pat(4096, 0x30), pat(4096, 0x40))
	s2 := slackSession(t, img, Options{Scan: ScanBudget{Bytes: 4096}})
	q := slackPlanOf(t, s2, SlackOptions{Partition: -1, Volume: true})
	if q.limit != "max-scan-bytes" || !q.volumeIncomplete() || len(q.vol) != 1 || q.vol[0].Length != 4096 {
		t.Fatalf("limit %q vol %+v", q.limit, q.vol)
	}
}

// fatCutSlack is a FAT16 image whose directory /D has its chain broken after the first cluster.
func fatCutSlack(t *testing.T, patch bool) (*slackFAT, []string) {
	t.Helper()
	files := []fattest.File{{Path: "A.TXT", Data: slackFill(10)}, {Path: "D", Dir: true}, {Path: "D/ALPHA.TXT", Data: slackFill(10)}}
	for i := range 29 {
		files = append(files, fattest.File{Path: fmt.Sprintf("D/F%02d.TXT", i), Data: []byte{1}})
	}
	o := fattest.Options{Type: 16}
	img, built := fattest.BuildLayout(o, files)
	f := &slackFAT{img: img, g: built.Geometry, built: built}
	d := built.Find("D", false)
	if len(d.Clusters) < 2 {
		t.Fatalf("setup: /D has %d clusters", len(d.Clusters))
	}
	for n := range f.g.NumFATs {
		o := int(f.g.FATStart(n))*f.g.SectorSize + int(d.Clusters[0])*2
		binary.LittleEndian.PutUint16(f.img[o:], 0xF000) // the chain leaves the volume
	}
	if patch {
		f.patchTail("A.TXT", false, 10)
		f.patchTail("D/ALPHA.TXT", false, 10)
	}
	return f, nil
}

func TestPlanSlackCutDirectoryCountsWalkError(t *testing.T) {
	f, _ := fatCutSlack(t, true)
	p := slackPlanOf(t, slackSession(t, f.img, Options{}), SlackOptions{Partition: -1, Files: true})
	if p.walkErrors != 1 || !strings.Contains(p.firstWalkError, "/D") {
		t.Fatalf("walk errors %d first %q", p.walkErrors, p.firstWalkError)
	}
	if _, ok := f.segIn(p, "/A.TXT"); !ok {
		t.Errorf("the file before the cut lost its slack: %+v", p.file)
	}
	if _, ok := f.segIn(p, "/D/ALPHA.TXT"); !ok {
		t.Errorf("a file read before the cut lost its slack: %+v", p.file)
	}
	if !p.fileIncomplete() {
		t.Error("a walk error makes the file part incomplete")
	}
}

func TestPlanSlackIncompleteKeepsOmitted(t *testing.T) {
	f, _ := fatCutSlack(t, false) // every tail is zero: uniform
	s := slackSession(t, f.img, Options{})
	p := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	if p.walkErrors != 1 || p.uniformSegs < 2 || len(p.omitted) != p.uniformSegs || p.omittedLost != 0 {
		t.Fatalf("walk errors %d uniform %d omitted %d", p.walkErrors, p.uniformSegs, len(p.omitted))
	}
	if p.omitted[0].FSPath != "/A.TXT" || !p.omitted[0].Uniform || p.omitted[0].Length == 0 {
		t.Errorf("omitted[0] = %+v", p.omitted[0])
	}
	// The list is bounded by its cap; the counters stay exact.
	setCap(t, &slackOmittedCap, 1)
	q := slackPlanOf(t, s, SlackOptions{Partition: -1, Files: true})
	if len(q.omitted) != 1 || q.omittedLost != q.uniformSegs-1 || q.uniformSegs != p.uniformSegs {
		t.Errorf("omitted %d lost %d uniform %d", len(q.omitted), q.omittedLost, q.uniformSegs)
	}
}

func TestPlanSlackIsDeterministic(t *testing.T) {
	f, _ := threeTails(t)
	img := slices.Concat(f.img, pat(4096, 1), make([]byte, 4096))
	s := slackSession(t, img, Options{})
	a := slackPlanOf(t, s, SlackOptions{Partition: -1})
	b := slackPlanOf(t, s, SlackOptions{Partition: -1})
	if !reflect.DeepEqual(a, b) || len(a.file) != 3 || len(a.vol) != 1 {
		t.Fatalf("plans differ or are empty:\n%+v\n%+v", a, b)
	}
}

func TestPlanSlackWritesNothing(t *testing.T) {
	f, _ := threeTails(t)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, slices.Concat(f.img, pat(4096, 1)), Options{})
	audit0, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	man0, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	slackPlanOf(t, s, SlackOptions{Partition: -1})
	audit1, _ := c.ReadAudit()
	man1, _ := c.Manifest()
	if len(audit1) != len(audit0) || !reflect.DeepEqual(man0, man1) {
		t.Errorf("planning wrote: audit %d -> %d, manifest %d -> %d", len(audit0), len(audit1), len(man0), len(man1))
	}
}

// ---- hostile input -------------------------------------------------------------------------------------

// millionFS lists n files whose runs all end in one cluster.
type millionFS struct {
	n    int
	size int64
	kids []filesys.Entry
}

type millionFile struct{}

func (millionFile) ReadAt(_ []byte, _ int64) (int, error) { return 0, io.EOF }
func (millionFile) Size() int64                           { return 100 }
func (millionFile) Runs() []filesys.Run                   { return []filesys.Run{{Offset: 4096, Length: 100}} }

func (millionFile) AllocatedRuns() []filesys.Run { return []filesys.Run{{Offset: 4096, Length: 512}} }

func (m *millionFS) Info() filesys.Info {
	return filesys.Info{Type: "millionfs", BlockSize: 512, Size: m.size}
}
func (m *millionFS) Root() filesys.Entry { return filesys.Entry{ID: "root", Type: filesys.TypeDir} }
func (m *millionFS) ReadDir(d filesys.Entry) ([]filesys.Entry, error) {
	if d.ID != "root" {
		return nil, nil
	}
	if m.kids == nil {
		m.kids = make([]filesys.Entry, m.n)
		for i := range m.kids {
			m.kids[i] = filesys.Entry{Name: fmt.Sprintf("f%07d", i), ID: fmt.Sprintf("f%d", i), Type: filesys.TypeFile, Size: 100}
		}
	}
	return m.kids, nil
}

func (m *millionFS) Lookup(string) (filesys.Entry, error) {
	return filesys.Entry{}, filesys.ErrNotFound
}
func (m *millionFS) Open(filesys.Entry) (filesys.File, error) { return millionFile{}, nil }
func (m *millionFS) Unallocated() ([]filesys.Run, error)      { return nil, nil }

func TestPlanSlackMillionDirentsOnOneClusterIsBounded(t *testing.T) {
	const n = 1_000_000
	img := make([]byte, 2<<20)
	fsys := &millionFS{n: n, size: int64(len(img))}
	opts := Options{Drivers: []detect.Driver{{
		Name:  "millionfs",
		Probe: func(io.ReaderAt, int64) bool { return true },
		Open:  func(io.ReaderAt, int64) (filesys.FileSystem, error) { return fsys, nil },
	}}}
	s := slackSession(t, img, opts)
	reads := withObserver(t)
	p := slackPlanOf(t, s, SlackOptions{Partition: -1})
	if p.files != n || p.skipped["shared-unit"] != n || len(p.file) != 0 || len(*reads) != 0 {
		t.Fatalf("files %d skipped %v segs %d reads %d", p.files, p.skipped, len(p.file), len(*reads))
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc > 512<<20 {
		t.Errorf("heap after the plan = %d MiB", ms.HeapAlloc>>20)
	}
	if len(p.warnings) > slackWarnCap+2 {
		t.Errorf("%d warnings kept", len(p.warnings))
	}
	runtime.KeepAlive(fsys)
}
