package sqlitefile

import "fmt"

// pageSet is a bitset over page numbers 1..max, the visited set of a whole
// traversal. It is sized from the addressable page limit (never from a
// declared count) and charged to the budget.
type pageSet struct {
	bits []uint64
	max  uint32
}

// newPageSet returns a set for page numbers 1..addressable, charged to l.
func newPageSet(l *ledger, addressable uint32) (*pageSet, error) {
	words := int64(addressable)/64 + 1
	if err := l.alloc(words * 8); err != nil {
		return nil, fmt.Errorf("page set of %d pages: %w", addressable, err)
	}
	return &pageSet{bits: make([]uint64, words), max: addressable}, nil
}

// mark records pgno and reports whether it was new. A page number outside
// 1..max is never recorded and reports false: callers range-check first.
func (s *pageSet) mark(pgno uint32) bool {
	if pgno == 0 || pgno > s.max {
		return false
	}
	w, b := pgno/64, uint64(1)<<(pgno%64)
	if s.bits[w]&b != 0 {
		return false
	}
	s.bits[w] |= b
	return true
}

// bytes is what the set is charged.
func (s *pageSet) bytes() int64 { return int64(len(s.bits)) * 8 }

// release gives the charge back.
func (s *pageSet) release(l *ledger) { l.free(s.bytes()) }
