package sqlitefile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Origin says where a page image of the history comes from. The values are the
// order Pages yields them in.
type Origin uint8

// The origins of a page image.
const (
	OriginLiveBTree     Origin = iota + 1 // a live page, listed only for its in-page free space
	OriginFreelistLeaf                    // a freelist leaf of the live state
	OriginFreelistTrunk                   // spans only: the bytes after the leaf list
	OriginOrphan                          // a page no structure of the live state refers to
	OriginBeyondEnd                       // file bytes past the live page count
	OriginDBUnderWAL                      // the database image of a page a committed frame overlays
	OriginDBRolledBack                    // the database-file page a rollback hides: bytes an uncommitted transaction left behind
	OriginWALSuperseded                   // a committed frame older than the page's latest committed frame
	OriginWALUncommitted
	OriginWALStale
	OriginWALUnverified // broken, detached or unanchored frames
	OriginJournalBefore // a journal record's before-image (Applied or not)
)

const originCount = int(OriginJournalBefore)

func (o Origin) String() string {
	names := [...]string{
		"unknown", "live-btree", "freelist-leaf", "freelist-trunk", "orphan", "beyond-end", "db-under-wal",
		"db-rolled-back", "wal-superseded", "wal-uncommitted", "wal-stale", "wal-unverified", "journal-before",
	}
	if int(o) < len(names) {
		return names[o]
	}
	return fmt.Sprintf("origin(%d)", uint8(o))
}

// SpanKind says what a Span is.
type SpanKind uint8

// The kinds of span.
const (
	SpanGap       SpanKind = iota + 1 // between the cell pointer array and the cell content area
	SpanFreeblock                     // a freeblock of the page's chain
	SpanTrunkTail                     // the bytes of a freelist trunk after its leaf list
)

// Span is a free area of a page image: Offset and Length are within the page,
// FileOffset is where it lies in the file the image comes from (Loc.File).
type Span struct {
	Kind       SpanKind
	Offset     int
	Length     int
	FileOffset int64
}

// WALProv is the provenance of a page image that comes from a WAL frame.
type WALProv struct {
	Frame             uint32 // 1-based slot
	Salt1, Salt2      uint32
	State             FrameState
	Committed, Linked bool
	Generation        int    // index into WALInfo.Generations
	Note              string // e.g. "page-beyond-commit-size"
}

// JournalNoteBeyondInitialSize is the Note of a database page past the journal's
// initial page count: the rollback truncates it, so no journal record holds it.
const JournalNoteBeyondInitialSize = "page-beyond-initial-size"

// JournalProv is the provenance of a page image that comes from a rollback
// journal record, or (OriginDBRolledBack) of the journal that hides the page.
// Note is set (and Record is meaningless, 0) for a database page the journal's
// truncation hides: no record holds an image of it.
type JournalProv struct {
	Record, Segment int
	Note            string
	ChecksumOK      bool
	Hot, Applied    bool
	Nonce           uint32
}

// PageImage is one version of a page: where it lies (Loc), which source it
// comes from (Origin, WAL, Journal) and the free spans found in it. A page image
// that is not live is recovered data: it is never served by Live().
// A live, freelist or orphan image whose bytes the live view serves from a WAL
// frame or a journal record has Loc.File set to that file and Loc.Frame or
// Loc.Record as its only pointer: WAL and Journal describe only the images of
// recovered origins.
type PageImage struct {
	Number  uint32
	Origin  Origin
	Loc     PageLoc
	WAL     *WALProv
	Journal *JournalProv
	Spans   []Span
	// Partial is set for a trailing partial page of the database file: Bytes is
	// shorter than a page and Note says by how much. Nothing is padded.
	Partial bool
	Note    string

	h *Hist
}

// PageParse is a page image read as a b-tree page.
type PageParse struct {
	Header          PageHeader
	Cells           []Cell
	Spans           []Span
	FragmentedBytes int
	Rejected        int
}

// genPage keys the frames of one WAL generation by page.
type genPage struct {
	gen  int
	page uint32
}

// Hist is the history of a database: the page images the live state does not
// show, with their provenance. It is built from the companion files attached
// when History was called and never changes what Live shows. Release gives its
// memory back (optional).
type Hist struct {
	d     *DB
	a     *attachedWAL
	jr    *attachedJournal
	live  *View
	warns *warnings

	mu       sync.Mutex
	idx      map[genPage][]uint32 // the slots of each generation's frames of a page, ascending
	jready   bool
	jwin     map[uint32]int // the rollback state: page -> record (last playable wins)
	jinitial uint32
	jlimited bool
	charge   int64
}

// HistSummary holds page-level facts of the history; row counts are RowStats.
type HistSummary struct {
	PagesByOrigin              map[Origin]int64 // read in Origin order, never by map iteration
	FreePages, FreePagesZeroed int64            // free pages; all-zero freelist leaves: secure_delete evidence
	LiveSpanBytes              int64            // the span bytes of the live b-tree pages
}

// History returns the history of the database as it is now: Attach* calls made
// after it do not reach it.
func (d *DB) History() *Hist {
	a, jr, ws := d.companions()
	return &Hist{d: d, a: a, jr: jr, live: d.viewOf(true, a, jr, ws), warns: newWarnings(d.env.opts.Limits.MaxWarnings)}
}

// Warnings returns the anomalies found so far: those of the live state the
// history is built on, then its own (broken freeblock chains, pages a snapshot
// cannot supply, a cut sequence).
func (h *Hist) Warnings() []Warning {
	return append(h.live.Warnings(), h.warns.snapshot()...)
}

// Release gives the budget the history holds back. The history stays usable.
func (h *Hist) Release() {
	h.live.Release()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.charge > 0 {
		h.d.env.budget.Free(h.charge)
		h.charge = 0
	}
	h.idx, h.jwin, h.jready = nil, nil, false
}

// refusal is the error of a live view that has no state to present (a WAL of an
// unsupported version, a hot journal that is not applied), or nil.
func (v *View) refusal() error {
	if r, ok := v.src.(refusedSource); ok {
		return r.err
	}
	return nil
}

// Pages yields every page image of the history, in a deterministic order: by
// origin (the order of the Origin constants), then page number, then WAL
// generation Age, then slot, or journal record. The sequence ends after
// Limits.MaxHistoryPages images (a limit-reached warning), or when visit returns
// false. When the live state cannot be presented (see ErrLiveUnavailable and
// ErrEngineRefuses) Pages returns that error: it lists nothing.
func (h *Hist) Pages(ctx context.Context, visit func(PageImage) bool) (err error) {
	defer guard(&err)
	return h.walk(ctx, func(it histItem) bool { return visit(it.img) })
}

// Summary counts the page images by origin and the free pages. FreePages and
// FreePagesZeroed count the freelist pages the live view can read (an
// unreadable one is skipped, with the view's own warning); LiveSpanBytes is the
// sum of the span lengths and excludes the fragmented bytes (PageParse reports
// those).
func (h *Hist) Summary(ctx context.Context) (s HistSummary, err error) {
	defer guard(&err)
	s.PagesByOrigin = map[Origin]int64{}
	var counts [originCount + 1]int64
	err = h.walk(ctx, func(it histItem) bool {
		counts[it.img.Origin]++
		switch it.img.Origin {
		case OriginLiveBTree:
			for _, sp := range it.img.Spans {
				s.LiveSpanBytes += int64(sp.Length)
			}
		case OriginFreelistTrunk:
			s.FreePages++
		case OriginFreelistLeaf:
			s.FreePages++
			if it.data != nil && !slices.ContainsFunc(it.data, func(b byte) bool { return b != 0 }) {
				s.FreePagesZeroed++
			}
		}
		return true
	})
	if err != nil {
		return HistSummary{}, err
	}
	for o := 1; o <= originCount; o++ { // in Origin order
		if counts[o] > 0 {
			s.PagesByOrigin[Origin(o)] = counts[o]
		}
	}
	return s, nil
}

// histItem is an image with the bytes it was read from (nil when unreadable).
type histItem struct {
	img  PageImage
	data []byte
}

var errHistStop = errors.New("history walk stopped")

// histWalk is one pass over the history.
type histWalk struct {
	h     *Hist
	ctx   context.Context
	visit func(histItem) bool
	limit int64
	n     int64
	lay   *Layout
}

func (h *Hist) walk(ctx context.Context, visit func(histItem) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := h.live.refusal(); err != nil {
		return err
	}
	w := &histWalk{h: h, ctx: ctx, visit: visit, limit: h.d.env.opts.Limits.MaxHistoryPages}
	lay, err := h.live.Layout(ctx)
	if err != nil {
		return err
	}
	w.lay = lay
	w.warnUnattributed()
	steps := []func() error{
		func() error { return w.classPass(OriginLiveBTree, ClassBTreeInterior, ClassBTreeLeaf) },
		func() error { return w.classPass(OriginFreelistLeaf, ClassFreelistLeaf) },
		func() error { return w.classPass(OriginFreelistTrunk, ClassFreelistTrunk) },
		func() error { return w.classPass(OriginOrphan, ClassOrphan) },
		w.beyondEnd, w.dbUnderWAL, w.dbRolledBack, w.walFrames, w.journalRecords,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			if errors.Is(err, errHistStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

// emit hands one image to the visitor; errHistStop ends the walk.
func (w *histWalk) emit(o Origin, pg uint32, loc PageLoc, wal *WALProv, jr *JournalProv, data []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.n >= w.limit {
		w.h.warns.add(Warning{Code: WarnLimitReached, Msg: fmt.Sprintf("the history holds more than %d page images (Limits.MaxHistoryPages); the rest are not listed", w.limit)})
		return errHistStop
	}
	w.n++
	img := PageImage{Number: pg, Origin: o, Loc: loc, WAL: wal, Journal: jr, h: w.h}
	if ps := w.h.d.info.PageSize; data != nil && loc.File == FileDB && len(data) < ps {
		img.Partial, img.Note = true, fmt.Sprintf("partial-page: %d of %d bytes", len(data), ps)
	}
	if data != nil {
		img.Spans = w.h.imageSpans(img, data)
	}
	if !w.visit(histItem{img: img, data: data}) {
		return errHistStop
	}
	return nil
}

// imageSpans lists the spans of an image: the trunk tail of a freelist trunk,
// none for a freelist leaf (its bytes are stale content), and the gap and
// freeblocks of anything else that reads as a b-tree page.
func (h *Hist) imageSpans(img PageImage, data []byte) []Span {
	usable := h.d.info.UsableSize
	switch img.Origin {
	case OriginFreelistTrunk:
		return trunkTail(data, usable, img.Loc.Offset)
	case OriginFreelistLeaf:
		return nil
	}
	hd, err := ParsePageHeader(data, img.Number)
	if err != nil {
		return nil
	}
	sr := freeSpans(data, hd, usable, img.Loc.Offset)
	if sr.rejected > 0 {
		h.warnChain(img, sr.problem)
	}
	return sr.spans
}

// classPass yields the pages of the live layout of the given classes, in page
// order, as the live view serves them.
func (w *histWalk) classPass(o Origin, classes ...PageClass) error {
	for pg := uint32(1); pg <= w.lay.Addressable; pg++ {
		if !slices.Contains(classes, w.lay.Class[pg]) {
			continue
		}
		data, loc, err := w.h.live.cache.read(pg)
		if err != nil {
			if isUnavailable(err) {
				continue // the view's own warning says so
			}
			return err
		}
		if err := w.emit(o, pg, loc, nil, nil, data); err != nil {
			return err
		}
	}
	return nil
}

// rawPage reads page pg of the database file as found: nil when the file does
// not hold it.
func (h *Hist) rawPage(pg uint32) ([]byte, error) {
	data, err := h.d.readRawPage(pg)
	if err != nil {
		if isUnavailable(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// hasRaw reports whether the database file holds page pg.
func (h *Hist) hasRaw(pg uint32) bool { return dbSource{h.d}.has(pg) }

// rolled reports whether the hot journal is applied to the live state.
func (h *Hist) rolled() bool { return h.jr != nil && h.jr.scan.Info.Applied }

// beyondEnd yields the database-file pages past the live page count that no
// rollback truncation accounts for.
func (w *histWalk) beyondEnd() error {
	h := w.h
	hi := uint64(h.d.info.FilePages)
	if h.rolled() {
		hi = min(hi, uint64(h.jr.scan.Info.InitialPages))
	}
	hi = min(hi, uint64(h.d.env.opts.Limits.MaxPages))
	for pg := uint64(h.live.info.PageCount) + 1; pg <= hi; pg++ {
		data, err := h.rawPage(uint32(pg))
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		if err := w.emit(OriginBeyondEnd, uint32(pg), PageLoc{File: FileDB, Offset: PageOffset(h.d.info.PageSize, uint32(pg))}, nil, nil, data); err != nil {
			return err
		}
	}
	return nil
}

// jprov is the provenance of journal record i.
func (h *Hist) jprov(i int, applied bool) *JournalProv {
	r, in := h.jr.scan.Records[i], &h.jr.scan.Info
	return &JournalProv{Record: r.Index, Segment: r.Segment, ChecksumOK: r.ChecksumOK, Hot: in.Hot, Applied: applied, Nonce: in.Nonce}
}

// dbUnderWAL yields the database-file image of every page a committed frame
// overlays within the commit's size (a page the journal rollback hides is listed
// as rolled back instead).
func (w *histWalk) dbUnderWAL() error {
	h := w.h
	if h.a == nil || !h.a.scan.Info.UsedByLive || h.a.scan.Info.LastCommit == 0 {
		return nil
	}
	l := h.d.env.newLedger()
	defer l.free(l.n)
	if err := l.alloc(4 * int64(len(h.a.scan.latest))); err != nil {
		return fmt.Errorf("history of the database under the WAL: %w", err)
	}
	s := h.a.scan
	pages := make([]uint32, 0, len(s.latest))
	for pg := range s.latest {
		pages = append(pages, pg)
	}
	slices.Sort(pages)
	for _, pg := range pages {
		if pg > s.Info.DBPagesAfterCommit || !h.hasRaw(pg) {
			continue
		}
		if h.rolled() {
			if _, hidden := h.jr.scan.winner[pg]; hidden {
				continue
			}
		}
		data, err := h.rawPage(pg)
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		if err := w.emit(OriginDBUnderWAL, pg, PageLoc{File: FileDB, Offset: PageOffset(h.d.info.PageSize, pg)}, nil, nil, data); err != nil {
			return err
		}
	}
	return nil
}

// dbRolledBack yields the database-file page of every page the applied journal
// overrides, and the pages above the journal's initial size that the rollback
// truncates away: the bytes an uncommitted transaction left behind.
func (w *histWalk) dbRolledBack() error {
	h := w.h
	if !h.rolled() {
		return nil
	}
	l := h.d.env.newLedger()
	defer l.free(l.n)
	if err := l.alloc(4 * int64(len(h.jr.scan.winner))); err != nil {
		return fmt.Errorf("history of the rolled-back pages: %w", err)
	}
	s := h.jr.scan
	pages := make([]uint32, 0, len(s.winner))
	for pg := range s.winner {
		pages = append(pages, pg)
	}
	slices.Sort(pages)
	for _, pg := range pages {
		data, err := h.rawPage(pg)
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		if err := w.emit(OriginDBRolledBack, pg, PageLoc{File: FileDB, Offset: PageOffset(h.d.info.PageSize, pg)}, nil, h.jprov(s.winner[pg], true), data); err != nil {
			return err
		}
	}
	hi := min(uint64(h.d.info.FilePages), uint64(h.d.env.opts.Limits.MaxPages))
	for pg := uint64(s.Info.InitialPages) + 1; pg <= hi; pg++ {
		data, err := h.rawPage(uint32(pg))
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		prov := &JournalProv{Note: JournalNoteBeyondInitialSize, Hot: s.Info.Hot, Applied: true, Nonce: s.Info.Nonce}
		if err := w.emit(OriginDBRolledBack, uint32(pg), PageLoc{File: FileDB, Offset: PageOffset(h.d.info.PageSize, uint32(pg))}, nil, prov, data); err != nil {
			return err
		}
	}
	return nil
}

// walFrames yields the WAL frames the live state does not show: committed frames
// that are not the latest of their page (or lie beyond the commit's size),
// uncommitted, stale and unverified ones.
func (w *histWalk) walFrames() error {
	h := w.h
	if h.a == nil {
		return nil
	}
	s := h.a.scan
	var groups [4][]int // superseded, uncommitted, stale, unverified: indexes into s.Frames
	l := h.d.env.newLedger()
	defer l.free(l.n)
	if err := l.alloc(8 * int64(len(s.Frames))); err != nil {
		return fmt.Errorf("history of the WAL: %w", err)
	}
	limit := s.Info.DBPagesAfterCommit
	for i := range s.Frames {
		f := &s.Frames[i]
		switch f.State {
		case FrameCommitted:
			if f.Page > limit || s.latest[f.Page] != f.Slot {
				groups[0] = append(groups[0], i)
			}
		case FrameUncommitted:
			groups[1] = append(groups[1], i)
		case FrameStale:
			groups[2] = append(groups[2], i)
		default: // broken, detached, unanchored
			groups[3] = append(groups[3], i)
		}
	}
	origins := [4]Origin{OriginWALSuperseded, OriginWALUncommitted, OriginWALStale, OriginWALUnverified}
	gens := s.Info.Generations
	for k, g := range groups {
		slices.SortStableFunc(g, func(a, b int) int {
			fa, fb := &s.Frames[a], &s.Frames[b]
			return cmp.Or(
				cmp.Compare(fa.Page, fb.Page),
				cmp.Compare(gens[fa.Generation].Age, gens[fb.Generation].Age),
				cmp.Compare(fa.Generation, fb.Generation),
				cmp.Compare(fa.Slot, fb.Slot),
			)
		})
		for _, i := range g {
			f := &s.Frames[i]
			prov := &WALProv{Frame: f.Slot, Salt1: f.Salt1, Salt2: f.Salt2, State: f.State, Committed: f.State == FrameCommitted, Linked: f.Linked, Generation: f.Generation}
			if k == 0 && f.Page > limit {
				prov.Note = "page-beyond-commit-size"
			}
			loc := PageLoc{File: FileWAL, Offset: f.Offset + walFrameHeader, Frame: f.Slot}
			data, err := h.readLoc(loc)
			if err != nil && !isUnavailable(err) {
				return err
			}
			if err := w.emit(origins[k], f.Page, loc, prov, nil, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// journalRecords yields every record of the journal (valid or not, applied or
// not) as a before-image.
func (w *histWalk) journalRecords() error {
	h := w.h
	if h.jr == nil {
		return nil
	}
	l := h.d.env.newLedger()
	defer l.free(l.n)
	if err := l.alloc(8 * int64(len(h.jr.scan.Records))); err != nil {
		return fmt.Errorf("history of the journal: %w", err)
	}
	recs := h.jr.scan.Records
	order := make([]int, len(recs))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(recs[a].Page, recs[b].Page), cmp.Compare(a, b))
	})
	for _, i := range order {
		r := recs[i]
		loc := PageLoc{File: FileJournal, Offset: r.Offset, Record: r.Index}
		data, err := h.readLoc(loc)
		if err != nil && !isUnavailable(err) {
			return err
		}
		if err := w.emit(OriginJournalBefore, r.Page, loc, nil, h.jprov(i, r.Applied), data); err != nil {
			return err
		}
	}
	return nil
}

// journalPageSize is the size of a journal record's page image.
func (h *Hist) journalPageSize() int64 {
	if ps := int64(h.jr.scan.Info.PageSize); ps > 0 {
		return ps
	}
	return int64(h.d.info.PageSize)
}

// readLoc reads the image at loc: a database page (short at the end of the
// file), a WAL frame's page or a journal record's page. A page that does not lie
// wholly inside the file is ErrPageUnavailable: never padded with zeros.
func (h *Hist) readLoc(loc PageLoc) ([]byte, error) {
	d := h.d
	ps := int64(d.info.PageSize)
	var r io.ReaderAt
	var n, size int64
	switch loc.File {
	case FileDB:
		r, size, n = d.r, d.size, min(ps, d.size-loc.Offset)
	case FileWAL:
		if h.a == nil {
			return nil, fmt.Errorf("%w: no WAL is attached", ErrPageUnavailable)
		}
		r, size, n = h.a.r, h.a.size, ps
	case FileJournal:
		if h.jr == nil {
			return nil, fmt.Errorf("%w: no journal is attached", ErrPageUnavailable)
		}
		r, size, n = h.jr.r, h.jr.size, h.journalPageSize()
	default:
		return nil, fmt.Errorf("%w: no file %v", ErrPageUnavailable, loc.File)
	}
	if loc.Offset < 0 || n <= 0 || loc.Offset+n > size {
		return nil, fmt.Errorf("%w: the %d bytes at offset %d do not lie inside the %s (%d bytes)", ErrPageUnavailable, n, loc.Offset, loc.File, size)
	}
	buf := make([]byte, n)
	if err := readFull(r, buf, loc.Offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// Bytes returns a copy of the page image: the bytes of Loc.File at Loc.Offset,
// one page long (shorter for a trailing partial page of the database file). An
// image that does not lie wholly inside its file is ErrPageUnavailable.
func (p PageImage) Bytes() (b []byte, err error) {
	defer guard(&err)
	if p.h == nil {
		return nil, fmt.Errorf("%w: the image does not belong to a history", ErrPageUnavailable)
	}
	return p.h.readLoc(p.Loc)
}

// warnUnattributed raises the one warning that says how many pages the layout
// could not attribute because the schema was read incompletely. The history does
// not list them (they are neither live nor orphan), so the examiner must be told.
// Identical warnings collapse, so repeated walks raise it once.
func (w *histWalk) warnUnattributed() {
	n := 0
	for pg := uint32(1); pg <= w.lay.Addressable; pg++ {
		if w.lay.Class[pg] == ClassUnattributed {
			n++
		}
	}
	if n > 0 {
		w.h.warns.add(Warning{Code: WarnPagesUnattributed, File: FileDB, Msg: fmt.Sprintf("%d pages are unattributed: the schema was read incompletely, so their owner is unknown; they are not listed in the history", n)})
	}
}
