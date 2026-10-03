package fattest_test

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// These tests read the image bytes directly, so the builder is checked
// against the format and not against the reader it feeds.

func rootDir(img []byte, g fattest.Geometry) []byte {
	off := int(g.FATStart(g.NumFATs)) * g.SectorSize
	return img[off : off+int(g.RootEntries)*32]
}

func fat16(img []byte, g fattest.Geometry, c uint32) uint16 {
	return binary.LittleEndian.Uint16(img[int(g.FATStart(0))*g.SectorSize+int(c)*2:])
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", name)
		}
	}()
	f()
}

func TestBuildBootAndFATCopies(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		o := fattest.Options{Type: typ, Label: "VOL", VolID: 0xCAFE0001}
		g := fattest.Layout(o)
		img := fattest.Build(o, nil)
		if len(img) != int(g.TotalSectors)*g.SectorSize {
			t.Fatalf("fat%d: image is %d bytes, geometry says %d sectors", typ, len(img), g.TotalSectors)
		}
		if img[510] != 0x55 || img[511] != 0xAA {
			t.Errorf("fat%d: no boot signature", typ)
		}
		fatLen := int(g.FATSectors) * g.SectorSize
		a, b := int(g.FATStart(0))*g.SectorSize, int(g.FATStart(1))*g.SectorSize
		if !bytes.Equal(img[a:a+fatLen], img[b:b+fatLen]) {
			t.Errorf("fat%d: FAT copies differ", typ)
		}
		// Reserved entries 0 and 1.
		switch typ {
		case 12:
			if !bytes.Equal(img[a:a+3], []byte{0xF8, 0xFF, 0xFF}) {
				t.Errorf("fat12 entries 0,1: % x", img[a:a+3])
			}
		case 16:
			if !bytes.Equal(img[a:a+4], []byte{0xF8, 0xFF, 0xFF, 0xFF}) {
				t.Errorf("fat16 entries 0,1: % x", img[a:a+4])
			}
		case 32:
			if got := binary.LittleEndian.Uint32(img[a:]); got != 0x0FFFFFF8 {
				t.Errorf("fat32 entry 0 = %#x", got)
			}
			if got := binary.LittleEndian.Uint32(img[a+8:]); got != 0x0FFFFFFF { // root directory: one cluster, end of chain
				t.Errorf("fat32 root chain entry = %#x", got)
			}
			if !bytes.Equal(img[6*512:7*512], img[:512]) {
				t.Error("fat32 backup boot sector differs")
			}
			if binary.LittleEndian.Uint32(img[512:]) != 0x41615252 {
				t.Error("fat32 FSInfo lead signature missing")
			}
		}
	}
	// The geometry honours the cluster-count rule it is built for.
	if c := fattest.Layout(fattest.Options{Type: 12}).Clusters; c >= 4085 {
		t.Errorf("fat12 default has %d clusters", c)
	}
	if c := fattest.Layout(fattest.Options{Type: 16}).Clusters; c < 4085 || c >= 65525 {
		t.Errorf("fat16 default has %d clusters", c)
	}
	if c := fattest.Layout(fattest.Options{Type: 32}).Clusters; c < 65525 {
		t.Errorf("fat32 default has %d clusters", c)
	}
}

func TestBuildShortEntryLabelAndTimes(t *testing.T) {
	o := fattest.Options{Type: 16, Label: "MY DISK"}
	g := fattest.Layout(o)
	create := time.Date(2024, 3, 15, 13, 45, 31, 340_000_000, time.UTC) // odd second: tenth = 100+34
	modify := time.Date(2025, 12, 31, 23, 59, 58, 0, time.UTC)
	access := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	data := bytes.Repeat([]byte{0xAB}, 700)
	img := fattest.Build(o, []fattest.File{{Path: "/README.TXT", Data: data, Times: [3]time.Time{create, modify, access}, Attr: 0x21}})
	root := rootDir(img, g)

	if string(root[:11]) != "MY DISK    " || root[11] != 0x08 {
		t.Errorf("label entry: %q attr %#x", root[:11], root[11])
	}
	e := root[32:64]
	if string(e[:11]) != "README  TXT" || e[11] != 0x21 || e[12] != 0 {
		t.Errorf("name/attr/ntres: %q %#x %#x", e[:11], e[11], e[12])
	}
	u16 := func(off int) int { return int(binary.LittleEndian.Uint16(e[off:])) }
	if e[13] != 134 {
		t.Errorf("create tenth = %d, want 134", e[13])
	}
	if got, want := u16(14), 13<<11|45<<5|31/2; got != want {
		t.Errorf("create time = %#x, want %#x", got, want)
	}
	if got, want := u16(16), (2024-1980)<<9|3<<5|15; got != want {
		t.Errorf("create date = %#x, want %#x", got, want)
	}
	if got, want := u16(18), (2026-1980)<<9|1<<5|2; got != want {
		t.Errorf("access date = %#x, want %#x", got, want)
	}
	if got, want := u16(22), 23<<11|59<<5|58/2; got != want {
		t.Errorf("modify time = %#x, want %#x", got, want)
	}
	if got, want := u16(24), (2025-1980)<<9|12<<5|31; got != want {
		t.Errorf("modify date = %#x, want %#x", got, want)
	}
	if hi, lo := u16(20), u16(26); hi != 0 || lo != 2 {
		t.Errorf("first cluster hi %d lo %d, want 0/2", hi, lo)
	}
	if size := binary.LittleEndian.Uint32(e[28:]); size != 700 {
		t.Errorf("size = %d", size)
	}
	// Data: 700 bytes over two clusters, chained 2 -> 3 -> EOC.
	if fat16(img, g, 2) != 3 || fat16(img, g, 3) != 0xFFFF || fat16(img, g, 4) != 0 {
		t.Errorf("chain: %#x %#x %#x", fat16(img, g, 2), fat16(img, g, 3), fat16(img, g, 4))
	}
	if !bytes.Equal(img[g.ClusterOffset(2):][:512], data[:512]) || !bytes.Equal(img[g.ClusterOffset(3):][:188], data[512:]) {
		t.Error("file data not at its clusters")
	}
	if img[int(g.ClusterOffset(3))+188] != 0 {
		t.Error("tail of the last cluster is not zero")
	}
}

func TestBuildLowerCaseNTRes(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := fattest.Build(o, []fattest.File{
		{Path: "readme.txt"}, {Path: "notes.TXT"}, {Path: "TODO.txt"},
	})
	root := rootDir(img, g)
	for i, want := range []byte{0x18, 0x08, 0x10} {
		if got := root[i*32+12]; got != want {
			t.Errorf("entry %d: NTRes %#x, want %#x", i, got, want)
		}
		if want := []string{"README  ", "NOTES   ", "TODO    "}[i]; string(root[i*32:i*32+8]) != want {
			t.Errorf("entry %d: name %q", i, root[i*32:i*32+11])
		}
	}
	mustPanic(t, "mixed case without LongName", func() { fattest.Build(o, []fattest.File{{Path: "Name"}}) })
	mustPanic(t, "9-char base without LongName", func() { fattest.Build(o, []fattest.File{{Path: "ABCDEFGHI.TXT"}}) })
	mustPanic(t, "duplicate short name", func() { fattest.Build(o, []fattest.File{{Path: "A.TXT"}, {Path: "a.txt"}}) })
	mustPanic(t, "missing parent", func() { fattest.Build(o, []fattest.File{{Path: "D/A.TXT"}}) })
}

func TestBuildLongName(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := fattest.Build(o, []fattest.File{
		{Path: "Hello World.txt", LongName: true},
		{Path: "Hello World 2.txt", LongName: true},
	})
	root := rootDir(img, g)
	// "Hello World.txt" is 15 units + NUL = 16: two LFN entries, ordinal 2 (last) first.
	if root[0] != 0x42 || root[32] != 0x01 || root[11] != 0x0F || root[32+11] != 0x0F {
		t.Fatalf("LFN ordinals/attrs: %#x %#x %#x %#x", root[0], root[32], root[11], root[43])
	}
	short := root[64:75]
	if string(short) != "HELLOW~1TXT" {
		t.Fatalf("short name %q", short)
	}
	sum := byte(0)
	for _, c := range short {
		sum = (sum&1)<<7 + sum>>1 + c
	}
	if root[13] != sum || root[32+13] != sum {
		t.Errorf("checksums %#x %#x, want %#x", root[13], root[45], sum)
	}
	// Collect the name from the entries (ordinal 1 first).
	var units []uint16
	for _, e := range [][]byte{root[32:64], root[0:32]} {
		for _, r := range [][2]int{{1, 11}, {14, 26}, {28, 32}} {
			for p := r[0]; p < r[1]; p += 2 {
				units = append(units, binary.LittleEndian.Uint16(e[p:]))
			}
		}
	}
	want := []uint16{}
	for _, r := range "Hello World.txt" {
		want = append(want, uint16(r))
	}
	want = append(want, 0)
	for len(want) < 26 {
		want = append(want, 0xFFFF)
	}
	if !slices.Equal(units, want) {
		t.Errorf("name units % x, want % x", units, want)
	}
	// The second name gets a different ~N.
	var shorts []string
	for i := 0; i+32 <= len(root) && root[i] != 0; i += 32 {
		if root[i+11] != 0x0F {
			shorts = append(shorts, string(root[i:i+11]))
		}
	}
	if !slices.Equal(shorts, []string{"HELLOW~1TXT", "HELLOW~2TXT"}) {
		t.Errorf("short names %q", shorts)
	}
	// 255 CJK characters take 20 entries.
	cjk := string(bytes.Repeat([]byte("漢"), 255))
	img = fattest.Build(o, []fattest.File{{Path: cjk, LongName: true}})
	root = rootDir(img, g)
	if root[0] != 0x40|20 {
		t.Errorf("255-unit name: first ordinal %#x, want %#x", root[0], 0x40|20)
	}
	if root[20*32+11] == 0x0F {
		t.Error("more than 20 LFN entries")
	}
	mustPanic(t, "256 units", func() { fattest.Build(o, []fattest.File{{Path: cjk + "x", LongName: true}}) })
}

func TestBuildDeleted(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	data := bytes.Repeat([]byte{0xCD}, 1000)
	img := fattest.Build(o, []fattest.File{
		{Path: "Deleted File.dat", Data: data, LongName: true, Deleted: true},
		{Path: "KEEP.TXT", Data: []byte("keep")},
	})
	root := rootDir(img, g)
	// Two LFN entries and the short entry all start with 0xE5.
	for i := 0; i < 3; i++ {
		if root[i*32] != 0xE5 {
			t.Errorf("entry %d first byte %#x, want 0xE5", i, root[i*32])
		}
	}
	if root[11] != 0x0F || root[32+11] != 0x0F {
		t.Error("LFN attributes lost")
	}
	if string(root[3*32:3*32+8]) != "KEEP    " {
		t.Errorf("live entry %q", root[3*32:3*32+11])
	}
	// The deleted file's clusters (2,3) are freed in the FAT but not reused, and
	// still hold the content.
	if fat16(img, g, 2) != 0 || fat16(img, g, 3) != 0 {
		t.Errorf("deleted chain not freed: %#x %#x", fat16(img, g, 2), fat16(img, g, 3))
	}
	if first := binary.LittleEndian.Uint16(root[2*32+26:]); first != 2 {
		t.Errorf("deleted entry's first cluster %d, want 2", first)
	}
	if !bytes.Equal(img[g.ClusterOffset(2):][:512], data[:512]) {
		t.Error("deleted content not on disk")
	}
	if first := binary.LittleEndian.Uint16(root[3*32+26:]); first != 4 {
		t.Errorf("live file starts at cluster %d, want 4 (2,3 stay reserved)", first)
	}
	if fat16(img, g, 4) != 0xFFFF {
		t.Errorf("live chain %#x", fat16(img, g, 4))
	}
}

func TestBuildFragmentedInterleaves(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := fattest.Build(o, []fattest.File{
		{Path: "FRAG.BIN", Data: make([]byte, 3*512), Fragmented: true},
		{Path: "FILL.BIN", Data: make([]byte, 2*512)},
	})
	chain := func(first uint16) []uint16 {
		var out []uint16
		for c := first; c < 0xFFF8 && len(out) < 10; c = fat16(img, g, uint32(c)) {
			out = append(out, c)
		}
		return out
	}
	root := rootDir(img, g)
	f1 := binary.LittleEndian.Uint16(root[26:])
	f2 := binary.LittleEndian.Uint16(root[32+26:])
	if got := chain(f1); !slices.Equal(got, []uint16{2, 4, 6}) {
		t.Errorf("fragmented chain %v, want [2 4 6]", got)
	}
	if got := chain(f2); !slices.Equal(got, []uint16{3, 5}) {
		t.Errorf("interleaving chain %v, want [3 5]", got)
	}
}

func TestBuildDirectories(t *testing.T) {
	o := fattest.Options{Type: 32}
	g := fattest.Layout(o)
	when := time.Date(2020, 5, 6, 7, 8, 10, 0, time.UTC)
	img := fattest.Build(o, []fattest.File{
		{Path: "/SUB", Dir: true, Times: [3]time.Time{when, when, when}},
		{Path: "/SUB/DEEP", Dir: true},
		{Path: "/SUB/DEEP/X.TXT", Data: []byte("x")},
	})
	u32 := func(b []byte) uint32 {
		return uint32(binary.LittleEndian.Uint16(b[20:]))<<16 | uint32(binary.LittleEndian.Uint16(b[26:]))
	}
	root := img[g.ClusterOffset(2):][:512]
	if string(root[:11]) != "SUB        " || root[11] != 0x10 || u32(root) != 3 {
		t.Fatalf("SUB entry: %q attr %#x first %d", root[:11], root[11], u32(root))
	}
	sub := img[g.ClusterOffset(3):][:512]
	if string(sub[:11]) != ".          " || u32(sub) != 3 {
		t.Errorf("'.' entry: %q first %d", sub[:11], u32(sub))
	}
	if string(sub[32:43]) != "..         " || u32(sub[32:]) != 0 {
		t.Errorf("'..' entry: %q first %d (0 = root)", sub[32:43], u32(sub[32:]))
	}
	if string(sub[64:75]) != "DEEP       " || u32(sub[64:]) != 4 {
		t.Errorf("DEEP entry: %q first %d", sub[64:75], u32(sub[64:]))
	}
	deep := img[g.ClusterOffset(4):][:512]
	if u32(deep[32:]) != 3 {
		t.Errorf("DEEP/.. points to %d, want 3", u32(deep[32:]))
	}
	if string(deep[64:72]) != "X       " || u32(deep[64:]) != 5 {
		t.Errorf("X.TXT entry: %q first %d", deep[64:75], u32(deep[64:]))
	}
	if binary.LittleEndian.Uint16(root[24:]) != uint16((2020-1980)<<9|5<<5|6) {
		t.Errorf("SUB modify date %#x", binary.LittleEndian.Uint16(root[24:]))
	}
	// A directory outgrows its cluster: 20 files of 32-byte entries need 2 clusters.
	var files []fattest.File
	files = append(files, fattest.File{Path: "BIG", Dir: true})
	for i := 0; i < 20; i++ {
		files = append(files, fattest.File{Path: "BIG/F" + string(rune('A'+i)) + ".TXT"})
	}
	img = fattest.Build(o, files)
	a := img[int(g.FATStart(0))*512:]
	if binary.LittleEndian.Uint32(a[3*4:]) != 4 || binary.LittleEndian.Uint32(a[4*4:]) != 0x0FFFFFFF {
		t.Errorf("BIG directory chain: %#x %#x", binary.LittleEndian.Uint32(a[12:]), binary.LittleEndian.Uint32(a[16:]))
	}
}
