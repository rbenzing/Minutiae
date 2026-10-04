package sqlitefile

const (
	minPageSize = 512
	maxPageSize = 65536
	// lockByteOffset is the file offset of the byte the engine reserves for
	// locking; the page holding it is never used (files longer than 1 GiB).
	lockByteOffset = 1 << 30
)

// validPageSize reports whether n is a power of two in 512..65536.
func validPageSize(n int) bool {
	return n >= minPageSize && n <= maxPageSize && n&(n-1) == 0
}

// PageOffset returns the file offset of page pgno, (pgno-1)*pageSize; 0 for
// pgno 0 (there is no page 0) and for a non-positive page size.
func PageOffset(pageSize int, pgno uint32) int64 {
	if pgno == 0 || pageSize <= 0 {
		return 0
	}
	return int64(pgno-1) * int64(pageSize)
}

// LockBytePage returns the number of the page that holds the lock byte,
// 1073741824/pageSize + 1; 0 for a non-positive page size.
func LockBytePage(pageSize int) uint32 {
	if pageSize <= 0 {
		return 0
	}
	return uint32(lockByteOffset/pageSize + 1)
}

// ptrmapPageno returns the number of the pointer-map page that describes page
// pgno in an auto-vacuum database, 0 for pgno < 2. A pointer-map page covers
// the U/5 pages after it; the page that would be the lock-byte page is skipped
// (the map moves to the next page). Written from the format description
// (the plan's Format reference marks it [M]; Task 6 pins it with a pure-function
// test and a sparse reader test).
func ptrmapPageno(pageSize, reserved int, pgno uint32) uint32 {
	if pgno < 2 || pageSize <= 0 {
		return 0
	}
	n := uint64(pageSize-reserved)/5 + 1
	r := (uint64(pgno)-2)/n*n + 2
	if r == uint64(LockBytePage(pageSize)) {
		r++
	}
	if r > 1<<32-1 {
		return 0
	}
	return uint32(r)
}

// isPtrmapPage reports whether pgno is a pointer-map page of an auto-vacuum
// database with the given geometry.
func isPtrmapPage(pageSize, reserved int, pgno uint32) bool {
	return pgno >= 2 && ptrmapPageno(pageSize, reserved, pgno) == pgno
}
