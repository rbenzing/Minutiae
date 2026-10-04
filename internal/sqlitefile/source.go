package sqlitefile

import "fmt"

// PageLoc says where the image of a page lies on disk.
type PageLoc struct {
	File   FileKind
	Offset int64  // byte offset of the page image in File
	Frame  uint32 // WAL slot (1-based) when File == FileWAL
	Record int    // journal record index (0-based) when File == FileJournal
}

// Page is one page image. Data is a copy the caller owns; it holds the bytes
// present, which for a trailing partial page of a truncated file is fewer than
// a page.
type Page struct {
	Number uint32
	Data   []byte
	Loc    PageLoc
}

// pageSource supplies page images. Implementations: dbSource (the database
// file as found), the page cache wrapped around any source, and later the
// WAL and journal overlays.
type pageSource interface {
	has(pgno uint32) bool
	// read returns the page image (shared and read-only: callers copy what
	// they keep) and where it lies. err is ErrPageUnavailable for a page the
	// source cannot supply, or an I/O error as it is.
	read(pgno uint32) (data []byte, loc PageLoc, err error)
}

// dbSource reads pages of the database file as found.
type dbSource struct{ d *DB }

func (s dbSource) has(pgno uint32) bool {
	return pgno != 0 && int64(pgno) <= s.d.env.opts.Limits.MaxPages &&
		PageOffset(s.d.info.PageSize, pgno) < s.d.size
}

func (s dbSource) read(pgno uint32) ([]byte, PageLoc, error) {
	data, err := s.d.readRawPage(pgno)
	if err != nil {
		return nil, PageLoc{}, err
	}
	return data, PageLoc{File: FileDB, Offset: PageOffset(s.d.info.PageSize, pgno)}, nil
}

// visitor is the one visited-set contract of the library: mark records pgno
// and reports whether this is the first time. Whole-tree traversals pass a
// *pageSet; point reads and per-image chains pass a bounded mapVisitor.
type visitor interface {
	mark(pgno uint32) (first bool)
}

// mapVisitorEntryCost is what one visited page costs in a mapVisitor, charged
// up front for the whole capacity.
const mapVisitorEntryCost = 64

// maxMapVisited bounds a mapVisitor.
const maxMapVisited = 1 << 16

// mapVisitor is a small bounded visited set. Past its capacity mark returns
// false and full reports true: the walk must end as if it had met a cycle.
type mapVisitor struct {
	seen map[uint32]struct{}
	max  int
	full bool
	cost int64
}

// newMapVisitor returns a visitor for at most n pages (n is clamped to
// maxMapVisited); its capacity is charged to l and given back by release.
func newMapVisitor(l *ledger, n int) (*mapVisitor, error) {
	n = min(max(n, 1), maxMapVisited)
	cost := int64(n) * mapVisitorEntryCost
	if err := l.alloc(cost); err != nil {
		return nil, fmt.Errorf("visited set: %w", err)
	}
	return &mapVisitor{seen: map[uint32]struct{}{}, max: n, cost: cost}, nil
}

func (m *mapVisitor) mark(pgno uint32) bool {
	if _, dup := m.seen[pgno]; dup {
		return false
	}
	if len(m.seen) >= m.max {
		m.full = true
		return false
	}
	m.seen[pgno] = struct{}{}
	return true
}

// release gives the capacity back.
func (m *mapVisitor) release(l *ledger) { l.free(m.cost); m.cost = 0 }
