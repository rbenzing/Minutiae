package fat_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// Boot sector offsets (independent of the reader's own constants).
const (
	offBytsPerSec = 11
	offSecPerClus = 13
	offRsvd       = 14
	offNumFATs    = 16
	offRootEnt    = 17
	offTotSec16   = 19
	offMedia      = 21
	offFATSz16    = 22
	offTotSec32   = 32
	offFATSz32    = 36
	offExtFlags   = 40
	offRootClus   = 44
	offLabelFAT   = 43
	offLabel32    = 71
	offSig        = 510
)

func put16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

func build(o fattest.Options, files ...fattest.File) []byte { return fattest.Build(o, files) }

func openImg(t testing.TB, img []byte) *fat.FS {
	t.Helper()
	f, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func openErr(img []byte) error {
	_, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	return err
}

func probe(img []byte) bool { return fat.Probe(bytes.NewReader(img), int64(len(img))) }

// setFAT writes entry c of FAT copy idx with its own packer (not the reader's
// or the builder's).
func setFAT(img []byte, g fattest.Geometry, idx int, c, v uint32) {
	base := int(g.FATStart(idx)) * g.SectorSize
	switch g.Type {
	case 12:
		o := base + int(c)*3/2
		if c%2 == 0 {
			img[o] = byte(v)
			img[o+1] = img[o+1]&0xF0 | byte(v>>8&0x0F)
		} else {
			img[o] = img[o]&0x0F | byte(v&0x0F)<<4
			img[o+1] = byte(v >> 4)
		}
	case 16:
		put16(img, base+int(c)*2, uint16(v))
	default:
		put32(img, base+int(c)*4, v)
	}
}

// firstCluster is the first data cluster free for files: FAT32 keeps the root
// directory in cluster 2.
func firstCluster(typ int) uint32 {
	if typ == 32 {
		return 3
	}
	return 2
}

func isCorrupt(err error) bool {
	var ce *filesys.CorruptError
	return errors.As(err, &ce) && errors.Is(err, filesys.ErrCorrupt)
}

func TestProbeFAT(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		img := build(fattest.Options{Type: typ})
		if !probe(img) {
			t.Errorf("FAT%d image not recognised", typ)
		}
		if !probe(img[:len(img)/2]) {
			// A truncated image still has a valid boot sector.
			t.Errorf("truncated FAT%d image not recognised", typ)
		}
	}

	good := build(fattest.Options{Type: 16})
	mut := func(f func(b []byte)) []byte {
		b := slices.Clone(good)
		f(b)
		return b
	}
	exfat := make([]byte, 4096)
	copy(exfat, []byte{0xEB, 0x76, 0x90})
	copy(exfat[3:], "EXFAT   ")
	exfat[510], exfat[511] = 0x55, 0xAA

	mbrGarbage := bytes.Repeat([]byte{0x90}, 4096)
	copy(mbrGarbage[446:], []byte{0x80, 0x01, 0x01, 0x00, 0x0C, 0xFE, 0xFF, 0xFF, 0x00, 0x08, 0, 0, 0, 0x10, 0, 0})
	mbrGarbage[510], mbrGarbage[511] = 0x55, 0xAA
	mbrZeroBPB := slices.Clone(mbrGarbage)
	clear(mbrZeroBPB[3:62])
	copy(mbrZeroBPB, []byte{0xEB, 0x3C, 0x90})

	cases := []struct {
		name string
		img  []byte
	}{
		{"exFAT boot sector", exfat},
		{"MBR with partition entries, code in the BPB area", mbrGarbage},
		{"MBR with partition entries, empty BPB area", mbrZeroBPB},
		{"NTFS OEM id", mut(func(b []byte) { copy(b[3:], "NTFS    ") })},
		{"EXFAT OEM id on a FAT BPB", mut(func(b []byte) { copy(b[3:], "EXFAT   ") })},
		{"no 0x55AA", mut(func(b []byte) { b[510] = 0 })},
		{"bad jump", mut(func(b []byte) { b[0] = 0x12 })},
		{"EB without 90", mut(func(b []byte) { b[2] = 0x00 })},
		{"all zero", make([]byte, 4096)},
		{"bad media", mut(func(b []byte) { b[offMedia] = 0x42 })},
	}
	for _, c := range cases {
		if probe(c.img) {
			t.Errorf("%s: recognised as FAT", c.name)
		}
	}
	// 0xE9 jump is accepted.
	if !probe(mut(func(b []byte) { b[0], b[1], b[2] = 0xE9, 0x00, 0x00 })) {
		t.Error("0xE9 jump rejected")
	}
}

type countingReader struct {
	r     io.ReaderAt
	reads int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.r.ReadAt(p, off)
}

func TestProbeChecksSizeBeforeReading(t *testing.T) {
	img := build(fattest.Options{Type: 16})
	cr := &countingReader{r: bytes.NewReader(img)}
	for _, size := range []int64{0, 1, 511, -5} {
		if fat.Probe(cr, size) {
			t.Errorf("size %d recognised", size)
		}
	}
	if cr.reads != 0 {
		t.Errorf("Probe read %d times for sizes below 512", cr.reads)
	}
	// A 4096-byte sector size needs at least one whole sector.
	big := build(fattest.Options{Type: 12, SectorSize: 4096})
	if !probe(big) {
		t.Fatal("4096-byte-sector image not recognised")
	}
	if fat.Probe(bytes.NewReader(big[:1024]), 1024) {
		t.Error("image smaller than one 4096-byte sector recognised")
	}
}

// layoutFor finds TotalSectors giving exactly clusters data clusters.
func layoutFor(t *testing.T, o fattest.Options, clusters uint32) fattest.Options {
	t.Helper()
	for tot := clusters + 1; tot < clusters+4096; tot++ {
		o.TotalSectors = tot
		if fattest.Layout(o).Clusters == clusters {
			return o
		}
	}
	t.Fatalf("no geometry with %d clusters", clusters)
	return o
}

func TestTypeByClusterCount(t *testing.T) {
	for _, c := range []struct {
		n    uint64
		want int
	}{{1, 12}, {4084, 12}, {4085, 16}, {65524, 16}, {65525, 32}, {1 << 28, 32}} {
		if got := fat.TypeByCount(c.n); got != c.want {
			t.Errorf("TypeByCount(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	// The same rule decides the type of an opened image.
	for _, c := range []struct {
		layout   int
		clusters uint32
		want     string
	}{
		{12, 4084, "fat12"},
		{16, 4085, "fat16"},
		{16, 65524, "fat16"},
		{32, 65525, "fat32"},
	} {
		o := layoutFor(t, fattest.Options{Type: c.layout}, c.clusters)
		f := openImg(t, build(o))
		if f.ClusterCount() != c.clusters {
			t.Errorf("%d clusters: reader counts %d", c.clusters, f.ClusterCount())
		}
		if got := f.Info().Type; got != c.want {
			t.Errorf("%d clusters: Type %q, want %q", c.clusters, got, c.want)
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("%d clusters: unexpected warnings %q", c.clusters, w)
		}
	}
	// A FAT16-layout BPB with only 4084 clusters is FAT12 by the rule.
	o := layoutFor(t, fattest.Options{Type: 16}, 4084)
	if got := openImg(t, build(o)).Info().Type; got != "fat12" {
		t.Errorf("4084 clusters in a 16-bit layout: Type %q, want fat12", got)
	}
}

func TestLayoutWinsOverClusterCount(t *testing.T) {
	// A FAT32 BPB with few clusters is read as FAT32 (its fields are laid out
	// that way) and flagged.
	f := openImg(t, build(fattest.Options{Type: 32, TotalSectors: 4096}))
	if f.Info().Type != "fat32" {
		t.Errorf("Type %q, want fat32", f.Info().Type)
	}
	if len(f.Info().Warnings) == 0 {
		t.Error("no warning for a FAT32 layout with a FAT12-sized cluster count")
	}
	// A FAT16 BPB with 65525+ clusters stays FAT16 and is flagged.
	f = openImg(t, build(layoutFor(t, fattest.Options{Type: 16}, 65525)))
	if f.Info().Type != "fat16" || len(f.Info().Warnings) == 0 {
		t.Errorf("Type %q, warnings %q", f.Info().Type, f.Info().Warnings)
	}
}

func TestChainFollowsAndDetectsCycle(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		t.Run(fmt.Sprintf("fat%d", typ), func(t *testing.T) {
			o := fattest.Options{Type: typ}
			cs := 512
			files := []fattest.File{
				{Path: "A.BIN", Data: make([]byte, 3*cs)},
				{Path: "B.BIN", Data: make([]byte, 1*cs)},
				{Path: "F.BIN", Data: make([]byte, 3*cs), Fragmented: true},
				{Path: "G.BIN", Data: make([]byte, 2*cs)},
			}
			img := build(o, files...)
			f := openImg(t, img)
			c0 := firstCluster(typ)
			want := map[uint32][]uint32{
				c0:     {c0, c0 + 1, c0 + 2},
				c0 + 3: {c0 + 3},
				c0 + 4: {c0 + 4, c0 + 6, c0 + 8},
				c0 + 5: {c0 + 5, c0 + 7},
			}
			for first, w := range want {
				got, err := f.Chain(first)
				if err != nil || !slices.Equal(got, w) {
					t.Errorf("Chain(%d) = %v, %v; want %v", first, got, err, w)
				}
			}
			if got, err := f.Chain(0); err != nil || len(got) != 0 {
				t.Errorf("Chain(0) = %v, %v; want an empty chain", got, err)
			}

			// A cycle: the last cluster of A points back to its first.
			g := fattest.Layout(o)
			setFAT(img, g, 0, c0+2, c0)
			f = openImg(t, img)
			got, err := f.Chain(c0)
			if !isCorrupt(err) {
				t.Fatalf("cycle: err = %v, want CorruptError", err)
			}
			if !slices.Equal(got, []uint32{c0, c0 + 1, c0 + 2}) {
				t.Errorf("cycle: partial chain %v", got)
			}
			// A self-loop.
			setFAT(img, g, 0, c0+3, c0+3)
			f = openImg(t, img)
			if _, err := f.Chain(c0 + 3); !isCorrupt(err) {
				t.Errorf("self-loop: err = %v, want CorruptError", err)
			}
		})
	}
}

func TestChainOutOfRangeIsCorrupt(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		o := fattest.Options{Type: typ}
		g := fattest.Layout(o)
		c0 := firstCluster(typ)
		img := build(o, fattest.File{Path: "A.BIN", Data: make([]byte, 3*512)})
		last := g.Clusters + 1
		bad := map[string]uint32{
			"free":              0,
			"reserved cluster":  1,
			"past the last":     last + 1,
			"reserved range":    map[int]uint32{12: 0xFF6, 16: 0xFFF6, 32: 0x0FFFFFF6}[typ],
			"bad cluster mark":  map[int]uint32{12: 0xFF7, 16: 0xFFF7, 32: 0x0FFFFFF7}[typ],
			"far past the last": map[int]uint32{12: 0xFF0, 16: 0xFFF0, 32: 0x0FFFFFF0}[typ],
		}
		for name, v := range bad {
			t.Run(fmt.Sprintf("fat%d/%s", typ, name), func(t *testing.T) {
				im := slices.Clone(img)
				setFAT(im, g, 0, c0+1, v)
				f := openImg(t, im)
				got, err := f.Chain(c0)
				if !isCorrupt(err) {
					t.Fatalf("err = %v, want CorruptError", err)
				}
				if !slices.Equal(got, []uint32{c0, c0 + 1}) && !slices.Equal(got, []uint32{c0}) {
					t.Errorf("partial chain %v", got)
				}
			})
		}
		f := openImg(t, img)
		for _, first := range []uint32{1, last + 1, 0xFFFFFFFF} {
			if _, err := f.Chain(first); !isCorrupt(err) {
				t.Errorf("fat%d: Chain(%d) err = %v, want CorruptError", typ, first, err)
			}
		}
		// ChainN stops cleanly at its limit.
		got, trunc, err := f.ChainN(c0, 2)
		if err != nil || !trunc || len(got) != 2 {
			t.Errorf("fat%d: ChainN limit: %v %v %v", typ, got, trunc, err)
		}
	}
	// FAT32 ignores the top four bits of an entry.
	o := fattest.Options{Type: 32}
	g := fattest.Layout(o)
	img := build(o, fattest.File{Path: "A.BIN", Data: make([]byte, 2*512)})
	setFAT(img, g, 0, 3, 0xF0000004)
	got, err := openImg(t, img).Chain(3)
	if err != nil || !slices.Equal(got, []uint32{3, 4}) {
		t.Errorf("masked entry: %v, %v", got, err)
	}
}

func TestFAT12PackedEntries(t *testing.T) {
	o := fattest.Options{Type: 12}
	g := fattest.Layout(o)
	img := build(o)
	vals := []uint32{0x123, 0xABC, 0x456, 0xDEF, 0x789, 0x012, 0xFFF, 0x000, 0x800, 0x001, 0xF00, 0x0F0}
	for i, v := range vals {
		setFAT(img, g, 0, uint32(i)+2, v)
	}
	f := openImg(t, img)
	for i, v := range vals {
		got, err := f.Entry(uint32(i) + 2)
		if err != nil || got != v {
			t.Errorf("entry %d = %#x, %v; want %#x", i+2, got, err, v)
		}
	}
	// Entries 0 and 1 (media descriptor and end-of-chain) are intact too.
	if e0, _ := f.Entry(0); e0 != 0xFF8 {
		t.Errorf("entry 0 = %#x, want 0xFF8", e0)
	}
	if e1, _ := f.Entry(1); e1 != 0xFFF {
		t.Errorf("entry 1 = %#x, want 0xFFF", e1)
	}
	// A chain over odd/even boundaries.
	for c, next := range map[uint32]uint32{2: 3, 3: 4, 4: 5, 5: 6, 6: 0xFFF} {
		setFAT(img, g, 0, c, next)
	}
	got, err := openImg(t, img).Chain(2)
	if err != nil || !slices.Equal(got, []uint32{2, 3, 4, 5, 6}) {
		t.Errorf("chain = %v, %v", got, err)
	}
	// The last entry in the table (its two bytes end exactly on the FAT's end).
	last := g.Clusters + 1
	setFAT(img, g, 0, last, 0xFF8)
	setFAT(img, g, 0, last-1, last)
	got, err = openImg(t, img).Chain(last - 1)
	if err != nil || !slices.Equal(got, []uint32{last - 1, last}) {
		t.Errorf("tail chain = %v, %v", got, err)
	}
}

func TestActiveFATMirroringDisabled(t *testing.T) {
	o := fattest.Options{Type: 32, NumFATs: 2}
	g := fattest.Layout(o)
	base := build(o, fattest.File{Path: "A.BIN", Data: make([]byte, 3*512)})
	// FAT 1 differs from FAT 0: it skips cluster 4.
	setFAT(base, g, 1, 3, 5)
	for _, c := range []struct {
		name     string
		flags    uint16
		active   int
		want     []uint32
		warnings bool
	}{
		{"mirroring enabled reads FAT 0", 0x0000, 0, []uint32{3, 4, 5}, false},
		{"mirroring enabled ignores the low bits", 0x0001, 0, []uint32{3, 4, 5}, false},
		{"disabled, FAT 0 active", 0x0080, 0, []uint32{3, 4, 5}, false},
		{"disabled, FAT 1 active", 0x0081, 1, []uint32{3, 5}, false},
		{"disabled, high bits ignored", 0x0181 | 0x0070, 1, []uint32{3, 5}, false},
		{"disabled, FAT 2 of 2 falls back to FAT 0", 0x0082, 0, []uint32{3, 4, 5}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			img := slices.Clone(base)
			put16(img, offExtFlags, c.flags)
			f := openImg(t, img)
			if f.ActiveFAT() != c.active {
				t.Errorf("ActiveFAT = %d, want %d", f.ActiveFAT(), c.active)
			}
			got, err := f.Chain(3)
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("Chain(3) = %v, %v; want %v", got, err, c.want)
			}
			if has := len(f.Info().Warnings) > 0; has != c.warnings {
				t.Errorf("warnings = %q", f.Info().Warnings)
			}
		})
	}
	// FAT12/16 have no mirroring flags: the field is not read.
	o16 := fattest.Options{Type: 16, NumFATs: 2}
	img := build(o16)
	put16(img, offExtFlags, 0x0081)
	if got := openImg(t, img).ActiveFAT(); got != 0 {
		t.Errorf("FAT16 ActiveFAT = %d, want 0", got)
	}
}

func TestOpenHostileBPB(t *testing.T) {
	o16 := fattest.Options{Type: 16}
	g16 := fattest.Layout(o16)
	good16 := build(o16)
	o32 := fattest.Options{Type: 32, TotalSectors: 4096}
	good32 := build(o32)

	cases := []struct {
		name string
		base []byte
		mut  func(b []byte)
	}{
		{"BytsPerSec 0", good16, func(b []byte) { put16(b, offBytsPerSec, 0) }},
		{"BytsPerSec 513", good16, func(b []byte) { put16(b, offBytsPerSec, 513) }},
		{"BytsPerSec 256", good16, func(b []byte) { put16(b, offBytsPerSec, 256) }},
		{"BytsPerSec 8192", good16, func(b []byte) { put16(b, offBytsPerSec, 8192) }},
		{"SecPerClus 0", good16, func(b []byte) { b[offSecPerClus] = 0 }},
		{"SecPerClus 3", good16, func(b []byte) { b[offSecPerClus] = 3 }},
		{"SecPerClus 255", good16, func(b []byte) { b[offSecPerClus] = 255 }},
		{"RsvdSecCnt 0", good16, func(b []byte) { put16(b, offRsvd, 0) }},
		{"RsvdSecCnt past the image", good16, func(b []byte) { put16(b, offRsvd, 0xFFFF) }},
		{"NumFATs 0", good16, func(b []byte) { b[offNumFATs] = 0 }},
		{"NumFATs 3", good16, func(b []byte) { b[offNumFATs] = 3 }},
		{"NumFATs 255", good16, func(b []byte) { b[offNumFATs] = 255 }},
		{"RootEntCnt 0 in FAT16", good16, func(b []byte) { put16(b, offRootEnt, 0) }},
		{"FATSz16 0 and FATSz32 0", good16, func(b []byte) { put16(b, offFATSz16, 0) }},
		{"FATSz32 0 in FAT32", good32, func(b []byte) { put32(b, offFATSz32, 0) }},
		{"FATSz16 65535", good16, func(b []byte) { put16(b, offFATSz16, 0xFFFF) }},
		{"FATSz32 0xFFFFFFFF", good32, func(b []byte) { put32(b, offFATSz32, 0xFFFFFFFF) }},
		{"total sectors 0", good16, func(b []byte) { put16(b, offTotSec16, 0); put32(b, offTotSec32, 0) }},
		{"total sectors below the data start", good16, func(b []byte) { put16(b, offTotSec16, 10) }},
		{"total sectors equal to the data start", good16, func(b []byte) { put16(b, offTotSec16, uint16(g16.DataStart())) }},
		{"cluster count 0", good16, func(b []byte) { put16(b, offTotSec16, uint16(g16.DataStart())); b[offSecPerClus] = 1 }},
		{"less than one cluster of data", good16, func(b []byte) {
			put16(b, offTotSec16, uint16(g16.DataStart()+3))
			b[offSecPerClus] = 4
		}},
		{"media byte 0", good16, func(b []byte) { b[offMedia] = 0 }},
		{"no signature", good16, func(b []byte) { b[offSig] = 0 }},
		{"FAT32 with a root directory region", good32, func(b []byte) { put16(b, offRootEnt, 512) }},
		{"FAT32 RootClus 0", good32, func(b []byte) { put32(b, offRootClus, 0) }},
		{"FAT32 RootClus 1", good32, func(b []byte) { put32(b, offRootClus, 1) }},
		{"FAT32 RootClus past the last cluster", good32, func(b []byte) { put32(b, offRootClus, 0x0FFFFFF0) }},
		{"FAT32 RootClus 0xFFFFFFFF", good32, func(b []byte) { put32(b, offRootClus, 0xFFFFFFFF) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := slices.Clone(c.base)
			c.mut(img)
			f, err := func() (f *fat.FS, err error) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic: %v", r)
					}
				}()
				return fat.Open(bytes.NewReader(img), int64(len(img)))
			}()
			if err == nil {
				t.Fatalf("Open succeeded (%+v)", f.Info())
			}
			if !isCorrupt(err) {
				t.Fatalf("err = %v, want a CorruptError", err)
			}
			if probe(img) {
				t.Errorf("Probe accepts a BPB that Open rejects")
			}
		})
	}

	// Images too small for a boot sector.
	for _, n := range []int{0, 1, 100, 511} {
		if err := openErr(good16[:n]); !isCorrupt(err) {
			t.Errorf("size %d: err = %v", n, err)
		}
	}
}

// TestOpenHugeTotSecDoesNotAllocatePerClaimedCluster: TotSec32 = 2^32-1 on a
// 1 MB image must clamp the cluster count to what the image holds.
func TestOpenHugeTotSecDoesNotAllocatePerClaimedCluster(t *testing.T) {
	for _, typ := range []int{16, 32} {
		img := build(fattest.Options{Type: typ, TotalSectors: 2048})
		if len(img) != 1<<20 {
			t.Fatalf("image is %d bytes", len(img))
		}
		put16(img, offTotSec16, 0)
		put32(img, offTotSec32, 0xFFFFFFFF)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f := openImg(t, img)
		_, _ = f.Chain(firstCluster(typ))
		runtime.ReadMemStats(&after)
		if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
			t.Errorf("fat%d: Open allocated %d bytes", typ, d)
		}
		info := f.Info()
		if info.Size > int64(len(img)) {
			t.Errorf("fat%d: Size %d exceeds the image", typ, info.Size)
		}
		if f.ClusterCount() > 2048 {
			t.Errorf("fat%d: ClusterCount %d exceeds the image", typ, f.ClusterCount())
		}
		if len(info.Warnings) == 0 {
			t.Errorf("fat%d: no warning about the clamp", typ)
		}
	}
}

func TestOpenClampsToTruncatedImage(t *testing.T) {
	o := fattest.Options{Type: 16}
	img := build(o)
	cut := img[:len(img)/2+100]
	f := openImg(t, cut)
	info := f.Info()
	if info.Size > int64(len(cut)) || info.Size%512 != 0 {
		t.Errorf("Size = %d for a %d-byte image", info.Size, len(cut))
	}
	if f.ClusterCount() >= fattest.Layout(o).Clusters {
		t.Errorf("ClusterCount %d not clamped (declared %d)", f.ClusterCount(), fattest.Layout(o).Clusters)
	}
	if len(info.Warnings) == 0 {
		t.Error("no warning for a truncated image")
	}
	// The same image uncut has no warnings.
	if w := openImg(t, img).Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a clean image: %q", w)
	}
}

func TestOpenClampsToFATCapacity(t *testing.T) {
	// A BPB claiming more clusters than its FAT can describe.
	img := build(fattest.Options{Type: 16})
	put16(img, offFATSz16, 4) // 4 sectors hold 1024 entries
	f := openImg(t, img)
	if got := f.ClusterCount(); got > 1022 {
		t.Errorf("ClusterCount %d exceeds the FAT's capacity", got)
	}
	if len(f.Info().Warnings) == 0 {
		t.Error("no warning")
	}
}

func TestInfo(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		o := fattest.Options{Type: typ, Label: "EVIDENCE 1", VolID: 0x1234ABCD}
		g := fattest.Layout(o)
		f := openImg(t, build(o))
		info := f.Info()
		if want := fmt.Sprintf("fat%d", typ); info.Type != want {
			t.Errorf("Type %q, want %q", info.Type, want)
		}
		if info.Label != "EVIDENCE 1" {
			t.Errorf("fat%d: Label %q", typ, info.Label)
		}
		if info.UUID != "1234-ABCD" {
			t.Errorf("fat%d: UUID %q", typ, info.UUID)
		}
		if info.BlockSize != 512 || info.Size != int64(g.TotalSectors)*512 {
			t.Errorf("fat%d: BlockSize %d, Size %d", typ, info.BlockSize, info.Size)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("fat%d: warnings %q", typ, info.Warnings)
		}
	}
	// Cluster size follows SecPerClus and SectorSize.
	f := openImg(t, build(fattest.Options{Type: 16, SectorSize: 1024, SecPerClus: 4}))
	if f.Info().BlockSize != 4096 {
		t.Errorf("BlockSize %d, want 4096", f.Info().BlockSize)
	}
}

func TestInfoLabelSources(t *testing.T) {
	// The root-directory label entry wins over the BPB label.
	for _, typ := range []int{16, 32} {
		o := fattest.Options{Type: typ, Label: "ROOTLABEL"}
		img := build(o)
		off := offLabelFAT
		if typ == 32 {
			off = offLabel32
		}
		copy(img[off:off+11], "BPB LABEL  ")
		if got := openImg(t, img).Info().Label; got != "ROOTLABEL" {
			t.Errorf("fat%d: Label %q, want the root entry's", typ, got)
		}
		// Without a root entry the BPB label is used.
		img = build(fattest.Options{Type: typ})
		copy(img[off:off+11], "BPB LABEL  ")
		if got := openImg(t, img).Info().Label; got != "BPB LABEL" {
			t.Errorf("fat%d: Label %q, want the BPB's", typ, got)
		}
		// "NO NAME" is the placeholder for no label.
		if got := openImg(t, build(fattest.Options{Type: typ})).Info().Label; got != "" {
			t.Errorf("fat%d: Label %q, want none", typ, got)
		}
	}
	// A label entry that follows other entries is still found; the scan stops at
	// the first free entry.
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	img := build(o, fattest.File{Path: "A.TXT", Data: []byte("x")})
	root := int(g.FATStart(g.NumFATs)) * g.SectorSize
	clear(img[root : root+32]) // slot 0 becomes the end marker
	lab := make([]byte, 32)
	copy(lab, "AFTER END  ")
	lab[11] = 0x08
	copy(img[root+32:], lab)
	if got := openImg(t, img).Info().Label; got != "" {
		t.Errorf("Label %q read past the end-of-directory marker", got)
	}
	// Deleted label entries are ignored.
	img = build(fattest.Options{Type: 16, Label: "GONE"})
	img[root] = 0xE5
	copy(img[offLabelFAT:offLabelFAT+11], "BPBL       ")
	if got := openImg(t, img).Info().Label; got != "BPBL" {
		t.Errorf("Label %q, want the BPB's (the root entry is deleted)", got)
	}
	// A corrupt FAT32 root chain does not stop Open; it is a warning.
	o32 := fattest.Options{Type: 32, Label: "X"}
	g32 := fattest.Layout(o32)
	img = build(o32)
	setFAT(img, g32, 0, 2, 0)
	f := openImg(t, img)
	if f.Info().Label != "X" {
		t.Errorf("Label %q (the first root cluster is still readable)", f.Info().Label)
	}
	// With no label in the readable part, the break is reported.
	img = build(fattest.Options{Type: 32})
	for i := 0; i < 16; i++ { // a cluster full of live entries: the scan runs off its end
		e := img[g32.ClusterOffset(2)+int64(i)*32:][:32]
		copy(e, "FILLER  BIN")
		e[11] = 0x20
	}
	setFAT(img, g32, 0, 2, 0)
	f = openImg(t, img)
	if f.Info().Label != "" {
		t.Errorf("Label %q", f.Info().Label)
	}
	if w := f.Info().Warnings; len(w) != 1 || !strings.Contains(w[0], "root directory") {
		t.Errorf("warnings = %q, want one about the broken root chain", w)
	}
}

func TestWarningsDedupedCappedSnapshot(t *testing.T) {
	f := openImg(t, build(fattest.Options{Type: 16}))
	for range 5 {
		f.Warn("same problem %q", "x")
	}
	if w := f.Info().Warnings; len(w) != 1 || w[0] != `same problem "x"` {
		t.Fatalf("warnings = %q", w)
	}
	snap := f.Info().Warnings
	snap[0] = "mutated"
	if f.Info().Warnings[0] == "mutated" {
		t.Error("Info().Warnings aliases the live list")
	}
	for i := range 1500 {
		f.Warn("problem %d", i)
	}
	w := f.Info().Warnings
	if len(w) != 1001 || w[1000] != "further warnings suppressed" {
		t.Fatalf("len %d, last %q", len(w), w[len(w)-1])
	}
	// More distinct warnings do not add more lines.
	f.Warn("one more")
	if len(f.Info().Warnings) != 1001 {
		t.Error("cap exceeded")
	}
	// Safe for concurrent use.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				f.Warn("g%d-%d", i, j)
				_ = f.Info()
			}
		}()
	}
	wg.Wait()
}

// TestOpenMutatedBootNeverPanics flips random boot-sector bytes and exercises
// every entry point on whatever still opens.
func TestOpenMutatedBootNeverPanics(t *testing.T) {
	bases := [][]byte{
		build(fattest.Options{Type: 12, TotalSectors: 2048}, fattest.File{Path: "A.BIN", Data: make([]byte, 2000)}),
		build(fattest.Options{Type: 16, TotalSectors: 2048}, fattest.File{Path: "A.BIN", Data: make([]byte, 2000)}),
		build(fattest.Options{Type: 32, TotalSectors: 2048}, fattest.File{Path: "A.BIN", Data: make([]byte, 2000)}),
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	interesting := []byte{0, 1, 2, 3, 0x7F, 0x80, 0xFF}
	opened := 0
	for i := range 3000 {
		img := slices.Clone(bases[i%len(bases)])
		for range 1 + rng.IntN(4) {
			pos := rng.IntN(90)
			if rng.IntN(8) == 0 {
				pos = 510 + rng.IntN(2)
			}
			if rng.IntN(2) == 0 {
				img[pos] = interesting[rng.IntN(len(interesting))]
			} else {
				img[pos] = byte(rng.IntN(256))
			}
		}
		_ = probe(img)
		f, err := fat.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			if !isCorrupt(err) {
				t.Fatalf("iteration %d: %v", i, err)
			}
			continue
		}
		opened++
		_ = f.Info()
		for c := uint32(0); c < 6; c++ {
			_, _ = f.Entry(c)
			_, _ = f.Chain(c)
		}
		_, _ = f.Chain(0xFFFFFFFF)
	}
	if opened == 0 {
		t.Error("no mutated image opened: the test exercises nothing")
	}
}

func TestCorruptReasonQuotesOnDiskStrings(t *testing.T) {
	img := build(fattest.Options{Type: 16})
	copy(img[3:], "NTFS    ")
	err := openErr(img)
	if !isCorrupt(err) || !strings.Contains(err.Error(), `"NTFS    "`) {
		t.Errorf("err = %v, want the OEM id %q quoted", err, "NTFS    ")
	}
}
