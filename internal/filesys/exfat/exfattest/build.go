// Package exfattest synthesizes small, deterministic exFAT images for unit and
// hostile-input tests of the exfat reader. It is an independent encoder of the
// on-disk format (it shares no code with the reader), so a reader/builder pair
// that agrees is evidence the format is understood, not that one side mirrors
// the other. Real mkfs.exfat images live under tools/fixtures.
//
// Build lays out: the main boot region (sectors 0-11, the checksum sector
// last) and its backup (sectors 12-23), one FAT at sector 24, then the cluster
// heap holding, in this order, the allocation bitmap, the up-case table, the
// root directory and every file and directory in the order of the File list.
// Files are contiguous with NoFatChain unless FatChain or Fragmented says
// otherwise. A deleted file keeps its data and its entry set (InUse cleared)
// but its clusters are free in the bitmap. Options and File only ever gain
// fields, so existing callers keep working.
package exfattest

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// Offset is a UTC offset field of a File entry: Valid is bit 7 and Quarters
// the signed 7-bit count of 15-minute increments (-64..63).
type Offset struct {
	Valid    bool
	Quarters int8
}

// File describes one file or directory to place in the image.
type File struct {
	Path string // "/a/b.txt"; parents the list does not mention are created
	Data []byte
	Dir  bool

	// Deleted writes the entry set and the data and then clears the InUse bit
	// of every entry of the set; the clusters are free in the bitmap. The set
	// checksum is recomputed over the cleared types unless
	// DeletedStaleChecksum keeps the checksum of the live set.
	Deleted              bool
	DeletedStaleChecksum bool

	// FatChain stores the cluster chain in the FAT instead of NoFatChain.
	// Fragmented implies FatChain and leaves one free cluster between the
	// clusters of the file.
	FatChain   bool
	Fragmented bool

	// ValidLength sets ValidDataLength (default: the data length). The data
	// on disk is the whole of Data, so a reader must return zeros past it.
	ValidLength *int64

	// Attr is OR-ed into FileAttributes (a directory also gets 0x10).
	Attr uint16

	// Times are the create, modify and access times: their wall-clock fields
	// are stored as they are (the location is ignored). The zero time means
	// the fixed default time. Offsets are the three UTC offset fields; Inc10ms
	// are the create and modify 10 ms increments added to the sub-two-second
	// part (the seconds' odd bit is always stored there).
	Times   [3]time.Time
	Offsets [3]Offset
	Inc10ms [2]uint8

	// RawName replaces the UTF-16 name derived from the last component of Path
	// (to store a name that is not valid UTF-16).
	RawName []uint16

	// BadChecksum stores a wrong SetChecksum.
	BadChecksum bool
}

// Options selects the geometry and the optional parts of the image.
type Options struct {
	BytesPerSectorShift    uint8 // 9..12 (default 9)
	SectorsPerClusterShift uint8 // default 3 (0 means the default; see OneSectorClusters)
	OneSectorClusters      bool  // clusters of a single sector (shift 0)
	ClusterCount           int   // default 256
	Label                  string
	Serial                 uint32

	// Upcase maps one UTF-16 unit to its upper-case form; nil is the default
	// (a-z, U+00E0-U+00FE except U+00F7, and U+00FF). NoUpcase leaves the
	// up-case table out of the image.
	Upcase   func(uint16) uint16
	NoUpcase bool

	// FragmentBitmap leaves a free cluster between the clusters of the
	// allocation bitmap (chained through the FAT).
	FragmentBitmap bool

	// BadBootChecksum corrupts the checksum sector of the main boot region.
	BadBootChecksum bool
}

// EntryLoc locates one file's entry set in the image.
type EntryLoc struct {
	Path         string
	Deleted, Dir bool
	DirCluster   uint32   // first cluster of the directory holding the set
	Index        int      // entry index of the File entry in that directory
	Offset       int64    // image offset of the File entry
	FirstCluster uint32   // first cluster of the content (0 = none)
	Clusters     []uint32 // every cluster of the content, in order
}

// Layout describes where Build put things.
type Layout struct {
	SectorSize, ClusterSize int
	FatOffset, HeapOffset   int64 // byte offsets
	ClusterCount            uint32
	RootCluster             uint32
	BitmapCluster           uint32
	BitmapClusters          []uint32 // every cluster of the bitmap, in order
	UpcaseCluster           uint32   // 0 with NoUpcase
	Entries                 []EntryLoc
	free                    []uint32
}

// ClusterOffset returns the image offset of cluster c (c >= 2).
func (l *Layout) ClusterOffset(c uint32) int64 {
	return l.HeapOffset + int64(c-2)*int64(l.ClusterSize)
}

// FatEntryOffset returns the image offset of the FAT entry of cluster c.
func (l *Layout) FatEntryOffset(c uint32) int64 { return l.FatOffset + 4*int64(c) }

// FreeClusters returns the clusters that are free in the allocation bitmap,
// ascending.
func (l *Layout) FreeClusters() []uint32 { return append([]uint32(nil), l.free...) }

// Find returns the location of the live (or, with deleted, the deleted) entry
// set for path. It panics when there is none.
func (l *Layout) Find(path string, deleted bool) EntryLoc {
	for _, e := range l.Entries {
		if e.Path == path && e.Deleted == deleted {
			return e
		}
	}
	panic(fmt.Sprintf("exfattest: no entry %q (deleted=%v)", path, deleted))
}

// Entry type bytes.
const (
	typeBitmap = 0x81
	typeUpcase = 0x82
	typeLabel  = 0x83
	typeFile   = 0x85
	typeStream = 0xC0
	typeName   = 0xC1

	entrySize       = 32
	namePerEntry    = 15
	fatStart        = 24 // sectors
	maxImageBytes   = 256 << 20
	defaultClusters = 256
)

// defaultTime is the time of an entry whose Times are unset (2023-11-14 22:13:20).
var defaultTime = time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)

type node struct {
	f        File
	name     []uint16
	dir      bool
	parent   *node
	children []*node
	clusters []uint32
	noFat    bool
}

type builder struct {
	o        Options
	ss, cs   int
	spc      int
	spcShift uint8
	cc       int
	fatSecs  int
	heapSecs int
	volSecs  int
	img      []byte
	used     []bool // per cluster index (cluster-2)
	next     int    // next unallocated cluster index
	fat      []uint32
	root     *node
	nodes    []*node // allocation order (the root excluded)
	lay      Layout
}

// Build returns an exFAT image for the options. It panics on invalid options
// (it is a test helper).
func Build(o Options, files []File) []byte {
	img, _ := BuildLayout(o, files)
	return img
}

// BuildLayout is Build plus the Layout of the image.
func BuildLayout(o Options, files []File) ([]byte, *Layout) {
	b := &builder{o: o}
	b.geometry()
	b.tree(files)
	b.allocate()
	b.write()
	return b.img, &b.lay
}

func orDefault[T comparable](v, d T) T {
	var zero T
	if v == zero {
		return d
	}
	return v
}

func (b *builder) geometry() {
	bps := orDefault(b.o.BytesPerSectorShift, 9)
	spcShift := orDefault(b.o.SectorsPerClusterShift, 3)
	if b.o.OneSectorClusters {
		spcShift = 0
	}
	b.spcShift = spcShift
	if bps < 9 || bps > 12 || int(bps)+int(spcShift) > 25 {
		panic(fmt.Sprintf("exfattest: shifts %d+%d", bps, spcShift))
	}
	b.ss = 1 << bps
	b.spc = 1 << spcShift
	b.cs = b.ss * b.spc
	b.cc = orDefault(b.o.ClusterCount, defaultClusters)
	if b.cc < 8 {
		panic("exfattest: need at least 8 clusters")
	}
	b.fatSecs = ((b.cc+2)*4 + b.ss - 1) / b.ss
	b.heapSecs = (fatStart + b.fatSecs + b.spc - 1) / b.spc * b.spc
	b.volSecs = b.heapSecs + b.cc*b.spc
	if int64(b.volSecs)*int64(b.ss) > maxImageBytes {
		panic("exfattest: image too large")
	}
	b.img = make([]byte, b.volSecs*b.ss)
	b.used = make([]bool, b.cc)
	b.fat = make([]uint32, b.cc+2)
	b.lay = Layout{
		SectorSize: b.ss, ClusterSize: b.cs,
		FatOffset:    int64(fatStart * b.ss),
		HeapOffset:   int64(b.heapSecs) * int64(b.ss),
		ClusterCount: uint32(b.cc),
	}
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (n *node) child(name string) *node {
	for _, c := range n.children {
		if c.dir && string(utf16.Decode(c.name)) == name {
			return c
		}
	}
	return nil
}

// tree builds the directory tree from the file list, creating implicit parents.
func (b *builder) tree(files []File) {
	b.root = &node{dir: true}
	for _, f := range files {
		parts := splitPath(f.Path)
		if len(parts) == 0 {
			panic("exfattest: empty path")
		}
		cur := b.root
		for i, p := range parts[:len(parts)-1] {
			next := cur.child(p)
			if next == nil {
				next = &node{dir: true, parent: cur, name: utf16.Encode([]rune(p)), f: File{Path: "/" + strings.Join(parts[:i+1], "/"), Dir: true}}
				cur.children = append(cur.children, next)
				b.nodes = append(b.nodes, next)
			}
			cur = next
		}
		n := &node{f: f, dir: f.Dir, parent: cur, name: utf16.Encode([]rune(parts[len(parts)-1]))}
		if f.RawName != nil {
			n.name = f.RawName
		}
		cur.children = append(cur.children, n)
		b.nodes = append(b.nodes, n)
	}
}

func nameEntries(units int) int { return (units + namePerEntry - 1) / namePerEntry }

func setEntries(n *node) int { return 2 + nameEntries(len(n.name)) }

// rootExtras is the number of entries before the first file set of the root.
func (b *builder) rootExtras() int {
	n := 1 // bitmap
	if !b.o.NoUpcase {
		n++
	}
	if b.o.Label != "" {
		n++
	}
	return n
}

func (b *builder) dirEntryCount(d *node) int {
	n := 0
	if d == b.root {
		n = b.rootExtras()
	}
	for _, c := range d.children {
		n += setEntries(c)
	}
	return n
}

func (b *builder) alloc(n int) []uint32 {
	if b.next+n > b.cc {
		panic("exfattest: out of clusters")
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = uint32(b.next + i + 2)
		b.used[b.next+i] = true
	}
	b.next += n
	return out
}

func (b *builder) chainFAT(cl []uint32) {
	for i, c := range cl {
		if i+1 < len(cl) {
			b.fat[c] = cl[i+1]
		} else {
			b.fat[c] = 0xFFFFFFFF
		}
	}
}

func (b *builder) clustersFor(bytes int) int { return (bytes + b.cs - 1) / b.cs }

func (b *builder) allocate() {
	b.fat[0], b.fat[1] = 0xFFFFFFF8, 0xFFFFFFFF
	nbm := b.clustersFor((b.cc + 7) / 8)
	var bm []uint32
	if b.o.FragmentBitmap {
		for i := 0; i < nbm; i++ {
			bm = append(bm, b.alloc(1)...)
			if i+1 < nbm {
				b.next++ // a free cluster between the bitmap clusters
			}
		}
	} else {
		bm = b.alloc(nbm)
	}
	b.chainFAT(bm)
	b.lay.BitmapCluster = bm[0]
	b.lay.BitmapClusters = bm
	if !b.o.NoUpcase {
		uc := b.alloc(b.clustersFor(len(b.upcaseTable())))
		b.chainFAT(uc)
		b.lay.UpcaseCluster = uc[0]
	}
	rootN := max(1, b.clustersFor(b.dirEntryCount(b.root)*entrySize))
	b.root.clusters = b.alloc(rootN)
	b.chainFAT(b.root.clusters)
	b.lay.RootCluster = b.root.clusters[0]

	for _, n := range b.nodes {
		var count int
		if n.dir {
			count = max(1, b.clustersFor(b.dirEntryCount(n)*entrySize))
		} else {
			count = b.clustersFor(len(n.f.Data))
		}
		switch {
		case count == 0:
		case n.f.Fragmented:
			n.noFat = false
			for i := 0; i < count; i++ {
				n.clusters = append(n.clusters, b.alloc(1)...)
				if i+1 < count {
					b.next++ // leave a free cluster behind
				}
			}
			b.chainFAT(n.clusters)
		default:
			n.clusters = b.alloc(count)
			if n.f.FatChain {
				b.chainFAT(n.clusters)
			} else {
				n.noFat = true
			}
		}
		if n.f.Deleted {
			for _, c := range n.clusters {
				b.used[c-2] = false
			}
		}
	}
	// A deleted directory's children live in clusters that are free too; they
	// were allocated as part of the list, so free the subtree.
	for _, n := range b.nodes {
		if n.f.Deleted && n.dir {
			b.freeSubtree(n)
		}
	}
	for i, u := range b.used {
		if !u {
			b.lay.free = append(b.lay.free, uint32(i+2))
		}
	}
}

func (b *builder) freeSubtree(n *node) {
	for _, c := range n.children {
		for _, cl := range c.clusters {
			b.used[cl-2] = false
		}
		b.freeSubtree(c)
	}
}

func le16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func le32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func le64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

func (b *builder) clusterOff(c uint32) int { return int(b.lay.HeapOffset) + int(c-2)*b.cs }

func (b *builder) write() {
	b.writeFAT()
	b.writeBitmap()
	if !b.o.NoUpcase {
		copy(b.img[b.clusterOff(b.lay.UpcaseCluster):], b.upcaseTable())
	}
	b.writeDir(b.root)
	for _, n := range b.nodes {
		if !n.dir && len(n.clusters) > 0 {
			b.writeData(n)
		}
	}
	b.writeBoot()
}

func (b *builder) writeFAT() {
	for c, v := range b.fat {
		le32(b.img, int(b.lay.FatOffset)+4*c, v)
	}
}

func (b *builder) writeBitmap() {
	bm := make([]byte, len(b.lay.BitmapClusters)*b.cs)
	for i, u := range b.used {
		if u {
			bm[i/8] |= 1 << (i % 8)
		}
	}
	for i, c := range b.lay.BitmapClusters {
		copy(b.img[b.clusterOff(c):], bm[i*b.cs:(i+1)*b.cs])
	}
}

func (b *builder) writeData(n *node) {
	data := n.f.Data
	for i, c := range n.clusters {
		lo := i * b.cs
		hi := min(lo+b.cs, len(data))
		copy(b.img[b.clusterOff(c):], data[lo:hi])
	}
}

// upper is the up-case function of the image.
func (b *builder) upper(u uint16) uint16 {
	if b.o.Upcase != nil {
		return b.o.Upcase(u)
	}
	switch {
	case u >= 'a' && u <= 'z':
		return u - 0x20
	case u >= 0xE0 && u <= 0xFE && u != 0xF7:
		return u - 0x20
	case u == 0xFF:
		return 0x178
	}
	return u
}

// upcaseTable renders the table: the first 256 units mapped, then the other
// 65280 as one compressed run of identity entries (0xFFFF, count) when they
// are identity in the image's mapping.
func (b *builder) upcaseTable() []byte {
	var out []byte
	put := func(v uint16) { out = binary.LittleEndian.AppendUint16(out, v) }
	i := 0
	for i < 0x10000 {
		if b.upper(uint16(i)) != uint16(i) || i < 0x100 {
			put(b.upper(uint16(i)))
			i++
			continue
		}
		j := i
		for j < 0x10000 && b.upper(uint16(j)) == uint16(j) {
			j++
		}
		if j-i >= 3 {
			put(0xFFFF)
			put(uint16(j - i))
			i = j
			continue
		}
		put(b.upper(uint16(i)))
		i++
	}
	return out
}

func tableChecksum(data []byte) uint32 {
	var sum uint32
	for _, c := range data {
		sum = (sum&1)<<31 | sum>>1
		sum += uint32(c)
	}
	return sum
}

// enc encodes the wall-clock fields of t as an exFAT timestamp and the 10 ms
// increment (the odd second plus the sub-second part).
func enc(t time.Time, extra uint8) (uint32, uint8) {
	if t.IsZero() {
		t = defaultTime
	}
	y, mo, d := t.Date()
	h, mi, s := t.Clock()
	if y < 1980 || y > 2107 {
		panic("exfattest: year out of range")
	}
	v := uint32(y-1980)<<25 | uint32(mo)<<21 | uint32(d)<<16 | uint32(h)<<11 | uint32(mi)<<5 | uint32(s/2)
	inc := (s%2)*100 + t.Nanosecond()/10_000_000 + int(extra)
	if inc > 199 {
		panic("exfattest: 10ms increment out of range")
	}
	return v, uint8(inc)
}

func (o Offset) byteVal() byte {
	if !o.Valid {
		return 0
	}
	return 0x80 | byte(o.Quarters)&0x7F
}

func nameHash(b *builder, units []uint16) uint16 {
	var h uint16
	for _, u := range units {
		for _, c := range [2]byte{byte(b.upper(u)), byte(b.upper(u) >> 8)} {
			h = (h&1)<<15 | h>>1
			h += uint16(c)
		}
	}
	return h
}

func setChecksum(entries []byte) uint16 {
	var sum uint16
	for i, c := range entries {
		if i == 2 || i == 3 {
			continue
		}
		sum = (sum&1)<<15 | sum>>1
		sum += uint16(c)
	}
	return sum
}

// fileSet renders the entry set of n.
func (b *builder) fileSet(n *node) []byte {
	nn := nameEntries(len(n.name))
	set := make([]byte, entrySize*(2+nn))
	p, s := set[:entrySize], set[entrySize:2*entrySize]
	p[0] = typeFile
	p[1] = byte(1 + nn)
	attr := n.f.Attr
	if n.dir {
		attr |= 0x10
	}
	le16(p, 4, attr)
	cv, cinc := enc(n.f.Times[0], n.f.Inc10ms[0])
	mv, minc := enc(n.f.Times[1], n.f.Inc10ms[1])
	av, _ := enc(n.f.Times[2], 0)
	le32(p, 8, cv)
	le32(p, 12, mv)
	le32(p, 16, av)
	p[20], p[21] = cinc, minc
	p[22], p[23], p[24] = n.f.Offsets[0].byteVal(), n.f.Offsets[1].byteVal(), n.f.Offsets[2].byteVal()

	size := uint64(len(n.f.Data))
	if n.dir {
		size = uint64(len(n.clusters) * b.cs)
	}
	valid := size
	if n.f.ValidLength != nil {
		valid = uint64(*n.f.ValidLength)
	}
	s[0] = typeStream
	s[1] = 0x01
	if n.noFat {
		s[1] |= 0x02
	}
	s[3] = byte(len(n.name))
	le16(s, 4, nameHash(b, n.name))
	le64(s, 8, valid)
	if len(n.clusters) > 0 {
		le32(s, 20, n.clusters[0])
	}
	le64(s, 24, size)

	for i := 0; i < nn; i++ {
		e := set[entrySize*(2+i) : entrySize*(3+i)]
		e[0] = typeName
		for k := 0; k < namePerEntry; k++ {
			if idx := i*namePerEntry + k; idx < len(n.name) {
				le16(e, 2+2*k, n.name[idx])
			}
		}
	}

	liveSum := setChecksum(set)
	if n.f.Deleted {
		for i := 0; i < len(set); i += entrySize {
			set[i] &= 0x7F
		}
	}
	sum := setChecksum(set)
	if n.f.Deleted && n.f.DeletedStaleChecksum {
		sum = liveSum
	}
	if n.f.BadChecksum {
		sum = ^sum
	}
	le16(set, 2, sum)
	return set
}

func (b *builder) writeDir(d *node) {
	var buf []byte
	if d == b.root {
		bm := make([]byte, entrySize)
		bm[0] = typeBitmap
		le32(bm, 20, b.lay.BitmapCluster)
		le64(bm, 24, uint64((b.cc+7)/8))
		buf = append(buf, bm...)
		if !b.o.NoUpcase {
			uc := make([]byte, entrySize)
			uc[0] = typeUpcase
			tab := b.upcaseTable()
			le32(uc, 4, tableChecksum(tab))
			le32(uc, 20, b.lay.UpcaseCluster)
			le64(uc, 24, uint64(len(tab)))
			buf = append(buf, uc...)
		}
		if b.o.Label != "" {
			lab := make([]byte, entrySize)
			lab[0] = typeLabel
			u := utf16.Encode([]rune(b.o.Label))
			if len(u) > 11 {
				panic("exfattest: label longer than 11 units")
			}
			lab[1] = byte(len(u))
			for i, v := range u {
				le16(lab, 2+2*i, v)
			}
			buf = append(buf, lab...)
		}
	}
	dirFirst := d.clusters[0]
	for _, c := range d.children {
		idx := len(buf) / entrySize
		buf = append(buf, b.fileSet(c)...)
		loc := EntryLoc{Path: c.f.Path, Deleted: c.f.Deleted, Dir: c.dir, DirCluster: dirFirst, Index: idx, Clusters: c.clusters}
		if len(c.clusters) > 0 {
			loc.FirstCluster = c.clusters[0]
		}
		loc.Offset = int64(b.clusterOff(dirFirst)) + int64(idx)*entrySize
		b.lay.Entries = append(b.lay.Entries, loc)
	}
	// Directory clusters are contiguous, so the buffer maps linearly.
	copy(b.img[b.clusterOff(dirFirst):], buf)
	for _, c := range d.children {
		if c.dir {
			b.writeDir(c)
		}
	}
}

func (b *builder) writeBoot() {
	boot := b.img[:12*b.ss]
	boot[0], boot[1], boot[2] = 0xEB, 0x76, 0x90
	copy(boot[3:], "EXFAT   ")
	le64(boot, 72, uint64(b.volSecs))
	le32(boot, 80, fatStart)
	le32(boot, 84, uint32(b.fatSecs))
	le32(boot, 88, uint32(b.heapSecs))
	le32(boot, 92, uint32(b.cc))
	le32(boot, 96, b.lay.RootCluster)
	le32(boot, 100, b.o.Serial)
	le16(boot, 104, 0x0100)
	boot[108] = orDefault(b.o.BytesPerSectorShift, 9)
	boot[109] = b.spcShift
	boot[110] = 1
	boot[111] = 0x80
	boot[510], boot[511] = 0x55, 0xAA
	for s := 1; s <= 8; s++ { // extended boot sectors end with the 0xAA550000 signature
		boot[s*b.ss+b.ss-2], boot[s*b.ss+b.ss-1] = 0x55, 0xAA
	}
	FixBootChecksum(b.img)
	if b.o.BadBootChecksum {
		le32(b.img, 11*b.ss, binary.LittleEndian.Uint32(b.img[11*b.ss:])^0xDEADBEEF)
	}
	copy(b.img[12*b.ss:], b.img[:12*b.ss])
}

// BootChecksum computes the boot region checksum of the 11 sectors in region
// (ss bytes each), skipping VolumeFlags and PercentInUse.
func BootChecksum(region []byte) uint32 {
	var sum uint32
	for i, c := range region {
		if i == 106 || i == 107 || i == 112 {
			continue
		}
		sum = (sum&1)<<31 | sum>>1
		sum += uint32(c)
	}
	return sum
}

// FixBootChecksum recomputes the checksum sector (sector 11) of the main boot
// region of img, e.g. after a test patched the boot sector.
func FixBootChecksum(img []byte) {
	ss := 1 << img[108]
	sum := BootChecksum(img[:11*ss])
	for i := 0; i < ss; i += 4 {
		le32(img, 11*ss+i, sum)
	}
}

// FixSetChecksum recomputes the SetChecksum of the entry set whose primary
// entry is at image offset off (the entries must be contiguous in the image).
func FixSetChecksum(img []byte, off int64) {
	n := (int(img[off+1]) + 1) * entrySize
	le16(img, int(off)+2, setChecksum(img[off:int(off)+n]))
}
