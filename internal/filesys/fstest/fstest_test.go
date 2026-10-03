package fstest_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

const bs = 512

func pattern(n int, seed int64) []byte {
	b := make([]byte, n)
	newPRNG(seed).Read(b)
	return b
}

func open(t *testing.T, img []byte) filesys.FileSystem {
	t.Helper()
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return fsys
}

func readAll(t *testing.T, f filesys.File) []byte {
	t.Helper()
	buf := make([]byte, f.Size())
	n, err := f.ReadAt(buf, 0)
	if n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt = %d, %v; want %d", n, err, len(buf))
	}
	return buf
}

func lookupOpen(t *testing.T, fsys filesys.FileSystem, p string) (filesys.Entry, filesys.File) {
	t.Helper()
	e, err := fsys.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	return e, f
}

// readRuns concatenates the bytes of runs straight from the image (holes read
// as zeros), independently of the filesystem reader.
func readRuns(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		if r.Offset < 0 {
			out = append(out, make([]byte, r.Length)...)
			continue
		}
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

func TestMTFSRoundTrip(t *testing.T) {
	hello := []byte("hello world")
	big := pattern(10*bs-100, 1) // 10 blocks, last one partial
	holey := pattern(4*bs, 2)
	img := fstest.Build(fstest.BuildSpec{
		Label: "TESTVOL",
		Nodes: []fstest.Node{
			{Path: "/hello.txt", Data: hello, Mode: 0o600, UID: 1000, GID: 1001, MTime: 1700000000},
			{Path: "/dir/sub/big.bin", Data: big, Fragments: 4},
			{Path: "/dir/lnk", Link: "../hello.txt"},
			{Path: "/holey", Data: holey, Hole: true},
			{Path: "/empty", Dir: true},
			{Path: "/secret", Data: []byte("ciphertext"), Encrypted: true},
		},
	})
	if string(img[:8]) != "MTFS0001" {
		t.Fatalf("magic = %q", img[:8])
	}
	if !fstest.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatal("Probe false on an MTFS image")
	}
	fsys := open(t, img)

	info := fsys.Info()
	if info.Type != "mtfs" || info.Label != "TESTVOL" || info.BlockSize != bs || info.Size != int64(len(img)) || !info.Encrypted {
		t.Errorf("Info = %+v", info)
	}

	root := fsys.Root()
	if root.ID != "1" || root.Type != filesys.TypeDir {
		t.Fatalf("root = %+v", root)
	}
	kids, err := fsys.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range kids {
		names = append(names, k.Name)
	}
	if got := strings.Join(names, ","); got != "dir,empty,hello.txt,holey,secret" {
		t.Errorf("root listing = %s", got)
	}

	e, f := lookupOpen(t, fsys, "/hello.txt")
	if e.Type != filesys.TypeFile || e.Size != int64(len(hello)) || e.Mode != 0o100600 || e.UID != 1000 || e.GID != 1001 ||
		e.Times.Modified.T.Unix() != 1700000000 || !e.Times.Modified.ZoneKnown {
		t.Errorf("hello entry = %+v", e)
	}
	if got := readAll(t, f); !bytes.Equal(got, hello) {
		t.Errorf("hello = %q", got)
	}
	if runs := f.Runs(); len(runs) != 1 || runs[0].Length != bs || !bytes.Equal(readRuns(img, runs)[:len(hello)], hello) {
		t.Errorf("hello runs = %+v", runs)
	}

	// Fragmented file: 10 blocks in 4 runs (3,3,2,2) separated by one free block.
	_, f = lookupOpen(t, fsys, "/dir/sub/big.bin")
	if got := readAll(t, f); !bytes.Equal(got, big) {
		t.Error("big.bin content differs")
	}
	runs := f.Runs()
	wantBlocks := []int64{3, 3, 2, 2}
	if len(runs) != 4 {
		t.Fatalf("big.bin runs = %+v", runs)
	}
	for i, r := range runs {
		if r.Length != wantBlocks[i]*bs {
			t.Errorf("run %d = %+v, want %d blocks", i, r, wantBlocks[i])
		}
		if i > 0 && r.Offset != runs[i-1].Offset+runs[i-1].Length+bs {
			t.Errorf("run %d = %+v: want exactly one free block after run %d", i, r, i-1)
		}
	}
	if got := readRuns(img, runs)[:len(big)]; !bytes.Equal(got, big) {
		t.Error("bytes at the reported runs differ from the content")
	}
	// ReadAt across a run boundary and at the end.
	part := make([]byte, 600)
	if n, err := f.ReadAt(part, 3*bs-100); n != 600 || err != nil || !bytes.Equal(part, big[3*bs-100:3*bs+500]) {
		t.Errorf("cross-run ReadAt = %d, %v", n, err)
	}
	tail := make([]byte, 300)
	if n, err := f.ReadAt(tail, int64(len(big))-50); n != 50 || !errors.Is(err, io.EOF) || !bytes.Equal(tail[:50], big[len(big)-50:]) {
		t.Errorf("tail ReadAt = %d, %v", n, err)
	}
	if n, err := f.ReadAt(tail, int64(len(big))); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at size = %d, %v", n, err)
	}

	// Hole: the second block is a hole and reads as zeros.
	_, f = lookupOpen(t, fsys, "/holey")
	want := append([]byte(nil), holey...)
	clear(want[bs : 2*bs])
	if got := readAll(t, f); !bytes.Equal(got, want) {
		t.Error("holey content: second block must read as zeros")
	}
	runs = f.Runs()
	if len(runs) != 3 || runs[0].Length != bs || runs[1] != (filesys.Run{Offset: -1, Length: bs}) || runs[2].Length != 2*bs {
		t.Fatalf("holey runs = %+v", runs)
	}
	if runs[2].Offset != runs[0].Offset+bs {
		t.Errorf("a hole takes no storage: runs = %+v", runs)
	}

	// Symlink: Open yields the target bytes, no runs.
	le, lf := lookupOpen(t, fsys, "/dir/lnk")
	if le.Type != filesys.TypeSymlink || le.LinkTarget != "../hello.txt" {
		t.Errorf("link entry = %+v", le)
	}
	if got := readAll(t, lf); string(got) != "../hello.txt" {
		t.Errorf("link content = %q", got)
	}

	// Directories and encryption.
	de, err := fsys.Lookup("/dir/sub")
	if err != nil || de.Type != filesys.TypeDir {
		t.Fatalf("Lookup dir = %+v, %v", de, err)
	}
	if _, err := fsys.Open(de); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(dir) err = %v, want ErrUnsupported", err)
	}
	if se, err := fsys.Lookup("/secret"); err != nil || !se.Encrypted {
		t.Errorf("secret = %+v, %v", se, err)
	}
	for _, p := range []string{"/", "", "/dir/"} {
		if _, err := fsys.Lookup(p); err != nil {
			t.Errorf("Lookup(%q): %v", p, err)
		}
	}
	for _, p := range []string{"/nope", "/hello.txt/x", "/dir/../hello.txt", "/dir/nope/x"} {
		if _, err := fsys.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) err = %v, want ErrNotFound", p, err)
		}
	}
	if _, err := fsys.ReadDir(e); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(file) err = %v", err)
	}
}

func TestMTFS4097Fragments(t *testing.T) {
	const n = 4097
	data := pattern(n*bs, 3)
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/frag", Data: data, Fragments: n}}})
	fsys := open(t, img)
	_, f := lookupOpen(t, fsys, "/frag")
	runs := f.Runs()
	if len(runs) != n {
		t.Fatalf("runs = %d, want %d", len(runs), n)
	}
	for i, r := range runs {
		if r.Length != bs || (i > 0 && r.Offset != runs[i-1].Offset+2*bs) {
			t.Fatalf("run %d = %+v after %+v", i, r, runs[max(i-1, 0)])
		}
	}
	if !bytes.Equal(readAll(t, f), data) {
		t.Error("content differs")
	}
	// Random ReadAt through the fragments.
	rng := newPRNG(4)
	for range 200 {
		off := rng.Intn(len(data))
		l := rng.Intn(3000) + 1
		buf := make([]byte, l)
		got, err := f.ReadAt(buf, int64(off))
		exp := min(l, len(data)-off)
		if got != exp || !bytes.Equal(buf[:got], data[off:off+exp]) || (got < l && !errors.Is(err, io.EOF)) || (got == l && err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("ReadAt(%d,%d) = %d, %v", off, l, got, err)
		}
	}
}

func TestMTFSUnallocated(t *testing.T) {
	data := pattern(3*bs, 5)
	img := fstest.Build(fstest.BuildSpec{
		FreeBlocks: 2,
		Nodes:      []fstest.Node{{Path: "/f", Data: data, Fragments: 3}},
	})
	fsys := open(t, img)
	_, f := lookupOpen(t, fsys, "/f")
	r := f.Runs()
	if len(r) != 3 {
		t.Fatalf("runs = %+v", r)
	}
	got, err := fsys.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	// Independent expectation: one free block after runs 0 and 1 (the gaps),
	// and the two trailing free blocks after run 2.
	exp := []filesys.Run{
		{Offset: r[0].Offset + bs, Length: bs},
		{Offset: r[1].Offset + bs, Length: bs},
		{Offset: r[2].Offset + bs, Length: 2 * bs},
	}
	if fmt.Sprint(got) != fmt.Sprint(exp) {
		t.Errorf("Unallocated = %+v, want %+v", got, exp)
	}
	merged := filesys.MergeRuns(got)
	if fmt.Sprint(merged) != fmt.Sprint(got) {
		t.Errorf("Unallocated is not sorted and merged: %+v", got)
	}

	// No free space at all: nothing unallocated.
	img = fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/f", Data: data}}})
	if got, err := open(t, img).Unallocated(); err != nil || len(got) != 0 {
		t.Errorf("Unallocated = %+v, %v; want none", got, err)
	}
}

func TestMTFSDeleted(t *testing.T) {
	img := fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/gone.txt", Data: []byte("deleted data"), Deleted: true},
		{Path: "/gonedir", Dir: true, Deleted: true},
		{Path: "/live.txt", Data: []byte("live")},
	}})
	fsys := open(t, img)
	kids, err := fsys.ReadDir(fsys.Root())
	if err != nil {
		t.Fatal(err)
	}
	del := map[string]bool{}
	for _, k := range kids {
		del[k.Name] = k.Deleted
	}
	if len(del) != 3 || !del["gone.txt"] || !del["gonedir"] || del["live.txt"] {
		t.Errorf("deleted flags = %v", del)
	}
	for _, p := range []string{"/gone.txt", "/gonedir"} {
		if _, err := fsys.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) err = %v, want ErrNotFound", p, err)
		}
	}
	for _, k := range kids {
		if k.Name == "gone.txt" {
			if _, err := fsys.Open(k); !errors.Is(err, filesys.ErrDeleted) {
				t.Errorf("Open(deleted) err = %v, want ErrDeleted", err)
			}
		}
	}
	// A caller-built entry cannot bypass the table's deleted flag.
	for _, k := range kids {
		if k.Name == "gone.txt" {
			k.Deleted = false
			if _, err := fsys.Open(k); !errors.Is(err, filesys.ErrDeleted) {
				t.Errorf("Open(entry with cleared flag) err = %v, want ErrDeleted", err)
			}
		}
	}
}

func TestMTFSCorruptTable(t *testing.T) {
	root := `{"id":"1","parent_id":"","name":"","type":"dir"}`
	good := func(extra string) []byte {
		tbl := `{"label":"x","block_size":512,"entries":[` + root + extra + `]}`
		return fstest.Encode([]byte(tbl), make([]byte, 4*bs))
	}
	hdr := func(tlen uint64, body int) []byte {
		b := make([]byte, 16+body)
		copy(b, "MTFS0001")
		binary.LittleEndian.PutUint64(b[8:], tlen)
		return b
	}
	cases := []struct {
		name string
		img  []byte
		size int64 // 0 = len(img)
	}{
		{"too short", []byte("MTFS0001\x00"), 0},
		{"bad magic", append([]byte("XXXXXXXX"), make([]byte, 100)...), 0},
		{"table longer than image", hdr(5000, 100), 0},
		{"table length near max uint64", hdr(^uint64(0), 100), 0},
		{"table over 16 MiB", hdr(17<<20, 100), 1 << 40},
		{"bad json", fstest.Encode([]byte(`{"entries":[`), nil), 0},
		{"no root", fstest.Encode([]byte(`{"block_size":512,"entries":[]}`), nil), 0},
		{"root not a dir", fstest.Encode([]byte(`{"block_size":512,"entries":[{"id":"1","type":"file"}]}`), nil), 0},
		{"zero block size", fstest.Encode([]byte(`{"block_size":0,"entries":[`+root+`]}`), nil), 0},
		{"huge block size", fstest.Encode([]byte(`{"block_size":1073741824,"entries":[`+root+`]}`), nil), 0},
		{"unknown type", good(`,{"id":"2","parent_id":"1","name":"a","type":"bogus"}`), 0},
		{"empty name", good(`,{"id":"2","parent_id":"1","name":"","type":"file"}`), 0},
		{"slash in name", good(`,{"id":"2","parent_id":"1","name":"a/b","type":"file"}`), 0},
		{"dot name", good(`,{"id":"2","parent_id":"1","name":"..","type":"file"}`), 0},
		{"run past end", good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":10,"runs":[{"offset":1000000,"length":512}]}`), 0},
		{"run overflows", good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":10,"runs":[{"offset":9223372036854775800,"length":512}]}`), 0},
		{"negative offset not a hole", good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":10,"runs":[{"offset":-5,"length":512}]}`), 0},
		{"zero length run", good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":10,"runs":[{"offset":600,"length":0}]}`), 0},
		{"negative size", good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":-1}`), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size := tc.size
			if size == 0 {
				size = int64(len(tc.img))
			}
			fsys, err := fstest.Open(bytes.NewReader(tc.img), size)
			if err == nil {
				t.Fatalf("Open succeeded: %+v", fsys.Info())
			}
			var ce *filesys.CorruptError
			if !errors.Is(err, filesys.ErrCorrupt) || !errors.As(err, &ce) {
				t.Errorf("err = %v (%T), want *CorruptError", err, err)
			}
		})
	}

	// Content larger than its runs is detected when the file is opened.
	img := good(`,{"id":"2","parent_id":"1","name":"a","type":"file","size":5000,"runs":[{"offset":1024,"length":512}]}`)
	fsys := open(t, img)
	e, err := fsys.Lookup("/a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open(size > runs) err = %v, want ErrCorrupt", err)
	}
}

func TestMTFSProbe(t *testing.T) {
	if fstest.Probe(bytes.NewReader([]byte("short")), 5) || fstest.Probe(bytes.NewReader(make([]byte, 64)), 64) {
		t.Error("Probe true on a non-MTFS image")
	}
}

func TestBuildPanicsOnBadSpec(t *testing.T) {
	bad := map[string]fstest.BuildSpec{
		"duplicate path":     {Nodes: []fstest.Node{{Path: "/a"}, {Path: "/a"}}},
		"file as parent":     {Nodes: []fstest.Node{{Path: "/a"}, {Path: "/a/b"}}},
		"empty path":         {Nodes: []fstest.Node{{Path: ""}}},
		"hole in 1-block":    {Nodes: []fstest.Node{{Path: "/a", Data: []byte("x"), Hole: true}}},
		"too many fragments": {Nodes: []fstest.Node{{Path: "/a", Data: []byte("x"), Fragments: 2}}},
		"data on dir":        {Nodes: []fstest.Node{{Path: "/a", Dir: true, Data: []byte("x")}}},
	}
	for name, spec := range bad {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Build did not panic")
				}
			}()
			fstest.Build(spec)
		})
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{
		{Path: "/a/b", Data: pattern(2000, 6), Fragments: 3},
		{Path: "/h", Data: pattern(3*bs, 7), Hole: true},
		{Path: "/l", Link: "a/b"},
		{Path: "/d", Dir: true, Deleted: true},
	}}))
	f.Add([]byte("MTFS0001\x02\x00\x00\x00\x00\x00\x00\x00{}"))
	f.Fuzz(func(t *testing.T, b []byte) {
		size := int64(len(b))
		fsys, err := fstest.Open(bytes.NewReader(b), size)
		if err != nil {
			return
		}
		var walk func(dir filesys.Entry, depth int)
		walk = func(dir filesys.Entry, depth int) {
			if depth > 8 {
				return
			}
			kids, err := fsys.ReadDir(dir)
			if err != nil {
				return
			}
			for _, k := range kids {
				if k.Type == filesys.TypeDir {
					walk(k, depth+1)
					continue
				}
				file, err := fsys.Open(k)
				if err != nil {
					continue
				}
				for _, r := range file.Runs() {
					if r.Offset != -1 && (r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > size) {
						t.Fatalf("run out of filesystem: %+v (size %d)", r, size)
					}
				}
				buf := make([]byte, min(file.Size(), 1<<16))
				_, _ = file.ReadAt(buf, 0)
			}
		}
		walk(fsys.Root(), 0)
		_, _ = fsys.Unallocated()
	})
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
