package fattest_test

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) + seed
	}
	return b
}

// readFAT decodes entry c of FAT copy n with an independent decoder.
func readFAT(img []byte, g fattest.Geometry, n int, c uint32) uint32 {
	base := int(g.FATStart(n)) * g.SectorSize
	switch g.Type {
	case 12:
		o := base + int(c+c/2)
		v := uint32(img[o]) | uint32(img[o+1])<<8
		if c&1 == 1 {
			return v >> 4
		}
		return v & 0xFFF
	case 16:
		return uint32(binary.LittleEndian.Uint16(img[base+int(c)*2:]))
	}
	return binary.LittleEndian.Uint32(img[base+int(c)*4:]) & 0x0FFFFFFF
}

func TestBuildLayoutLocatesEntries(t *testing.T) {
	live := pattern(1500, 1)
	long := pattern(600, 2)
	frag := pattern(1400, 3)
	o := fattest.Options{Type: 16}
	files := []fattest.File{
		{Path: "/LIVE.TXT", Data: live},
		{Path: "/a long deleted name.txt", Data: long, LongName: true, Deleted: true},
		{Path: "/FRAG.BIN", Data: frag, Fragmented: true, Deleted: true},
	}
	img, built := fattest.BuildLayout(o, files)
	if !bytes.Equal(img, fattest.Build(o, files)) {
		t.Fatal("BuildLayout image differs from Build")
	}
	g := built.Geometry
	if g != fattest.Layout(o) {
		t.Fatalf("geometry %+v differs from Layout", g)
	}

	check := func(path string, deleted bool, data []byte) fattest.EntryLoc {
		t.Helper()
		loc := built.Find(path, deleted)
		if loc.Deleted != deleted || loc.Dir || loc.DirCluster != 0 {
			t.Fatalf("%s: loc %+v", path, loc)
		}
		got := img[loc.Offset : loc.Offset+32]
		first := binary.LittleEndian.Uint16(got[26:])
		if uint32(first) != loc.FirstCluster || loc.FirstCluster != loc.Clusters[0] {
			t.Fatalf("%s: entry first %d, loc %d, clusters %v", path, first, loc.FirstCluster, loc.Clusters)
		}
		if size := binary.LittleEndian.Uint32(got[28:]); int(size) != len(data) {
			t.Fatalf("%s: size %d", path, size)
		}
		if deleted != (got[0] == 0xE5) {
			t.Fatalf("%s: first byte %#x, deleted %v", path, got[0], deleted)
		}
		var content []byte
		for _, c := range loc.Clusters {
			off := g.ClusterOffset(c)
			content = append(content, img[off:off+int64(g.SectorSize*g.SecPerClus)]...)
		}
		if !bytes.Equal(content[:len(data)], data) {
			t.Fatalf("%s: data is not at the cluster offsets", path)
		}
		return loc
	}

	l := check("/LIVE.TXT", false, live)
	if !bytes.HasPrefix(img[l.Offset:], []byte("LIVE    TXT")) || len(l.LFNOffsets) != 0 {
		t.Fatalf("live entry %q lfn %v", img[l.Offset:l.Offset+11], l.LFNOffsets)
	}
	walk := []uint32{l.FirstCluster}
	for c := readFAT(img, g, 0, l.FirstCluster); c < 0xFFF8; c = readFAT(img, g, 0, c) {
		walk = append(walk, c)
	}
	if len(walk) != 3 || !slices.Equal(walk, l.Clusters) {
		t.Fatalf("hand walk %v, Clusters %v", walk, l.Clusters)
	}
	if l.Index != 0 {
		t.Fatalf("index %d", l.Index)
	}

	d := check("/a long deleted name.txt", true, long)
	if len(d.LFNOffsets) != 2 { // 20 UTF-16 units: 2 entries
		t.Fatalf("lfn offsets %v", d.LFNOffsets)
	}
	for i, off := range d.LFNOffsets {
		if want := d.Offset - int64(len(d.LFNOffsets)-i)*32; off != want {
			t.Fatalf("lfn offset %d = %d, want %d", i, off, want)
		}
		if e := img[off : off+32]; e[0] != 0xE5 || e[11] != 0x0F {
			t.Fatalf("lfn entry %d: % x", i, e[:12])
		}
	}
	if d.Index != 3 { // LIVE (0), then 2 LFN entries (1, 2), then the short entry
		t.Fatalf("deleted index %d", d.Index)
	}
	if len(d.Clusters) != 2 {
		t.Fatalf("clusters %v", d.Clusters)
	}

	f := check("/FRAG.BIN", true, frag)
	if len(f.Clusters) != 3 || f.Clusters[1] == f.Clusters[0]+1 {
		t.Fatalf("fragmented clusters %v", f.Clusters)
	}
	for _, c := range f.Clusters {
		if readFAT(img, g, 0, c) != 0 {
			t.Fatalf("deleted chain not freed at %d", c)
		}
	}
}

func TestBuildLayoutLocatesSubdirectoryEntries(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		img, built := fattest.BuildLayout(fattest.Options{Type: typ}, []fattest.File{
			{Path: "/DIR", Dir: true},
			{Path: "/DIR/IN.TXT", Data: pattern(10, 4), Deleted: true},
			{Path: "/TOP.TXT", Data: pattern(10, 5)},
		})
		g := built.Geometry
		dir := built.Find("/DIR", false)
		if !dir.Dir || len(dir.Clusters) != 1 {
			t.Fatalf("type %d: dir %+v", typ, dir)
		}
		in := built.Find("DIR/IN.TXT", true)
		if in.DirCluster != dir.FirstCluster {
			t.Fatalf("type %d: DirCluster %d, want %d", typ, in.DirCluster, dir.FirstCluster)
		}
		if want := g.ClusterOffset(dir.FirstCluster) + 2*32; in.Offset != want || in.Index != 2 {
			t.Fatalf("type %d: offset %d index %d, want %d / 2", typ, in.Offset, in.Index, want)
		}
		if !bytes.HasPrefix(img[in.Offset:], []byte{0xE5, 'N', ' '}) {
			t.Fatalf("type %d: entry % x", typ, img[in.Offset:in.Offset+11])
		}
		top := built.Find("/TOP.TXT", false)
		wantDir := uint32(0)
		if typ == 32 {
			wantDir = 2 // the root directory chain starts at cluster 2
		}
		if top.DirCluster != wantDir || !bytes.HasPrefix(img[top.Offset:], []byte("TOP     TXT")) {
			t.Fatalf("type %d: top %+v", typ, top)
		}
	}
}

func TestFindPanicsWithoutEntry(t *testing.T) {
	_, built := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/A.TXT", Data: []byte("x")}})
	mustPanic(t, "Find of a missing entry", func() { built.Find("/A.TXT", true) })
}

func TestSetFATEntryAllTypes(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		img, built := fattest.BuildLayout(fattest.Options{Type: typ}, nil)
		g := built.Geometry
		fattest.SetFATEntry(img, g, 4, 0x9AB)
		fattest.SetFATEntry(img, g, 7, 0x7CD)
		for _, pair := range [][2]uint32{{5, 6}, {6, 5}} { // even then odd neighbour, and back
			a, b := pair[0], pair[1]
			before := readFAT(img, g, 0, b)
			fattest.SetFATEntry(img, g, a, 0xABC)
			if got := readFAT(img, g, 0, a); got != 0xABC {
				t.Fatalf("type %d: entry %d = %#x", typ, a, got)
			}
			if got := readFAT(img, g, 0, b); got != before {
				t.Fatalf("type %d: writing %d changed %d: %#x -> %#x", typ, a, b, before, got)
			}
			fattest.SetFATEntry(img, g, b, 0x123)
			if readFAT(img, g, 0, a) != 0xABC || readFAT(img, g, 0, b) != 0x123 {
				t.Fatalf("type %d: pair %d/%d not independent", typ, a, b)
			}
		}
		if readFAT(img, g, 0, 4) != 0x9AB || readFAT(img, g, 0, 7) != 0x7CD {
			t.Fatalf("type %d: outer neighbours 4/7 changed", typ)
		}
		for n := 1; n < g.NumFATs; n++ {
			x := img[int(g.FATStart(0))*g.SectorSize : int(g.FATStart(1))*g.SectorSize]
			y := img[int(g.FATStart(n))*g.SectorSize : int(g.FATStart(n+1))*g.SectorSize]
			if !bytes.Equal(x, y) {
				t.Fatalf("type %d: FAT copy %d differs from copy 0", typ, n)
			}
		}
	}
}

func TestSetFATEntryFAT32KeepsTopNibble(t *testing.T) {
	img, built := fattest.BuildLayout(fattest.Options{Type: 32}, nil)
	g := built.Geometry
	off := int(g.FATStart(0))*g.SectorSize + 7*4
	binary.LittleEndian.PutUint32(img[off:], 0xA0000000)
	fattest.SetFATEntry(img, g, 7, 0x0123456)
	if got := binary.LittleEndian.Uint32(img[off:]); got != 0xA0123456 {
		t.Fatalf("got %#x", got)
	}
}

func TestPatchEntryWritesHighHalfOnFAT32(t *testing.T) {
	img, built := fattest.BuildLayout(fattest.Options{Type: 32}, []fattest.File{{Path: "/A.TXT", Data: pattern(20, 1)}})
	loc := built.Find("/A.TXT", false)
	first, size := uint32(0x00012345), uint32(0xCAFEF00D)
	fattest.PatchEntry(img, built.Geometry, loc, &first, &size)
	e := img[loc.Offset : loc.Offset+32]
	if hi, lo := binary.LittleEndian.Uint16(e[20:]), binary.LittleEndian.Uint16(e[26:]); hi != 1 || lo != 0x2345 {
		t.Fatalf("halves %#x %#x", hi, lo)
	}
	if got := binary.LittleEndian.Uint32(e[28:]); got != size {
		t.Fatalf("size %#x", got)
	}
	// nil leaves the field alone
	sz2 := uint32(7)
	fattest.PatchEntry(img, built.Geometry, loc, nil, &sz2)
	e = img[loc.Offset : loc.Offset+32]
	if binary.LittleEndian.Uint16(e[20:]) != 1 || binary.LittleEndian.Uint16(e[26:]) != 0x2345 || binary.LittleEndian.Uint32(e[28:]) != 7 {
		t.Fatalf("nil first changed the entry: % x", e)
	}
}

func TestPatchEntryFAT16(t *testing.T) {
	img, built := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/A.TXT", Data: pattern(20, 1)}})
	loc := built.Find("/A.TXT", false)
	hi := binary.LittleEndian.Uint16(img[loc.Offset+20:])
	first := uint32(0x1234)
	fattest.PatchEntry(img, built.Geometry, loc, &first, nil)
	if binary.LittleEndian.Uint16(img[loc.Offset+26:]) != 0x1234 || binary.LittleEndian.Uint16(img[loc.Offset+20:]) != hi {
		t.Fatalf("FAT16 patch wrong: % x", img[loc.Offset:loc.Offset+32])
	}
	big := uint32(0x10000)
	mustPanic(t, "first above 0xFFFF on FAT16", func() { fattest.PatchEntry(img, built.Geometry, loc, &big, nil) })
}

func TestSetFATEntryRejectsClustersOutsideTheTable(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		img, built := fattest.BuildLayout(fattest.Options{Type: typ}, nil)
		g := built.Geometry
		before := bytes.Clone(img)
		for _, c := range []uint32{0, 1, g.Clusters + 2, g.Clusters + 100} {
			mustPanicLabelled(t, "cluster outside [2, clusters+1]", func() { fattest.SetFATEntry(img, g, c, 5) })
		}
		if !bytes.Equal(img, before) {
			t.Fatalf("type %d: a rejected write changed the image", typ)
		}
		// the first and last valid clusters are accepted
		fattest.SetFATEntry(img, g, 2, 5)
		fattest.SetFATEntry(img, g, g.Clusters+1, 6)
		if readFAT(img, g, 0, 2) != 5 || readFAT(img, g, 0, g.Clusters+1) != 6 {
			t.Fatalf("type %d: boundary clusters not written", typ)
		}
	}
}

func TestSetFATEntryRejectsAWriteOutsideTheFATRegion(t *testing.T) {
	img, built := fattest.BuildLayout(fattest.Options{Type: 16}, nil)
	g := built.Geometry
	before := bytes.Clone(img)
	g.FATSectors = 1 // the table claims to be one sector; cluster 300 lies beyond it
	mustPanicLabelled(t, "write past the FAT copy", func() { fattest.SetFATEntry(img, g, 300, 5) })
	if !bytes.Equal(img, before) {
		t.Fatal("a rejected write changed the image")
	}
}

func TestSetFATEntryRejectsValuesWiderThanTheEntry(t *testing.T) {
	for _, tc := range []struct {
		typ     int
		ok, bad uint32
	}{{12, 0xFFF, 0x1000}, {16, 0xFFFF, 0x10000}, {32, 0x0FFFFFFF, 0x10000000}} {
		img, built := fattest.BuildLayout(fattest.Options{Type: tc.typ}, nil)
		g := built.Geometry
		before := bytes.Clone(img)
		mustPanicLabelled(t, "value wider than the entry", func() { fattest.SetFATEntry(img, g, 5, tc.bad) })
		if !bytes.Equal(img, before) {
			t.Fatalf("type %d: a rejected write changed the image", tc.typ)
		}
		fattest.SetFATEntry(img, g, 5, tc.ok)
		if got := readFAT(img, g, 0, 5); got != tc.ok {
			t.Fatalf("type %d: widest value stored as %#x", tc.typ, got)
		}
	}
}

func TestPatchEntryRejectsAnEntryPastTheImage(t *testing.T) {
	img, built := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/A.TXT", Data: pattern(20, 1)}})
	size := uint32(1)
	for _, off := range []int64{int64(len(img)) - 31, int64(len(img)), -1} {
		loc := built.Find("/A.TXT", false)
		loc.Offset = off
		mustPanicLabelled(t, "entry past the image", func() { fattest.PatchEntry(img, built.Geometry, loc, nil, &size) })
	}
	loc := built.Find("/A.TXT", false)
	loc.Offset = int64(len(img)) - 32 // the last whole entry is accepted
	fattest.PatchEntry(img, built.Geometry, loc, nil, &size)
}

// mustPanicLabelled requires a panic whose message starts with "fattest:", so a
// bare runtime index error does not pass as a deliberate check.
func mustPanicLabelled(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if s, ok := r.(string); !ok || !strings.HasPrefix(s, "fattest: ") {
			t.Errorf("%s: panic %v is not a labelled fattest panic", name, r)
		}
	}()
	f()
}
