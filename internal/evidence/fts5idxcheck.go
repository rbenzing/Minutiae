package evidence

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ftsQuerier is what the index check reads through: a ReadHandle of the case or a *sql.DB of a test.
type ftsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type ftsIdxKind int

const (
	ftsIdxMissing   ftsIdxKind = iota // the leaf pages produce a row the index does not hold
	ftsIdxWrong                       // the index holds a row for a page, different from what the leaf pages produce
	ftsIdxNoLeaf                      // the index holds a row for a page that does not exist
	ftsIdxBadClass                    // a row with the wrong storage classes
	ftsDlidxDiffers                   // a doclist index row missing or different
	ftsDlidxExtra                     // a doclist index row no doclist needs
	ftsLeafBad                        // a leaf page that is not valid
)

// ftsLostHit is a page's first term and its first rowid: a search for them must find the record.
type ftsLostHit struct {
	Term  []byte
	Rowid int64
}

// ftsIdxFinding is one difference between the stored rows and the recomputed ones.
type ftsIdxFinding struct {
	Kind  ftsIdxKind
	Seg   int64
	Page  int64
	Level int
	Term  []byte // the term of the row (with the index prefix byte)
	// Want is what the leaf pages produce, for ftsIdxWrong
	WantTerm []byte
	WantPage int64
	WantFlag bool
	Classes  string
	Err      error
	Confirm  *ftsLostHit // set when a search for the page's first term can confirm the difference
}

// ftsShowTerm renders a term of the index for a message: the prefix byte is not part of what a search sees.
func ftsShowTerm(t []byte) string {
	if len(t) > 0 && t[0] == ftsMainPrefix {
		t = t[1:]
	}
	return clipText(string(t), 80)
}

func (f ftsIdxFinding) text(table string) string {
	switch f.Kind {
	case ftsIdxMissing:
		return fmt.Sprintf("%s_idx: segment %d: the row for page %d (term %q) is missing (recomputed from the leaf pages)", table, f.Seg, f.Page, ftsShowTerm(f.Term))
	case ftsIdxWrong:
		if f.WantPage == 0 { // no row is expected for this key
			return fmt.Sprintf("%s_idx: segment %d: the row (term %q, page %d) is not what the leaf pages produce", table, f.Seg, ftsShowTerm(f.Term), f.Page)
		}
		return fmt.Sprintf("%s_idx: segment %d: the row (term %q, page %d) is not what the leaf pages produce (they produce term %q, page %d, doclist index %t)",
			table, f.Seg, ftsShowTerm(f.Term), f.Page, ftsShowTerm(f.WantTerm), f.WantPage, f.WantFlag)
	case ftsIdxNoLeaf:
		return fmt.Sprintf("%s_idx: segment %d: row (term %q, page %d) has no leaf page behind it", table, f.Seg, ftsShowTerm(f.Term), f.Page)
	case ftsIdxBadClass:
		return fmt.Sprintf("%s_idx: segment %d: a row has the storage classes %s instead of (blob, integer)", table, f.Seg, f.Classes)
	case ftsDlidxDiffers:
		return fmt.Sprintf("%s: segment %d: the doclist index of term %q differs from the leaf pages", table, f.Seg, ftsShowTerm(f.Term))
	case ftsDlidxExtra:
		return fmt.Sprintf("%s: segment %d: a doclist index row (level %d, page %d) belongs to no doclist the leaf pages show", table, f.Seg, f.Level, f.Page)
	}
	var le *ftsLeafError
	if errors.As(f.Err, &le) {
		return fmt.Sprintf("%s: segment %d page %d is not a valid leaf: %s", table, le.Seg, le.Page, le.Reason)
	}
	return fmt.Sprintf("%s: segment %d is not a valid leaf sequence: %v", table, f.Seg, f.Err)
}

// ftsIdxStats says what a check looked at.
type ftsIdxStats struct {
	Segments, Empty, Trimmed int
	Pages                    int64
	IdxRows                  int64 // stored rows that equal a recomputed row
	StaleIdxRows             int64 // rows of a trimmed segment below its first page
	DlidxRows                int   // stored doclist-index rows that equal a recomputed row
	StaleDlidxRows           int
	FullKeys                 int // rows whose key is the whole first term (V12)
}

// errFTSIdxStructure says the structure record cannot be read: checkUnreferenced reports that, and there
// is no segment to recompute from.
var errFTSIdxStructure = errors.New("the index structure cannot be read")

const ftsIdxProgressPages = 1024

// ftsIdxPgsz reads the page size of an index: the pgsz row of its _config, or the default.
func ftsIdxPgsz(ctx context.Context, q ftsQuerier, table string) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT typeof(v), v FROM `+table+`_config WHERE k = 'pgsz'`) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	pgsz := ftsDefaultPgsz
	if rows.Next() {
		var typ string
		var v any
		if err := rows.Scan(&typ, &v); err != nil {
			return 0, err
		}
		// FTS5 accepts 32 <= pgsz <= 65536; anything else is a config difference compareConfig reports
		if n, ok := v.(int64); ok && typ == "integer" && n >= 32 && n <= 65536 {
			pgsz = int(n)
		}
	}
	return pgsz, rows.Err()
}

// ftsIdxRun is one check of one table.
type ftsIdxRun struct {
	ctx      context.Context
	q        ftsQuerier
	table    string
	report   func(ftsIdxFinding)
	progress func(pages int)
	rowSeen  func(ftsIdxRow)
	pgsz     int
	st       ftsIdxStats
	pending  int // pages since the last progress call
}

// checkFTSIndex recomputes the _idx rows and the doclist-index rows of every segment of table from its
// leaf pages and compares them with the stored rows. A difference goes to report. The error is for
// what stopped the check (a read error, the end of the context); errFTSIdxStructure for an unreadable
// structure record.
func checkFTSIndex(ctx context.Context, q ftsQuerier, table string, report func(ftsIdxFinding), progress func(pages int)) (ftsIdxStats, error) {
	return checkFTSIndexRows(ctx, q, table, report, progress, nil)
}

// checkFTSIndexRows is checkFTSIndex that also tells rowSeen about every recomputed _idx row.
func checkFTSIndexRows(ctx context.Context, q ftsQuerier, table string, report func(ftsIdxFinding), progress func(pages int), rowSeen func(ftsIdxRow)) (ftsIdxStats, error) {
	r := &ftsIdxRun{ctx: ctx, q: q, table: table, report: report, progress: progress, rowSeen: rowSeen}
	segs, err := r.structure()
	if err != nil {
		return r.st, err
	}
	if r.pgsz, err = ftsIdxPgsz(ctx, q, table); err != nil {
		return r.st, err
	}
	slices.SortFunc(segs, func(a, b ftsSegment) int { return int(a.ID - b.ID) })
	for _, seg := range segs {
		if err := ctx.Err(); err != nil {
			return r.st, err
		}
		if err := r.segment(seg); err != nil {
			return r.st, err
		}
	}
	r.flushProgress()
	return r.st, nil
}

func (r *ftsIdxRun) flushProgress() {
	if r.progress != nil && r.pending > 0 {
		r.progress(r.pending)
	}
	r.pending = 0
}

func (r *ftsIdxRun) structure() ([]ftsSegment, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT block FROM `+r.table+`_data WHERE id = ?`, ftsStructureRowID) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var block []byte
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, errFTSIdxStructure
	}
	if err := rows.Scan(&block); err != nil {
		return nil, err
	}
	segs, err := decodeFTSStructureSegments(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFTSIdxStructure, err)
	}
	return segs, nil
}

// ftsStoredIdx is a row of _idx as read.
type ftsStoredIdx struct {
	term []byte
	page int64
	flag bool
}

// idxCursor reads the rows of one segment of <table>_idx in term order, one ahead.
type idxCursor struct {
	r    *ftsIdxRun
	seg  ftsSegment
	rows *sql.Rows
	cur  ftsStoredIdx
	has  bool
}

func (c *idxCursor) advance() error {
	c.has = false
	for c.rows.Next() {
		var tt, tp string
		var term, pgno any
		if err := c.rows.Scan(&tt, &term, &tp, &pgno); err != nil {
			return err
		}
		t, ok1 := term.([]byte)
		p, ok2 := pgno.(int64)
		if tt != "blob" || tp != "integer" || !ok1 || !ok2 {
			c.r.report(ftsIdxFinding{Kind: ftsIdxBadClass, Seg: c.seg.ID, Classes: "(" + tt + ", " + tp + ")"})
			continue
		}
		row := ftsStoredIdx{term: t, page: p >> 1, flag: p&1 == 1}
		// a row below the first page of a trimmed segment is left over and never used
		if c.seg.First > 1 && row.page < c.seg.First {
			c.r.st.StaleIdxRows++
			continue
		}
		c.cur, c.has = row, true
		return nil
	}
	return c.rows.Err()
}

// compare matches one expected row with the stored ones, in term order.
func (c *idxCursor) compare(e ftsIdxRow) error {
	page := e.Pgno >> 1
	flag := e.Pgno&1 == 1
	hit := &ftsLostHit{Term: e.FirstTerm, Rowid: e.FirstRowid}
	if !e.HasRowid {
		hit = nil
	}
	for {
		if !c.has {
			c.r.report(ftsIdxFinding{Kind: ftsIdxMissing, Seg: e.Segid, Page: page, Term: e.FirstTerm, Confirm: hit})
			return nil
		}
		s := c.cur
		match := e.AnyKey && s.page == page || !e.AnyKey && (bytes.Equal(s.term, e.Term) || e.Alt != nil && bytes.Equal(s.term, e.Alt))
		switch {
		case match:
			if s.page != page || s.flag != flag {
				c.r.report(ftsIdxFinding{Kind: ftsIdxWrong, Seg: e.Segid, Page: s.page, Term: s.term, WantTerm: e.Term, WantPage: page, WantFlag: flag, Confirm: hit})
			} else {
				c.r.st.IdxRows++
				if e.Alt != nil && bytes.Equal(s.term, e.Alt) {
					c.r.st.FullKeys++
				}
			}
			return c.advance()
		case e.AnyKey && s.page > page, !e.AnyKey && bytes.Compare(s.term, e.Term) > 0:
			c.r.report(ftsIdxFinding{Kind: ftsIdxMissing, Seg: e.Segid, Page: page, Term: e.FirstTerm, Confirm: hit})
			return nil
		}
		// a stored row no expected row matches
		c.extra()
		if err := c.advance(); err != nil {
			return err
		}
	}
}

// extra reports the current stored row, which no recomputed row matches.
func (c *idxCursor) extra() {
	s := c.cur
	if s.page < c.seg.First || s.page > c.seg.Last {
		c.r.report(ftsIdxFinding{Kind: ftsIdxNoLeaf, Seg: c.seg.ID, Page: s.page, Term: s.term})
		return
	}
	c.r.report(ftsIdxFinding{Kind: ftsIdxWrong, Seg: c.seg.ID, Page: s.page, Term: s.term, WantPage: s.page})
}

// ftsIdxSeg is the check of one segment: the _idx cursor, the builder fed by the decoder's leaf callback,
// and the doclist-index rows. The index pass of verify (readLeaves) and checkFTSIndex both drive it.
type ftsIdxSeg struct {
	r      *ftsIdxRun
	seg    ftsSegment
	rd     *ftsSegReader
	b      *ftsIdxBuilder
	rows   *sql.Rows
	cur    *idxCursor
	cmpErr error
	ended  bool // the leaf pages are over
}

// beginSegment opens the check of a segment that is not Empty. rd is the reader that decodes its pages.
func (r *ftsIdxRun) beginSegment(seg ftsSegment, rd *ftsSegReader) (*ftsIdxSeg, error) {
	r.st.Segments++
	if seg.First > 1 {
		r.st.Trimmed++
	}
	// the row id of a page is segid<<37+page: a segment id that does not fit has no rows
	if seg.ID >= 1<<(63-ftsSegidShift)-1 {
		return nil, fmt.Errorf("segment id %d does not fit a row id", seg.ID)
	}
	ir, err := r.q.QueryContext(r.ctx, `SELECT typeof(term), term, typeof(pgno), pgno FROM `+r.table+`_idx WHERE segid = ? ORDER BY term`, seg.ID) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return nil, err
	}
	s := &ftsIdxSeg{r: r, seg: seg, rd: rd, rows: ir}
	s.cur = &idxCursor{r: r, seg: seg, rows: ir}
	if err := s.cur.advance(); err != nil {
		_ = ir.Close()
		return nil, err
	}
	s.b = newFTSIdxBuilder(seg, r.pgsz, func(e ftsIdxRow) {
		if r.rowSeen != nil {
			r.rowSeen(e)
		}
		if s.cmpErr == nil {
			s.cmpErr = s.cur.compare(e)
		}
	})
	return s, nil
}

// leaf is the callback for ftsSink.Leaf.
func (s *ftsIdxSeg) leaf(l ftsLeafInfo) {
	if s.cmpErr == nil {
		s.b.leaf(l, s.rd.term)
	}
}

// endLeaves says the leaf pages decoded completely (Finish succeeded): the last rows are compared.
func (s *ftsIdxSeg) endLeaves() error {
	if s.ended {
		return s.cmpErr
	}
	s.ended = true
	if s.cmpErr == nil {
		s.b.finish()
	}
	for s.cmpErr == nil && s.cur.has {
		s.cur.extra()
		s.cmpErr = s.cur.advance()
	}
	return s.cmpErr
}

// dlidx compares one stored doclist-index row; it needs endLeaves first.
func (s *ftsIdxSeg) dlidx(id int64, block []byte) {
	r := s.r
	level := int(id >> 31 & 31)
	pgno := id & (1<<31 - 1)
	want, ok := s.b.dl[id]
	if !ok {
		if s.seg.First > 1 && pgno < s.seg.First {
			r.st.StaleDlidxRows++
			return
		}
		r.report(ftsIdxFinding{Kind: ftsDlidxExtra, Seg: s.seg.ID, Page: pgno, Level: level})
		return
	}
	delete(s.b.dl, id)
	if !bytes.Equal(want.Block, block) {
		r.report(ftsIdxFinding{Kind: ftsDlidxDiffers, Seg: s.seg.ID, Page: pgno, Level: level, Term: want.Term, Confirm: &ftsLostHit{Term: want.Term, Rowid: want.FirstRowid}})
		return
	}
	r.st.DlidxRows++
}

// endDlidx reports the doclist-index rows the leaf pages produce and the index does not hold.
func (s *ftsIdxSeg) endDlidx() {
	missing := make([]*ftsDlidxRow, 0, len(s.b.dl))
	for _, row := range s.b.dl {
		missing = append(missing, row)
	}
	slices.SortFunc(missing, func(a, c *ftsDlidxRow) int { return int(a.ID - c.ID) })
	for _, row := range missing {
		s.r.report(ftsIdxFinding{Kind: ftsDlidxDiffers, Seg: s.seg.ID, Page: row.Pgno, Level: row.Level, Term: row.Term, Confirm: &ftsLostHit{Term: row.Term, Rowid: row.FirstRowid}})
	}
}

func (s *ftsIdxSeg) close() { _ = s.rows.Close() }

// segment checks one segment on its own: it reads the rows of the segment from _data itself.
func (r *ftsIdxRun) segment(seg ftsSegment) error {
	if seg.Empty() { // drained by a merge (D143)
		r.st.Segments++
		r.st.Empty++
		return nil
	}
	rd := newFTSSegReader(seg)
	s, err := r.beginSegment(seg, rd)
	if err != nil {
		return err
	}
	defer s.close()
	base := seg.ID << ftsSegidShift
	dr, err := r.q.QueryContext(r.ctx, `SELECT id, block FROM `+r.table+`_data WHERE id >= ? AND id < ? ORDER BY id`, base, base+1<<ftsSegidShift) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return err
	}
	defer func() { _ = dr.Close() }()
	sink := ftsSink{Leaf: s.leaf}
	bad := false
	endLeaves := func() error {
		if s.ended || bad {
			return nil
		}
		if err := rd.Finish(); err != nil {
			bad = true
			r.report(ftsIdxFinding{Kind: ftsLeafBad, Seg: seg.ID, Err: err})
			return nil
		}
		return s.endLeaves()
	}
	for dr.Next() {
		var id int64
		var block sql.RawBytes
		if err := dr.Scan(&id, &block); err != nil {
			return err
		}
		local := id - base
		if local < ftsLeafLimit { // a leaf page
			if bad {
				continue
			}
			if err := rd.Page(local, block, sink); err != nil {
				bad = true
				r.report(ftsIdxFinding{Kind: ftsLeafBad, Seg: seg.ID, Err: err})
				continue
			}
			if s.cmpErr != nil {
				return s.cmpErr
			}
			r.st.Pages++
			if r.pending++; r.pending >= ftsIdxProgressPages {
				r.flushProgress()
				if err := r.ctx.Err(); err != nil {
					return err
				}
			}
			continue
		}
		if err := endLeaves(); err != nil {
			return err
		}
		if bad || local&ftsDlidxFlag == 0 {
			continue // an interior page of the old format cannot occur in an index SQLite 3.53 writes
		}
		s.dlidx(id, block)
	}
	if err := dr.Err(); err != nil {
		return err
	}
	if err := endLeaves(); err != nil {
		return err
	}
	if !bad {
		s.endDlidx()
	}
	return nil
}

// ftsLostHitTerm is the term of a hit as a search is asked for it: without the index prefix byte.
func ftsLostHitTerm(t []byte) string {
	return strings.TrimPrefix(string(t), string([]byte{ftsMainPrefix}))
}
