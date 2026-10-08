package sqlitetest

import "fmt"

// Pointer-map types.
const (
	ptrRoot      = 1
	ptrFree      = 2
	ptrOverflow1 = 3
	ptrOverflow2 = 4
	ptrNonRoot   = 5
)

type ptrEntry struct {
	typ    byte
	parent uint32
}

// isPtrmap reports whether pgno is a pointer-map page of this database: with
// auto-vacuum on, page 2 and every usable/5+1 pages after it. (The page of the
// lock byte only exists in files over 1 GiB and is not modelled.)
func (b *Builder) isPtrmap(pgno uint32) bool {
	if b.o.AutoVacuum == 0 || pgno < 2 {
		return false
	}
	n := uint32(b.usable()/5 + 1)
	return (pgno-2)%n == 0
}

// ptrPageFor is the pointer-map page that describes pgno.
func (b *Builder) ptrPageFor(pgno uint32) uint32 {
	n := uint32(b.usable()/5 + 1)
	return (pgno-2)/n*n + 2
}

// WritePtrmap computes every pointer-map entry of an auto-vacuum database from
// the layout (a root page: type 1; a free page: 2; the first overflow page of a
// cell: 3, its parent the page holding the cell; a later overflow page: 4, its
// parent the previous overflow page; any other b-tree page: 5, its parent the
// interior page) and writes them at every Build. It panics for a database that
// is not auto-vacuum.
func (b *Builder) WritePtrmap() {
	if b.o.AutoVacuum == 0 {
		panic("sqlitetest: WritePtrmap needs an auto-vacuum database")
	}
	b.ptrmap = true
	b.dirty = true
}

// SetPtrmapEntry overrides one pointer-map entry after the computed ones (and
// writes it even when WritePtrmap was not called): hostile and mismatch tests.
func (b *Builder) SetPtrmapEntry(pgno uint32, typ byte, parent uint32) {
	if b.o.AutoVacuum == 0 {
		panic("sqlitetest: SetPtrmapEntry needs an auto-vacuum database")
	}
	if pgno < 3 || b.isPtrmap(pgno) {
		panic(fmt.Sprintf("sqlitetest: page %d has no pointer-map entry", pgno))
	}
	if b.ptrSet == nil {
		b.ptrSet = map[uint32]ptrEntry{}
	}
	b.ptrSet[pgno] = ptrEntry{typ, parent}
	b.dirty = true
}

// layoutPtrmap writes the pointer-map pages.
func (b *Builder) layoutPtrmap() {
	if b.o.AutoVacuum == 0 {
		return
	}
	entries := map[uint32]ptrEntry{}
	if b.ptrmap {
		for _, t := range b.objs {
			if !t.dropped {
				entries[t.root] = ptrEntry{ptrRoot, 0}
				b.walkTree(entries, t.root, 0)
			}
		}
		b.walkTree(entries, 1, 100)
		for _, p := range b.freeList {
			entries[p] = ptrEntry{ptrFree, 0}
		}
	}
	for pg, e := range b.ptrSet {
		entries[pg] = e
	}
	for pg, e := range entries {
		if pg < 3 || int(pg) > len(b.pages) || b.isPtrmap(pg) {
			continue
		}
		r := b.ptrPageFor(pg)
		off := 5 * int(pg-r-1)
		if int(r) > len(b.pages) || off+5 > len(b.pages[r-1]) {
			continue
		}
		page := b.pages[r-1]
		page[off] = e.typ
		put32(page[off+1:], e.parent)
	}
}

// getVarint reads an SQLite varint (the builder's own decoder).
func getVarint(p []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(p); i++ {
		v = v<<7 | uint64(p[i]&0x7f)
		if p[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(p) >= 9 {
		return v<<8 | uint64(p[8]), 9
	}
	return 0, 0
}

// walkTree records the non-root pages, and the overflow chains, of the b-tree
// whose page pgno has its header at base.
func (b *Builder) walkTree(entries map[uint32]ptrEntry, pgno uint32, base int) {
	page := b.pages[pgno-1]
	flag := page[base]
	hdr := 8
	interior := flag == 0x02 || flag == 0x05
	if interior {
		hdr = 12
	}
	n := int(page[base+3])<<8 | int(page[base+4])
	if interior {
		child := func(c uint32) {
			entries[c] = ptrEntry{ptrNonRoot, pgno}
			b.walkTree(entries, c, 0)
		}
		for i := range n {
			off := int(page[base+hdr+2*i])<<8 | int(page[base+hdr+2*i+1])
			child(get32(page[off:]))
		}
		child(get32(page[base+8:]))
	}
	if flag == 0x05 {
		return
	}
	for i := range n {
		off := int(page[base+hdr+2*i])<<8 | int(page[base+hdr+2*i+1])
		pos := off
		if interior {
			pos += 4
		}
		p, k := getVarint(page[pos:])
		pos += k
		if flag == 0x0d {
			_, k = getVarint(page[pos:])
			pos += k
		}
		local, spills := localSize(b.usable(), flag == 0x0d, int(p))
		if !spills {
			continue
		}
		pos += local
		prev, parent, typ := get32(page[pos:]), pgno, byte(ptrOverflow1)
		for prev != 0 && int(prev) <= len(b.pages) {
			entries[prev] = ptrEntry{typ, parent}
			parent, typ = prev, ptrOverflow2
			prev = get32(b.pages[parent-1])
		}
	}
}
