package dbrecover

// Strict in-page slack carving (sub-project 3J, Task 14, ruling J5).
//
// PROBE VERDICT (slack_probe_test.go): carving is ENABLED, for table b-tree
// page images (leaf pages, and interior pages: the root of a split table is an
// interior page whose gap still holds the cells of the leaf it was), in the two
// kinds of in-page free space (the gap between the cell pointer array and the
// cell content area, and freeblocks). The probe built pages with an
// independent engine (secure_delete off): deleted rows, a defragmented page
// (old copies left in the gap) and freeblocks, and compared a structural
// scanner with the engine's own log of what it wrote. Results:
//
//   - gap bytes of live pages hold whole, byte-exact old cells: the probe's
//     scanner carved 24 cells, all tuples the engine wrote, and the real carver
//     recovers 23 of the 25 deleted rows of the engine scenario and delivers no
//     tuple the engine did not write (a zero-filled span would parse as one
//     all-NULL record; a record must hold a non-NULL value);
//   - gap bytes of superseded WAL images hold older cells the same way (23 of 23);
//   - a freeblock keeps its cell only after the engine overwrote its first 4
//     bytes with the next-block pointer and the size, so a freeblock cell is
//     carvable only when it was coalesced behind another deleted cell; none of
//     the single-cell freeblocks of the probe could be read, and none is
//     invented: their bytes are counted in SlackUnparsedBytes;
//   - with secure_delete the spans are zero: nothing is carved, nothing is said;
//   - the false positive that exists is structural, not random: a zero-filled
//     span parses as one huge all-NULL record. It is refused by the rule that a
//     record must hold at least one non-NULL value.
//
// So: (a) live table pages and every history image that still reads as a
// table page are carved; (b) freeblocks are scanned like the gap, but a
// cell whose start the engine overwrote is counted, never repaired. The bytes
// of a freelist trunk after its leaf list (SpanTrunkTail) and the free space of
// index pages are not carved; their non-zero bytes are counted in
// SlackUnparsedBytes. If the probe's numbers ever stop holding, set
// slackCarving to false: the feature stays off and nothing else changes.
//
// A candidate is a cell found by scanning, never read from a pointer, so every
// candidate must be fully consistent: canonical payload-length and rowid
// varints, a payload that fits the page without overflow, a record whose header
// length plus the declared body length is exactly the payload length, no
// reserved serial type, at least one non-NULL value, and the whole cell inside
// one span. Cells are taken left to right without overlap. Work per page is
// capped (slackPageBudget); the cap is a counted warning, never a silent cut.

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// slackCarving says whether the second pass runs. The probe decided yes (above);
// tests switch it off to compare against the no-slack baseline.
var slackCarving = true

// slackPageBudget is the work units one page image may spend: one per offset
// examined plus the header bytes a candidate makes the scanner read.
var slackPageBudget int64 = 1 << 20

// maxSlackWarnings bounds the warnings one carve keeps.
const maxSlackWarnings = 50

// noteBoundariesInferred marks a row whose cell start and length were found by
// scanning free space, not read from a cell pointer.
const noteBoundariesInferred = "boundaries-inferred"

// slackStats is what a carve found.
type slackStats struct {
	Carved          int64 // rows delivered
	DroppedLiveCopy int64 // byte-equal to a live cell of the same page: provably not deleted data
	CrossPageEqual  int64 // delivered with equals-live-row
	DroppedOverlap  int64 // a candidate over a live cell: the span was not free space there
	DroppedCrossed  int64 // a valid cell starts inside it and ends beyond it: its tail was overwritten
	UnparsedBytes   int64 // non-zero bytes of free space that no carved cell explains

	steps    int64    // work units spent over all pages
	warnings []string // bounded, in page order
}

func (s *slackStats) warn(format string, a ...any) {
	if len(s.warnings) < maxSlackWarnings {
		s.warnings = append(s.warnings, fmt.Sprintf(format, a...))
	} else if len(s.warnings) == maxSlackWarnings {
		s.warnings = append(s.warnings, "further slack warnings are not listed")
	}
}

type slackCandidate struct {
	off, n int
	rowid  int64
	rec    sqlitefile.Record
	cell   []byte
}

type slackLive struct{ off, end int }

// carveSpans carves the free space of every table page image of the
// history (see the package comment) and delivers the cells it finds. It reads
// the history and the live view and changes nothing.
func carveSpans(ctx context.Context, db *sqlitefile.DB, h *sqlitefile.Hist, emit func(sqlitefile.RecoveredRow) bool) (st slackStats, err error) {
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("slack carving: internal error: %v", x)
		}
	}()
	live := db.Live()
	sch, err := live.Schema(ctx)
	if err != nil {
		return st, err
	}
	info := db.Info()
	lim := sqlitefile.DefaultLimits()
	tables := map[string]*sqlitefile.Table{}
	var visitErr error
	perr := h.Pages(ctx, func(img sqlitefile.PageImage) bool {
		if err := ctx.Err(); err != nil {
			visitErr = err
			return false
		}
		var spans []sqlitefile.Span
		for _, sp := range img.Spans {
			if sp.Kind == sqlitefile.SpanTrunkTail {
				continue
			}
			spans = append(spans, sp)
		}
		if img.Partial || len(img.Spans) == 0 {
			return true
		}
		data, err := img.Bytes()
		if err != nil {
			st.warn("page %d (%s): the image could not be read: %v", img.Number, img.Origin, err)
			return true
		}
		if len(img.Spans) > len(spans) {
			for _, sp := range img.Spans {
				if sp.Kind == sqlitefile.SpanTrunkTail {
					st.UnparsedBytes += nonZero(data, sp.Offset, sp.Offset+sp.Length)
				}
			}
		}
		hd, err := sqlitefile.ParsePageHeader(data, img.Number)
		if err != nil {
			return true
		}
		if hd.Type != sqlitefile.PageTableLeaf && hd.Type != sqlitefile.PageTableInterior {
			for _, sp := range spans {
				st.UnparsedBytes += nonZero(data, sp.Offset, sp.Offset+sp.Length)
			}
			return true
		}
		cands, _, copies := scanPage(data, hd, img, spans, info, lim, &st)
		if copies > 0 {
			st.DroppedLiveCopy += copies
			st.warn("page %d (%s): %d carved cell(s) are byte-equal to live cells of the same page and are not delivered", img.Number, img.Origin, copies)
		}
		if len(cands) == 0 {
			return true
		}
		cells := make([][]sqlitefile.Value, len(cands))
		for i, c := range cands {
			cells[i] = c.rec.Values
		}
		table, basis := sch.FitPage(cells)
		for _, c := range cands {
			if st.Carved >= lim.MaxHistoryRows {
				st.warn("the slack rows reached the cap of %d rows; the rest are not listed", lim.MaxHistoryRows)
				return false
			}
			rid := c.rowid
			row := sqlitefile.RecoveredRow{
				Method: sqlitefile.MethodPageSlack, Origin: img.Origin, Table: table, TableBasis: basis,
				Rowid: &rid, Values: c.rec.Values, Relation: sqlitefile.RelUnknown,
				Loc: sqlitefile.Loc{
					File: img.Loc.File, Page: img.Number, Cell: -1, Offset: img.Loc.Offset + int64(c.off), Length: int64(c.n),
					PageOffset: img.Loc.Offset, Frame: img.Loc.Frame, Record: img.Loc.Record,
				},
				Notes: []string{noteBoundariesInferred},
			}
			if table != "" && (basis == sqlitefile.BasisFit || basis == sqlitefile.BasisGuess) {
				row.Notes = append([]string{sqlitefile.NoteIdentityByFitOnly}, row.Notes...)
				same, uncertain := equalsLiveRow(ctx, live, tables, table, rid, c.rec.Values)
				switch {
				case uncertain:
					row.Notes = append(row.Notes, sqlitefile.NoteLiveUncertain)
				case same:
					row.Notes = append(row.Notes, sqlitefile.NoteEqualsLiveRow)
					st.CrossPageEqual++
				}
			}
			if img.WAL != nil {
				w := *img.WAL
				row.WAL = &w
			}
			if img.Journal != nil {
				j := *img.Journal
				row.Journal = &j
			}
			st.Carved++
			if !emit(row) {
				return false
			}
		}
		return true
	})
	if visitErr != nil {
		return st, visitErr
	}
	return st, perr
}

// equalsLiveRow reports whether the live table has a row with this rowid and
// exactly these values. uncertain is set when the live lookup failed.
func equalsLiveRow(ctx context.Context, live *sqlitefile.View, cache map[string]*sqlitefile.Table, name string, rowid int64, vals []sqlitefile.Value) (same, uncertain bool) {
	t, ok := cache[name]
	if !ok {
		var err error
		if t, err = live.Table(ctx, name); err != nil {
			t = nil
		}
		cache[name] = t
	}
	if t == nil {
		return false, true
	}
	row, found, err := t.Get(ctx, rowid)
	if err != nil {
		return false, true
	}
	if !found || len(row.Values) != len(vals) {
		return false, false
	}
	for i := range vals {
		if !sameStored(row.Values[i], vals[i]) {
			return false, false
		}
	}
	return true, false
}

// sameStored is true only for two conclusively equal stored values: the same
// storage class and the same bytes. An omitted, clipped or unread value is
// inconclusive, never equal.
func sameStored(a, b sqlitefile.Value) bool {
	if a.Omitted || b.Omitted || a.Clipped || b.Clipped || a.Unread || b.Unread || a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case sqlitefile.KindNull:
		return true
	case sqlitefile.KindInt:
		return a.Int == b.Int
	case sqlitefile.KindFloat:
		return math.Float64bits(a.Float) == math.Float64bits(b.Float)
	}
	return a.Len == b.Len && bytes.Equal(a.Bytes, b.Bytes)
}

func nonZero(b []byte, from, to int) (n int64) {
	from, to = max(from, 0), min(to, len(b))
	for i := from; i < to; i++ {
		if b[i] != 0 {
			n++
		}
	}
	return n
}

// scanPage scans the spans of one table-leaf image. It returns the accepted
// cells (ascending, non-overlapping), the live cells of the page and the count
// of candidates that were byte copies of a live cell. Counters go into st.
func scanPage(data []byte, hd sqlitefile.PageHeader, img sqlitefile.PageImage, spans []sqlitefile.Span, info sqlitefile.Info, lim sqlitefile.Limits, st *slackStats) (cands []slackCandidate, live []slackLive, copies int64) {
	usable := info.UsableSize
	var liveBytes [][]byte
	if cp, err := sqlitefile.CellPointers(data, hd, usable); err == nil {
		for _, g := range cp.Good {
			c, err := sqlitefile.ParseCell(data, usable, hd, g.Offset)
			if err != nil || c.Offset+c.Length > len(data) {
				continue
			}
			live = append(live, slackLive{c.Offset, c.Offset + c.Length})
			liveBytes = append(liveBytes, data[c.Offset:c.Offset+c.Length])
		}
	}
	maxLocal := usable - 35
	var spent int64
	var crossedHere int64
	capped := false
	for si, sp := range spans {
		end := min(sp.Offset+sp.Length, len(data))
		p := max(sp.Offset, 0)
		for p < end {
			if data[p] == 0 {
				p++
				spent++
				continue
			}
			if spent >= slackPageBudget {
				capped = true
				break
			}
			spent++
			cost, c, ok := parseSlackCell(data, p, end, maxLocal, info, lim)
			spent += cost
			if !ok {
				st.UnparsedBytes++
				p++
				continue
			}
			if overlapsLive(live, c.off, c.off+c.n) {
				st.DroppedOverlap++
				p += c.n
				continue
			}
			if slicesContainsBytes(liveBytes, c.cell) {
				copies++
				p += c.n
				continue
			}
			crossed, extra := crossedByCandidate(data, c, min(len(data), usable), maxLocal, info, lim)
			spent += extra
			if crossed {
				// a valid cell starts inside this one and ends beyond it: the later
				// cell was written over the tail of this one, whose bytes are damaged
				st.DroppedCrossed++
				crossedHere++
				st.UnparsedBytes++
				p++
				continue
			}
			cands = append(cands, c)
			p += c.n
		}
		if capped {
			st.UnparsedBytes += nonZero(data, p, end)
			for _, rest := range spans[si+1:] {
				st.UnparsedBytes += nonZero(data, rest.Offset, rest.Offset+rest.Length)
			}
			st.warn("page %d (%s): the work cap (%d units) was reached; the rest of its free space was not examined", img.Number, img.Origin, slackPageBudget)
			break
		}
	}
	if crossedHere > 0 {
		st.warn("page %d (%s): %d carved cell(s) are crossed by a later valid cell (their tail was overwritten) and are not delivered", img.Number, img.Origin, crossedHere)
	}
	st.steps += spent
	return cands, live, copies
}

func overlapsLive(live []slackLive, a, b int) bool {
	for _, l := range live {
		if a < l.end && l.off < b {
			return true
		}
	}
	return false
}

func slicesContainsBytes(set [][]byte, b []byte) bool {
	for _, s := range set {
		if bytes.Equal(s, b) {
			return true
		}
	}
	return false
}

// canonicalVarint reports whether a varint of n bytes holding v is the shortest
// form of v, as the engine writes it.
func canonicalVarint(v uint64, n int) bool {
	switch {
	case n <= 1:
		return true
	case n == 9:
		return v >= 1<<56
	}
	return v >= 1<<(7*(n-1))
}

// parseSlackCell tests whether a whole table-leaf cell starts at data[p] and
// ends by end. cost is the extra work the test spent (header bytes read).
func parseSlackCell(data []byte, p, end, maxLocal int, info sqlitefile.Info, lim sqlitefile.Limits) (cost int64, c slackCandidate, ok bool) {
	pl, n1 := sqlitefile.GetVarint(data[p:end])
	if n1 == 0 || !canonicalVarint(pl, n1) || pl < 2 || pl > uint64(maxLocal) {
		return 0, c, false
	}
	rid, n2 := sqlitefile.GetVarint(data[p+n1 : end])
	if n2 == 0 || !canonicalVarint(rid, n2) {
		return 0, c, false
	}
	total := n1 + n2 + int(pl)
	if p+total > end {
		return 0, c, false
	}
	rec := data[p+n1+n2 : p+total]
	if hl, _ := sqlitefile.GetVarint(rec); hl <= pl {
		cost = int64(hl)
	}
	serials, hl, body, err := sqlitefile.ParseRecordHeader(rec, lim.MaxColumns)
	if err != nil || len(serials) == 0 || int64(hl)+body != int64(len(rec)) {
		return cost, c, false
	}
	nonNull := false
	for _, s := range serials {
		if s == 10 || s == 11 {
			return cost, c, false
		}
		nonNull = nonNull || s != 0
	}
	if !nonNull {
		return cost, c, false
	}
	cell := bytes.Clone(data[p : p+total])
	r, err := sqlitefile.DecodeRecord(cell[n1+n2:], info.Encoding, lim)
	if err != nil || r.Truncated || r.LengthMismatch {
		return cost, c, false
	}
	return cost, slackCandidate{off: p, n: total, rowid: int64(rid), rec: r, cell: cell}, true
}

// crossedByCandidate reports whether a cell head that is consistent with a record
// starts strictly inside c and its declared extent ends beyond c (limit is the
// end of the usable area, not of the span: the later cell may itself be damaged
// by a third cell, and is still the proof that c was overwritten). Such a cell was written over the tail of c after c was freed,
// so c's body is partly another cell's bytes and must not be delivered as
// c's. A cell wholly inside c (a record that stores a cell image, for example)
// does not damage c. extra is the work spent.
func crossedByCandidate(data []byte, c slackCandidate, limit, maxLocal int, info sqlitefile.Info, lim sqlitefile.Limits) (crossed bool, extra int64) {
	cEnd := c.off + c.n
	for q := c.off + 1; q < cEnd; q++ {
		extra++
		if data[q] == 0 {
			continue
		}
		cost, d, ok := parseSlackCell(data, q, limit, maxLocal, info, lim)
		extra += cost
		if ok && d.off+d.n > cEnd {
			return true, extra
		}
	}
	return false, extra
}
