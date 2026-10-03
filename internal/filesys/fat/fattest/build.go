// Package fattest synthesizes small, deterministic FAT12/16/32 images for unit
// and hostile-input tests of the FAT reader. It is an independent encoder of
// the on-disk format (it shares no code with the reader), so a reader/builder
// pair that agrees is evidence the format is understood, not that one side
// mirrors the other. Real mkfs.fat images live under tools/fixtures.
//
// Build writes a boot sector (and, for FAT32, the FSInfo sector and the backup
// boot sector), every FAT copy, the root directory (a fixed region for
// FAT12/16, a cluster chain for FAT32), directories and file data. Options and
// File only ever gain fields, so existing callers keep working.
package fattest

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// File describes one file or directory to place in the image. A directory must
// be listed before anything inside it. Entries are written to their directory
// in the order given.
type File struct {
	Path string // "/a/b.txt" or "a/b.txt"
	Data []byte
	Dir  bool

	// Deleted writes the entry and then deletes it the way an OS does: the first
	// byte of its short entry and of every LFN entry becomes 0xE5 and the FAT
	// chain is freed (zeroed). The clusters stay reserved in the builder so no
	// later file reuses them, and the content stays on disk.
	Deleted bool

	// Fragmented interleaves the file's clusters with the next file's: it takes
	// every other free cluster, leaving the ones between for whatever is
	// allocated after it. (No effect on a file of fewer than two clusters.)
	Fragmented bool

	// LongName stores Path's last component in VFAT long-name entries (up to 255
	// UTF-16 units) and gives the entry a generated short name ("LONGNA~1.TXT").
	// Without it the name must be a valid 8.3 name; a name whose letters are all
	// lower case (per part) sets the NTRes lower-case flags like Windows does.
	LongName bool

	// Times are the create, modify and access times written to the entry (the
	// access time keeps only its date). A zero time writes zeros.
	Times [3]time.Time

	// Attr is OR-ed into the entry's attribute byte (the directory bit is set
	// for a Dir).
	Attr byte
}

// Options selects the geometry of the image. Zero values mean: SectorSize 512,
// SecPerClus 1, NumFATs 2, a TotalSectors that yields about 2048 (FAT12), 8192
// (FAT16) or 65600 (FAT32) clusters, no volume label and VolID 0.
type Options struct {
	Type         int // 12, 16 or 32
	SectorSize   int
	SecPerClus   int
	NumFATs      int
	TotalSectors uint32
	Label        string // up to 11 characters; also written as a root-directory label entry
	VolID        uint32
}

// Geometry is the layout Build derives from Options.
type Geometry struct {
	Type           int
	SectorSize     int
	SecPerClus     int
	NumFATs        int
	Reserved       uint32 // reserved sectors (1, or 32 for FAT32)
	FATSectors     uint32 // sectors per FAT copy
	RootEntries    uint32 // 512 for FAT12/16, 0 for FAT32
	RootDirSectors uint32
	TotalSectors   uint32
	Clusters       uint32 // CountOfClusters
}

// DataStart returns the first sector of the data area (cluster 2).
func (g Geometry) DataStart() uint32 {
	return g.Reserved + uint32(g.NumFATs)*g.FATSectors + g.RootDirSectors
}

// FATStart returns the first sector of FAT copy n.
func (g Geometry) FATStart(n int) uint32 { return g.Reserved + uint32(n)*g.FATSectors }

// ClusterOffset returns the byte offset of cluster c.
func (g Geometry) ClusterOffset(c uint32) int64 {
	return (int64(g.DataStart()) + int64(c-2)*int64(g.SecPerClus)) * int64(g.SectorSize)
}

const (
	media = 0xF8

	attrVolume = 0x08
	attrDir    = 0x10
	attrLFN    = 0x0F
)

// fatBytes returns the bytes n FAT entries occupy.
func fatBytes(typ int, n uint64) uint64 {
	switch typ {
	case 12:
		return (n*3 + 1) / 2
	case 16:
		return n * 2
	default:
		return n * 4
	}
}

func (o Options) withDefaults() Options {
	if o.Type != 12 && o.Type != 16 && o.Type != 32 {
		panic(fmt.Sprintf("fattest: Type %d (want 12, 16 or 32)", o.Type))
	}
	if o.SectorSize == 0 {
		o.SectorSize = 512
	}
	if o.SecPerClus == 0 {
		o.SecPerClus = 1
	}
	if o.NumFATs == 0 {
		o.NumFATs = 2
	}
	if len(o.Label) > 11 {
		panic(fmt.Sprintf("fattest: label %q is longer than 11 characters", o.Label))
	}
	if o.TotalSectors == 0 {
		target := map[int]uint64{12: 2048, 16: 8192, 32: 65600}[o.Type]
		rsvd, rootEnt := reservedAndRoot(o.Type)
		rds := (uint64(rootEnt)*32 + uint64(o.SectorSize) - 1) / uint64(o.SectorSize)
		fs := (fatBytes(o.Type, target+2) + uint64(o.SectorSize) - 1) / uint64(o.SectorSize)
		o.TotalSectors = uint32(uint64(rsvd) + rds + uint64(o.NumFATs)*fs + target*uint64(o.SecPerClus))
	}
	return o
}

func reservedAndRoot(typ int) (rsvd, rootEnt uint32) {
	if typ == 32 {
		return 32, 0
	}
	return 1, 512
}

// Layout returns the geometry Build uses for o.
func Layout(o Options) Geometry {
	o = o.withDefaults()
	g := Geometry{Type: o.Type, SectorSize: o.SectorSize, SecPerClus: o.SecPerClus, NumFATs: o.NumFATs, TotalSectors: o.TotalSectors}
	g.Reserved, g.RootEntries = reservedAndRoot(o.Type)
	bps := uint64(o.SectorSize)
	g.RootDirSectors = uint32((uint64(g.RootEntries)*32 + bps - 1) / bps)
	fixed := uint64(g.Reserved) + uint64(g.RootDirSectors)
	if uint64(o.TotalSectors) <= fixed+uint64(o.NumFATs) {
		panic(fmt.Sprintf("fattest: %d sectors are too few", o.TotalSectors))
	}
	// The smallest FAT that describes the clusters left over after it.
	fs := uint64(1)
	for {
		rest := uint64(o.TotalSectors) - fixed - uint64(o.NumFATs)*fs
		if uint64(o.NumFATs)*fs >= uint64(o.TotalSectors)-fixed {
			panic(fmt.Sprintf("fattest: %d sectors are too few", o.TotalSectors))
		}
		count := rest / uint64(o.SecPerClus)
		need := (fatBytes(o.Type, count+2) + bps - 1) / bps
		if need <= fs {
			g.Clusters = uint32(count)
			break
		}
		fs = need
	}
	g.FATSectors = uint32(fs)
	return g
}

// Build returns a FAT image holding files.
func Build(o Options, files []File) []byte {
	o = o.withDefaults()
	g := Layout(o)
	if g.Clusters == 0 {
		panic("fattest: no data clusters")
	}
	bps := g.SectorSize
	cs := bps * g.SecPerClus
	img := make([]byte, int(g.TotalSectors)*bps)

	b := &builder{g: g, o: o, img: img, cs: cs, used: make([]bool, g.Clusters+2), fat: make([]uint32, g.Clusters+2)}
	b.tree(files)
	b.allocate()
	b.writeData()
	b.writeFATs()
	b.writeBoot()
	return img
}

type node struct {
	f      *File
	name   string
	parent *dirNode
	dir    *dirNode // contents, for a directory
	short  [11]byte
	ntres  byte
	lfn    []uint16 // nil when the name has no long-name entries
	chain  []uint32
	slots  int // directory entries the node occupies (LFN entries + 1)
}

type dirNode struct {
	self   *node // nil for the root
	kids   []*node
	shorts map[[11]byte]bool
	chain  []uint32 // clusters of a directory (FAT32 root too)
}

func (d *dirNode) first() uint32 {
	if d.self == nil || len(d.chain) == 0 {
		return 0
	}
	return d.chain[0]
}

type builder struct {
	g     Geometry
	o     Options
	img   []byte
	cs    int
	root  *dirNode
	nodes []*node
	used  []bool
	fat   []uint32
}

func (b *builder) tree(files []File) {
	b.root = &dirNode{shorts: map[[11]byte]bool{}}
	dirs := map[string]*dirNode{"": b.root}
	for i := range files {
		f := &files[i]
		comps := strings.Split(strings.Trim(f.Path, "/"), "/")
		name := comps[len(comps)-1]
		if name == "" {
			panic("fattest: empty path")
		}
		parent, ok := dirs[strings.Join(comps[:len(comps)-1], "/")]
		if !ok {
			panic(fmt.Sprintf("fattest: parent directory of %q is not listed before it", f.Path))
		}
		n := &node{f: f, name: name, parent: parent}
		n.makeNames(parent)
		parent.kids = append(parent.kids, n)
		b.nodes = append(b.nodes, n)
		if f.Dir {
			n.dir = &dirNode{self: n, shorts: map[[11]byte]bool{}}
			dirs[strings.Join(comps, "/")] = n.dir
		}
	}
}

const shortChars = "$%'-_@~`!(){}^#&"

func validShortChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.IndexByte(shortChars, c) >= 0
}

// splitName splits at the last dot; a leading dot is part of the base.
func splitName(name string) (base, ext string) {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

// caseOf reports whether s has lower-case and/or upper-case letters.
func caseOf(s string) (lower, upper bool) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		}
	}
	return lower, upper
}

func padName(base, ext string) (n [11]byte) {
	for i := range n {
		n[i] = ' '
	}
	copy(n[:8], strings.ToUpper(base))
	copy(n[8:], strings.ToUpper(ext))
	return n
}

func (n *node) makeNames(parent *dirNode) {
	base, ext := splitName(n.name)
	fits := len(base) >= 1 && len(base) <= 8 && len(ext) <= 3
	for _, c := range []byte(base + ext) {
		fits = fits && validShortChar(c)
	}
	if !n.f.LongName {
		if !fits {
			panic(fmt.Sprintf("fattest: %q is not a valid 8.3 name (set LongName)", n.name))
		}
		bl, bu := caseOf(base)
		el, eu := caseOf(ext)
		if bl && bu || el && eu {
			panic(fmt.Sprintf("fattest: %q mixes letter cases within a part (set LongName)", n.name))
		}
		if bl {
			n.ntres |= 0x08
		}
		if el {
			n.ntres |= 0x10
		}
		n.short = padName(base, ext)
		if parent.shorts[n.short] {
			panic(fmt.Sprintf("fattest: duplicate short name %q", n.name))
		}
		parent.shorts[n.short] = true
		n.slots = 1
		return
	}
	n.lfn = utf16.Encode([]rune(n.name))
	if len(n.lfn) > 255 {
		panic(fmt.Sprintf("fattest: long name of %d UTF-16 units (max 255)", len(n.lfn)))
	}
	n.slots = 1 + (len(n.lfn)+12)/13
	if fits && strings.IndexFunc(n.name, func(r rune) bool { return r > 0x7F }) < 0 {
		if s := padName(base, ext); !parent.shorts[s] {
			n.short = s
			parent.shorts[s] = true
			return
		}
	}
	clean := func(s string, limit int) string {
		var o []byte
		for _, r := range strings.ToUpper(s) {
			switch {
			case r > 0x7F:
				o = append(o, '_')
			case validShortChar(byte(r)):
				o = append(o, byte(r))
			}
		}
		if len(o) > limit {
			o = o[:limit]
		}
		return string(o)
	}
	cb, ce := clean(base, 6), clean(ext, 3)
	for i := 1; ; i++ {
		tail := fmt.Sprintf("~%d", i)
		stem := cb
		if room := 8 - len(tail); len(stem) > room {
			stem = stem[:room]
		}
		s := padName(stem+tail, ce)
		if !parent.shorts[s] {
			n.short = s
			parent.shorts[s] = true
			return
		}
	}
}

func (b *builder) entriesOf(d *dirNode) int {
	n := 0
	if d.self != nil {
		n += 2 // "." and ".."
	} else if b.o.Label != "" {
		n++
	}
	for _, k := range d.kids {
		n += k.slots
	}
	return n
}

func (b *builder) clustersFor(bytes int) int { return (bytes + b.cs - 1) / b.cs }

// alloc reserves n clusters: the lowest free ones, or with frag every other
// free one.
func (b *builder) alloc(n int, frag bool) []uint32 {
	var out []uint32
	skip := false
	for c := uint32(2); c < uint32(len(b.used)) && len(out) < n; c++ {
		if b.used[c] {
			continue
		}
		if frag && n > 1 && skip {
			skip = false
			continue
		}
		b.used[c] = true
		out = append(out, c)
		skip = true
	}
	if len(out) < n {
		panic("fattest: image is full")
	}
	return out
}

func (b *builder) allocate() {
	if b.g.Type == 32 {
		b.root.chain = b.alloc(max(1, b.clustersFor(b.entriesOf(b.root)*32)), false)
	} else if b.entriesOf(b.root) > int(b.g.RootEntries) {
		panic(fmt.Sprintf("fattest: %d root entries do not fit in %d", b.entriesOf(b.root), b.g.RootEntries))
	}
	for _, n := range b.nodes {
		if n.dir != nil {
			n.dir.chain = b.alloc(max(1, b.clustersFor(b.entriesOf(n.dir)*32)), false)
			n.chain = n.dir.chain
			continue
		}
		if c := b.clustersFor(len(n.f.Data)); c > 0 {
			n.chain = b.alloc(c, n.f.Fragmented)
		}
	}
	for _, n := range b.nodes {
		if n.f.Deleted {
			continue // chain freed
		}
		for i, c := range n.chain {
			if i+1 < len(n.chain) {
				b.fat[c] = n.chain[i+1]
			} else {
				b.fat[c] = b.eoc()
			}
		}
	}
	if b.g.Type == 32 {
		for i, c := range b.root.chain {
			if i+1 < len(b.root.chain) {
				b.fat[c] = b.root.chain[i+1]
			} else {
				b.fat[c] = b.eoc()
			}
		}
	}
	b.fat[0] = 0x0FFFFF00 | media
	b.fat[1] = b.eoc()
}

func (b *builder) eoc() uint32 {
	switch b.g.Type {
	case 12:
		return 0xFFF
	case 16:
		return 0xFFFF
	}
	return 0x0FFFFFFF
}

func (b *builder) writeData() {
	for _, n := range b.nodes {
		if n.dir != nil {
			continue
		}
		for i, c := range n.chain {
			off := b.g.ClusterOffset(c)
			lo := i * b.cs
			copy(b.img[off:off+int64(b.cs)], n.f.Data[lo:min(len(n.f.Data), lo+b.cs)])
		}
	}
	b.writeDir(b.root)
	for _, n := range b.nodes {
		if n.dir != nil {
			b.writeDir(n.dir)
		}
	}
}

func lfnChecksum(short [11]byte) byte {
	var sum byte
	for _, c := range short {
		sum = (sum&1)<<7 + sum>>1 + c
	}
	return sum
}

func dosDate(t time.Time) uint16 {
	if t.IsZero() {
		return 0
	}
	if y := t.Year(); y < 1980 || y > 2107 {
		panic(fmt.Sprintf("fattest: year %d is not representable", y))
	}
	return uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day())
}

func dosTime(t time.Time) uint16 {
	if t.IsZero() {
		return 0
	}
	return uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)
}

func tenth(t time.Time) byte {
	if t.IsZero() {
		return 0
	}
	return byte(t.Second()%2*100 + t.Nanosecond()/10_000_000)
}

// shortEntry encodes a 32-byte directory entry.
func shortEntry(name [11]byte, attr, ntres byte, times [3]time.Time, first uint32, size uint32) []byte {
	e := make([]byte, 32)
	copy(e, name[:])
	e[11] = attr
	e[12] = ntres
	e[13] = tenth(times[0])
	binary.LittleEndian.PutUint16(e[14:], dosTime(times[0]))
	binary.LittleEndian.PutUint16(e[16:], dosDate(times[0]))
	binary.LittleEndian.PutUint16(e[18:], dosDate(times[2]))
	binary.LittleEndian.PutUint16(e[20:], uint16(first>>16))
	binary.LittleEndian.PutUint16(e[22:], dosTime(times[1]))
	binary.LittleEndian.PutUint16(e[24:], dosDate(times[1]))
	binary.LittleEndian.PutUint16(e[26:], uint16(first))
	binary.LittleEndian.PutUint32(e[28:], size)
	return e
}

// lfnEntries encodes the long-name entries of n, highest ordinal first (the
// on-disk order).
func lfnEntries(n *node, deleted bool) [][]byte {
	count := (len(n.lfn) + 12) / 13
	sum := lfnChecksum(n.short)
	pos := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
	var out [][]byte
	for ord := count; ord >= 1; ord-- {
		e := make([]byte, 32)
		e[0] = byte(ord)
		if ord == count {
			e[0] |= 0x40
		}
		if deleted {
			e[0] = 0xE5
		}
		e[11] = attrLFN
		e[13] = sum
		for k, p := range pos {
			i := (ord-1)*13 + k
			var u uint16
			switch {
			case i < len(n.lfn):
				u = n.lfn[i]
			case i == len(n.lfn):
				u = 0
			default:
				u = 0xFFFF
			}
			binary.LittleEndian.PutUint16(e[p:], u)
		}
		out = append(out, e)
	}
	return out
}

func (b *builder) writeDir(d *dirNode) {
	var buf []byte
	if d.self == nil {
		if b.o.Label != "" {
			var name [11]byte
			copy(name[:], strings.Repeat(" ", 11))
			copy(name[:], b.o.Label)
			buf = append(buf, shortEntry(name, attrVolume, 0, [3]time.Time{}, 0, 0)...)
		}
	} else {
		self := d.self
		parent := d.self.parent.first()
		dot := [11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
		dotdot := [11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
		attr := byte(attrDir) | self.f.Attr
		buf = append(buf, shortEntry(dot, attr, 0, self.f.Times, d.first(), 0)...)
		buf = append(buf, shortEntry(dotdot, attr, 0, self.f.Times, parent, 0)...)
	}
	for _, k := range d.kids {
		if k.lfn != nil {
			for _, e := range lfnEntries(k, k.f.Deleted) {
				buf = append(buf, e...)
			}
		}
		attr := k.f.Attr
		var first, size uint32
		if k.f.Dir {
			attr |= attrDir
		} else {
			size = uint32(len(k.f.Data))
		}
		if len(k.chain) > 0 {
			first = k.chain[0]
		}
		e := shortEntry(k.short, attr, k.ntres, k.f.Times, first, size)
		if k.f.Deleted {
			e[0] = 0xE5
		}
		buf = append(buf, e...)
	}
	if d.self == nil && b.g.Type != 32 {
		off := int64(b.g.FATStart(b.g.NumFATs)) * int64(b.g.SectorSize)
		copy(b.img[off:], buf)
		return
	}
	for i, c := range d.chain {
		off := b.g.ClusterOffset(c)
		lo := i * b.cs
		if lo < len(buf) {
			copy(b.img[off:off+int64(b.cs)], buf[lo:min(len(buf), lo+b.cs)])
		}
	}
}

func (b *builder) writeFATs() {
	bps := b.g.SectorSize
	table := make([]byte, int(b.g.FATSectors)*bps)
	for c, v := range b.fat {
		switch b.g.Type {
		case 12:
			o := c + c/2
			if c&1 == 0 {
				table[o] = byte(v)
				table[o+1] = table[o+1]&0xF0 | byte(v>>8)&0x0F
			} else {
				table[o] = table[o]&0x0F | byte(v<<4)
				table[o+1] = byte(v >> 4)
			}
		case 16:
			binary.LittleEndian.PutUint16(table[c*2:], uint16(v))
		default:
			binary.LittleEndian.PutUint32(table[c*4:], v)
		}
	}
	for i := 0; i < b.g.NumFATs; i++ {
		copy(b.img[int(b.g.FATStart(i))*bps:], table)
	}
}

func (b *builder) writeBoot() {
	g := b.g
	s := make([]byte, g.SectorSize)
	pad := func(off int, v string, n int) {
		copy(s[off:off+n], v+strings.Repeat(" ", n))
	}
	if g.Type == 32 {
		copy(s, []byte{0xEB, 0x58, 0x90})
	} else {
		copy(s, []byte{0xEB, 0x3C, 0x90})
	}
	copy(s[3:], "MSWIN4.1")
	binary.LittleEndian.PutUint16(s[11:], uint16(g.SectorSize))
	s[13] = byte(g.SecPerClus)
	binary.LittleEndian.PutUint16(s[14:], uint16(g.Reserved))
	s[16] = byte(g.NumFATs)
	binary.LittleEndian.PutUint16(s[17:], uint16(g.RootEntries))
	if g.Type != 32 && g.TotalSectors < 0x10000 {
		binary.LittleEndian.PutUint16(s[19:], uint16(g.TotalSectors))
	} else {
		binary.LittleEndian.PutUint32(s[32:], g.TotalSectors)
	}
	s[21] = media
	if g.Type != 32 {
		binary.LittleEndian.PutUint16(s[22:], uint16(g.FATSectors))
	}
	binary.LittleEndian.PutUint16(s[24:], 63)
	binary.LittleEndian.PutUint16(s[26:], 255)
	label := b.o.Label
	if label == "" {
		label = "NO NAME"
	}
	if g.Type == 32 {
		binary.LittleEndian.PutUint32(s[36:], g.FATSectors)
		binary.LittleEndian.PutUint32(s[44:], 2)
		binary.LittleEndian.PutUint16(s[48:], 1)
		binary.LittleEndian.PutUint16(s[50:], 6)
		s[64] = 0x80
		s[66] = 0x29
		binary.LittleEndian.PutUint32(s[67:], b.o.VolID)
		pad(71, label, 11)
		pad(82, "FAT32", 8)
	} else {
		s[36] = 0x80
		s[38] = 0x29
		binary.LittleEndian.PutUint32(s[39:], b.o.VolID)
		pad(43, label, 11)
		pad(54, fmt.Sprintf("FAT%d", g.Type), 8)
	}
	s[510], s[511] = 0x55, 0xAA
	copy(b.img, s)
	if g.Type != 32 {
		return
	}
	free := 0
	for c := 2; c < len(b.fat); c++ {
		if b.fat[c] == 0 && !b.used[c] {
			free++
		}
	}
	fsinfo := make([]byte, g.SectorSize)
	binary.LittleEndian.PutUint32(fsinfo[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fsinfo[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fsinfo[488:], uint32(free))
	binary.LittleEndian.PutUint32(fsinfo[492:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(fsinfo[508:], 0xAA550000)
	copy(b.img[g.SectorSize:], fsinfo)
	copy(b.img[6*g.SectorSize:], s)
	copy(b.img[7*g.SectorSize:], fsinfo)
}
