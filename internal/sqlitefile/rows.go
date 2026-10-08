package sqlitefile

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// RowStats counts what one Rows pass did.
type RowStats struct {
	RowsByMethod                                    map[string]int64 // read in sorted key order, never by map iteration
	DuplicateOfLive, CellsRejected, InteriorSkipped int64
	Unknown                                         int64 // rows whose relation is RelUnknown
	// CellsShort counts the cells (also counted in CellsRejected) whose record
	// declares more bytes than the cell holds.
	CellsShort            int64
	OverflowPagesFollowed int64
	LimitsHit             []string
}

// The notes a recovered row can carry besides the ones the caller sets.
const (
	// NoteOwnerChanged: the page is a live page of a schema object today, but
	// the image does not fit that object strictly, so the owner is not taken as
	// the table of the row.
	NoteOwnerChanged = "owner-changed"
	// NoteLiveUncertain: the live row could not be looked up cleanly.
	NoteLiveUncertain = "live-lookup-uncertain"
	// NoteIdentityByFitOnly: the table was named by the column fit alone (BasisFit or BasisGuess), so
	// the row is never compared with the live rows of that table (a rowid
	// comparison would present a dropped table's rows as modified rows of a
	// live lookalike). Its relation is unknown.
	NoteIdentityByFitOnly = "identity-by-fit-only"
	// NoteCompareIncomplete: a value was omitted or clipped, so the row can be
	// neither proven equal to nor different from the live row.
	NoteCompareIncomplete = "compare-incomplete"
	// NoteValueUnread: a scalar value of the row (integer, real or NULL) was never
	// read (the tail of a record in a damaged overflow chain), so no live value
	// can be said to equal or differ from it. Its relation is unknown.
	NoteValueUnread = "value-unread"
	// NoteInvalidPageNumber: the page number the WAL frame or journal record
	// states is impossible (0, the lock-byte page, past Limits.MaxPages, or for a
	// journal record past the journal's initial size). The row's relation is
	// unknown: no live comparison is made.
	NoteInvalidPageNumber = "invalid-page-number"
)

// rowOrigins says which origins of Pages yield rows and with which method.
// FreelistTrunk, LiveBTree, Orphan and BeyondEnd never do.
func (h *Hist) rowMethod(img PageImage) string {
	switch img.Origin {
	case OriginDBUnderWAL, OriginWALSuperseded:
		return MethodWALPrior
	case OriginWALUncommitted:
		return MethodWALUncommitted
	case OriginDBRolledBack:
		return MethodJournalRolledBack
	case OriginWALStale, OriginWALUnverified:
		return MethodWALStale
	case OriginJournalBefore:
		if h.jr != nil && h.jr.scan.Info.ZeroedHeader {
			return MethodJournalPersist
		}
		return MethodJournalBefore
	case OriginFreelistLeaf:
		return MethodFreelist
	}
	return ""
}

// rowPass is the state of one Rows call.
type rowPass struct {
	h                  *Hist
	ctx                context.Context
	visit              func(RecoveredRow) bool
	l                  *ledger
	st                 RowStats
	sch                *Schema
	lay                *Layout
	tblItems, idxItems map[string]*fitItem // the fit items of the tables and indexes by name
	idxTable           map[string]string   // the table of each index
	wr                 map[string]*wrSet
	diffTotal          int64

	tables  map[string]*Table
	snaps   map[string]*snapState
	rows    int64
	cells   int64
	stopped bool
	limits  map[string]bool
}

// Rows delivers the rows (and index entries) the history holds, in the order of
// Pages and then the cell index. Every row carries its page origin and its
// byte range (Loc, WAL, Journal) and is never live. Rows identical to the live
// row of their table are not delivered but counted (DuplicateOfLive). It
// returns what Pages returns when the live state cannot be presented.
func (h *Hist) Rows(ctx context.Context, visit func(RecoveredRow) bool) (st RowStats, err error) {
	defer guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	l := h.d.env.newLedger()
	defer func() { l.free(l.n) }()
	defer l.guard(&err)
	rp := &rowPass{h: h, ctx: ctx, visit: visit, l: l, tables: map[string]*Table{}, snaps: map[string]*snapState{}, limits: map[string]bool{}}
	defer rp.releaseSnaps()
	rp.st.RowsByMethod = map[string]int64{}
	if err := h.live.refusal(); err != nil {
		return RowStats{}, err
	}
	if rp.sch, err = h.live.Schema(ctx); err != nil {
		return RowStats{}, err
	}
	if rp.lay, err = h.live.Layout(ctx); err != nil {
		return RowStats{}, err
	}
	rp.tblItems, rp.idxItems, rp.idxTable, rp.wr = map[string]*fitItem{}, map[string]*fitItem{}, map[string]string{}, map[string]*wrSet{}
	fi := rp.sch.fitSets()
	for i := range fi.tables.items {
		rp.tblItems[fi.tables.items[i].name] = &fi.tables.items[i]
	}
	for i := range fi.indexes.items {
		rp.idxItems[fi.indexes.items[i].name] = &fi.indexes.items[i]
	}
	for i := range rp.sch.Objects {
		if o := &rp.sch.Objects[i]; o.Type == "index" {
			rp.idxTable[o.Name] = rp.indexTable(o)
		}
	}
	var perr error
	werr := h.walk(ctx, func(it histItem) bool {
		if perr = rp.image(it); perr != nil {
			return false
		}
		return !rp.stopped
	})
	if perr != nil {
		return RowStats{}, perr
	}
	if werr != nil {
		return RowStats{}, werr
	}
	return rp.st, nil
}

// limitHit records a limit once.
func (rp *rowPass) limitHit(name, msg string) {
	if rp.limits[name] {
		return
	}
	rp.limits[name] = true
	rp.st.LimitsHit = append(rp.st.LimitsHit, name)
	rp.h.warns.add(Warning{Code: WarnLimitReached, Msg: msg})
}

// imgCell is a cell of an image that parsed, with the kinds of its record.
type imgCell struct {
	ptr  CellPointer
	cell Cell
	kind []Value // the record's values with no content: only the kinds are set
}

// image reads the rows of one page image.
func (rp *rowPass) image(it histItem) error {
	h, img := rp.h, it.img
	method := h.rowMethod(img)
	if method == "" || it.data == nil {
		return nil
	}
	info := h.d.info
	data := it.data
	if img.Origin == OriginJournalBefore && len(data) != info.PageSize {
		return nil // a record of another page size is not a page of this database
	}
	hd, err := ParsePageHeader(data, img.Number)
	if err != nil {
		rp.rejectUnreadable(img, data)
		return nil
	}
	switch hd.Type {
	case PageTableLeaf, PageIndexLeaf:
	case PageIndexInterior:
		rp.st.InteriorSkipped += int64(hd.CellCount)
		return nil
	default:
		return nil
	}
	set, err := CellPointers(data, hd, info.UsableSize)
	if err != nil {
		rp.st.CellsRejected += int64(hd.CellCount)
		return nil
	}
	rp.st.CellsRejected += int64(len(set.Bad))
	var cells []imgCell
	var kindsHeld int64
	defer func() { rp.l.free(kindsHeld) }()
	for _, ptr := range set.Good {
		if err := rp.poll(); err != nil {
			return err
		}
		c, err := ParseCell(data, info.UsableSize, hd, ptr.Offset)
		if err != nil {
			rp.st.CellsRejected++
			continue
		}
		c.Index = ptr.Index
		rec, p, held, ok, err := rp.decode(img, c, true)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		p.release()
		kindsHeld += held
		cells = append(cells, imgCell{ptr: ptr, cell: c, kind: rec.Values})
	}
	if len(cells) == 0 {
		return nil
	}
	id := rp.identify(img, hd, cells)
	for i := range cells {
		if err := rp.emitCell(img, method, id, cells[i]); err != nil || rp.stopped {
			return err
		}
	}
	return nil
}

// poll looks at the context every 1024 cells.
func (rp *rowPass) poll() error {
	rp.cells++
	if rp.cells%1024 == 0 {
		return rp.ctx.Err()
	}
	return nil
}

// decode reads the record of cell c of image img. kindsOnly reads only the
// kinds (nothing of the values). ok is false for a cell that cannot yield a
// row (it is counted in CellsRejected here). On success p holds the overflow
// provenance (the caller releases it) and held is the budget the values keep
// charged (the caller frees it).
func (rp *rowPass) decode(img PageImage, c Cell, kindsOnly bool) (rec Record, p *payload, held int64, ok bool, err error) {
	h := rp.h
	e := h.d.env
	info := h.d.info
	lim := e.opts.Limits
	vis := &mapVisitor{seen: map[uint32]struct{}{}, max: int(min(e.overflowCap(info.UsableSize), maxMapVisited))}
	src := &countSrc{in: img.snapshotSource(), rp: rp, seen: map[uint32]struct{}{}}
	p = newPayload(src, rp.l, vis, info.UsableSize, e.overflowCap(info.UsableSize), c)
	p.ctx = rp.ctx
	at := cellCtx{File: img.Loc.File, Page: img.Number, Offset: img.Loc.Offset + int64(c.Offset)}
	var want func(int) bool
	var w *warnings
	if kindsOnly {
		want = func(int) bool { return false }
	} else {
		w = h.warns
	}
	rec, held, err = e.readRecordMode(rp.l, w, at, p, info.Encoding, want, true)
	if err != nil {
		p.release()
		rp.l.free(held) // whatever the failed read still holds
		switch {
		case errors.Is(err, ErrCorrupt):
			rp.st.CellsRejected++
			return Record{}, nil, 0, false, nil
		case errors.Is(err, ErrLimit):
			rp.st.CellsRejected++
			rp.limitHit("MaxPayloadBytes", fmt.Sprintf("a cell of page %d declares a payload over the %d byte cap and is not read", img.Number, lim.MaxPayloadBytes))
			return Record{}, nil, 0, false, nil
		}
		return Record{}, nil, 0, false, err
	}
	if _, _, dead := p.damaged(); rec.Truncated && !dead {
		p.release()
		rp.l.free(held)       // the record is skipped: its charge goes back now, not at the end of the pass
		rp.st.CellsRejected++ // the record declares more bytes than the cell holds
		rp.st.CellsShort++
		return Record{}, nil, 0, false, nil
	}
	return rec, p, held, true, nil
}

// emitCell reads cell c fully, relates it to the live state and delivers it.
func (rp *rowPass) emitCell(img PageImage, method string, id ident, ic imgCell) error {
	h := rp.h
	if err := rp.poll(); err != nil {
		return err
	}
	rec, p, held, ok, err := rp.decode(img, ic.cell, false)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer func() { rp.l.free(held) }()
	steps := p.provenance()
	_, _, dead := p.damaged()
	p.release()
	row := RecoveredRow{
		Method: method, Origin: img.Origin, Table: id.table, Index: id.index, TableBasis: id.basis, Values: rec.Values,
		Loc:       newLoc(img.Loc, img.Number, ic.ptr.Index, ic.cell, steps, h.d.env.opts.Limits.MaxLocOverflow),
		Truncated: rec.Truncated, Notes: append([]string(nil), id.notes...),
	}
	if ic.cell.HasRowid {
		rid := ic.cell.Rowid
		row.Rowid = &rid
	}
	if dead && rec.Truncated {
		row.OverflowHead = ic.cell.OverflowHead
	}
	if img.WAL != nil {
		w := *img.WAL
		row.WAL = &w
	}
	if img.Journal != nil {
		j := *img.Journal
		row.Journal = &j
	}
	uncommitted := img.Origin == OriginWALUncommitted || img.Origin == OriginDBRolledBack
	invalid := strings.HasPrefix(img.Note, NoteInvalidPageNumber+": ")
	if invalid {
		row.Notes = append(row.Notes, NoteInvalidPageNumber) // the page number is not believed: nothing is compared
	}
	rel := RelUnknown
	if id.basis == BasisFit || id.basis == BasisGuess {
		// identity by fit alone: the relation is unknown whatever the origin
		// (ruling C47); that the bytes are uncommitted is the Origin's fact
		row.Notes = append(row.Notes, NoteIdentityByFitOnly)
	} else if uncommitted {
		rel = RelUncommitted
	}
	if id.kind != kindNone && id.basis == BasisSchema && !invalid {
		res, err := rp.compare(id, &row)
		if err != nil {
			return err
		}
		switch res.kind {
		case cmpSame:
			rp.st.DuplicateOfLive++
			return nil
		case cmpDiffer:
			if !uncommitted {
				rel = RelSupersededVersion
			}
		case cmpAbsent:
			if !uncommitted {
				rel = RelAbsentFromLive
			}
		default:
			row.Notes = append(row.Notes, res.note)
		}
	}
	row.Relation = rel
	if rp.rows >= h.d.env.opts.Limits.MaxHistoryRows {
		rp.limitHit("MaxHistoryRows", fmt.Sprintf("the history holds more than %d rows (Limits.MaxHistoryRows); the rest are not listed", h.d.env.opts.Limits.MaxHistoryRows))
		rp.stopped = true
		return nil
	}
	rp.rows++
	rp.st.RowsByMethod[method]++
	if rel == RelUnknown {
		rp.st.Unknown++
	}
	if !rp.visit(row) {
		rp.stopped = true
	}
	return nil
}

// rejectUnreadable counts the cells of a leaf whose header is valid but whose
// pointer array does not fit the page: none of them can be read.
func (rp *rowPass) rejectUnreadable(img PageImage, data []byte) {
	base := 0
	if img.Number == 1 {
		base = 100
	}
	if len(data) < base+8 {
		return
	}
	if t := PageType(data[base]); t == PageTableLeaf || t == PageIndexLeaf {
		rp.st.CellsRejected += int64(binary.BigEndian.Uint16(data[base+3:]))
	}
}

// countSrc is the page source of one cell's overflow chain: it counts the pages
// followed across the whole pass and refuses past Limits.MaxHistoryOverflowPages.
type countSrc struct {
	in   pageSource
	rp   *rowPass
	seen map[uint32]struct{} // pages of this cell already counted
}

func (c *countSrc) has(pgno uint32) bool { return c.in.has(pgno) }

func (c *countSrc) read(pgno uint32) ([]byte, PageLoc, error) {
	_, counted := c.seen[pgno]
	if !counted {
		lim := c.rp.h.d.env.opts.Limits.MaxHistoryOverflowPages
		if c.rp.st.OverflowPagesFollowed >= lim {
			c.rp.limitHit("MaxHistoryOverflowPages", fmt.Sprintf("the history followed %d overflow pages (Limits.MaxHistoryOverflowPages); longer chains are cut", lim))
			return nil, PageLoc{}, fmt.Errorf("%w: page %d: the overflow page cap of the history is reached", ErrPageUnavailable, pgno)
		}
	}
	data, loc, err := c.in.read(pgno)
	if err == nil && !counted {
		c.seen[pgno] = struct{}{}
		c.rp.st.OverflowPagesFollowed++
	}
	return data, loc, err
}
