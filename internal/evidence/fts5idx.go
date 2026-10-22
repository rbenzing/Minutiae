package evidence

import "slices"

// The recomputation of the segment b-tree index (<table>_idx) and of the doclist indexes (the dlidx rows
// of <table>_data) from the leaf pages (5D Task 14j). It replaces the MATCH reachability pass: instead of
// asking the index whether a search still finds what a rebuild holds, it derives the rows the SQLite
// writer must have produced from the decoded leaves and compares them with the stored rows exactly. A
// row that is missing, different or without a page behind it is a difference, never a tolerated oddity,
// with the exceptions listed under "What cannot be recomputed".
//
// Pinned against the transpiled source of modernc.org/sqlite v1.60.1 (SQLite 3.53.4; the C names are in
// brackets; lines are lib/sqlite_g_0000000000000044.go unless a file is named). The writer is the state
// machine of Fts5SegWriter: it only ever sees pages in order, so this builder is fed the decoded leaves
// of one segment in order and keeps the same state.
//
//	_fts5WriteInit (sqlite_g_0000000000018000.go:11260): iBtPage = 1 and btterm empty, one doclist-index
//	    level. So the first page owns the row with the EMPTY term, whatever its first term is.
//	_fts5WriteAppendTerm (11530): the first term of a leaf other than page 1 calls _fts5WriteBtreeTerm
//	    with the split key n = 1 + _fts5PrefixCompress(previous term, term) bytes of the term (the previous
//	    term is the last term written, counted with the index prefix byte), or the whole term when the
//	    writer has no previous term (the first term written by an incremental-merge step: see V12 below).
//	    Every term (not only the first of a page) sets aDlidx[0].pgno to the page it is on.
//	_fts5WriteBtreeTerm (11629): _fts5WriteFlushBtree for the PENDING page, then the key and the page of
//	    this leaf become pending.
//	_fts5WriteFlushBtree (sqlite_g_0000000000018000.go:11234): when a page is pending, the _idx row
//	    (segid, pending key, pending page<<1 | bFlag) is written, with bFlag the result of
//	    _fts5WriteFlushDlidx (sqlite.go:23914): 1 when the level-0 doclist index buffer is not empty and
//	    nEmpty >= FTS5_MIN_DLIDX_SIZE (4), and that flag also decides whether the buffered levels are written
//	    (_fts5WriteDlidxClear, 11705, writes each non-empty level at segid<<37 | 1<<36 | level<<31 | pgno
//	    of the level and empties it); nEmpty is reset either way.
//	_fts5WriteFinish (11760): after the last leaf was flushed, the pending page is flushed too.
//	_fts5WriteFlushLeaf (11792): a leaf that holds no term calls _fts5WriteBtreeNoTerm (11605): nEmpty++,
//	    and, when no rowid was written on it either and the level-0 buffer is not empty, a 0x00 varint is
//	    appended to the level-0 buffer (a page that only carries the middle of a poslist).
//	_fts5WriteAppendRowid (11494), and the same lines in _fts5FlushOneHash (6935): the first rowid on a
//	    page, written before any term of that page (_fts5WriteAppendTerm clears bFirstRowidInPage, so a rowid
//	    that follows a term never counts), calls _fts5WriteDlidxAppend (11644): per level, a full buffer
//	    (>= pgsz bytes) is written out at once with its first byte set to 1, the next level takes the first
//	    rowid of the page that was written (when it is empty it starts with 0, pgno, first rowid), and the
//	    buffer restarts; then the entry is appended: a delta from the previous rowid, or, for the first entry
//	    of a buffer, a flag byte (1 when the buffer above was just started, 0 otherwise), the page number
//	    and the rowid.
//	_fts5IndexMergeLevel (sqlite_g_0000000000000044.go:7375): an incremental-merge step that continues a
//	    segment creates a new writer with pgno = pgnoLast+1 and iBtPage = 0 and no previous term; steps end
//	    at a term boundary and _fts5WriteFinish flushes the pending page, so a continuous simulation over
//	    the pages gives the same rows, with the one deviation V12.
//	_fts5TrimSegments (sqlite_g_0000000000000044.go:11050): the input segments of an unfinished merge are
//	    trimmed: their leaves below the first unmerged term are deleted, the first remaining leaf is rewritten
//	    to start at that term, pgnoFirst moves up, and a drained segment gets pgnoFirst = pgnoLast = 0. No
//	    _idx or doclist-index row is deleted.
//	_fts5SegIterSeekInit (:10181): a row whose page is below pgnoFirst is not used (the search starts at
//	    pgnoFirst, without a doclist index), and a segment with pgnoFirst = 0 is skipped by the iterators.
//
// V12: a split key is either the shortest prefix of the page's first term that is longer than the common
// prefix with the previous term, or the whole first term (the first term of an incremental-merge step). Both
// are accepted for every page; nothing else is. The number of whole-term keys met is counted
// (ftsIdxStats.FullKeys).
//
// What cannot be recomputed, and what is done instead:
//   - a segment drained by a merge (first page 0, last page 0) has no readable leaf; its rows are never
//     used by a search and are not compared (the segment still counts as referenced by the structure, so
//     checkUnreferenced keeps its rows);
//   - the _idx rows of a trimmed segment (first page > 1) for pages below its first page are left over from
//     before the trim and are never used by a search (see _fts5SegIterSeekInit): they are counted
//     (ftsIdxStats.StaleIdxRows) and not compared. The row of the first remaining page keeps the key of
//     the term that started that page before the trim, which the rewritten leaf no longer shows: only its
//     page and its doclist-index flag are compared.
//   - the doclist-index rows of a trimmed segment whose page number is below its first page belong to
//     doclists that were drained before the trim (the term of a doclist index is on the page the index
//     is named for, and a doclist with an index runs past that page, so a term on the first page or above
//     is never drained): they are counted (ftsIdxStats.StaleDlidxRows) and not compared. Every row the
//     leaf pages produce is compared, wherever it lies.
//
// The sizes are bounded by what is present: the builder holds the pending row, the doclist-index buffers
// (at most the bytes of the rows they will become) and the expected doclist-index rows of one segment.

const (
	ftsMinDlidxSize = 4    // FTS5_MIN_DLIDX_SIZE
	ftsDefaultPgsz  = 4050 // FTS5_DEFAULT_PAGE_SIZE
	ftsDlidxFlag    = int64(1) << 36
)

// ftsAppendVarint appends v the way SQLite's varint encoder does (canonical: the fewest bytes, 9 above 56 bits).
func ftsAppendVarint(b []byte, v uint64) []byte {
	var t [9]byte
	if v>>56 != 0 {
		t[8] = byte(v)
		v >>= 8
		for i := 7; i >= 0; i-- {
			t[i] = byte(v&0x7f) | 0x80
			v >>= 7
		}
		return append(b, t[:]...)
	}
	n := ftsVarintLen(v)
	for i := n - 1; i >= 0; i-- {
		t[i] = byte(v&0x7f) | 0x80
		v >>= 7
	}
	t[n-1] &= 0x7f
	return append(b, t[:n]...)
}

// ftsIdxRow is one expected row of <table>_idx.
type ftsIdxRow struct {
	Segid int64
	Term  []byte // the split key as stored, with the index prefix byte; empty for the first page
	Alt   []byte // the whole first term, accepted too when it differs from Term (V12)
	Pgno  int64  // as stored: page<<1, plus 1 when the doclist of the page's last term has a doclist index
	// AnyKey: the key cannot be recomputed (the first page of a trimmed segment), only Pgno is compared.
	AnyKey bool
	// For the confirmation by search: the first term of the page and the first rowid of its doclist.
	FirstTerm  []byte
	FirstRowid int64
	HasRowid   bool
}

// ftsDlidxRow is one expected row of the doclist index of a segment.
type ftsDlidxRow struct {
	ID         int64 // the _data id
	Level      int
	Pgno       int64
	Block      []byte
	Term       []byte // the term whose doclist it indexes (with the prefix byte)
	FirstRowid int64  // the first rowid the page holds
}

type ftsDlidxLevel struct {
	buf       []byte
	pgno      int64
	prevValid bool
	prev      int64
}

// ftsIdxBuilder is the part of Fts5SegWriter that produces the _idx rows and the doclist-index rows.
type ftsIdxBuilder struct {
	seg  ftsSegment
	pgsz int
	emit func(ftsIdxRow)

	started bool
	btPage  int64 // iBtPage: the page whose row is pending, 0 for none
	pending ftsIdxRow
	nEmpty  int
	lvls    []ftsDlidxLevel
	// dlTerm is the last term seen: the term of the doclist the open doclist index belongs to
	dlTerm []byte

	dl       map[int64]*ftsDlidxRow
	fullKeys int
}

func newFTSIdxBuilder(seg ftsSegment, pgsz int, emit func(ftsIdxRow)) *ftsIdxBuilder {
	if pgsz <= 0 {
		pgsz = ftsDefaultPgsz
	}
	return &ftsIdxBuilder{seg: seg, pgsz: pgsz, emit: emit, lvls: make([]ftsDlidxLevel, 1), dl: map[int64]*ftsDlidxRow{}}
}

// leaf takes the next page of the segment. last is the last term on the page (with the prefix byte).
func (b *ftsIdxBuilder) leaf(l ftsLeafInfo, last []byte) {
	first := l.Pgno == b.seg.First
	if !b.started {
		b.started = true
		b.btPage = b.seg.First
		b.pending = ftsIdxRow{Segid: b.seg.ID, AnyKey: b.seg.First != 1}
	}
	// the rowid that precedes every term of the page: first on the page, so it is a doclist-index entry
	if l.HasRowid {
		b.dlidxAppend(l.FirstRowid, l.Pgno)
	}
	if l.HasTerm {
		if !first {
			b.flushBtree()
			b.btPage = l.Pgno
			b.pending = ftsIdxRow{Segid: b.seg.ID}
			b.splitKey(l)
		}
		if first || b.pending.FirstTerm == nil {
			b.pending.FirstTerm = slices.Clone(l.FirstTerm)
		}
		// the first doclist that starts on the page: after the rowid that continues the previous doclist, if any
		at := 0
		if l.HasRowid {
			at = 1
		}
		if at < len(l.RowidsOnPage) {
			b.pending.FirstRowid, b.pending.HasRowid = l.RowidsOnPage[at], true
		}
		b.dlTerm = append(b.dlTerm[:0], last...)
		b.lvls[0].pgno = l.Pgno
		return
	}
	// a page without a term (_fts5WriteBtreeNoTerm)
	if !l.HasRowid && len(b.lvls[0].buf) > 0 {
		b.lvls[0].buf = ftsAppendVarint(b.lvls[0].buf, 0)
	}
	b.nEmpty++
}

// splitKey sets the key of the pending row for a page that starts with term l.FirstTerm.
func (b *ftsIdxBuilder) splitKey(l ftsLeafInfo) {
	term := l.FirstTerm
	if len(l.PrevTerm) == 0 {
		b.pending.Term = slices.Clone(term)
		return
	}
	nMin := min(len(l.PrevTerm), len(term))
	cp := 0
	for cp < nMin && l.PrevTerm[cp] == term[cp] {
		cp++
	}
	n := min(cp+1, len(term))
	b.pending.Term = slices.Clone(term[:n])
	if n < len(term) {
		b.pending.Alt = slices.Clone(term)
	}
}

// finish is _fts5WriteFinish: the pending row is written.
func (b *ftsIdxBuilder) finish() { b.flushBtree() }

func (b *ftsIdxBuilder) flushBtree() {
	if b.btPage == 0 {
		return
	}
	flag := int64(0)
	if len(b.lvls[0].buf) > 0 && b.nEmpty >= ftsMinDlidxSize {
		flag = 1
	}
	b.dlidxClear(flag == 1)
	b.nEmpty = 0
	row := b.pending
	row.Pgno = b.btPage<<1 + flag
	b.btPage = 0
	b.emit(row)
}

func (b *ftsIdxBuilder) dlidxClear(write bool) {
	for i := range b.lvls {
		lv := &b.lvls[i]
		if len(lv.buf) == 0 {
			break
		}
		if write {
			b.dlidxWrite(i, lv.pgno, lv.buf)
		}
		lv.buf = lv.buf[:0]
		lv.prevValid = false
	}
}

func (b *ftsIdxBuilder) dlidxWrite(level int, pgno int64, buf []byte) {
	id := b.seg.ID<<ftsSegidShift + ftsDlidxFlag + int64(level)<<31 + pgno
	row := &ftsDlidxRow{ID: id, Level: level, Pgno: pgno, Block: slices.Clone(buf), Term: slices.Clone(b.dlTerm)}
	// the first rowid of the page: flag byte, page number, rowid
	if _, n := SQLiteVarint(buf[1:]); n > 0 {
		if v, m := SQLiteVarint(buf[1+n:]); m > 0 {
			row.FirstRowid = int64(v)
		}
	}
	b.dl[id] = row
}

// dlidxAppend is _fts5WriteDlidxAppend for the rowid that is first on page writerPgno.
func (b *ftsIdxBuilder) dlidxAppend(rowid, writerPgno int64) {
	done := false
	for i := 0; !done; i++ {
		lv := &b.lvls[i]
		if len(lv.buf) >= b.pgsz {
			// the buffer is full: write it, push its first rowid up and start it again
			lv.buf[0] = 1
			b.dlidxWrite(i, lv.pgno, lv.buf)
			for len(b.lvls) < i+2 {
				b.lvls = append(b.lvls, ftsDlidxLevel{})
			}
			lv = &b.lvls[i]
			up := &b.lvls[i+1]
			if len(up.buf) == 0 {
				var first uint64
				if _, n := SQLiteVarint(lv.buf[1:]); n > 0 {
					first, _ = SQLiteVarint(lv.buf[1+n:])
				}
				up.pgno = lv.pgno
				up.buf = ftsAppendVarint(up.buf, 0)
				up.buf = ftsAppendVarint(up.buf, uint64(lv.pgno))
				up.buf = ftsAppendVarint(up.buf, first)
				up.prevValid, up.prev = true, int64(first)
			}
			lv.buf = lv.buf[:0]
			lv.prevValid = false
			lv.pgno++
		} else {
			done = true
		}
		var val uint64
		if lv.prevValid {
			val = uint64(rowid) - uint64(lv.prev)
		} else {
			pg := writerPgno
			if i > 0 {
				pg = b.lvls[i-1].pgno
			}
			flag := uint64(1)
			if done {
				flag = 0
			}
			lv.buf = ftsAppendVarint(lv.buf, flag)
			lv.buf = ftsAppendVarint(lv.buf, uint64(pg))
			val = uint64(rowid)
		}
		lv.buf = ftsAppendVarint(lv.buf, val)
		lv.prevValid, lv.prev = true, rowid
	}
}
