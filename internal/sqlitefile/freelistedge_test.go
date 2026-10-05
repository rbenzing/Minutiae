package sqlitefile_test

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Boundary cases of the freelist walk (Task 7 review, M-7).

// TestFreelistLeafAtAddressableBoundary: the last page of the file (the
// addressable maximum) is a usable leaf; the page after it is not.
func TestFreelistLeafAtAddressableBoundary(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetFreelist([][]uint32{{2, 3, 4}})
	data := b.Bytes()
	last := uint32(len(data) / ps)
	putAt(data, ps, 2, 8, last)    // leaf slot 0: the last page: usable
	putAt(data, ps, 2, 12, last+1) // leaf slot 1: one past the end: not
	fl, v := freeView(t, data, sqlitefile.Options{})
	if v.Addressable() != last {
		t.Fatalf("addressable %d, want %d", v.Addressable(), last)
	}
	if !slices.Equal(fl.Leaves, []uint32{last}) {
		t.Errorf("leaves %v, want only page %d (page %d is past the end)", fl.Leaves, last, last+1)
	}
	if !viewWarns(v, sqlitefile.WarnPageRange, 2) {
		t.Errorf("page-range missing: %v", v.Warnings())
	}
}

// TestFreelistTrunkCapacityBoundary: a trunk of 512-byte pages holds at most
// 126 leaves. Exactly 126 is clean; a stated 127 is freelist-leaf-count and the
// list is cut at 126.
func TestFreelistTrunkCapacityBoundary(t *testing.T) {
	const ps = 512
	build := func() []byte {
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		trunks, _ := chain(2, 127, 126)
		if len(trunks) != 1 || len(trunks[0]) != 127 {
			t.Fatalf("fixture %d trunks", len(trunks))
		}
		b.SetFreelist(trunks)
		return b.Bytes()
	}
	fl, v := freeView(t, build(), sqlitefile.Options{})
	if len(fl.Leaves) != 126 || len(v.Warnings()) != 0 {
		t.Errorf("126 leaves: got %d, warnings %v", len(fl.Leaves), v.Warnings())
	}
	data := build()
	putAt(data, ps, 2, 4, 127)
	fl, v = freeView(t, data, sqlitefile.Options{})
	if len(fl.Leaves) != 126 || !viewWarns(v, sqlitefile.WarnFreelistLeafCount, 2) {
		t.Errorf("127 stated: %d leaves, %v", len(fl.Leaves), v.Warnings())
	}
}

// TestFreelistWalkedAboveHeaderCountWarns: a list longer than the header says
// is as much an anomaly as a shorter one.
func TestFreelistWalkedAboveHeaderCountWarns(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetFreelist([][]uint32{{2, 3, 4, 5}})
	data := b.Bytes()
	binary.BigEndian.PutUint32(data[36:], 2)
	fl, v := freeView(t, data, sqlitefile.Options{})
	if fl.Walked != 4 || fl.HeaderCount != 2 || !viewWarns(v, sqlitefile.WarnFreelistCount, 0) {
		t.Errorf("walked %d header %d %v", fl.Walked, fl.HeaderCount, v.Warnings())
	}
}

// shortTrunkFile returns a database whose LAST page is a freelist trunk of
// only n bytes (a truncated file).
func shortTrunkFile(n int, leaves ...uint32) []byte {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetFreelist([][]uint32{{2, 3, 4}})
	data := b.Bytes() // 4 pages
	binary.BigEndian.PutUint32(data[32:], 4)
	binary.BigEndian.PutUint32(data[36:], uint32(1+len(leaves)))
	clear(data[3*ps : 4*ps])
	putAt(data, ps, 4, 4, uint32(len(leaves)))
	for i, l := range leaves {
		putAt(data, ps, 4, 8+4*i, l)
	}
	return data[:3*ps+n]
}

func TestFreelistTrunkOnAShortPage(t *testing.T) {
	// 16 bytes hold the count and exactly two leaf slots: the last slot ends at the page end.
	fl, v := freeView(t, shortTrunkFile(16, 2, 3), sqlitefile.Options{})
	if !slices.Equal(fl.Trunks, []uint32{4}) || !slices.Equal(fl.Leaves, []uint32{2, 3}) {
		t.Errorf("exact fit: trunks %v leaves %v %v", fl.Trunks, fl.Leaves, v.Warnings())
	}
	// A third slot would reach past the bytes present: reported, the first two kept.
	fl, v = freeView(t, shortTrunkFile(16, 2, 3, 4), sqlitefile.Options{})
	if !slices.Equal(fl.Leaves, []uint32{2, 3}) || !viewWarns(v, sqlitefile.WarnPageUnavailable, 4) {
		t.Errorf("slots past the end: leaves %v %v", fl.Leaves, v.Warnings())
	}
	// Eight bytes are a whole trunk header with no leaves; seven are not a header.
	fl, _ = freeView(t, shortTrunkFile(8), sqlitefile.Options{})
	if !slices.Equal(fl.Trunks, []uint32{4}) || len(fl.Leaves) != 0 {
		t.Errorf("8 bytes: %+v", fl)
	}
	fl, v = freeView(t, shortTrunkFile(7), sqlitefile.Options{})
	if len(fl.Trunks) != 0 || !viewWarns(v, sqlitefile.WarnPageUnavailable, 4) {
		t.Errorf("7 bytes: %+v %v", fl, v.Warnings())
	}
}

// TestFreelistTrunkSlotHalfPresent: a slot of which only two bytes are in the
// file is not read.
func TestFreelistTrunkSlotHalfPresent(t *testing.T) {
	fl, v := freeView(t, shortTrunkFile(18, 2, 3, 4), sqlitefile.Options{})
	if !slices.Equal(fl.Leaves, []uint32{2, 3}) || !viewWarns(v, sqlitefile.WarnPageUnavailable, 4) {
		t.Errorf("leaves %v %v", fl.Leaves, v.Warnings())
	}
}
