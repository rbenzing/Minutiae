package filesys_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestMulAddOK(t *testing.T) {
	const maxInt = math.MaxInt64
	mul := []struct {
		a, b, want int64
		ok         bool
	}{
		{3, 4, 12, true},
		{0, maxInt, 0, true},
		{maxInt, 0, 0, true},
		{1, maxInt, maxInt, true},
		{1 << 31, 1 << 31, 1 << 62, true},
		{1 << 32, 1 << 31, 0, false}, // 2^63
		{maxInt, 2, 0, false},
		{2, maxInt, 0, false},
		{-1, 5, 0, false},
		{5, -1, 0, false},
		{math.MinInt64, 0, 0, false},
	}
	for _, c := range mul {
		if got, ok := filesys.MulOK(c.a, c.b); got != c.want || ok != c.ok {
			t.Errorf("MulOK(%d,%d) = %d,%v; want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
	add := []struct {
		a, b, want int64
		ok         bool
	}{
		{1, 2, 3, true},
		{maxInt, 0, maxInt, true},
		{0, maxInt, maxInt, true},
		{maxInt - 1, 1, maxInt, true},
		{maxInt, 1, 0, false},
		{1, maxInt, 0, false},
		{maxInt, maxInt, 0, false},
		{-1, 1, 0, false},
		{1, -1, 0, false},
	}
	for _, c := range add {
		if got, ok := filesys.AddOK(c.a, c.b); got != c.want || ok != c.ok {
			t.Errorf("AddOK(%d,%d) = %d,%v; want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestMergeRuns(t *testing.T) {
	R := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	cases := []struct {
		name     string
		in, want []filesys.Run
	}{
		{"nil", nil, []filesys.Run{}},
		{"sorted and disjoint", []filesys.Run{R(0, 10), R(20, 5)}, []filesys.Run{R(0, 10), R(20, 5)}},
		{"unsorted", []filesys.Run{R(20, 5), R(0, 10)}, []filesys.Run{R(0, 10), R(20, 5)}},
		{"adjacent", []filesys.Run{R(0, 10), R(10, 5), R(15, 1)}, []filesys.Run{R(0, 16)}},
		{"overlapping", []filesys.Run{R(0, 10), R(5, 10)}, []filesys.Run{R(0, 15)}},
		{"contained", []filesys.Run{R(0, 100), R(10, 5)}, []filesys.Run{R(0, 100)}},
		{"empty dropped", []filesys.Run{R(5, 0), R(0, 3), R(50, -1)}, []filesys.Run{R(0, 3)}},
		{"holes dropped", []filesys.Run{R(-1, 512), R(0, 4)}, []filesys.Run{R(0, 4)}},
		{"overflow dropped", []filesys.Run{R(math.MaxInt64-2, 10), R(0, 1)}, []filesys.Run{R(0, 1)}},
	}
	for _, c := range cases {
		in := append([]filesys.Run(nil), c.in...)
		got := filesys.MergeRuns(c.in)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: MergeRuns = %v, want %v", c.name, got, c.want)
		}
		if fmt.Sprint(c.in) != fmt.Sprint(in) {
			t.Errorf("%s: input was modified: %v -> %v", c.name, in, c.in)
		}
	}
}

func TestEntryTypeAndCorruptError(t *testing.T) {
	for typ, want := range map[filesys.EntryType]string{
		filesys.TypeOther: "other", filesys.TypeFile: "file", filesys.TypeDir: "dir", filesys.TypeSymlink: "symlink", 99: "other",
	} {
		if got := typ.String(); got != want {
			t.Errorf("EntryType(%d) = %q, want %q", typ, got, want)
		}
	}
	var err error = &filesys.CorruptError{Structure: "superblock", Offset: 1024, Reason: "bad magic"}
	if got := err.Error(); got != "corrupt superblock at offset 1024: bad magic" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", err), filesys.ErrCorrupt) {
		t.Error("errors.Is(CorruptError, ErrCorrupt) = false")
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Error("errors.Is(CorruptError, ErrNotFound) = true")
	}
}

// countingReader counts calls to ReadAt on a byte slice.
type countingReader struct {
	b     []byte
	calls atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return bytes.NewReader(c.b).ReadAt(p, off)
}

func TestCachedReaderMatchesSource(t *testing.T) {
	src := make([]byte, 10000)
	newPRNG(1).Read(src)
	cr := &countingReader{b: src}
	cached := filesys.NewCachedReader(cr, 512, 8)
	rng := newPRNG(2)
	for range 2000 {
		off := int64(rng.Intn(len(src) + 700))
		l := rng.Intn(2000)
		want := make([]byte, l)
		wn, werr := bytes.NewReader(src).ReadAt(want, off)
		got := make([]byte, l)
		gn, gerr := cached.ReadAt(got, off)
		if gn != wn || !bytes.Equal(got[:gn], want[:wn]) || (werr == nil) != (gerr == nil) || (werr != nil && !errors.Is(gerr, io.EOF)) {
			t.Fatalf("ReadAt(len %d, off %d) = %d,%v; source %d,%v", l, off, gn, gerr, wn, werr)
		}
	}
	if _, err := cached.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("negative offset accepted")
	}
}

func TestCachedReaderHitsCacheAndEvicts(t *testing.T) {
	src := make([]byte, 4096)
	newPRNG(3).Read(src)
	cr := &countingReader{b: src}
	cached := filesys.NewCachedReader(cr, 512, 2)
	buf := make([]byte, 100)
	read := func(off int64) {
		t.Helper()
		if n, err := cached.ReadAt(buf, off); n != 100 || err != nil || !bytes.Equal(buf, src[off:off+100]) {
			t.Fatalf("ReadAt(%d) = %d, %v", off, n, err)
		}
	}
	read(0)
	read(10)
	read(400)
	if got := cr.calls.Load(); got != 1 {
		t.Fatalf("source calls after 3 reads in block 0 = %d, want 1", got)
	}
	read(512)  // block 1
	read(1024) // block 2 evicts block 0 (LRU: 0 is oldest)
	if got := cr.calls.Load(); got != 3 {
		t.Fatalf("source calls = %d, want 3", got)
	}
	read(512) // still cached; also makes block 1 most recent
	if got := cr.calls.Load(); got != 3 {
		t.Fatalf("block 1 should be cached: calls = %d", got)
	}
	read(0) // evicted: refetched, evicting block 2
	if got := cr.calls.Load(); got != 4 {
		t.Fatalf("block 0 should have been evicted: calls = %d, want 4", got)
	}
	read(512)
	if got := cr.calls.Load(); got != 4 {
		t.Fatalf("block 1 should still be cached: calls = %d", got)
	}
}

func TestCachedReaderShortSourceAndErrors(t *testing.T) {
	src := make([]byte, 1000) // block 1 (512..1000) is short
	newPRNG(4).Read(src)
	cr := &countingReader{b: src}
	cached := filesys.NewCachedReader(cr, 512, 4)
	for range 2 { // the second pass is served from the cache and must behave identically
		buf := make([]byte, 300)
		n, err := cached.ReadAt(buf, 900)
		if n != 100 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], src[900:]) {
			t.Fatalf("ReadAt across the end = %d, %v", n, err)
		}
		if n, err := cached.ReadAt(buf, 1000); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt at the end = %d, %v", n, err)
		}
		if n, err := cached.ReadAt(buf, 5000); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt past the end = %d, %v", n, err)
		}
	}
	if got := cr.calls.Load(); got > 3 { // blocks 1, 2 (empty) and 9 (empty)
		t.Errorf("source calls = %d, short/empty blocks should be cached", got)
	}

	// Non-EOF errors propagate and are not cached.
	boom := errors.New("boom")
	fail := &failingReader{err: boom}
	c2 := filesys.NewCachedReader(fail, 512, 4)
	for range 2 {
		if _, err := c2.ReadAt(make([]byte, 10), 0); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	}
	if fail.calls != 2 {
		t.Errorf("failed reads were cached: calls = %d", fail.calls)
	}
	fail.err = nil
	if n, err := c2.ReadAt(make([]byte, 10), 0); n != 10 || err != nil {
		t.Errorf("after recovery ReadAt = %d, %v", n, err)
	}
}

type failingReader struct {
	err   error
	calls int
}

func (f *failingReader) ReadAt(p []byte, _ int64) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return len(p), nil
}

func TestCachedReaderConcurrent(t *testing.T) {
	src := make([]byte, 64*1024)
	newPRNG(5).Read(src)
	cached := filesys.NewCachedReader(bytes.NewReader(src), 512, 4)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := newPRNG(int64(g))
			buf := make([]byte, 1500)
			for range 500 {
				off := rng.Intn(len(src) - len(buf))
				if n, err := cached.ReadAt(buf, int64(off)); n != len(buf) || err != nil || !bytes.Equal(buf, src[off:off+len(buf)]) {
					t.Errorf("concurrent ReadAt(%d) = %d, %v", off, n, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

type visit struct {
	path    string
	deleted bool
	err     error
}

func walkAll(t *testing.T, fsys filesys.FileSystem, fn func(p string, e filesys.Entry, err error) error) []visit {
	t.Helper()
	var out []visit
	err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		out = append(out, visit{p, e.Deleted, err})
		if fn != nil {
			return fn(p, e, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

func paths(vs []visit) string {
	var s []string
	for _, v := range vs {
		s = append(s, v.path)
	}
	return strings.Join(s, " ")
}

func TestWalkOrderSkipDirAndCycle(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/b.txt", Data: []byte("b")},
		{Path: "/a/b/c", Data: []byte("c")},
		{Path: "/a/a.txt", Data: []byte("a")},
		{Path: "/d", Dir: true, Deleted: true},
		{Path: "/d/hidden", Data: []byte("h")},
		{Path: "/z", Dir: true},
	}})
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}

	got := walkAll(t, fsys, nil)
	if want := "/a /a/a.txt /a/b /a/b/c /b.txt /d /z"; paths(got) != want {
		t.Errorf("walk order = %s\nwant        %s", paths(got), want)
	}
	for _, v := range got {
		if v.err != nil {
			t.Errorf("%s: unexpected error %v", v.path, v.err)
		}
		if v.deleted != (v.path == "/d") {
			t.Errorf("%s: deleted = %v", v.path, v.deleted)
		}
	}

	// SkipDir on a directory skips its children only.
	got = walkAll(t, fsys, func(p string, _ filesys.Entry, _ error) error {
		if p == "/a" {
			return filesys.SkipDir
		}
		return nil
	})
	if want := "/a /b.txt /d /z"; paths(got) != want {
		t.Errorf("after SkipDir(/a): %s, want %s", paths(got), want)
	}

	// SkipDir on a file skips the rest of its directory.
	got = walkAll(t, fsys, func(p string, _ filesys.Entry, _ error) error {
		if p == "/a/a.txt" {
			return filesys.SkipDir
		}
		return nil
	})
	if want := "/a /a/a.txt /b.txt /d /z"; paths(got) != want {
		t.Errorf("after SkipDir(/a/a.txt): %s, want %s", paths(got), want)
	}

	// SkipDir for a deleted directory is a no-op for its (never visited)
	// children and the walk continues with its siblings.
	got = walkAll(t, fsys, func(p string, _ filesys.Entry, _ error) error {
		if p == "/d" {
			return filesys.SkipDir
		}
		return nil
	})
	if want := "/a /a/a.txt /a/b /a/b/c /b.txt /d /z"; paths(got) != want {
		t.Errorf("after SkipDir(/d): %s, want %s", paths(got), want)
	}

	// A callback error stops the walk and is returned.
	stop := errors.New("stop")
	var seen []string
	err = filesys.Walk(fsys, fsys.Root(), "/", func(p string, _ filesys.Entry, _ error) error {
		seen = append(seen, p)
		if p == "/a/b" {
			return stop
		}
		return nil
	})
	if err != stop || strings.Join(seen, " ") != "/a /a/a.txt /a/b" {
		t.Errorf("Walk = %v after %v", err, seen)
	}

	// Walking a subdirectory reports paths under dirPath.
	a, err := fsys.Lookup("/a")
	if err != nil {
		t.Fatal(err)
	}
	var sub []string
	_ = filesys.Walk(fsys, a, "/a", func(p string, _ filesys.Entry, _ error) error { sub = append(sub, p); return nil })
	if strings.Join(sub, " ") != "/a/a.txt /a/b /a/b/c" {
		t.Errorf("subtree = %v", sub)
	}

	// Cycle: directory 2 lists a directory with the same id as its own child.
	tbl := `{"block_size":512,"entries":[
		{"id":"1","parent_id":"","name":"","type":"dir"},
		{"id":"2","parent_id":"1","name":"a","type":"dir"},
		{"id":"2","parent_id":"2","name":"loop","type":"dir"},
		{"id":"3","parent_id":"1","name":"z","type":"file"}]}`
	cyc := fstest.Encode([]byte(tbl), nil)
	cfs, err := fstest.Open(bytes.NewReader(cyc), int64(len(cyc)))
	if err != nil {
		t.Fatal(err)
	}
	got = walkAll(t, cfs, nil)
	if want := "/a /a/loop /z"; paths(got) != want {
		t.Fatalf("cycle walk = %s, want %s", paths(got), want)
	}
	if !errors.Is(got[1].err, filesys.ErrCorrupt) || got[0].err != nil || got[2].err != nil {
		t.Errorf("errors = %v / %v / %v; want only /a/loop to report ErrCorrupt", got[0].err, got[1].err, got[2].err)
	}
}

func TestWalkDepthCap(t *testing.T) {
	// A chain of distinct directories deeper than MaxWalkDepth.
	var sb strings.Builder
	sb.WriteString(`{"block_size":512,"entries":[{"id":"1","parent_id":"","name":"","type":"dir"}`)
	const chain = filesys.MaxWalkDepth + 10
	for i := 2; i < chain+2; i++ {
		fmt.Fprintf(&sb, `,{"id":"%d","parent_id":"%d","name":"d","type":"dir"}`, i, i-1)
	}
	sb.WriteString(`]}`)
	img := fstest.Encode([]byte(sb.String()), nil)
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	got := walkAll(t, fsys, nil)
	if len(got) != filesys.MaxWalkDepth+1 {
		t.Fatalf("visited %d entries, want %d", len(got), filesys.MaxWalkDepth+1)
	}
	last := got[len(got)-1]
	if !errors.Is(last.err, filesys.ErrCorrupt) {
		t.Errorf("last visit %q err = %v, want ErrCorrupt", last.path, last.err)
	}
	for _, v := range got[:len(got)-1] {
		if v.err != nil {
			t.Fatalf("%s: unexpected error %v", v.path, v.err)
		}
	}
}

// prng is a tiny deterministic generator (xorshift64*) for reproducible test data.
type prng struct{ s uint64 }

func newPRNG(seed int64) *prng { return &prng{s: uint64(seed)*0x9E3779B97F4A7C15 + 1} }

func (p *prng) next() uint64 {
	p.s ^= p.s >> 12
	p.s ^= p.s << 25
	p.s ^= p.s >> 27
	return p.s * 0x2545F4914F6CDD1D
}

func (p *prng) Intn(n int) int { return int(p.next() % uint64(n)) }

func (p *prng) Read(b []byte) {
	for i := range b {
		b[i] = byte(p.next() >> 32)
	}
}

func TestCheckRuns(t *testing.T) {
	R := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	const fsSize = 10000
	cases := []struct {
		name string
		runs []filesys.Run
		size int64
		ok   bool
	}{
		{"empty file", nil, 0, true},
		{"one run", []filesys.Run{R(512, 100)}, 100, true},
		{"run to fs end", []filesys.Run{R(9900, 100)}, 100, true},
		{"holes count", []filesys.Run{R(0, 512), R(-1, 512), R(1024, 76)}, 1100, true},
		{"all hole", []filesys.Run{R(-1, 4096)}, 4096, true},
		{"zero-length run allowed", []filesys.Run{R(0, 0), R(10, 5)}, 5, true},
		{"sum short", []filesys.Run{R(0, 512)}, 513, false},
		{"sum long (untrimmed last run)", []filesys.Run{R(0, 512)}, 100, false},
		{"no runs but size", nil, 1, false},
		{"negative length", []filesys.Run{R(0, 10), R(20, -10)}, 0, false},
		{"offset -2", []filesys.Run{R(-2, 10)}, 10, false},
		{"run past fs end", []filesys.Run{R(9901, 100)}, 100, false},
		{"offset overflow", []filesys.Run{R(math.MaxInt64-1, 10)}, 10, false},
		{"length overflow", []filesys.Run{R(-1, math.MaxInt64), R(-1, 10)}, 10, false},
		{"negative size", nil, -1, false},
	}
	for _, c := range cases {
		err := filesys.CheckRuns(c.runs, c.size, fsSize)
		if (err == nil) != c.ok {
			t.Errorf("%s: CheckRuns = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err %v is not ErrCorrupt", c.name, err)
		}
	}
}

func TestCachedReaderClampsHostileParameters(t *testing.T) {
	src := make([]byte, 5000)
	newPRNG(8).Read(src)
	for _, p := range [][2]int{{1 << 40, 1 << 40}, {-5, -5}, {0, 0}, {1, 1}} {
		cached := filesys.NewCachedReader(bytes.NewReader(src), p[0], p[1])
		buf := make([]byte, 700)
		if n, err := cached.ReadAt(buf, 4500); n != 500 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], src[4500:]) {
			t.Errorf("params %v: ReadAt = %d, %v", p, n, err)
		}
		if n, err := cached.ReadAt(buf, 100); n != 700 || err != nil || !bytes.Equal(buf, src[100:800]) {
			t.Errorf("params %v: ReadAt = %d, %v", p, n, err)
		}
	}
}

// zeroSource is a ReaderAt of size zero bytes that never allocates.
type zeroSource struct {
	size  int64
	calls atomic.Int64
}

func (z *zeroSource) ReadAt(p []byte, off int64) (int, error) {
	z.calls.Add(1)
	if off >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.size-off))
	clear(p[:n])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestCachedReaderCapsTotalBytes(t *testing.T) {
	const mib = 1 << 20
	src := &zeroSource{size: 300 * mib}
	// 4096 blocks of 1 MiB would be 4 GiB; the total is capped at 64 MiB.
	cached := filesys.NewCachedReader(src, mib, filesys.MaxCacheCapacity)
	buf := make([]byte, 1)
	for i := range 100 {
		if _, err := cached.ReadAt(buf, int64(i)*mib); err != nil {
			t.Fatal(err)
		}
	}
	before := src.calls.Load()
	if _, err := cached.ReadAt(buf, 99*mib); err != nil || src.calls.Load() != before {
		t.Errorf("most recent block should be cached (err %v, calls %d -> %d)", err, before, src.calls.Load())
	}
	if _, err := cached.ReadAt(buf, 0); err != nil || src.calls.Load() != before+1 {
		t.Errorf("block 0 should have been evicted by the 64 MiB cap (err %v, calls %d -> %d)", err, before, src.calls.Load())
	}
}

func TestCachedReaderEmptyBlocksReuseOneBuffer(t *testing.T) {
	const mib = 1 << 20
	src := &zeroSource{size: 0}
	cached := filesys.NewCachedReader(src, mib, 64)
	buf := make([]byte, 1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range 200 {
		if n, err := cached.ReadAt(buf, int64(i)*mib); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt = %d, %v; want 0, EOF", n, err)
		}
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*mib {
		t.Errorf("200 empty 1 MiB blocks allocated %d MiB; want one reused buffer", grew/mib)
	}
}

func TestWalkPathCap(t *testing.T) {
	// 4096 nested directories with 255-byte names: the full path would reach
	// about 1 MiB. Walk must stop at MaxWalkPath instead of building it.
	name := strings.Repeat("n", 255)
	var sb strings.Builder
	sb.WriteString(`{"block_size":512,"entries":[{"id":"1","parent_id":"","name":"","type":"dir"}`)
	for i := 2; i < filesys.MaxWalkDepth+2; i++ {
		fmt.Fprintf(&sb, `,{"id":"%d","parent_id":"%d","name":%q,"type":"dir"}`, i, i-1, name)
	}
	sb.WriteString(`]}`)
	img := fstest.Encode([]byte(sb.String()), nil)
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := walkAll(t, fsys, nil)
	runtime.ReadMemStats(&after)
	if len(got) == 0 || len(got) >= filesys.MaxWalkDepth {
		t.Fatalf("visited %d entries; want a walk stopped by the path cap well before depth %d", len(got), filesys.MaxWalkDepth)
	}
	last := got[len(got)-1]
	var ce *filesys.CorruptError
	if !errors.As(last.err, &ce) || !errors.Is(last.err, filesys.ErrCorrupt) || !strings.Contains(ce.Reason, "path") {
		t.Fatalf("last visit err = %v, want a path-length CorruptError", last.err)
	}
	if len(last.path) > filesys.MaxWalkPath {
		t.Errorf("reported path is %d bytes, over the %d cap", len(last.path), filesys.MaxWalkPath)
	}
	for _, v := range got[:len(got)-1] {
		if v.err != nil || len(v.path) > filesys.MaxWalkPath {
			t.Fatalf("%.40s...: err %v, len %d", v.path, v.err, len(v.path))
		}
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 256<<20 {
		t.Errorf("walk allocated %d MiB", grew>>20)
	}
}

func TestCheckRunsPrefix(t *testing.T) {
	R := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	const fsSize = 10000
	cases := []struct {
		name    string
		runs    []filesys.Run
		size    int64
		covered int64
		ok      bool
	}{
		{"exact cover", []filesys.Run{R(512, 100)}, 100, 100, true},
		{"strict prefix", []filesys.Run{R(512, 100)}, 300, 100, true},
		{"prefix with hole", []filesys.Run{R(0, 512), R(-1, 512)}, 5000, 1024, true},
		{"no runs", nil, 50, 0, true},
		{"empty file", nil, 0, 0, true},
		{"cover exceeds size", []filesys.Run{R(0, 512)}, 100, 0, false},
		{"second run exceeds size", []filesys.Run{R(0, 60), R(600, 60)}, 100, 0, false},
		{"negative length", []filesys.Run{R(0, 10), R(20, -10)}, 100, 0, false},
		{"offset -2", []filesys.Run{R(-2, 10)}, 100, 0, false},
		{"run past fs end", []filesys.Run{R(9901, 100)}, 300, 0, false},
		{"offset overflow", []filesys.Run{R(math.MaxInt64-1, 10)}, 100, 0, false},
		{"length overflow", []filesys.Run{R(-1, math.MaxInt64), R(-1, 10)}, math.MaxInt64, 0, false},
		{"negative size", nil, -1, 0, false},
	}
	for _, c := range cases {
		covered, err := filesys.CheckRunsPrefix(c.runs, c.size, fsSize)
		if (err == nil) != c.ok {
			t.Errorf("%s: CheckRunsPrefix err = %v, want ok=%v", c.name, err, c.ok)
			continue
		}
		if err != nil && !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err %v is not ErrCorrupt", c.name, err)
		}
		if covered != c.covered {
			t.Errorf("%s: covered = %d, want %d", c.name, covered, c.covered)
		}
		// CheckRuns must accept exactly the lists CheckRunsPrefix accepts with full cover.
		wantFull := c.ok && c.covered == c.size
		if err := filesys.CheckRuns(c.runs, c.size, fsSize); (err == nil) != wantFull {
			t.Errorf("%s: CheckRuns err = %v, want ok=%v", c.name, err, wantFull)
		}
	}
}
