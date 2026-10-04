package records

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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
func segments(desc bool, cur *cursor) []segment {
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
	var out []query
	for _, seg := range segments(desc, cur) {
		w := base
		w.conds = slices.Clone(base.conds)
		w.args = slices.Clone(base.args)
		if seg.cond != "" {
			w.add(seg.cond, seg.args...)
		}
		q := "SELECT " + rowColumns + ", " + supersededColumn(have) + fromRecords + joinParsers + joinBatches +
			w.whereSQL() + seg.order + " LIMIT ?"
		out = append(out, query{q, append(w.args, 0)})
	}
	return out, nil
}

// List returns one page of the records f selects, in the order
// (untimed last, ts, id), or the exact reverse with Page.Desc. Pages are keyset
// pages: across the pages of an unchanged database every row appears exactly
// once, whatever the page size, even with tied or missing timestamps. A cursor
// holds a position and the fingerprint of the case, filter, order and direction
// that produced it: used with any other it is ErrBadCursor.
func (r *Reader) List(ctx context.Context, f Filter, p Page) (Result, error) {
	limit, err := p.limit()
	if err != nil {
		return Result{}, err
	}
	if _, err := f.compile(false); err != nil { // validate before touching the database
		return Result{}, err
	}
	fp := f.fingerprint(r.caseIdentity(), p.Desc)
	cur, err := p.position(fp)
	if err != nil {
		return Result{}, err
	}
	var res Result
	err = r.c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		qs, err := buildList(f, p.Desc, cur, have)
		if err != nil {
			return err
		}
		for _, q := range qs { // one query (one open Rows) at a time
			want := limit + 1 - len(res.Rows)
			if want <= 0 {
				break
			}
			q.args[len(q.args)-1] = want
			if err := collect(ctx, h, q, &res.Rows); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
		next, err := cursorAfter(res.Rows[limit-1], p.Desc, fp)
		if err != nil {
			return Result{}, err
		}
		res.NextCursor = next.encode()
	}
	return res, nil
}

// collect appends the rows of q to dst.
func collect(ctx context.Context, h evidence.ReadHandle, q query, dst *[]Row) error {
	rows, err := h.QueryContext(ctx, q.sql, q.args...)
	if err != nil {
		return fmt.Errorf("records: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return fmt.Errorf("records: list: %w", err)
		}
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
	if _, err := f.compile(false); err != nil {
		return 0, false, err
	}
	err = r.c.ReadTx(ctx, func(h evidence.ReadHandle) error {
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
