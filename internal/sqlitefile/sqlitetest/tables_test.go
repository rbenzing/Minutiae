package sqlitetest_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Test-side page readers: written here from the format description, so the
// builder is checked against code that shares nothing with it.

func u16(b []byte) int    { return int(binary.BigEndian.Uint16(b)) }
func u32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func varint(b []byte) (v uint64, n int) {
	for i := 0; i < 8; i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v<<8 | uint64(b[8]), 9
}

type pageInfo struct {
	flag         byte
	freeblock    int
	count        int
	contentStart int
	frag         int
	right        uint32
	ptrs         []int
}

func parsePage(p []byte, base int) pageInfo {
	i := pageInfo{flag: p[base], freeblock: u16(p[base+1:]), count: u16(p[base+3:]), contentStart: u16(p[base+5:]), frag: int(p[base+7])}
	if i.contentStart == 0 {
		i.contentStart = 65536
	}
	hdr := 8
	if i.flag == 0x02 || i.flag == 0x05 {
		hdr = 12
		i.right = u32(p[base+8:])
	}
	for k := 0; k < i.count; k++ {
		i.ptrs = append(i.ptrs, u16(p[base+hdr+2*k:]))
	}
	return i
}

// freeblocks walks the freeblock chain: {offset, size} in chain order.
func freeblocks(p []byte, first int) [][2]int {
	var out [][2]int
	for off := first; off != 0 && len(out) < 1000; off = u16(p[off:]) {
		out = append(out, [2]int{off, u16(p[off+2:])})
	}
	return out
}

// tableLeafRowids returns the rowids of a leaf, in pointer order.
func tableLeafRowids(p []byte, base int) []int64 {
	var out []int64
	for _, off := range parsePage(p, base).ptrs {
		_, n := varint(p[off:])
		r, _ := varint(p[off+n:])
		out = append(out, int64(r))
	}
	return out
}

// walkTable walks a table tree from page pg, checking the shape, and returns
// the rowids under it, the depth and the largest rowid; it checks that every
// interior key equals the largest rowid of its left subtree and that all
// leaves are at the same depth.
func walkTable(t *testing.T, img *sqlitetest.Image, pg uint32, leafDepth *int, depth int) (rowids []int64) {
	t.Helper()
	p := img.Page(pg)
	base := 0
	if pg == 1 {
		base = 100
	}
	info := parsePage(p, base)
	switch info.flag {
	case 0x0d:
		if *leafDepth == 0 {
			*leafDepth = depth
		} else if *leafDepth != depth {
			t.Errorf("leaf %d at depth %d, others at %d", pg, depth, *leafDepth)
		}
		return tableLeafRowids(p, base)
	case 0x05:
		for _, off := range info.ptrs {
			child := u32(p[off:])
			key, _ := varint(p[off+4:])
			sub := walkTable(t, img, child, leafDepth, depth+1)
			if len(sub) == 0 || int64(key) != sub[len(sub)-1] {
				t.Errorf("interior page %d: key %d, but the largest rowid of child %d is %v", pg, int64(key), child, sub[max(0, len(sub)-1):])
			}
			rowids = append(rowids, sub...)
		}
		return append(rowids, walkTable(t, img, info.right, leafDepth, depth+1)...)
	}
	t.Fatalf("page %d has flag %#x in a table tree", pg, info.flag)
	return nil
}

func TestBuilderMultiLevelTreeShape(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
	const n = 5000
	for i := int64(1); i <= n; i++ {
		tb.Insert(i*2, fmt.Sprintf("row %d", i), i)
	}
	img := b.Snapshot()
	if tb.Depth() < 3 {
		t.Fatalf("depth %d, want >= 3 for %d rows on 512-byte pages", tb.Depth(), n)
	}
	leafDepth := 0
	rowids := walkTable(t, img, tb.Root(), &leafDepth, 1)
	if leafDepth != tb.Depth() {
		t.Errorf("the walk found depth %d, Depth() says %d", leafDepth, tb.Depth())
	}
	if len(rowids) != n || !slices.IsSorted(rowids) || rowids[0] != 2 || rowids[n-1] != 2*n {
		t.Errorf("rows under the root: %d, sorted %v", len(rowids), slices.IsSorted(rowids))
	}
	if len(tb.Leaves()) < 100 || len(tb.Interiors()) < 2 {
		t.Errorf("%d leaves, %d interior pages", len(tb.Leaves()), len(tb.Interiors()))
	}
	if !slices.Contains(tb.Interiors(), tb.Root()) {
		t.Error("the root of a multi-level tree is an interior page")
	}
	// The root is the page the schema names: object 0 has page 2.
	if tb.Root() != 2 {
		t.Errorf("root page %d, want 2", tb.Root())
	}
	// Every leaf is listed once, in key order.
	var viaLeaves []int64
	for _, pg := range tb.Leaves() {
		viaLeaves = append(viaLeaves, tableLeafRowids(img.Page(pg), 0)...)
	}
	if !slices.Equal(viaLeaves, rowids) {
		t.Error("Leaves() does not list the leaves in key order")
	}
	// A header page count matches the file.
	if got := u32(img.Page(1)[28:]); got != img.Pages() {
		t.Errorf("header page count %d, file has %d pages", got, img.Pages())
	}
}

func TestBuilderSmallTableIsOneLeaf(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	tb.Insert(1, "x")
	if tb.Depth() != 1 || tb.Root() != 2 || len(tb.Interiors()) != 0 || !slices.Equal(tb.Leaves(), []uint32{2}) {
		t.Errorf("depth %d root %d leaves %v interiors %v", tb.Depth(), tb.Root(), tb.Leaves(), tb.Interiors())
	}
	// The schema row lives on page 1 and names root page 2.
	p := b.PageBytes(1)
	info := parsePage(p, 100)
	if info.flag != 0x0d || info.count != 1 {
		t.Fatalf("page 1: %+v", info)
	}
	off := info.ptrs[0]
	if !bytes.Contains(p[off:], []byte("CREATE TABLE t(a)")) || !bytes.Contains(p[off:], []byte("table")) {
		t.Error("the schema row must hold the type, the name and the sql")
	}
}

func TestBuilderOverflowLayout(t *testing.T) {
	// U = 4096, P = 5000: the vector of the format description. A blob of
	// 4997 bytes in a one-column record has a 3-byte header.
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	blob := make([]byte, 4997)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	tb.Insert(1, blob)
	cell, pg, off := tb.CellBytes(1)
	if pg != 2 {
		t.Fatalf("leaf page %d", pg)
	}
	ov := tb.Overflow(1)
	if len(ov) != 1 {
		t.Fatalf("overflow pages %v, want one (5000-908 = 4092 = U-4)", ov)
	}
	// Cell: varint 5000 (0xa7 0x08), rowid 1, 908 local bytes, the overflow pointer.
	if len(cell) != 2+1+908+4 || cell[0] != 0xa7 || cell[1] != 0x08 || cell[2] != 1 {
		t.Fatalf("cell is %d bytes starting % x", len(cell), cell[:4])
	}
	if u32(cell[len(cell)-4:]) != ov[0] {
		t.Errorf("overflow pointer %d, chain starts at %d", u32(cell[len(cell)-4:]), ov[0])
	}
	img := b.Snapshot()
	if !bytes.Equal(img.Page(pg)[off:off+len(cell)], cell) {
		t.Error("CellBytes does not match the page")
	}
	// The overflow page: next = 0, then the last 4092 payload bytes.
	op := img.Page(ov[0])
	if u32(op) != 0 || !bytes.Equal(op[4:4+4092], func() []byte {
		// the record is header(3 bytes) + blob; its bytes 908.. are blob[905:]
		return blob[908-3:]
	}()) {
		t.Error("the overflow page does not hold the tail of the payload")
	}

	t.Run("three-page chain at 512", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "CREATE TABLE t(a)")
		big := bytes.Repeat([]byte("0123456789"), 160) // 1600 bytes
		tb.Insert(7, big)
		tb.Insert(8, "small")
		cell, _, _ := tb.CellBytes(7)
		chain := tb.Overflow(7)
		// P = 1600 + 3-byte header; U=512: X=477, M=39, K = 39 + (1603-39) mod 508 = 39 + 40 = 79.
		if len(chain) != 3 {
			t.Fatalf("chain %v, want 3 pages: (1603-79)/508 = 3", chain)
		}
		img := b.Snapshot()
		var assembled []byte
		for i, pg := range chain {
			p := img.Page(pg)
			wantNext := uint32(0)
			if i+1 < len(chain) {
				wantNext = chain[i+1]
			}
			if u32(p) != wantNext {
				t.Errorf("page %d next = %d, want %d", pg, u32(p), wantNext)
			}
			assembled = append(assembled, p[4:]...)
		}
		localLen := len(cell) - 1 - 2 - 4 // varint(1603)=2 bytes, rowid 1 byte, pointer 4
		if localLen != 79 {
			t.Errorf("local payload %d bytes, want 79", localLen)
		}
		// local + overflow reproduce the record: header 0x03 0x?? ... then the blob.
		full := append(append([]byte(nil), cell[3:3+localLen]...), assembled[:1603-localLen]...)
		if !bytes.HasSuffix(full, big) || len(full) != 1603 {
			t.Errorf("the payload reassembled from local and overflow bytes is wrong (%d bytes)", len(full))
		}
		for i, pg := range chain {
			if slices.Contains(chain[i+1:], pg) || pg == tb.Root() {
				t.Errorf("page %d is used twice", pg)
			}
		}
	})
}

// deletedCellFixture builds a leaf of five 11-byte cells ("value 1".."value 5"
// on a 512-byte page: cell i lies at 512 - 11*i) and returns the builder, the
// table and the original cell of every row.
func deletedCellFixture(t *testing.T) (*sqlitetest.Builder, *sqlitetest.Table, map[int64][]byte, map[int64]int) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	for i := int64(1); i <= 5; i++ {
		tb.Insert(i, fmt.Sprintf("value %d", i))
	}
	cells := map[int64][]byte{}
	offs := map[int64]int{}
	for i := int64(1); i <= 5; i++ {
		c, pg, off := tb.CellBytes(i)
		if pg != 2 || len(c) != 11 || off != 512-11*int(i) {
			t.Fatalf("row %d: cell of %d bytes at page %d offset %d", i, len(c), pg, off)
		}
		cells[i], offs[i] = c, off
	}
	return b, tb, cells, offs
}

func TestBuilderUpdateDeleteLeaveResidue(t *testing.T) {
	t.Run("a deleted cell in the middle becomes a freeblock", func(t *testing.T) {
		b, tb, cells, offs := deletedCellFixture(t)
		tb.Delete(3)
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if info.count != 4 || slices.Contains(info.ptrs, offs[3]) {
			t.Fatalf("pointers %v still hold the deleted cell at %d", info.ptrs, offs[3])
		}
		if fb := freeblocks(p, info.freeblock); !slices.Equal(fb, [][2]int{{offs[3], 11}}) {
			t.Errorf("freeblocks %v, want one at %d of 11 bytes", fb, offs[3])
		}
		if info.contentStart != offs[5] || info.frag != 0 {
			t.Errorf("content start %d, want %d; fragmented %d", info.contentStart, offs[5], info.frag)
		}
		// The freeblock header overwrites the first four bytes; the rest of the cell stays.
		if !bytes.Equal(p[offs[3]+4:offs[3]+11], cells[3][4:]) {
			t.Errorf("residue % x, want % x", p[offs[3]+4:offs[3]+11], cells[3][4:])
		}
		if u16(p[offs[3]:]) != 0 || u16(p[offs[3]+2:]) != 11 {
			t.Errorf("freeblock header % x", p[offs[3]:offs[3]+4])
		}
		for _, r := range []int64{1, 2, 4, 5} {
			if !bytes.Equal(p[offs[r]:offs[r]+11], cells[r]) {
				t.Errorf("live row %d moved or changed", r)
			}
		}
	})
	t.Run("a deleted cell at the lowest address is swallowed by the gap, bytes intact", func(t *testing.T) {
		b, tb, cells, offs := deletedCellFixture(t)
		tb.Delete(5)
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if info.freeblock != 0 || info.contentStart != offs[5]+11 || info.count != 4 {
			t.Errorf("freeblock %d, content start %d (want %d), count %d", info.freeblock, info.contentStart, offs[5]+11, info.count)
		}
		if !bytes.Equal(p[offs[5]:offs[5]+11], cells[5]) {
			t.Error("the bytes of the deleted cell in the gap must be untouched")
		}
	})
	t.Run("a deleted cell at the highest address is a freeblock", func(t *testing.T) {
		b, tb, cells, offs := deletedCellFixture(t)
		tb.Delete(1)
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if fb := freeblocks(p, info.freeblock); !slices.Equal(fb, [][2]int{{offs[1], 11}}) || info.contentStart != offs[5] {
			t.Errorf("freeblocks %v content start %d", fb, info.contentStart)
		}
		if !bytes.Equal(p[offs[1]+4:offs[1]+11], cells[1][4:]) {
			t.Error("residue of the first cell changed")
		}
	})
	t.Run("adjacent deleted cells share one freeblock", func(t *testing.T) {
		b, tb, _, offs := deletedCellFixture(t)
		tb.Delete(2)
		tb.Delete(3)
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if fb := freeblocks(p, info.freeblock); !slices.Equal(fb, [][2]int{{offs[3], 22}}) {
			t.Errorf("freeblocks %v, want {%d 22}", fb, offs[3])
		}
	})
	t.Run("separate deleted cells chain in ascending order", func(t *testing.T) {
		b, tb, _, offs := deletedCellFixture(t)
		tb.Delete(2)
		tb.Delete(4)
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		want := [][2]int{{offs[4], 11}, {offs[2], 11}}
		if fb := freeblocks(p, info.freeblock); !slices.Equal(fb, want) {
			t.Errorf("freeblocks %v, want %v", fb, want)
		}
	})
	t.Run("a same-length update rewrites in place", func(t *testing.T) {
		b, tb, _, offs := deletedCellFixture(t)
		tb.Update(3, "VALUE 3")
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if info.freeblock != 0 || info.count != 5 || !bytes.Contains(p[offs[3]:offs[3]+11], []byte("VALUE 3")) {
			t.Errorf("freeblock %d, count %d, cell % x", info.freeblock, info.count, p[offs[3]:offs[3]+11])
		}
	})
	t.Run("an update that changes the length leaves the old cell behind", func(t *testing.T) {
		b, tb, cells, offs := deletedCellFixture(t)
		tb.Update(3, "a much longer value")
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if info.count != 5 {
			t.Fatalf("count %d", info.count)
		}
		fb := freeblocks(p, info.freeblock)
		if len(fb) != 1 || fb[0][1] != 11 {
			t.Fatalf("freeblocks %v, want the old 11-byte cell", fb)
		}
		old := fb[0][0]
		if !bytes.Equal(p[old+4:old+11], cells[3][4:]) {
			t.Error("the old cell's bytes are not left behind")
		}
		// The new cell is live and is found through the pointer array.
		_, pg, off := tb.CellBytes(3)
		if pg != 2 || !slices.Contains(info.ptrs, off) || off == offs[3] && old == offs[3] {
			t.Errorf("new cell at %d, pointers %v", off, info.ptrs)
		}
		if !bytes.Contains(p[off:off+40], []byte("a much longer value")) {
			t.Error("the new cell does not hold the new value")
		}
	})
	t.Run("residue that does not fit is dropped, not forced in", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "CREATE TABLE t(a)")
		for i := int64(1); i <= 20; i++ {
			tb.Insert(i, bytes.Repeat([]byte{byte(i)}, 40))
		}
		for i := int64(1); i <= 20; i++ {
			tb.Delete(i)
		}
		// Everything is deleted: the leaf is empty and the residue that fits stays in it.
		p := b.PageBytes(2)
		info := parsePage(p, 0)
		if info.count != 0 || info.flag != 0x0d {
			t.Errorf("%+v", info)
		}
		kept := 0
		for v := 1; v <= 20; v++ {
			if bytes.Contains(p, bytes.Repeat([]byte{byte(v)}, 40)) {
				kept++
			}
		}
		if kept != 11 { // 504 usable bytes hold 11 cells of 44
			t.Errorf("%d of 20 residue cells kept, want the 11 that fit", kept)
		}
	})
}

func TestBuilderInsertRawKeepsBytesVerbatim(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
	tb.InsertRaw(5, []uint64{1, 10, 11, 0}, []byte{0x2a, 0xde, 0xad})
	cell, _, _ := tb.CellBytes(5)
	// P = 8: the header (length 5, four serial bytes) and the three body bytes, verbatim.
	want := []byte{0x08, 5, 0x05, 0x01, 0x0a, 0x0b, 0x00, 0x2a, 0xde, 0xad}
	if !bytes.Equal(cell, want) {
		t.Errorf("cell % x, want % x", cell, want)
	}
}

func TestBuilderDropTableFreesPages(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	keep := b.CreateTable("keep", "CREATE TABLE keep(a)")
	gone := b.CreateTable("gone", "CREATE TABLE gone(a, b)")
	b.CreateIndex("gone_a", "gone", "CREATE INDEX gone_a ON gone(a)", 0)
	keep.Insert(1, "kept row")
	for i := int64(1); i <= 300; i++ {
		var v any = fmt.Sprintf("v%d", i)
		if i%5 == 0 {
			v = bytes.Repeat([]byte{byte(i)}, 9000) // overflow
		}
		gone.Insert(i, v, i)
	}
	before := b.Snapshot()
	var dropped []uint32
	dropped = append(dropped, gone.Pages()...)
	idx := b.Object("gone_a")
	dropped = append(dropped, idx.Pages()...)
	dropped = slices.Compact(slices.Sorted(slices.Values(dropped)))

	b.DropTable("gone")
	after := b.Snapshot()
	if b.Object("gone") != nil || b.Object("gone_a") != nil {
		t.Error("the table and its index are gone")
	}

	// Walk the freelist from the header.
	h := after.Page(1)
	trunk, count := u32(h[32:]), u32(h[36:])
	var free, trunks []uint32
	for pg := trunk; pg != 0 && len(trunks) < 100; {
		trunks = append(trunks, pg)
		free = append(free, pg)
		p := after.Page(pg)
		n := int(u32(p[4:]))
		if n > 1024/4-2 {
			t.Fatalf("trunk %d lists %d leaves", pg, n)
		}
		for k := 0; k < n; k++ {
			free = append(free, u32(p[8+4*k:]))
		}
		pg = u32(p)
	}
	slices.Sort(free)
	if uint32(len(free)) != count {
		t.Errorf("header count %d, the list holds %d pages", count, len(free))
	}
	if !slices.Equal(free, dropped) {
		t.Errorf("freelist %v\nwant every page of the dropped tree and index %v", free, dropped)
	}
	if len(trunks) < 2 {
		t.Errorf("%d pages should need more than one trunk of 254 leaves", len(free))
	}
	// Contents of freed leaf pages are intact; the kept table is untouched.
	for _, pg := range free {
		if slices.Contains(trunks, pg) {
			continue
		}
		if !bytes.Equal(before.Page(pg), after.Page(pg)) {
			t.Errorf("freed page %d changed", pg)
		}
	}
	for _, pg := range append([]uint32{keep.Root()}, keep.Leaves()...) {
		if slices.Contains(free, pg) {
			t.Errorf("page %d of the kept table is on the freelist", pg)
		}
	}
	if after.Pages() != before.Pages() {
		t.Errorf("dropping must not shrink the file: %d -> %d pages", before.Pages(), after.Pages())
	}
	// A trunk keeps its old bytes after its list.
	tp := after.Page(trunks[len(trunks)-1])
	n := int(u32(tp[4:]))
	if !bytes.Equal(tp[8+4*n:], before.Page(trunks[len(trunks)-1])[8+4*n:]) {
		t.Error("a trunk keeps its old bytes after the leaf list")
	}
}

func TestBuilderBuildIsIdempotentAndPatchesSurvive(t *testing.T) {
	mk := func() *sqlitetest.Builder {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512, Encoding: 3})
		tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
		for i := int64(1); i <= 200; i++ {
			tb.Insert(i, fmt.Sprintf("row %d", i), bytes.Repeat([]byte{byte(i)}, int(i)))
		}
		b.CreateIndex("t_a", "t", "CREATE INDEX t_a ON t(a)", 0)
		return b
	}
	b := mk()
	first := b.Bytes()
	b.Build()
	b.Build()
	if !bytes.Equal(first, b.Bytes()) {
		t.Error("Build is not idempotent")
	}
	if !bytes.Equal(first, mk().Bytes()) {
		t.Error("two builders given the same operations must produce the same bytes")
	}
	snap := b.Snapshot()
	b.Patch(60, 0xab)
	if snap.Page(1)[60] == 0xab {
		t.Error("a snapshot is a copy")
	}
	b.SetHeaderPages(77, false)
	tb := b.Object("t")
	tb.Insert(1000, "late", nil) // a change after the patch: the patch is applied again after the rebuild
	got := b.Bytes()
	if got[60] != 0xab || u32(got[28:]) != 77 || u32(got[92:]) == u32(got[24:]) {
		t.Errorf("Patch and SetHeaderPages must survive a rebuild: %x %d", got[60], u32(got[28:]))
	}
}

func TestBuilderRejectsMisuse(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	tb.Insert(1, "x")
	mustPanic("duplicate rowid", func() { tb.Insert(1, "y") })
	mustPanic("duplicate table", func() { b.CreateTable("t", "CREATE TABLE t(a)") })
	mustPanic("delete of a missing row", func() { tb.Delete(9) })
	mustPanic("update of a missing row", func() { tb.Update(9, "x") })
	mustPanic("unsupported value", func() { tb.Insert(2, struct{}{}) })
	mustPanic("index of a missing table", func() { b.CreateIndex("i", "nope", "CREATE INDEX i ON nope(a)", 0) })
}
