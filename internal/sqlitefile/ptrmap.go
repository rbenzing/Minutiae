package sqlitefile

import (
	"context"
	"encoding/binary"
	"fmt"
)

// PtrmapType is the kind of a pointer-map entry.
type PtrmapType uint8

// The pointer-map entry types.
const (
	PtrRoot      PtrmapType = 1 // a b-tree root; parent 0
	PtrFree      PtrmapType = 2 // a freelist page; parent 0
	PtrOverflow1 PtrmapType = 3 // the first overflow page of a cell; parent is the page holding the cell
	PtrOverflow2 PtrmapType = 4 // a later overflow page; parent is the previous overflow page
	PtrNonRoot   PtrmapType = 5 // a b-tree page that is not a root; parent is its interior page
)

// PtrmapPageFor returns the pointer-map page that describes page pgno of an
// auto-vacuum database with the given page size and usable size: 0 for pgno
// below 2. A map page covers the usable/5 pages after it, so the map pages are
// 2, 2+n, 2+2n ... with n = usable/5+1; the one that would be the lock-byte page
// is moved to the next page.
func PtrmapPageFor(pageSize, usable int, pgno uint32) uint32 {
	return ptrmapPageno(pageSize, pageSize-usable, pgno)
}

// ptrmapEntrySize is the size of one entry: a type byte and a 4-byte parent.
const ptrmapEntrySize = 5

// PtrmapEntry reads the pointer-map entry of page pgno. ok is false, with a nil
// error, for a database that is not auto-vacuum, for page 1, a pointer-map page,
// the lock-byte page or a page outside 1..Addressable, and for an entry whose type is
// not 1..5 (never written). An error is for a pointer-map page that cannot be read.
func (v *View) PtrmapEntry(ctx context.Context, pgno uint32) (t PtrmapType, parent uint32, ok bool, err error) {
	defer guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	info := v.info
	if info.AutoVacuum == AVNone || pgno < 3 || pgno > v.addr || pgno == LockBytePage(info.PageSize) ||
		isPtrmapPage(info.PageSize, info.Reserved, pgno) {
		return 0, 0, false, nil
	}
	r := ptrmapPageno(info.PageSize, info.Reserved, pgno)
	if r == 0 || r > v.addr {
		return 0, 0, false, nil
	}
	data, _, err := v.cache.read(r)
	if err != nil {
		return 0, 0, false, fmt.Errorf("pointer-map page %d: %w", r, err)
	}
	off := ptrmapEntrySize * int(pgno-r-1)
	if off+ptrmapEntrySize > len(data) || off+ptrmapEntrySize > info.UsableSize {
		return 0, 0, false, nil
	}
	typ := PtrmapType(data[off])
	if typ < PtrRoot || typ > PtrNonRoot {
		return 0, 0, false, nil
	}
	return typ, binary.BigEndian.Uint32(data[off+1:]), true, nil
}
