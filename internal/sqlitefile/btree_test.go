package sqlitefile_test

import (
	"errors"
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
		want    []int // -1 marks a rejected pointer
		wantErr bool
	}{
		{"all good", 0, 256, []uint16{300, 400, 479}, 512, []int{300, 400, 479}, false},
		{"empty", 0, 480, nil, 512, []int{}, false},
		{"into the header area", 0, 256, []uint16{300, 4, 400}, 512, []int{300, -1, 400}, true},
		{"into the pointer array", 0, 256, []uint16{12, 300, 400}, 512, []int{-1, 300, 400}, true},
		{"content start itself", 0, 256, []uint16{256, 300, 400}, 512, []int{256, 300, 400}, false},
		{"below the content start", 0, 256, []uint16{255, 300, 400}, 512, []int{-1, 300, 400}, true},
		{"last usable byte", 0, 256, []uint16{479, 300, 400}, 512, []int{479, 300, 400}, false},
		{"first reserved byte", 0, 256, []uint16{480, 300, 400}, 512, []int{-1, 300, 400}, true},
		{"inside the reserved region", 0, 256, []uint16{300, 500, 400}, 512, []int{300, -1, 400}, true},
		{"past the page", 0, 256, []uint16{300, 600, 65535}, 512, []int{300, -1, -1}, true},
		{"count x 2 beyond the content start", 0, 10, []uint16{12, 14, 300}, 512, []int{-1, 14, 300}, true},
		{"page 1 pointer into the database header", 100, 256, []uint16{50, 300, 400}, 512, []int{-1, 300, 400}, true},
		{"page 1 pointer into its pointer array", 100, 256, []uint16{110, 300, 400}, 512, []int{-1, 300, 400}, true},
		{"page 1 good", 100, 256, []uint16{300, 400, 479}, 512, []int{300, 400, 479}, false},
		{"a pointer beyond the bytes present", 0, 256, []uint16{300, 370, 400}, 360, []int{300, -1, -1}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page := pageWith(c.size, c.base, 0x0d, 0, uint16(len(c.ptrs)), c.content, 0, 0, c.ptrs...)
			h, err := sqlitefile.ParsePageHeader(page, map[bool]uint32{true: 1, false: 2}[c.base == 100])
			if err != nil {
				t.Fatal(err)
			}
			got, err := sqlitefile.CellPointers(page, h, usable)
			if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, sqlitefile.ErrCorrupt)) {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("pointers = %v, want %v", got, c.want)
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
		if !errors.Is(err, sqlitefile.ErrCorrupt) || got != nil {
			t.Errorf("(%v, %v), want (nil, ErrCorrupt)", got, err)
		}
	})
}
