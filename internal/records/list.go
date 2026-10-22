package records

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// rowColumns are the columns scanRow reads, over the aliases r (records),
// p (parsers) and b (record_batches).
const rowColumns = `r.id, r.type, r.payload_v, r.artifact_id, r.source_path, r.locator, r.src_offset, r.src_length,
	r.ts, r.ts_end, r.ts_basis, r.tz_offset_min, r.deleted, r.recovered, r.recovery_method, r.confidence,
	p.name, p.version, p.hash, r.summary, b.ingest_id`

// supersededColumn is the last column of every row query: 1 when the record's run
// was superseded for its artifact. While record_superseded is empty it is the
// constant 0 and nothing is probed per row.
func supersededColumn(have bool) string {
	if have {
		return supersededExists
	}
	return "0"
}

type scanner interface{ Scan(dest ...any) error }

// scanRow reads one row of rowColumns plus the superseded column; extra
// destinations follow them.
func scanRow(s scanner, extra ...any) (Row, error) {
	var (
		row                          Row
		srcPath, locator, basis      sql.NullString
		method, pname, pver, phash   sql.NullString
		ingest                       sql.NullString
		off, length, ts, tsEnd       sql.NullInt64
		tz, conf                     sql.NullInt64
		deleted, recovered, superset int64
	)
	dest := append([]any{
		&row.ID, &row.Type, &row.PayloadV, &row.ArtifactID, &srcPath, &locator, &off, &length,
		&ts, &tsEnd, &basis, &tz, &deleted, &recovered, &method, &conf,
		&pname, &pver, &phash, &row.Summary, &ingest, &superset,
	}, extra...)
	if err := s.Scan(dest...); err != nil {
		return Row{}, err
	}
	row.SourcePath, row.Locator, row.Method = srcPath.String, locator.String, method.String
	if off.Valid && length.Valid {
		row.Range = &Range{Offset: off.Int64, Length: length.Int64}
	}
	if ts.Valid {
		row.TS = storedTime(ts.Int64, basis, tz)
	}
	if tsEnd.Valid {
		row.TSEnd = storedTime(tsEnd.Int64, basis, tz)
	}
	row.Deleted, row.Recovered, row.Superseded = deleted != 0, recovered != 0, superset != 0
	if conf.Valid {
		c := int(conf.Int64)
		row.Confidence = &c
	}
	row.Parser = ParserInfo{Name: pname.String, Version: pver.String, Hash: phash.String}
	row.IngestID = ingest.String
	return row, nil
}

// storedTime is the Time of a stored ts: Unix microseconds, the row's basis and
// offset.
func storedTime(us int64, basis sql.NullString, tz sql.NullInt64) *Time {
	t := Time{T: time.UnixMicro(us).UTC(), Basis: Basis(basis.String)}
	if tz.Valid {
		t.OffsetMin = int(tz.Int64)
	}
	return &t
}

// hasSuperseded reports whether any run has been superseded.
func hasSuperseded(ctx context.Context, h evidence.ReadHandle) (bool, error) {
	var n int
	if err := h.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM record_superseded)`).Scan(&n); err != nil {
		return false, fmt.Errorf("records: probe supersession: %w", err)
	}
	return n != 0, nil
}

func (p Page) limit() (int, error) {
	switch {
	case p.Limit == 0:
		return DefaultLimit, nil
	case p.Limit < 0 || p.Limit > MaxLimit:
		return 0, fmt.Errorf("%w: limit %d is outside 1..%d", ErrInvalidPage, p.Limit, MaxLimit)
	}
	return p.Limit, nil
}

// position returns the decoded cursor of the page, nil for the first page. fp is
// the fingerprint of the listing being asked for: a cursor of another one is
// ErrBadCursor.
func (p Page) position(fp string) (*cursor, error) {
	if p.Cursor == "" {
		return nil, nil
	}
	c, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	if (c.D == 1) != p.Desc {
		return nil, fmt.Errorf("%w: the cursor belongs to the other direction", ErrBadCursor)
	}
	if c.F != fp {
		return nil, fmt.Errorf("%w: the cursor belongs to another filter, order or case", ErrBadCursor)
	}
	return &c, nil
}

// A segment is one query of a listing: an extra condition, the ORDER BY that
// goes with it and the arguments of the condition. A page is the concatenation
// of the rows of its segments, in order, up to the page size.
//
// The listing order is (ts IS NULL), ts, id, which the expression indexes
// serve. The keyset after a row is the single range "everything after
// (ts IS NULL, ts, id)", but SQLite cannot seek an expression index with a
// row-value comparison (it scans the index from the start and filters, so a deep
// page would cost as much as every page before it). The range is therefore split
// at the boundary between timed and untimed rows into at most two queries, each
// an equality on the index's first expression plus a range on the next columns,
// which SQLite does seek, and whose rows come out of the index already in
// order. Together the segments select exactly the rows after the cursor:
//
//	ascending  after a timed row  : timed with (ts, id) > (ts0, id0), then every untimed row
//	ascending  after an untimed row: untimed with id > id0
//	descending after a timed row  : timed with (ts, id) < (ts0, id0)
//	descending after an untimed row: untimed with id < id0, then every timed row
type segment struct {
	cond  string // "" = none
	order string
	args  []any
}

// listOrder names the order of a listing in its fingerprint.
const listOrder = "untimed-last,ts,id"

// caseIdentity names the case a cursor belongs to.
func (r *Reader) caseIdentity() string { return r.c.Meta.ID + "\x00" + r.c.Meta.Created }

const (
	timed   = "(r.ts IS NULL) = 0"
	untimed = "(r.ts IS NULL) = 1 AND r.ts IS NULL" // r.ts IS NULL lets the id range seek past the ts column
)

// segments returns the queries of the page after cur (nil = the first page).
func segments(desc bool, cur *cursor, timedOnly bool) []segment {
	segs := allSegments(desc, cur)
	if !timedOnly {
		return segs
	}
	// every selected row has a ts: the untimed segments select nothing, and the leading
	// (ts IS NULL) of the order is constant, which lets the planner read the index in order
	out := segs[:0:0]
	for _, s := range segs {
		switch {
		case s.cond == "" && desc:
			s.order = " ORDER BY r.ts DESC, r.id DESC"
		case s.cond == "":
			s.order = " ORDER BY r.ts, r.id"
		case strings.HasPrefix(s.cond, untimed):
			continue
		}
		out = append(out, s)
	}
	return out
}

func allSegments(desc bool, cur *cursor) []segment {
	switch {
	case cur == nil && !desc:
		return []segment{{order: " ORDER BY (r.ts IS NULL), r.ts, r.id"}}
	case cur == nil:
		return []segment{{order: " ORDER BY (r.ts IS NULL) DESC, r.ts DESC, r.id DESC"}}
	case !desc && cur.N == 0:
		return []segment{
			{timed + " AND (r.ts, r.id) > (?, ?)", " ORDER BY r.ts, r.id", []any{cur.TS, cur.ID}},
			{untimed, " ORDER BY r.id", nil},
		}
	case !desc:
		return []segment{{untimed + " AND r.id > ?", " ORDER BY r.id", []any{cur.ID}}}
	case cur.N == 0:
		return []segment{{timed + " AND (r.ts, r.id) < (?, ?)", " ORDER BY r.ts DESC, r.id DESC", []any{cur.TS, cur.ID}}}
	}
	return []segment{
		{untimed + " AND r.id < ?", " ORDER BY r.id DESC", []any{cur.ID}},
		{timed, " ORDER BY r.ts DESC, r.id DESC", nil},
	}
}

// query is one SQL statement with its arguments (the LIMIT is the last one).
type query struct {
	sql  string
	args []any
}

// buildList assembles the queries of a listing page from constant fragments;
// every value is a ? placeholder. The last argument of each is its LIMIT, to be
// set by the caller (it holds the placeholder value 0 here).
func buildList(f Filter, desc bool, cur *cursor, have bool) ([]query, error) {
	base, err := f.compile(have)
	if err != nil {
		return nil, err
	}
	// A common text query is driven by the time-ordered index (D14): the planner would otherwise
	// materialize every matching rowid before it could order by time. A tiny filter or a rare query
	// carries no hint (the planner or the filter's own index drives).
	hint := ""
	if f.Text != nil && (f.Text.shape == shapeHintedIN || f.Text.shape == shapeExists) {
		hint = " INDEXED BY records_ts"
		if len(uniqSorted(f.Types)) == 1 {
			hint = " INDEXED BY records_type_ts"
		}
	}
	var out []query
	for _, seg := range segments(desc, cur, f.timedOnly()) {
		w := base
		w.conds = slices.Clone(base.conds)
		w.args = slices.Clone(base.args)
		if seg.cond != "" {
			w.add(seg.cond, seg.args...)
		}
		q := "SELECT " + rowColumns + ", " + supersededColumn(have) + fromRecords + hint + joinParsers + joinBatches +
			w.whereSQL() + seg.order + " LIMIT ?"
		out = append(out, query{q, append(w.args, 0)})
	}
	return out, nil
}

// refuseRank refuses a filter whose query asks for relevance order where there is none: only Search
// ranks, and an option is never ignored silently.
func refuseRank(f Filter) error {
	if f.Text != nil && f.Text.rank {
		return fmt.Errorf("%w: a relevance-ordered query is only for Search", ErrInvalidPage)
	}
	return nil
}

// List returns one page of the records f selects, in the order
// (untimed last, ts, id), or the exact reverse with Page.Desc. Pages are keyset
// pages: across the pages of an unchanged database every row appears exactly
// once, whatever the page size, even with tied or missing timestamps. A cursor
// holds a position and the fingerprint of the case, filter, order and direction
// that produced it: used with any other it is ErrBadCursor.
func (r *Reader) List(ctx context.Context, f Filter, p Page) (Result, error) {
	if err := refuseRank(f); err != nil {
		return Result{}, err
	}
	limit, err := p.limit()
	if err != nil {
		return Result{}, err
	}
	return r.page(ctx, f, p, limit, nil)
}

// page is List, with an optional step that runs on the page's rows inside the same read transaction
// (Search builds its snippets there).
func (r *Reader) page(ctx context.Context, f Filter, p Page, limit int, with func(evidence.ReadHandle, []Row) error) (Result, error) {
	if _, err := f.compile(false); err != nil { // validate before touching the database
		return Result{}, err
	}
	fp := f.fingerprint(r.caseIdentity(), p.Desc)
	cur, err := p.position(fp)
	if err != nil {
		return Result{}, err
	}
	var res Result
	err = r.readTx(ctx, f.Text != nil, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		if f.Text != nil && !f.Text.rank {
			shape, err := r.chooseShape(ctx, h, f, have)
			if err != nil {
				return err
			}
			tq := *f.Text
			tq.shape = shape
			f.Text = &tq
		}
		parts := f.split()
		if parts == nil {
			parts = []Filter{f}
		}
		lists := make([][]Row, len(parts))
		for k, part := range parts {
			qs, err := buildList(part, p.Desc, cur, have)
			if err != nil {
				return err
			}
			for _, q := range qs { // one query (one open Rows) at a time
				want := limit + 1 - len(lists[k])
				if want <= 0 {
					break
				}
				q.args[len(q.args)-1] = want
				if f.Text != nil {
					if err := r.matching(ctx); err != nil {
						return err
					}
				}
				if err := r.collect(ctx, h, q, &lists[k]); err != nil {
					return err
				}
			}
		}
		res.Rows = mergeRows(lists, p.Desc, limit+1)
		if len(res.Rows) > limit {
			res.Rows = res.Rows[:limit]
			next, err := cursorAfter(res.Rows[limit-1], p.Desc, fp)
			if err != nil {
				return err
			}
			res.NextCursor = next.encode()
		}
		if with != nil {
			return with(h, res.Rows)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// collect appends the rows of q to dst.
func (r *Reader) collect(ctx context.Context, h evidence.ReadHandle, q query, dst *[]Row) error {
	if r.queryLog != nil {
		r.queryLog(q.sql)
	}
	rows, err := h.QueryContext(ctx, q.sql, q.args...)
	if err != nil {
		return fmt.Errorf("records: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	r.started()
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return fmt.Errorf("records: list: %w", err)
		}
		r.rowsRead.Add(1)
		*dst = append(*dst, row)
	}
	return rows.Err()
}

// cursorAfter is the position after row in the listing fp.
func cursorAfter(row Row, desc bool, fp string) (cursor, error) {
	c := cursor{ID: row.ID, F: fp}
	if desc {
		c.D = 1
	}
	if row.TS == nil {
		c.N = 1
		return c, nil
	}
	us, ok := unixMicros(row.TS.T)
	if !ok {
		return cursor{}, fmt.Errorf("records: timestamp of record %d does not fit in Unix microseconds", row.ID)
	}
	c.TS = us
	return c, nil
}

// fromSQL is the FROM clause of a query that reads only what w needs, plus the
// joins the caller forces.
func (w where) fromSQL(parser, batch bool) string {
	s := fromRecords
	if w.needParser || parser {
		s += joinParsers
	}
	if w.needBatch || batch {
		s += joinBatches
	}
	return s
}

// Count counts the records f selects, reading at most limit+1 of them: n is
// exact up to limit, and capped is true when there are more (n is then limit).
// A limit of 0 counts everything.
func (r *Reader) Count(ctx context.Context, f Filter, limit int) (n int64, capped bool, err error) {
	if limit < 0 {
		return 0, false, fmt.Errorf("%w: count limit %d is negative", ErrInvalidPage, limit)
	}
	if err := refuseRank(f); err != nil {
		return 0, false, err
	}
	if _, err := f.compile(false); err != nil {
		return 0, false, err
	}
	err = r.readTx(ctx, f.Text != nil, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		w, err := f.compile(have)
		if err != nil {
			return err
		}
		lim := int64(-1) // no limit
		if limit > 0 {
			lim = int64(limit) + 1
		}
		q := "SELECT count(*) FROM (SELECT 1" + w.fromSQL(false, false) + w.whereSQL() + " LIMIT ?)"
		if f.Text != nil {
			if err := r.matching(ctx); err != nil {
				return err
			}
		}
		return h.QueryRowContext(ctx, q, append(w.args, lim)...).Scan(&n)
	})
	if err != nil {
		return 0, false, fmt.Errorf("records: count: %w", err)
	}
	if limit > 0 && n > int64(limit) {
		return int64(limit), true, nil
	}
	return n, false, nil
}

// maxMerge is the longest value list answered by one ordered query per value; a longer one falls
// back to a single IN query, which may have to sort every match.
const maxMerge = 32

func uniqSorted(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return slices.Compact(out)
}

// split returns one filter per value of the first multi-valued list (Types, else ArtifactIDs) when
// that list has 2..maxMerge distinct values, else nil. A page is the merge of the pages of the parts.
func (f Filter) split() []Filter {
	if vals := uniqSorted(f.Types); len(vals) >= 2 && len(vals) <= maxMerge {
		out := make([]Filter, len(vals))
		for i, v := range vals {
			out[i] = f
			out[i].Types = []string{v}
		}
		return out
	}
	if vals := uniqSorted(f.ArtifactIDs); len(uniqSorted(f.Types)) < 2 && len(vals) >= 2 && len(vals) <= maxMerge {
		out := make([]Filter, len(vals))
		for i, v := range vals {
			out[i] = f
			out[i].ArtifactIDs = []string{v}
		}
		return out
	}
	return nil
}

// mergeRows merges lists that are each in the list order (ascending, or descending when desc) and
// returns the first n rows. The lists are disjoint (a record has one type and one artifact).
func mergeRows(lists [][]Row, desc bool, n int) []Row {
	var all []Row
	for _, l := range lists {
		all = append(all, l...)
	}
	slices.SortFunc(all, func(a, b Row) int {
		c := cmpListOrder(a, b)
		if desc {
			return -c
		}
		return c
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// cmpListOrder is the ascending order of ORDER BY (ts IS NULL), ts, id.
func cmpListOrder(a, b Row) int {
	au, bu := a.TS == nil, b.TS == nil
	switch {
	case au && !bu:
		return 1
	case bu && !au:
		return -1
	case !au:
		if c := cmp.Compare(a.TS.T.UnixMicro(), b.TS.T.UnixMicro()); c != 0 {
			return c
		}
	}
	return cmp.Compare(a.ID, b.ID)
}

// searchShape is the plan of a text-search page. Every shape selects the same records in the same order
// (a plan is never part of a cursor).
type searchShape int

const (
	// shapeFTSFirst: r.id IN (full-text rowids), no hint; the planner is free. A rare query.
	shapeFTSFirst searchShape = iota
	// shapeHintedIN: the same IN set, with the time-order index hint: the set is built once and the
	// first hits in time order end the scan. A query with many hits that cannot afford a per-record probe.
	shapeHintedIN
	// shapeExists: the hinted index drives, with one EXISTS on the full-text rowid per record. A very
	// common single word: no set is ever built.
	shapeExists
	// shapeExistsNoHint: EXISTS per record without the hint, so the filter's own index drives. A tiny filter.
	shapeExistsNoHint
)

// chooseShape picks the plan of a page from one bounded full-text probe and, when a filter is present
// and the query is not rare, one bounded filter probe, both in the page's read transaction.
func (r *Reader) chooseShape(ctx context.Context, h evidence.ReadHandle, f Filter, have bool) (searchShape, error) {
	hits, err := r.probeText(ctx, h, f.Text)
	if err != nil {
		return shapeFTSFirst, err
	}
	if hits < searchProbeLimit {
		return shapeFTSFirst, nil
	}
	tiny, err := r.probeFilter(ctx, h, f, have)
	if err != nil {
		return shapeFTSFirst, err
	}
	return pickShape(hits, f.Text.reexpands(), tiny), nil
}

// pickShape is the plan of a query with at least searchProbeLimit hits (hits is capped at
// searchINLimit+1). A prefix or multi-term query never takes EXISTS: the set is built once.
func pickShape(hits int, reexpands, tinyFilter bool) searchShape {
	switch {
	case tinyFilter && reexpands:
		return shapeFTSFirst // the planner may drive by the filter's index and probe the set
	case tinyFilter:
		return shapeExistsNoHint
	case hits <= searchINLimit || reexpands:
		return shapeHintedIN
	}
	return shapeExists
}

// probeText counts the full-text hits of q up to searchINLimit+1, with one bounded statement, to
// choose the plan of a page (the filter is not applied: the probe classifies the query's selectivity).
func (r *Reader) probeText(ctx context.Context, h evidence.ReadHandle, q *TextQuery) (int, error) {
	if q.match == "" || (q.table != evidence.FTSWordTable && q.table != evidence.FTSSubTable) {
		return 0, invalidFilter("the text query was not compiled by CompileQuery or CompileTerms")
	}
	// the table is one of the two constants above, never taken from the query's text
	stmt := "SELECT count(*) FROM (SELECT 1 FROM " + q.table + " WHERE " + q.table + " MATCH ? LIMIT ?)"
	if err := r.matching(ctx); err != nil {
		return 0, err
	}
	if r.queryLog != nil {
		r.queryLog(stmt) // logged like every statement, so a test can count it
	}
	var n int
	if err := h.QueryRowContext(ctx, stmt, q.match, max(searchINLimit, searchProbeLimit)+1).Scan(&n); err != nil {
		return 0, fmt.Errorf("records: list: %w", err)
	}
	return n, nil
}

// probeFilter reports whether the filter, without its text query, selects fewer than searchFilterProbe
// records: one bounded count in the page's read transaction (never ANALYZE: that would write into the
// case). A filter with no condition of its own is never tiny.
func (r *Reader) probeFilter(ctx context.Context, h evidence.ReadHandle, f Filter, have bool) (bool, error) {
	own := f
	own.Text, own.IncludeSuperseded = nil, true // the implicit hiding of superseded runs is not a filter
	ownW, err := own.compile(have)
	if err != nil {
		return false, err
	}
	if len(ownW.conds) == 0 {
		return false, nil
	}
	f.Text = nil
	w, err := f.compile(have) // the real selection, hiding included
	if err != nil {
		return false, err
	}
	stmt := "SELECT count(*) FROM (SELECT 1" + w.fromSQL(false, false) + w.whereSQL() + " LIMIT ?)"
	if err := r.matching(ctx); err != nil {
		return false, err
	}
	if r.queryLog != nil {
		r.queryLog(stmt)
	}
	var n int
	if err := h.QueryRowContext(ctx, stmt, append(w.args, searchFilterProbe)...).Scan(&n); err != nil {
		return false, fmt.Errorf("records: list: %w", err)
	}
	return n < searchFilterProbe, nil
}

// The plan bands, by full-text hits: fewer than searchProbeLimit is FTS-first, up to searchINLimit is
// the hinted IN, more is EXISTS. A filter that selects fewer than searchFilterProbe records drops the
// hint. They are variables only so that a test can move them.
var (
	searchProbeLimit  = 4096
	searchINLimit     = 262144
	searchFilterProbe = 4096
)
