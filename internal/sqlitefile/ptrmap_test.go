package sqlitefile_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestPtrmapPageNumbers(t *testing.T) {
	for _, c := range []struct {
		ps, usable int
		pgno, want uint32
	}{
		{512, 512, 0, 0},
		{512, 512, 1, 0},
		{512, 512, 2, 2},
		{512, 512, 3, 2},
		{512, 512, 104, 2},
		{512, 512, 105, 105},
		{512, 512, 106, 105},
		{512, 512, 207, 105},
		{512, 512, 208, 208},
		{512, 512, 300, 208},
		{4096, 4096, 2, 2},
		{4096, 4096, 821, 2},
		{4096, 4096, 822, 822},
		{4096, 4096, 1641, 822},
		{4096, 4096, 1642, 1642},
		{4096, 4000, 802, 2}, // a reserved region shrinks the usable size: 4000/5+1 = 801 pages per map page
		{4096, 4000, 803, 803},
	} {
		if got := sqlitefile.PtrmapPageFor(c.ps, c.usable, c.pgno); got != c.want {
			t.Errorf("PtrmapPageFor(%d, %d, %d) = %d, want %d", c.ps, c.usable, c.pgno, got, c.want)
		}
	}
}

// TestPtrmapSkipsLockBytePage: when the map page that would cover a range is
// the lock-byte page, the map moves to the next page. Page size 1024 with 770
// usable bytes: 155 pages per map page, and the 6766th map page is page 1048577,
// the lock-byte page.
func TestPtrmapSkipsLockBytePage(t *testing.T) {
	const ps, usable = 1024, 770
	lock := sqlitefile.LockBytePage(ps)
	if lock != 1048577 {
		t.Fatalf("lock-byte page %d", lock)
	}
	for _, pgno := range []uint32{lock, lock + 1, lock + 100, lock + 154} {
		if got := sqlitefile.PtrmapPageFor(ps, usable, pgno); got != lock+1 {
			t.Errorf("PtrmapPageFor(page %d) = %d, want %d (the lock-byte page is skipped)", pgno, got, lock+1)
		}
	}
	if got := sqlitefile.PtrmapPageFor(ps, usable, lock-1); got != lock-155 {
		t.Errorf("the page before: %d", got)
	}
	if got := sqlitefile.PtrmapPageFor(ps, usable, lock+155); got != lock+155 {
		t.Errorf("the next map page after the skip: %d, want %d", got, lock+155)
	}
}

// ptrmapDB builds an auto-vacuum database whose pointer map is written by the
// builder, with a two-level table, an overflow row and free pages.
func ptrmapDB(t *testing.T) (*sqlitetest.Builder, *sqlitetest.Table, []uint32) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512, AutoVacuum: 1})
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 80; i++ {
		tb.Insert(i, "row", bytes.Repeat([]byte{byte(i)}, 40))
	}
	tb.Insert(200, "big", bytes.Repeat([]byte("ovf"), 800))
	extra := []uint32{}
	base := uint32(len(b.Bytes())/512) + 1
	for pg := base; len(extra) < 6; pg++ {
		if (pg-2)%103 != 0 {
			extra = append(extra, pg)
		}
	}
	b.Free(extra...)
	b.WritePtrmap()
	return b, tb, extra
}

func TestPtrmapEntryRead(t *testing.T) {
	b, tb, extra := ptrmapDB(t)
	if tb.Depth() < 2 {
		t.Fatal("the table must have an interior page")
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	ctx := context.Background()
	check := func(what string, pgno uint32, typ sqlitefile.PtrmapType, parent uint32) {
		t.Helper()
		gt, gp, ok, err := v.PtrmapEntry(ctx, pgno)
		if err != nil || !ok || gt != typ || gp != parent {
			t.Errorf("%s (page %d): type %d parent %d ok %v err %v, want type %d parent %d", what, pgno, gt, gp, ok, err, typ, parent)
		}
	}
	check("root", tb.Root(), sqlitefile.PtrRoot, 0)
	for _, lf := range tb.Leaves() {
		check("leaf", lf, sqlitefile.PtrNonRoot, tb.Root())
	}
	ov := tb.Overflow(200)
	if len(ov) < 3 {
		t.Fatalf("overflow chain of %d", len(ov))
	}
	_, cellPage, _ := tb.CellBytes(200)
	check("first overflow page", ov[0], sqlitefile.PtrOverflow1, cellPage)
	for i := 1; i < len(ov); i++ {
		check("later overflow page", ov[i], sqlitefile.PtrOverflow2, ov[i-1])
	}
	for _, f := range extra {
		check("free page", f, sqlitefile.PtrFree, 0)
	}
	// Pages without an entry.
	for _, pg := range []uint32{1, 2, 105, 999999} {
		if _, _, ok, err := v.PtrmapEntry(ctx, pg); ok || err != nil {
			t.Errorf("page %d has no entry: ok %v err %v", pg, ok, err)
		}
	}
	// A database without auto-vacuum has no pointer map.
	plain := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	plain.CreateTable("t", "create table t(a)").Insert(1, 1)
	_, pv := openLive(t, plain.Bytes(), sqlitefile.Options{})
	defer pv.Release()
	if _, _, ok, err := pv.PtrmapEntry(ctx, 3); ok || err != nil {
		t.Errorf("no auto-vacuum: ok %v err %v", ok, err)
	}
}
