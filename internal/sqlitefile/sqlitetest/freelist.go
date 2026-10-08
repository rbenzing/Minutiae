package sqlitetest

import "fmt"

// Free puts pages on the freelist, in the trunk and leaf layout the engine
// writes (trunks of usable/4-2 leaves, chained in ascending page order), and
// leaves their bytes as they are (a trunk page gets its next-trunk, count and
// leaf slots written over the start of the page). A page beyond the end of the
// file extends the file with a zeroed page, so tests can free pages that no
// structure uses. It panics for page 0 and 1.
func (b *Builder) Free(pages ...uint32) {
	for _, p := range pages {
		if p < 2 {
			panic(fmt.Sprintf("sqlitetest: page %d cannot be freed", p))
		}
		b.userFree = append(b.userFree, p)
	}
	b.dirty = true
}

// SetFreelist lays the freelist out as given: each element is a trunk page
// followed by its leaf pages; trunk i points at trunk i+1 and the last points at
// 0; the header holds the first trunk and the number of pages named. Every page
// named that the file does not have extends it with a zeroed page. It replaces
// the layout Free and dropped tables would give. Hostile shapes (a cycle, a
// wrong leaf count, an out-of-range leaf) are made by Patch on top of it.
func (b *Builder) SetFreelist(trunks [][]uint32) {
	b.explicit = nil
	for _, t := range trunks {
		if len(t) == 0 {
			panic("sqlitetest: an empty trunk")
		}
		b.explicit = append(b.explicit, append([]uint32(nil), t...))
	}
	b.hasExplicit = true
	b.dirty = true
}

// extendTo appends zeroed pages until the file has n pages.
func (b *Builder) extendTo(n uint32) {
	for uint32(len(b.pages)) < n {
		b.pages = append(b.pages, make([]byte, b.o.PageSize))
	}
}

// layoutExplicit writes the trunks of SetFreelist and returns the header
// values.
func (b *Builder) layoutExplicit() (trunk, count uint32) {
	var maxPage uint32
	for _, t := range b.explicit {
		for _, p := range t {
			maxPage = max(maxPage, p)
		}
	}
	b.extendTo(maxPage)
	b.freeList = nil
	for i, t := range b.explicit {
		page := b.pages[t[0]-1]
		next := uint32(0)
		if i+1 < len(b.explicit) {
			next = b.explicit[i+1][0]
		}
		put32(page, next)
		put32(page[4:], uint32(len(t)-1))
		for k, l := range t[1:] {
			if 8+4*k+4 <= len(page) {
				put32(page[8+4*k:], l)
			}
		}
		b.freeList = append(b.freeList, t...)
		count += uint32(len(t))
	}
	return b.explicit[0][0], count
}
