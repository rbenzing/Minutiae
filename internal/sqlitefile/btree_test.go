package sqlitefile_test

import (
	"errors"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// pageWith returns a zeroed page of size bytes with a b-tree header written at
// base: flag, first freeblock, cell count, content start (as stored),
// fragmented bytes, right child, then the cell pointers.
func pageWith(size, base int, flag byte, freeblock, count, content uint16, frag byte, right uint32, ptrs ...uint16) []byte {
	p := make([]byte, size)
	p[base] = flag
	p[base+1], p[base+2] = byte(freeblock>>8), byte(freeblock)
	p[base+3], p[base+4] = byte(count>>8), byte(count)
	p[base+5], p[base+6] = byte(content>>8), byte(content)
	p[base+7] = frag
	hdr := 8
	if flag == 0x02 || flag == 0x05 {
		hdr = 12
		p[base+8], p[base+9], p[base+10], p[base+11] = byte(right>>24), byte(right>>16), byte(right>>8), byte(right)
	}
	for i, v := range ptrs {
		p[base+hdr+2*i], p[base+hdr+2*i+1] = byte(v>>8), byte(v)
	}
	return p
}

func TestPageHeaderParse(t *testing.T) {
	good := []struct {
		name string
		page []byte
		pgno uint32
		want sqlitefile.PageHeader
	}{
		{
			"table leaf", pageWith(4096, 0, 0x0d, 0x0123, 3, 0x0f00, 5, 0), 2,
			sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, FirstFreeblock: 0x123, CellCount: 3, ContentStart: 0xf00, Fragmented: 5, HeaderSize: 8},
		},
		{
			"index leaf", pageWith(4096, 0, 0x0a, 0, 0, 4096&0xffff, 0, 0), 9,
			sqlitefile.PageHeader{Type: sqlitefile.PageIndexLeaf, ContentStart: 4096, HeaderSize: 8},
		},
		{
			"table interior with right child", pageWith(1024, 0, 0x05, 0, 2, 0x0300, 0, 0x01020304), 3,
			sqlitefile.PageHeader{Type: sqlitefile.PageTableInterior, CellCount: 2, ContentStart: 0x300, RightChild: 0x01020304, HeaderSize: 12},
		},
		{
			"index interior", pageWith(1024, 0, 0x02, 0, 1, 0x0300, 0, 77), 3,
			sqlitefile.PageHeader{Type: sqlitefile.PageIndexInterior, CellCount: 1, ContentStart: 0x300, RightChild: 77, HeaderSize: 12},
		},
		{
			"page 1 header at 100", pageWith(4096, 100, 0x0d, 0, 4, 0x0e00, 0, 0), 1,
			sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, CellCount: 4, ContentStart: 0xe00, Base: 100, HeaderSize: 8},
		},
		{
			"page 1 interior", pageWith(4096, 100, 0x05, 0, 1, 0x0e00, 0, 5), 1,
			sqlitefile.PageHeader{Type: sqlitefile.PageTableInterior, CellCount: 1, ContentStart: 0xe00, RightChild: 5, Base: 100, HeaderSize: 12},
		},
		{
			"content start 0 means 65536", pageWith(65536, 0, 0x0d, 0, 0, 0, 0, 0), 2,
			sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, ContentStart: 65536, HeaderSize: 8},
		},
		{"flag byte at 0 on page 1 is not read", func() []byte {
			p := pageWith(512, 100, 0x0d, 0, 0, 512, 0, 0)
			p[0] = 0xff // belongs to the database header
			return p
		}(), 1, sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, ContentStart: 512, Base: 100, HeaderSize: 8}},
	}
	for _, c := range good {
		t.Run(c.name, func(t *testing.T) {
			got, err := sqlitefile.ParsePageHeader(c.page, c.pgno)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("header = %+v, want %+v", got, c.want)
			}
		})
	}

	bad := []struct {
		name string
		page []byte
		pgno uint32
	}{
		{"flag 0", pageWith(512, 0, 0x00, 0, 0, 512, 0, 0), 2},
		{"flag 1", pageWith(512, 0, 0x01, 0, 0, 512, 0, 0), 2},
		{"flag 0x0c", pageWith(512, 0, 0x0c, 0, 0, 512, 0, 0), 2},
		{"flag 0x0f", pageWith(512, 0, 0x0f, 0, 0, 512, 0, 0), 2},
		{"flag 0xff", pageWith(512, 0, 0xff, 0, 0, 512, 0, 0), 2},
		{"empty page", nil, 2},
		{"header past the page (leaf)", pageWith(512, 0, 0x0d, 0, 0, 512, 0, 0)[:7], 2},
		{"header past the page (interior)", pageWith(512, 0, 0x05, 0, 0, 512, 0, 0)[:11], 2},
		{"header past the page (page 1)", pageWith(512, 100, 0x0d, 0, 0, 512, 0, 0)[:107], 1},
		{"cell count that cannot fit", pageWith(512, 0, 0x0d, 0, 1000, 512, 0, 0), 2},
		{"cell count one too many", pageWith(512, 0, 0x0d, 0, 253, 512, 0, 0), 2},
		{"cell count on page 1", pageWith(512, 100, 0x0d, 0, 205, 512, 0, 0), 1},
	}
	for _, c := range bad {
		t.Run("bad "+c.name, func(t *testing.T) {
			if _, err := sqlitefile.ParsePageHeader(c.page, c.pgno); !errors.Is(err, sqlitefile.ErrCorrupt) {
				t.Errorf("err = %v, want ErrCorrupt", err)
			}
		})
	}
	// The largest cell count that fits is accepted.
	if _, err := sqlitefile.ParsePageHeader(pageWith(512, 0, 0x0d, 0, 252, 512, 0, 0), 2); err != nil {
		t.Errorf("252 cells on a 512-byte page: %v", err)
	}
}

func TestCellPointersValidated(t *testing.T) {
	const usable = 480 // a 512-byte page with 32 reserved bytes
	cases := []struct {
		name    string
		base    int
		content uint16
		ptrs    []uint16
		size    int
		good    []sqlitefile.CellPointer
		bad     []sqlitefile.BadCellPointer
	}{
		{"all good", 0, 256, []uint16{300, 400, 479}, 512, goodPtrs(300, 400, 479), nil},
		{"empty", 0, 480, nil, 512, nil, nil},
		{"into the header area", 0, 256, []uint16{300, 4, 400}, 512, goodAt(map[int]int{0: 300, 2: 400}), badAt(map[int]int{1: 4})},
		{"into the pointer array", 0, 256, []uint16{12, 300, 400}, 512, goodAt(map[int]int{1: 300, 2: 400}), badAt(map[int]int{0: 12})},
		{"content start itself", 0, 256, []uint16{256, 300, 400}, 512, goodPtrs(256, 300, 400), nil},
		{"below the content start", 0, 256, []uint16{255, 300, 400}, 512, goodAt(map[int]int{1: 300, 2: 400}), badAt(map[int]int{0: 255})},
		{"last usable byte", 0, 256, []uint16{479, 300, 400}, 512, goodPtrs(479, 300, 400), nil},
		{"first reserved byte", 0, 256, []uint16{480, 300, 400}, 512, goodAt(map[int]int{1: 300, 2: 400}), badAt(map[int]int{0: 480})},
		{"inside the reserved region", 0, 256, []uint16{300, 500, 400}, 512, goodAt(map[int]int{0: 300, 2: 400}), badAt(map[int]int{1: 500})},
		{"past the page", 0, 256, []uint16{300, 600, 65535}, 512, goodAt(map[int]int{0: 300}), badAt(map[int]int{1: 600, 2: 65535})},
		{"count x 2 beyond the content start", 0, 10, []uint16{12, 14, 300}, 512, goodAt(map[int]int{1: 14, 2: 300}), badAt(map[int]int{0: 12})},
		{"page 1 pointer into the database header", 100, 256, []uint16{50, 300, 400}, 512, goodAt(map[int]int{1: 300, 2: 400}), badAt(map[int]int{0: 50})},
		{"page 1 pointer into its pointer array", 100, 256, []uint16{110, 300, 400}, 512, goodAt(map[int]int{1: 300, 2: 400}), badAt(map[int]int{0: 110})},
		{"page 1 good", 100, 256, []uint16{300, 400, 479}, 512, goodPtrs(300, 400, 479), nil},
		{"a pointer beyond the bytes present", 0, 256, []uint16{300, 370, 400}, 360, goodAt(map[int]int{0: 300}), badAt(map[int]int{1: 370, 2: 400})},
		{"every pointer bad", 0, 256, []uint16{1, 2, 3}, 512, nil, badAt(map[int]int{0: 1, 1: 2, 2: 3})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page := pageWith(c.size, c.base, 0x0d, 0, uint16(len(c.ptrs)), c.content, 0, 0, c.ptrs...)
			h, err := sqlitefile.ParsePageHeader(page, map[bool]uint32{true: 1, false: 2}[c.base == 100])
			if err != nil {
				t.Fatal(err)
			}
			got, err := sqlitefile.CellPointers(page, h, usable)
			if err != nil {
				t.Fatalf("a pointer array that fits is never an error: %v", err)
			}
			if !slices.Equal(got.Good, c.good) {
				t.Errorf("good = %v, want %v", got.Good, c.good)
			}
			if !slices.Equal(got.Bad, c.bad) {
				t.Errorf("bad = %v, want %v", got.Bad, c.bad)
			}
			if len(c.bad) > 0 {
				if e := got.Err(); !errors.Is(e, sqlitefile.ErrCorrupt) {
					t.Errorf("Err() = %v, want ErrCorrupt for a set with bad pointers", e)
				}
			} else if e := got.Err(); e != nil {
				t.Errorf("Err() = %v, want nil", e)
			}
		})
	}
	t.Run("pointer array that reaches into the reserved region", func(t *testing.T) {
		// 250 cells: the array ends at 508, past usable 480 but inside the page.
		page := pageWith(512, 0, 0x0d, 0, 250, 480, 0, 0)
		h, err := sqlitefile.ParsePageHeader(page, 2)
		if err != nil {
			t.Fatal(err)
		}
		got, err := sqlitefile.CellPointers(page, h, usable)
		if !errors.Is(err, sqlitefile.ErrCorrupt) || len(got.Good) != 0 || len(got.Bad) != 0 {
			t.Errorf("(%+v, %v), want (empty, ErrCorrupt)", got, err)
		}
	})
}

// TestCellPointersNeverYieldASentinel is the property behind the ruling that no
// caller can index with a rejected pointer: whatever the bytes, every good
// offset lies in the valid range, and good and bad indexes together name each
// cell exactly once, in order.
func TestCellPointersNeverYieldASentinel(t *testing.T) {
	const usable = 480
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input, not security
	for range 500 {
		n := rng.IntN(120)
		ptrs := make([]uint16, n)
		for i := range ptrs {
			if rng.IntN(4) == 0 {
				ptrs[i] = uint16(rng.IntN(65536))
			} else {
				ptrs[i] = uint16(rng.IntN(560))
			}
		}
		page := pageWith(512, 0, 0x0d, 0, uint16(n), uint16(rng.IntN(512)), 0, 0, ptrs...)
		h, err := sqlitefile.ParsePageHeader(page, 2)
		if err != nil {
			continue
		}
		got, err := sqlitefile.CellPointers(page, h, usable)
		if err != nil {
			continue
		}
		seen := map[int]bool{}
		for _, g := range got.Good {
			if g.Offset < 0 || g.Offset >= usable || g.Offset < h.ContentStart || g.Offset < 8+2*n {
				t.Fatalf("good pointer %+v outside the valid range (content %d, cells %d)", g, h.ContentStart, n)
			}
			seen[g.Index] = true
		}
		for _, b := range got.Bad {
			if seen[b.Index] {
				t.Fatalf("cell %d is both good and bad", b.Index)
			}
			seen[b.Index] = true
		}
		for i := range n {
			if !seen[i] {
				t.Fatalf("cell %d is in neither list", i)
			}
		}
		if len(seen) != n {
			t.Fatalf("%d cells named, want %d", len(seen), n)
		}
		if !slices.IsSortedFunc(got.Good, func(a, b sqlitefile.CellPointer) int { return a.Index - b.Index }) {
			t.Fatal("good pointers are not in array order")
		}
	}
}

func goodPtrs(offs ...int) []sqlitefile.CellPointer {
	m := map[int]int{}
	for i, o := range offs {
		m[i] = o
	}
	return goodAt(m)
}

func goodAt(m map[int]int) []sqlitefile.CellPointer {
	var out []sqlitefile.CellPointer
	for _, i := range slices.Sorted(maps.Keys(m)) {
		out = append(out, sqlitefile.CellPointer{Index: i, Offset: m[i]})
	}
	return out
}

func badAt(m map[int]int) []sqlitefile.BadCellPointer {
	var out []sqlitefile.BadCellPointer
	for _, i := range slices.Sorted(maps.Keys(m)) {
		out = append(out, sqlitefile.BadCellPointer{Index: i, Raw: m[i]})
	}
	return out
}
