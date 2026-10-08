package sqlitefile_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// cellPage returns a zeroed page of size bytes with cell at off.
func cellPage(size, off int, cell []byte) []byte {
	p := make([]byte, size)
	copy(p[off:], cell)
	return p
}

func seq(n int, start byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestCellParseAllTypes(t *testing.T) {
	const usable = 512
	tl := sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, HeaderSize: 8}
	ti := sqlitefile.PageHeader{Type: sqlitefile.PageTableInterior, HeaderSize: 12}
	il := sqlitefile.PageHeader{Type: sqlitefile.PageIndexLeaf, HeaderSize: 8}
	ii := sqlitefile.PageHeader{Type: sqlitefile.PageIndexInterior, HeaderSize: 12}
	cases := []struct {
		name string
		h    sqlitefile.PageHeader
		cell []byte
		want sqlitefile.Cell // Offset is set by the test
		loc  []byte          // expected LocalBytes
	}{
		{
			"table leaf, all local", tl,
			cat([]byte{0x05, 0x82, 0x2c}, seq(5, 0x10)), // payload 5, rowid 300
			sqlitefile.Cell{Length: 8, PayloadLen: 5, Rowid: 300, HasRowid: true},
			seq(5, 0x10),
		},
		{
			"table leaf, overflow pointer", tl,
			cat([]byte{0x87, 0x68, 0x01}, seq(39, 1), []byte{0, 0, 0, 9}), // payload 1000: 39 local (U=512), rowid 1
			sqlitefile.Cell{Length: 46, PayloadLen: 1000, Rowid: 1, HasRowid: true, OverflowHead: 9},
			seq(39, 1),
		},
		{
			"table leaf, rowid of nine bytes", tl,
			cat([]byte{0x01}, bytes.Repeat([]byte{0xff}, 9), []byte{0x7a}), // rowid -1
			sqlitefile.Cell{Length: 11, PayloadLen: 1, Rowid: -1, HasRowid: true},
			[]byte{0x7a},
		},
		{
			"table leaf, empty payload", tl,
			[]byte{0x00, 0x2a},
			sqlitefile.Cell{Length: 2, PayloadLen: 0, Rowid: 42, HasRowid: true},
			[]byte{},
		},
		{
			"table interior", ti,
			[]byte{0, 0, 0, 7, 0x81, 0x00}, // child 7, rowid 128
			sqlitefile.Cell{Length: 6, LeftChild: 7, Rowid: 128, HasRowid: true},
			nil,
		},
		{
			"index leaf, all local", il,
			[]byte{0x03, 0xaa, 0xbb, 0xcc},
			sqlitefile.Cell{Length: 4, PayloadLen: 3},
			[]byte{0xaa, 0xbb, 0xcc},
		},
		{
			"index leaf, overflow pointer", il,
			cat([]byte{0x82, 0x2c}, seq(39, 7), []byte{0, 0, 1, 0}), // payload 300: 39 local, overflow page 256
			sqlitefile.Cell{Length: 45, PayloadLen: 300, OverflowHead: 256},
			seq(39, 7),
		},
		{
			"index interior, all local", ii,
			cat([]byte{0, 0, 0, 2, 0x02}, []byte{0x11, 0x22}),
			sqlitefile.Cell{Length: 7, LeftChild: 2, PayloadLen: 2},
			[]byte{0x11, 0x22},
		},
		{
			"index interior, overflow pointer", ii,
			cat([]byte{0, 0, 0, 5, 0x82, 0x2c}, seq(39, 3), []byte{0, 0, 0, 0x0c}),
			sqlitefile.Cell{Length: 49, LeftChild: 5, PayloadLen: 300, OverflowHead: 12},
			seq(39, 3),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, off := range []int{8, 200, usable - len(c.cell)} {
				page := cellPage(512, off, c.cell)
				got, err := sqlitefile.ParseCell(page, usable, c.h, off)
				if err != nil {
					t.Fatalf("offset %d: %v", off, err)
				}
				want := c.want
				want.Offset = off
				local := got.LocalBytes
				got.LocalBytes = nil
				if !reflect.DeepEqual(got, want) {
					t.Errorf("offset %d: cell = %+v, want %+v", off, got, want)
				}
				if !bytes.Equal(local, c.loc) {
					t.Errorf("offset %d: local = % x, want % x", off, local, c.loc)
				}
				if len(local) > 0 && &local[0] != &page[off+got.Length-len(local)-func() int {
					if got.OverflowHead != 0 {
						return 4
					}
					return 0
				}()] {
					t.Errorf("offset %d: LocalBytes does not alias the page", off)
				}
				if cap(local) != len(local) {
					t.Errorf("LocalBytes capacity %d exceeds its length %d (an append would overwrite the page)", cap(local), len(local))
				}
			}
		})
	}
}

func TestCellParseRejectsDamage(t *testing.T) {
	const usable = 480 // a 512-byte page, 32 reserved
	tl := sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf, HeaderSize: 8}
	ti := sqlitefile.PageHeader{Type: sqlitefile.PageTableInterior, HeaderSize: 12}
	good := cat([]byte{0x05, 0x01}, seq(5, 1))
	for _, c := range []struct {
		name string
		page []byte
		h    sqlitefile.PageHeader
		off  int
		ok   bool
	}{
		{"wholly inside", cellPage(512, 100, good), tl, 100, true},
		{"ends exactly at usable", cellPage(512, usable-len(good), good), tl, usable - len(good), true},
		{"ends one past usable", cellPage(512, usable-len(good)+1, good), tl, usable - len(good) + 1, false},
		{"starts in the reserved region", cellPage(512, 490, good), tl, 490, false},
		{"offset negative", cellPage(512, 100, good), tl, -1, false},
		{"offset at the end of the page", cellPage(512, 100, good), tl, 512, false},
		{"offset 65535 on a small page", cellPage(512, 100, good), tl, 65535, false},
		{"payload length varint cut by the page end", cellPage(512, 100, good)[:101], tl, 100, false},
		{"rowid varint cut by the usable end", cat(make([]byte, 478), []byte{0x05, 0x81}), tl, 478, false},
		{"child pointer cut", cat(make([]byte, 478), []byte{0, 0}), ti, 478, false},
		{"interior rowid missing", cat(make([]byte, 476), []byte{0, 0, 0, 1}), ti, 476, false},
		{"payload length above int64", cellPage(512, 100, cat(bytes.Repeat([]byte{0xff}, 9), []byte{1})), tl, 100, false},
		{"truncated file: present bytes cover the cell", cellPage(512, 100, good)[:107], tl, 100, true},
		{"truncated file: cell reaches past the present bytes", cellPage(512, 100, good)[:106], tl, 100, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cell, err := sqlitefile.ParseCell(c.page, usable, c.h, c.off)
			if c.ok {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !errors.Is(err, sqlitefile.ErrCorrupt) {
				t.Errorf("(%+v, %v), want ErrCorrupt", cell, err)
			}
		})
	}
}

// TestLocalPayloadGolden: the U=4096, P=5000 vector of the format
// description, and a table derived by hand from the same formulas (table
// leaf X = U-35; index X = (U-12)*64/255-23; M = (U-12)*32/255-23; K = M +
// (P-M) mod (U-4); local = K when K <= X, else M) for U in {480 (a 512-byte
// page with 32 reserved), 512, 1024, 4096, 65536} at P = X, X+1, M, M+1 and
// 10*U.
func TestLocalPayloadGolden(t *testing.T) {
	if local, spills := sqlitefile.LocalPayload(4096, sqlitefile.PageTableLeaf, 5000); local != 908 || !spills {
		t.Errorf("U=4096 table P=5000: (%d, %v), want (908, true): %d spilled bytes = U-4 is one overflow page", local, spills, 5000-local)
	}
	if local, spills := sqlitefile.LocalPayload(4096, sqlitefile.PageIndexLeaf, 1002); local != 1002 || spills {
		t.Errorf("index X=1002: (%d, %v), want it all local", local, spills)
	}
	if local, spills := sqlitefile.LocalPayload(4096, sqlitefile.PageIndexLeaf, 1003); local != 489 || !spills {
		t.Errorf("index X+1: (%d, %v), want (489, true)", local, spills)
	}
	table := []struct {
		u     int
		tbl   bool
		p     int64
		local int64
		spill bool
	}{
		{480, true, 445, 445, false},
		{480, true, 446, 35, true},
		{480, true, 35, 35, false},
		{480, true, 36, 36, false},
		{480, true, 4800, 40, true},
		{480, false, 94, 94, false},
		{480, false, 95, 35, true},
		{480, false, 35, 35, false},
		{480, false, 36, 36, false},
		{480, false, 4800, 40, true},
		{512, true, 477, 477, false},
		{512, true, 478, 39, true},
		{512, true, 39, 39, false},
		{512, true, 40, 40, false},
		{512, true, 5120, 40, true},
		{512, false, 102, 102, false},
		{512, false, 103, 39, true},
		{512, false, 39, 39, false},
		{512, false, 40, 40, false},
		{512, false, 5120, 40, true},
		{1024, true, 989, 989, false},
		{1024, true, 990, 103, true},
		{1024, true, 103, 103, false},
		{1024, true, 104, 104, false},
		{1024, true, 10240, 103, true},
		{1024, false, 230, 230, false},
		{1024, false, 231, 103, true},
		{1024, false, 103, 103, false},
		{1024, false, 104, 104, false},
		{1024, false, 10240, 103, true},
		{4096, true, 4061, 4061, false},
		{4096, true, 4062, 489, true},
		{4096, true, 489, 489, false},
		{4096, true, 490, 490, false},
		{4096, true, 40960, 489, true},
		{4096, false, 1002, 1002, false},
		{4096, false, 1003, 489, true},
		{4096, false, 489, 489, false},
		{4096, false, 490, 490, false},
		{4096, false, 40960, 489, true},
		{65536, true, 65501, 65501, false},
		{65536, true, 65502, 8199, true},
		{65536, true, 8199, 8199, false},
		{65536, true, 8200, 8200, false},
		{65536, true, 655360, 8199, true},
		{65536, false, 16422, 16422, false},
		// P = X + (U-4): K lands exactly on X, so the payload spills but keeps X bytes locally.
		{480, true, 921, 445, true},
		{512, true, 985, 477, true},
		{1024, true, 2009, 989, true},
		{4096, true, 8153, 4061, true},
		{65536, true, 131033, 65501, true},
		{480, false, 570, 94, true},
		{512, false, 610, 102, true},
		{1024, false, 1250, 230, true},
		{4096, false, 5094, 1002, true},
		{65536, false, 81954, 16422, true},
		{65536, false, 16423, 8199, true},
		{65536, false, 8199, 8199, false},
		{65536, false, 8200, 8200, false},
		{65536, false, 655360, 8199, true},
	}
	for _, c := range table {
		typ := sqlitefile.PageIndexLeaf
		if c.tbl {
			typ = sqlitefile.PageTableLeaf
		}
		local, spills := sqlitefile.LocalPayload(c.u, typ, c.p)
		if local != c.local || spills != c.spill {
			t.Errorf("U=%d table=%v P=%d: (%d, %v), want (%d, %v)", c.u, c.tbl, c.p, local, spills, c.local, c.spill)
		}
	}
	// Properties for every usable size and payload: P <= X never spills,
	// local never exceeds X (nor drops below M when spilling), and the
	// overflow part is positive.
	for _, u := range []int{480, 481, 500, 512, 1000, 1024, 4096, 32768, 65536} {
		for _, typ := range []sqlitefile.PageType{sqlitefile.PageTableLeaf, sqlitefile.PageIndexLeaf, sqlitefile.PageIndexInterior} {
			x := int64((u-12)*64/255 - 23)
			if typ == sqlitefile.PageTableLeaf {
				x = int64(u - 35)
			}
			m := int64((u-12)*32/255 - 23)
			for p := int64(0); p < 4*int64(u); p += 7 {
				local, spills := sqlitefile.LocalPayload(u, typ, p)
				switch {
				case p <= x && (spills || local != p):
					t.Fatalf("U=%d %v P=%d <= X=%d: (%d, %v), want all local", u, typ, p, x, local, spills)
				case p > x && (!spills || local > x || local < m || local >= p):
					t.Fatalf("U=%d %v P=%d > X=%d: (%d, %v) breaks the bounds M=%d..X", u, typ, p, x, local, spills, m)
				}
			}
		}
	}
	if local, spills := sqlitefile.LocalPayload(4096, sqlitefile.PageTableInterior, 100); local != 0 || spills {
		t.Errorf("a table interior cell has no payload, got (%d, %v)", local, spills)
	}
	if local, spills := sqlitefile.LocalPayload(100, sqlitefile.PageTableLeaf, 100); local != 0 || spills {
		t.Errorf("an invalid usable size must not produce a local size, got (%d, %v)", local, spills)
	}
}
