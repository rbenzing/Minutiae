package evidence

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// The strict FTS5 leaf and segment decoder (5D Task 14h). It reads the leaf pages (the _data rows with
// height 0 and no doclist-index bit, so a local row id below 1<<31) of the segments a structure record
// names, and reports every posting, and every delete marker, exactly as a query would see them. It is
// strict on purpose: a page that SQLite itself could not have written is an error, not a tolerated
// oddity, because P11 proves that the index is what the audited records produce.
//
// Pinned against the transpiled source of modernc.org/sqlite v1.60.1 (SQLite 3.53.4; the C names are in
// brackets; line numbers are lib/sqlite_windows.go unless a file is named):
//
//	page   = u16be firstRowidOff | u16be szLeaf | body[4:szLeaf] | footer[szLeaf:]   [_fts5WriteFlushLeaf, 36216]
//	footer = varints: the first is the absolute offset of the first term start, each next one a delta (> 0)
//	         [_fts5WriteAppendTerm, 36055: pgidx gets buf.n - iPrevPgidx; _fts5WriteFlushLeaf appends it]
//	term   = first term on the page: varint nTerm, bytes; otherwise varint nPrefix, varint nSuffix, bytes,
//	         nPrefix = _fts5PrefixCompress(previous term, term), the maximal common prefix [_fts5WriteAppendTerm]
//	entry  = rowid varint, size varint (nBytes<<1 | delete flag), nBytes of poslist
//	         [_fts5WriteAppendRowid, 36019; _fts5FlushOneHash, 28679; _fts5GetPoslistSize]
//	rowid  = absolute after a term and as the first rowid on a page (when it is first, the page header holds
//	         its offset: _fts5WriteAppendRowid sets it only while bFirstRowidInPage is set, and
//	         _fts5WriteAppendTerm clears that flag, so a rowid that follows a term on the same page leaves
//	         the header 0); otherwise a delta from the previous rowid, > 0
//	poslist= varints: 1 starts a column (next varint the column, next the first offset + 2); otherwise
//	         previous offset + varint - 2 [_sqlite3Fts5PoslistNext64: a value below 2 after the column is a
//	         corrupt record]; _fts5GetPoslistSize and _fts5GetVarint32 read the size and the offsets as 32-bit
//	         varints
//
// What the writer does that shapes the decoder:
//   - _fts5FlushOneHash splits a long poslist with _fts5PoslistPrefix, which always keeps the first varint
//     (the size) and then whole varints: a poslist, and, when a page filled up right after a rowid, its size
//     varint, can start the next page. The reader therefore keeps an entry state across pages (the "poslist
//     can split across leaves" of the spike), and feeds the size and the poslist byte by byte into a
//     varint accumulator so that a split inside a varint would still decode (it never happens in a genuine
//     index, and the strict canonical check still applies to the joined varint).
//   - The first term of a page is stored in full, the others are prefix-compressed against the previous
//     term, which for the first term of a page is the last term of the previous page (not stored).
//   - Terms of the main index carry the prefix byte '0' (FTS5_MAIN_PREFIX, 0x30). Minutiae declares no
//     prefix indexes, so any other first byte is an error. A posting is reported with that byte removed,
//     as fts5vocab shows the term. The terms of an ftsLeafInfo keep it, as _idx stores it.
//   - The positions of a delete marker (size & 1) are validated like any other, and not reported.
//
// Segment age (pinned for 14j): _fts5MultiIterNew (lib/sqlite_g_0000000000000044.go:8775) walks the levels
// from 0 up and each level from its last segment to its first, and _fts5MultiIterDoCompare (:8608) lets the
// lower iterator win a tie of term and rowid, so level 0 is the newest and, inside a level, the last-listed
// segment is the newest. ftsSegmentAge numbers them that way.
//
// Facts established on genuine indexes (TestFTS5DecodeEqualsVocabOnGenuineIndexes and the other tests of
// this file): a genuine leaf is never empty (every flush follows a write of at least one byte), so szLeaf
// 4 is refused; every page First..Last of a segment exists, none is skipped, with ONE exception (5D-14h2 D143):
// in the middle of an incremental merge ("INSERT INTO t(t, rank) VALUES('merge', N)") _fts5TrimSegments leaves
// the structure listing a segment with First == Last == 0 and no leaf rows; ftsSegment.Empty() marks it and the
// reader reads nothing for it. And the first page of any other segment starts with a term at offset 4.
//
// Bounds (CLAUDE.md): nothing is sized from a declared count or length. The term buffer grows only by
// suffix bytes that are present in the page and never past fts5MaxTokenSize+1; a declared poslist size is
// only a counter that the following bytes decrement. Every loop consumes at least one byte per iteration.
// Rowids and positions are added with overflow checks.
// The rowid-delta bound assumes positive rowids, as Minutiae's are (record ids start at 1, and an FTS rowid is a
// record id): a delta is refused above MaxInt64 or when the sum does not exceed the previous rowid, which is
// exact for a previous rowid >= 0. A segment that holds negative rowids is outside what this decoder promises.

const (
	ftsMainPrefix  = 0x30 // FTS5_MAIN_PREFIX '0'
	ftsLeafHeader  = 4    // firstRowidOff and szLeaf
	ftsLeafColumns = 2    // summary and body: the columns of both Minutiae FTS tables
)

var errFTSLeaf = errors.New("malformed FTS5 leaf page")

// ftsLeafError is the typed error of the decoder. Off is the offset in the page where the problem is.
type ftsLeafError struct {
	Seg, Page int64
	Off       int
	Reason    string
}

func (e *ftsLeafError) Error() string {
	return fmt.Sprintf("%v: segment %d page %d offset %d: %s", errFTSLeaf, e.Seg, e.Page, e.Off, e.Reason)
}

func (e *ftsLeafError) Unwrap() error { return errFTSLeaf }

// ftsSegment is one segment of the structure record. Pos is the index inside its level.
type ftsSegment struct {
	ID          int64
	Level, Pos  int
	First, Last int64
}

// Empty says the segment holds no leaf pages: _fts5TrimSegments, in the middle of an incremental merge, leaves
// the structure listing a merged-away segment with pages 0 to 0 and deletes its rows (5D-14h2 D143). Its
// _data rows, if a tampered index has any, are still unreferenced-row candidates for checkUnreferenced.
func (s ftsSegment) Empty() bool { return s.First == 0 && s.Last == 0 }

// ftsSegmentAge numbers the segments of a structure: a larger number is newer (see the file comment).
func ftsSegmentAge(segs []ftsSegment) map[int64]int {
	order := slices.Clone(segs)
	slices.SortStableFunc(order, func(a, b ftsSegment) int {
		if a.Level != b.Level {
			return a.Level - b.Level
		}
		return b.Pos - a.Pos
	})
	age := make(map[int64]int, len(order))
	for i, s := range order {
		age[s.ID] = len(order) - i
	}
	return age
}

// ftsSink receives what the decoder finds. Any func may be nil. The term slices are valid only during
// the call.
//
// Contract (5D-14h2 D147): the decoder calls the funcs while it decodes a page, before it knows that the page,
// or a later page of the segment, is valid. When Page or Finish returns an error the caller must discard
// everything that segment contributed through the sink; only a segment whose pages and Finish all returned
// nil has contributed real postings. Task 14i enforces it.
type ftsSink struct {
	// Posting is one position of a term in a record. The term is as fts5vocab shows it (no index prefix byte).
	Posting func(term []byte, rowid int64, col int, off int64)
	// Delete is a delete marker of the segment for a record and a term.
	Delete func(term []byte, rowid int64)
	// Leaf is called once per page that decoded, after its postings.
	Leaf func(l ftsLeafInfo)
}

// ftsLeafInfo describes one decoded page. Its slices are valid only during the call. Terms are as stored,
// with the index prefix byte.
type ftsLeafInfo struct {
	Pgno, FirstRowid  int64
	HasRowid, HasTerm bool
	FirstTerm         []byte // the first term starting on this page
	PrevTerm          []byte // the last term before this page in the segment
	TermOffsets       []int  // the decoded footer: absolute offsets of the term starts
	RowidOffset       int    // the page header field
	SzLeaf            int
	DoclistContinues  bool    // a doclist runs from the previous page into this one
	RowidsOnPage      []int64 // the absolute rowid at the start of each doclist piece on this page, in order (for the doclist index)
}

// Entry states of the reader between two bytes.
const (
	ftsStRowid uint8 = iota // the next byte starts a rowid or a term
	ftsStSize               // inside or before the size varint of an entry
	ftsStPos                // inside the poslist of an entry
)

// ftsSegReader decodes the pages of one segment in order.
type ftsSegReader struct {
	seg  ftsSegment
	next int64
	err  error // latched: a reader that failed stays failed

	term     []byte // the current term, with the prefix byte
	haveTerm bool
	needDoc  bool // a term was read and no entry has followed yet
	rowid    int64
	haveRow  bool // rowid is valid for the doclist being read

	state uint8
	left  uint64 // poslist bytes still to come
	del   bool
	acc   uint64 // the varint being assembled
	nacc  int
	col   int
	off   int64
	have  bool  // a position was read in the current column
	pend  uint8 // 1: column number expected; 2: first offset of the column expected

	consumed int // bytes taken from pages (the bound test)
	pgno     int64
	sink     ftsSink

	offs     []int
	offsBuf  [8]int
	rowids   []int64
	rowidBuf [4]int64
	first    []byte
	prev     []byte
}

func newFTSSegReader(seg ftsSegment) *ftsSegReader {
	r := &ftsSegReader{seg: seg, next: seg.First}
	if seg.Empty() {
		r.next = seg.Last + 1 // nothing to read
	}
	return r
}

func (s *ftsSegReader) fail(off int, reason string) error {
	return &ftsLeafError{Seg: s.seg.ID, Page: s.pgno, Off: off, Reason: reason}
}

// ftsVarintLen is the length SQLite's encoder gives v: 9 bytes when any of the top eight bits is set,
// otherwise the fewest 7-bit groups.
func ftsVarintLen(v uint64) int {
	if v>>56 != 0 {
		return 9
	}
	n := (bits.Len64(v) + 6) / 7
	return max(n, 1)
}

// ftsVarint reads one canonical varint. n is 0 when b is truncated and -1 when the varint is not minimal.
func ftsVarint(b []byte) (v uint64, n int) {
	v, n = SQLiteVarint(b)
	if n != 0 && n != ftsVarintLen(v) {
		return 0, -1
	}
	return v, n
}

// feed adds one byte to the varint being assembled and says when it is complete. bad is set for a varint
// that is not minimal.
func (s *ftsSegReader) feed(b byte) (v uint64, done, bad bool) {
	if s.nacc < 8 {
		s.acc = s.acc<<7 | uint64(b&0x7f)
		s.nacc++
		if b&0x80 != 0 {
			return 0, false, false
		}
	} else {
		s.acc = s.acc<<8 | uint64(b)
		s.nacc++
	}
	v, n := s.acc, s.nacc
	s.acc, s.nacc = 0, 0
	return v, true, n != ftsVarintLen(v)
}

// Page decodes the leaf page pgno, which must be the next page of the segment.
func (s *ftsSegReader) Page(pgno int64, block []byte, sink ftsSink) error {
	if s.err != nil {
		return s.err
	}
	s.pgno, s.sink = pgno, sink
	if err := s.page(block); err != nil {
		s.err = err
		return err
	}
	return nil
}

// Finish says whether the segment was read completely.
func (s *ftsSegReader) Finish() error {
	if s.err != nil {
		return s.err
	}
	if s.seg.Empty() {
		return nil // an emptied segment (D143) holds no page, no term and nothing to miss
	}
	s.pgno = s.next
	switch {
	case s.next != s.seg.Last+1:
		return s.fail(0, "missing page")
	case s.state != ftsStRowid || s.nacc != 0:
		return s.fail(0, "poslist open at the end of the segment")
	case s.needDoc || !s.haveTerm:
		return s.fail(0, "term without a doclist")
	}
	return nil
}

func (s *ftsSegReader) page(block []byte) error {
	switch {
	case s.seg.Empty():
		return s.fail(0, "page of an empty segment")
	case s.pgno < s.seg.First || s.pgno > s.seg.Last:
		return s.fail(0, "page outside the segment")
	case s.pgno > s.next:
		return s.fail(0, "missing page")
	case s.pgno < s.next:
		return s.fail(0, "page out of order")
	}
	s.next++
	n := len(block)
	if n < ftsLeafHeader {
		return s.fail(0, "short page")
	}
	rowidOff := int(block[0])<<8 | int(block[1])
	szLeaf := int(block[2])<<8 | int(block[3])
	if szLeaf < ftsLeafHeader || szLeaf > n {
		return s.fail(2, "szLeaf out of range")
	}
	if szLeaf == ftsLeafHeader {
		return s.fail(2, "empty leaf")
	}
	s.consumed += ftsLeafHeader

	// the footer
	if s.offs == nil {
		s.offs = s.offsBuf[:0]
		s.rowids = s.rowidBuf[:0]
	}
	s.offs = s.offs[:0]
	prev := 0
	for p := szLeaf; p < n; {
		v, k := ftsVarint(block[p:n])
		if k <= 0 {
			return s.fail(p, varintReason(k, "truncated footer"))
		}
		// v is the offset (first) or the distance to the previous one; both must keep the offset inside the body
		if len(s.offs) == 0 {
			if v < ftsLeafHeader || v >= uint64(szLeaf) {
				return s.fail(p, "term offset out of range")
			}
		} else if v == 0 || v >= uint64(szLeaf-prev) {
			return s.fail(p, "term offset out of range")
		}
		prev += int(v)
		s.offs = append(s.offs, prev)
		p += k
		s.consumed += k
	}

	hasLeaf := s.sink.Leaf != nil
	if hasLeaf {
		s.prev = append(s.prev[:0], s.term...)
	}
	if s.pgno == s.seg.First && (len(s.offs) == 0 || s.offs[0] != ftsLeafHeader) {
		return s.fail(ftsLeafHeader, "first page of a segment does not start with a term")
	}

	var (
		pos         = ftsLeafHeader
		ti          = 0
		termOnPage  bool
		rowidOnPage bool
		firstRowid  int64
		firstPos    int
	)
	s.rowids = s.rowids[:0]
	// Invariant: pos never passes the next term start, because every read below is cut at it (limit) and a
	// term that runs into the next one is refused, so at the end of the body every footer offset was a term.
	for pos < szLeaf {
		if ti < len(s.offs) {
			if pos == s.offs[ti] {
				if s.state != ftsStRowid || s.nacc != 0 {
					return s.fail(pos, "footer mismatch")
				}
				k, err := s.readTerm(block[pos:szLeaf], ti == 0, pos)
				if err != nil {
					return err
				}
				if ti+1 < len(s.offs) && pos+k > s.offs[ti+1] {
					return s.fail(pos, "footer mismatch")
				}
				if ti == 0 && hasLeaf {
					s.first = append(s.first[:0], s.term...)
				}
				pos += k
				s.consumed += k
				ti++
				termOnPage = true
				continue
			}
		}
		limit := szLeaf
		if ti < len(s.offs) {
			limit = s.offs[ti]
		}
		cut := "truncated varint"
		if limit < szLeaf {
			cut = "footer mismatch"
		}
		switch s.state {
		case ftsStRowid:
			if !s.haveTerm {
				return s.fail(pos, "doclist before any term")
			}
			v, k := ftsVarint(block[pos:limit])
			if k <= 0 {
				return s.fail(pos, varintReason(k, cut))
			}
			abs := !s.haveRow || (!termOnPage && !rowidOnPage)
			if abs {
				r := int64(v)
				if s.haveRow && r <= s.rowid {
					return s.fail(pos, "rowid not ascending")
				}
				s.rowid = r
				s.rowids = append(s.rowids, r)
				if !termOnPage && !rowidOnPage {
					firstPos, firstRowid = pos, r
				}
			} else {
				// v > MaxInt64 is not a delta; a sum that does not exceed the old rowid wrapped around
				if v > math.MaxInt64 || s.rowid+int64(v) <= s.rowid {
					return s.fail(pos, "rowid not ascending")
				}
				s.rowid += int64(v)
			}
			s.haveRow, rowidOnPage, s.needDoc = true, true, false
			s.state = ftsStSize
			pos += k
			s.consumed += k
		case ftsStSize:
			for pos < limit && s.state == ftsStSize {
				v, done, bad := s.feed(block[pos])
				pos++
				s.consumed++
				if bad {
					return s.fail(pos-1, "non-canonical varint")
				}
				if done {
					if err := s.onSize(v, pos-1); err != nil {
						return err
					}
				}
			}
		default: // ftsStPos
			for pos < limit && s.state == ftsStPos {
				v, done, bad := s.feed(block[pos])
				pos++
				s.consumed++
				s.left--
				if bad {
					return s.fail(pos-1, "non-canonical varint")
				}
				if done {
					if err := s.onPosition(v, pos-1); err != nil {
						return err
					}
				}
				if s.left == 0 {
					if err := s.endPoslist(pos); err != nil {
						return err
					}
				}
			}
		}
	}
	if rowidOff != firstPos {
		return s.fail(0, "first rowid offset")
	}
	if hasLeaf {
		l := ftsLeafInfo{
			Pgno: s.pgno, FirstRowid: firstRowid, HasRowid: firstPos != 0, HasTerm: len(s.offs) > 0,
			PrevTerm: s.prev, TermOffsets: s.offs, RowidOffset: rowidOff, SzLeaf: szLeaf,
			DoclistContinues: len(s.offs) == 0 || s.offs[0] != ftsLeafHeader, RowidsOnPage: s.rowids,
		}
		if len(s.offs) > 0 {
			l.FirstTerm = s.first
		}
		s.sink.Leaf(l)
	}
	return nil
}

func varintReason(k int, truncated string) string {
	if k < 0 {
		return "non-canonical varint"
	}
	return truncated
}

// readTerm parses the term that starts at b[0] (b ends at szLeaf) and makes it the current term. It
// returns the bytes the term uses; the doclist follows.
func (s *ftsSegReader) readTerm(b []byte, first bool, at int) (int, error) {
	p := 0
	var nPrefix uint64
	if !first {
		v, k := ftsVarint(b)
		if k <= 0 {
			return 0, s.fail(at, varintReason(k, "truncated varint"))
		}
		nPrefix, p = v, k
	}
	nSuffix, k := ftsVarint(b[p:])
	if k <= 0 {
		return 0, s.fail(at+p, varintReason(k, "truncated varint"))
	}
	p += k
	if nSuffix > uint64(len(b)-p) {
		return 0, s.fail(at+p, "term overruns the page")
	}
	suffix := b[p : p+int(nSuffix)]
	p += int(nSuffix)
	if nSuffix == 0 {
		return 0, s.fail(at, "empty term")
	}
	if s.needDoc {
		return 0, s.fail(at, "term without a doclist")
	}
	if nPrefix > uint64(len(s.term)) {
		return 0, s.fail(at, "prefix longer than the previous term")
	}
	if nPrefix+nSuffix > fts5MaxTokenSize+1 {
		return 0, s.fail(at, "term too long")
	}
	if nPrefix == 0 && suffix[0] != ftsMainPrefix {
		return 0, s.fail(at, "unknown index prefix")
	}
	if nPrefix == 0 && nSuffix == 1 {
		return 0, s.fail(at, "empty term")
	}
	if s.haveTerm {
		if first {
			if bytes.Compare(s.term, suffix) >= 0 {
				return 0, s.fail(at, "terms not ascending")
			}
		} else {
			i := int(nPrefix)
			switch {
			case i == len(s.term):
			case suffix[0] == s.term[i]:
				return 0, s.fail(at, "prefix not maximal")
			case suffix[0] < s.term[i]:
				return 0, s.fail(at, "terms not ascending")
			}
		}
	}
	s.term = append(s.term[:nPrefix], suffix...)
	s.haveTerm, s.needDoc, s.haveRow = true, true, false
	return p, nil
}

// onSize handles the size varint of an entry.
func (s *ftsSegReader) onSize(v uint64, at int) error {
	if v > math.MaxUint32 {
		return s.fail(at, "poslist size out of range")
	}
	s.del = v&1 == 1
	s.left = v >> 1
	if s.left == 0 {
		if !s.del {
			return s.fail(at, "empty poslist")
		}
		s.state = ftsStRowid
		s.reportDelete()
		return nil
	}
	s.state = ftsStPos
	s.col, s.off, s.have, s.pend = 0, 0, false, 0
	if s.del {
		s.reportDelete()
	}
	return nil
}

func (s *ftsSegReader) reportDelete() {
	if s.sink.Delete != nil {
		s.sink.Delete(s.term[1:], s.rowid)
	}
}

// onPosition handles one varint of a poslist.
func (s *ftsSegReader) onPosition(v uint64, at int) error {
	if v > math.MaxUint32 {
		return s.fail(at, "position out of range")
	}
	switch s.pend {
	case 1:
		if v <= uint64(s.col) || v >= ftsLeafColumns {
			return s.fail(at, "column out of order or range")
		}
		s.col, s.off, s.have, s.pend = int(v), 0, false, 2
		return nil
	case 2:
		if v < 2 {
			return s.fail(at, "invalid first position after a column")
		}
		s.off, s.pend = int64(v-2), 0
	default:
		switch {
		case v == 0:
			return s.fail(at, "invalid position 0")
		case v == 1:
			s.pend = 1
			return nil
		case s.have && v == 2:
			return s.fail(at, "position not ascending")
		}
		s.off += int64(v - 2)
	}
	// one bound for both ways to reach an offset (an offset is a 31-bit position in FTS5)
	if s.off > math.MaxInt32 {
		return s.fail(at, "position out of range")
	}
	s.have = true
	if !s.del && s.sink.Posting != nil {
		s.sink.Posting(s.term[1:], s.rowid, s.col, s.off)
	}
	return nil
}

// endPoslist runs when the last byte of a poslist was taken.
func (s *ftsSegReader) endPoslist(at int) error {
	switch {
	case s.nacc != 0:
		return s.fail(at, "poslist ends inside a varint")
	case s.pend != 0:
		return s.fail(at, "poslist ends inside a column marker")
	}
	s.state = ftsStRowid
	return nil
}

// decodeFTSStructureSegments decodes the structure record into its segments, level by level, in the
// order the record lists them (see decodeFTSStructure for the format and the bounds).
func decodeFTSStructureSegments(b []byte) ([]ftsSegment, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: %d bytes", errFTSStructure, len(b))
	}
	b = b[4:] // the cookie
	next := func() (uint64, error) {
		v, n := SQLiteVarint(b)
		if n == 0 {
			return 0, fmt.Errorf("%w: truncated", errFTSStructure)
		}
		b = b[n:]
		return v, nil
	}
	nLevel, err := next()
	if err != nil {
		return nil, err
	}
	nSeg, err := next()
	if err != nil {
		return nil, err
	}
	if _, err = next(); err != nil { // the write counter
		return nil, err
	}
	const perSeg = 3 // segid, pgnoFirst, pgnoLast
	if nLevel > ftsMaxSegment || nSeg > ftsMaxSegment ||
		nLevel > uint64(len(b)/2) || nSeg > uint64(len(b)/perSeg) {
		return nil, fmt.Errorf("%w: %d levels and %d segments cannot fit in %d bytes", errFTSStructure, nLevel, nSeg, len(b))
	}
	var segs []ftsSegment
	for l := uint64(0); l < nLevel; l++ {
		nMerge, err := next()
		if err != nil {
			return nil, err
		}
		n, err := next()
		if err != nil {
			return nil, err
		}
		if n < nMerge || n > uint64(len(b)/perSeg) {
			return nil, fmt.Errorf("%w: %d segments (%d merging) cannot fit in %d bytes", errFTSStructure, n, nMerge, len(b))
		}
		for s := uint64(0); s < n; s++ {
			id, err := next()
			if err != nil {
				return nil, err
			}
			first, err := next()
			if err != nil {
				return nil, err
			}
			last, err := next()
			if err != nil {
				return nil, err
			}
			// segid is read as a 32-bit varint and a page number has 31 bits in the row id
			if id >= 1<<31 || last < first || last >= 1<<31 {
				return nil, fmt.Errorf("%w: segment id %d, pages %d to %d", errFTSStructure, id, first, last)
			}
			segs = append(segs, ftsSegment{ID: int64(id), Level: int(l), Pos: int(s), First: int64(first), Last: int64(last)})
		}
	}
	if uint64(len(segs)) != nSeg || len(b) != 0 {
		return nil, fmt.Errorf("%w: %d segments declared, %d found, %d bytes left", errFTSStructure, nSeg, len(segs), len(b))
	}
	return segs, nil
}
