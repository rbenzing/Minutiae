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
