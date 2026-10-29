package carve

import (
	"errors"
	"strconv"
)

// validateSQLite accepts a SQLite 3 database from its 100-byte header and the
// b-tree header of page 1. The in-header size (page count) is trusted only
// when the change counter equals the version-valid-for field and the count is
// non-zero (the file format's own rule); then the length is the page size
// times the page count, which must fit the window. A legacy file without a
// valid in-header size is bounded to the window end. No page past page 1 is
// read. The layout is from memory (docs/limitations.md).
func validateSQLite(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	var h [101]byte // the header (100) and page 1's b-tree page type
	if n, err := w.ReadAt(h[:], 0); err != nil {
		if n >= 16 && string(h[:16]) != "SQLite format 3\x00" {
			return reject("bad-signature")
		}
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	if string(h[:16]) != "SQLite format 3\x00" {
		return reject("bad-signature")
	}
	be32 := func(o int) uint64 {
		return uint64(h[o])<<24 | uint64(h[o+1])<<16 | uint64(h[o+2])<<8 | uint64(h[o+3])
	}
	pageSize := uint64(h[16])<<8 | uint64(h[17])
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		return reject("bad-page-size")
	}
	if h[18] < 1 || h[18] > 2 || h[19] < 1 || h[19] > 2 ||
		h[21] != 64 || h[22] != 32 || h[23] != 32 ||
		be32(56) < 1 || be32(56) > 3 || be32(44) < 1 || be32(44) > 4 {
		return reject("bad-header")
	}
	basis := []string{"page_size=" + strconv.FormatUint(pageSize, 10)}
	pages := be32(28)
	if pages == 0 || be32(24) != be32(92) {
		return Verdict{
			Outcome: Bounded, Length: w.Len(), Basis: basis,
			Assumptions: []string{"size-from-header-invalid"},
		}
	}
	// pages < 2^32 and pageSize <= 2^16, so the product cannot overflow.
	length := int64(pageSize * pages)
	if length > w.Len() {
		return reject("truncated")
	}
	if be32(32) > pages || be32(36) > pages {
		return reject("bad-header")
	}
	if h[100] != 0x0D && h[100] != 0x05 {
		return reject("bad-btree")
	}
	return Verdict{
		Outcome: Validated, Length: length,
		Basis: append(basis, "pages="+strconv.FormatUint(pages, 10)),
	}
}
