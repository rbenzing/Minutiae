package sqlitetest

import (
	"fmt"
	"slices"
)

// Image is a copy of all page images of a builder at one moment, for WAL and
// journal scenarios.
type Image struct{ pages [][]byte }

// Page returns page n (1-based) of the image; it panics for a page the image
// does not have.
func (i *Image) Page(n uint32) []byte {
	if n == 0 || int(n) > len(i.pages) {
		panic(fmt.Sprintf("sqlitetest: image has no page %d (it has %d)", n, len(i.pages)))
	}
	return i.pages[n-1]
}

// Pages is the number of pages in the image.
func (i *Image) Pages() uint32 { return uint32(len(i.pages)) }

// Snapshot builds the database and returns a copy of every page.
func (b *Builder) Snapshot() *Image {
	b.settle()
	img := &Image{pages: make([][]byte, len(b.pages))}
	for i, p := range b.pages {
		img.pages[i] = append([]byte(nil), p...)
	}
	return img
}

func (b *Builder) usable() int { return b.o.PageSize - b.o.Reserved }

// alloc appends a zeroed page and returns its number.
func (b *Builder) alloc() uint32 {
	b.pages = append(b.pages, make([]byte, b.o.PageSize))
	return uint32(len(b.pages))
}

// settle builds the database if rows or objects changed since the last Build.
func (b *Builder) settle() {
	if b.dirty {
		b.Build()
	}
}

// Build lays out leaves, interior levels, overflow chains, the schema and the
// freelist from the current rows. It is idempotent: building again from the
// same state gives the same bytes. Calls to Patch and SetHeaderPages made
// earlier are applied again on top of the new layout.
func (b *Builder) Build() {
	if len(b.objs) == 0 && !b.dirty {
		return
	}
	b.pages = nil
	b.freed = nil
	b.alloc() // page 1
	for _, t := range b.objs {
		t.root = b.alloc()
	}
	for _, t := range b.objs {
		t.layout()
		if t.dropped {
			b.freed = append(b.freed, t.pages...)
		}
	}
	b.layoutSchema()
	trunk, count := b.layoutFreelist()

	h := b.pages[0]
	writeHeader(h, b.o)
	put32(h[28:], uint32(len(b.pages)))
	put32(h[32:], trunk)
	put32(h[36:], count)
	live := 0
	for _, t := range b.objs {
		if !t.dropped {
			live++
		}
	}
	put32(h[40:], uint32(live)) // schema cookie
	if b.hdrPages != nil {
		put32(h[28:], b.hdrPages.n)
		counter := get32(h[24:])
		if !b.hdrPages.valid {
			counter++
		}
		put32(h[92:], counter)
	}
	for _, p := range b.patches {
		if p.off >= 0 && p.off+len(p.p) <= len(b.pages)*b.o.PageSize {
			for i, v := range p.p {
				b.pages[(p.off+i)/b.o.PageSize][(p.off+i)%b.o.PageSize] = v
			}
		}
	}
	b.dirty = false
}

// layoutSchema writes the schema table (page 1): one row per live object.
func (b *Builder) layoutSchema() {
	s := &Table{b: b, name: "sqlite_schema", root: 1}
	rowid := int64(0)
	for _, t := range b.objs {
		if t.dropped {
			continue
		}
		rowid++
		kind, tbl := "table", t.name
		if t.index {
			kind, tbl = "index", t.parent.name
		}
		s.rows = append(s.rows, s.newRow(rowid, []any{kind, t.name, tbl, int64(t.root), t.sql}))
	}
	s.layout()
}

// layoutFreelist writes the freelist trunks over the first pages of b.freed
// (the rest are leaves; their contents stay as they were) and returns the
// first trunk and the number of pages on the list.
func (b *Builder) layoutFreelist() (trunk, count uint32) {
	if len(b.freed) == 0 {
		return 0, 0
	}
	free := slices.Clone(b.freed)
	slices.Sort(free)
	free = slices.Compact(free) // a dropped table lists the overflow pages of its residue cells twice
	maxLeaves := b.usable()/4 - 2
	trunk = free[0]
	for i := 0; i < len(free); {
		page := b.pages[free[i]-1]
		leaves := free[i+1 : min(i+1+maxLeaves, len(free))]
		next := uint32(0)
		if i+1+len(leaves) < len(free) {
			next = free[i+1+len(leaves)]
		}
		put32(page, next)
		put32(page[4:], uint32(len(leaves)))
		for k, l := range leaves {
			put32(page[8+4*k:], l)
		}
		i += 1 + len(leaves)
	}
	return trunk, uint32(len(free))
}
