package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// statKeys are the grouping keys of Stats: the key expression (constant SQL)
// and the joins it needs.
var statKeys = map[string]struct {
	expr          string
	parser, batch bool
}{
	"type":     {expr: "r.type"},
	"parser":   {expr: "COALESCE(p.name, '') || '/' || COALESCE(p.version, '')", parser: true},
	"artifact": {expr: "r.artifact_id"},
	"deleted":  {expr: "CASE WHEN r.recovered = 1 THEN 'recovered' WHEN r.deleted = 1 THEN 'deleted' ELSE 'live' END"},
	"run":      {expr: "COALESCE(b.ingest_id, '')", batch: true},
}

// MaxStatGroups is the most groups one Stats answer holds.
const MaxStatGroups = 100_000

var maxStatGroups = MaxStatGroups

// StatsResult is a capped Stats answer: the first groups in key order, and whether more exist.
type StatsResult struct {
	Rows []StatRow
	More bool
}

// Stats counts the records f selects by one key: type, parser ("name/version"),
// artifact (id), deleted ("live", "deleted" or "recovered") or run (ingest id),
// with each group's earliest and latest ts. Groups come in key order. When f has
// more than MaxStatGroups groups it returns ErrStatsGroupCap and no rows (use
// StatsTop, or narrow the filter): a cut answer is never returned as a whole one.
func (r *Reader) Stats(ctx context.Context, f Filter, by string) ([]StatRow, error) {
	res, err := r.stats(ctx, f, by, maxStatGroups)
	if err != nil {
		return nil, err
	}
	if res.More {
		return nil, fmt.Errorf("%w: more than %d groups by %s (use StatsTop, or narrow the filter)", ErrStatsGroupCap, maxStatGroups, by)
	}
	return res.Rows, nil
}

// StatsTop is Stats limited to the first limit groups in key order (1..MaxStatGroups, else
// ErrInvalidPage). More says that further groups exist; the cut is never silent.
func (r *Reader) StatsTop(ctx context.Context, f Filter, by string, limit int) (StatsResult, error) {
	if limit < 1 || limit > MaxStatGroups {
		return StatsResult{}, fmt.Errorf("%w: group limit %d is outside 1..%d", ErrInvalidPage, limit, MaxStatGroups)
	}
	return r.stats(ctx, f, by, limit)
}

func (r *Reader) stats(ctx context.Context, f Filter, by string, limit int) (StatsResult, error) {
	if err := refuseRank(f); err != nil {
		return StatsResult{}, err
	}
	key, ok := statKeys[by]
	if !ok {
		return StatsResult{}, invalidFilter("cannot group by %q (type, parser, artifact, deleted or run)", by)
	}
	if _, err := f.compile(false); err != nil {
		return StatsResult{}, err
	}
	var out StatsResult
	err := r.readTx(ctx, f.Text != nil, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		w, err := f.compile(have)
		if err != nil {
			return err
		}
		q := "SELECT " + key.expr + " AS k, count(*), min(r.ts), max(r.ts)" + w.fromSQL(key.parser, key.batch) +
			w.whereSQL() + " GROUP BY k ORDER BY k LIMIT ?"
		if f.Text != nil {
			if err := r.matching(ctx); err != nil {
				return err
			}
		}
		rows, err := h.QueryContext(ctx, q, append(slices.Clone(w.args), limit+1)...)
		if err != nil {
			return fmt.Errorf("records: stats by %s: %w", by, err)
		}
		defer func() { _ = rows.Close() }()
		r.started()
		for rows.Next() {
			var s StatRow
			var k sql.NullString
			var lo, hi sql.NullInt64
			if err := rows.Scan(&k, &s.Count, &lo, &hi); err != nil {
				return fmt.Errorf("records: stats by %s: %w", by, err)
			}
			s.Key = k.String
			if lo.Valid {
				s.TSMin, s.TSMax = &lo.Int64, &hi.Int64
			}
			if len(out.Rows) == limit {
				out.More = true // the extra group proves more exist; it is not returned
				break
			}
			out.Rows = append(out.Rows, s)
		}
		return rows.Err()
	})
	if err != nil {
		return StatsResult{}, err
	}
	return out, nil
}

// overviewQuery is one scalar statement of Overview and its arguments.
type overviewQuery struct {
	sql  string
	args []any
}

// overviewStatements are the six scalar statements Overview runs, each served by an index when no
// run is superseded: the total, the deleted, recovered and untimed counts, and the first and last
// ts. The conditions added to the filter's own are implied by the schema (CHECK recovered = 0 OR
// deleted = 1, and (ts IS NULL) = 1 is ts IS NULL), so they select exactly what the old single scan
// summed, but spelled so that records_deleted and records_ts apply. With superseded runs present each
// statement also joins record_batches and probes record_superseded per row, and is not index-only
// (measured in matrix.list.super.* and stats.super_s).
func overviewStatements(w where) []overviewQuery {
	mk := func(sel, extra, tail string) overviewQuery {
		x := w
		x.conds = slices.Clone(w.conds)
		if extra != "" {
			x.conds = append(x.conds, extra)
		}
		return overviewQuery{sql: sel + x.fromSQL(false, false) + x.whereSQL() + tail, args: slices.Clone(w.args)}
	}
	return []overviewQuery{
		mk("SELECT count(*)", "", ""),
		mk("SELECT count(*)", "r.deleted = 1", ""),
		mk("SELECT count(*)", "r.deleted = 1 AND r.recovered = 1", ""),
		mk("SELECT count(*)", "(r.ts IS NULL) = 1", ""),
		mk("SELECT r.ts", "(r.ts IS NULL) = 0", " ORDER BY r.ts ASC LIMIT 1"),
		mk("SELECT r.ts", "(r.ts IS NULL) = 0", " ORDER BY r.ts DESC LIMIT 1"),
	}
}

// Overview summarises the records f selects: totals, the ts range, and the
// number of runs of the whole case by outcome. SupersededRecords counts the
// records of superseded runs among those f selects ignoring supersession, so
// with the default filter it is the number of records the default listing hides.
func (r *Reader) Overview(ctx context.Context, f Filter) (Overview, error) {
	if err := refuseRank(f); err != nil {
		return Overview{}, err
	}
	if _, err := f.compile(false); err != nil {
		return Overview{}, err
	}
	ov := Overview{Runs: map[string]int64{}}
	err := r.readTx(ctx, f.Text != nil, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		w, err := f.compile(have)
		if err != nil {
			return err
		}
		if f.Text != nil {
			if err := r.matching(ctx); err != nil {
				return err
			}
		}
		qs := overviewStatements(w)
		for i, dst := range []*int64{&ov.Records, &ov.Deleted, &ov.Recovered, &ov.Untimed} {
			if err := h.QueryRowContext(ctx, qs[i].sql, qs[i].args...).Scan(dst); err != nil {
				return fmt.Errorf("records: overview: %w", err)
			}
		}
		var lo, hi sql.NullInt64
		for i, dst := range []*sql.NullInt64{&lo, &hi} {
			q := qs[4+i]
			if err := h.QueryRowContext(ctx, q.sql, q.args...).Scan(dst); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("records: overview: %w", err)
			}
		}
		if lo.Valid && hi.Valid {
			ov.TSMin, ov.TSMax = &lo.Int64, &hi.Int64
		}
		return overviewRest(ctx, h, f, have, &ov)
	})
	if err != nil {
		return Overview{}, err
	}
	if err := r.overviewGaps(&ov); err != nil {
		return Overview{}, err
	}
	return ov, nil
}

// overviewRest reads what the scalar statements do not: the records of superseded runs among those f
// selects, and every run of the case by outcome.
func overviewRest(ctx context.Context, h evidence.ReadHandle, f Filter, have bool, ov *Overview) error {
	if have {
		all := f
		all.IncludeSuperseded = true
		aw, err := all.compile(have)
		if err != nil {
			return err
		}
		aw.add(supersededExists)
		aw.needBatch = true
		q := "SELECT count(*)" + aw.fromSQL(false, true) + aw.whereSQL()
		if err := h.QueryRowContext(ctx, q, aw.args...).Scan(&ov.SupersededRecords); err != nil {
			return fmt.Errorf("records: overview: %w", err)
		}
	}
	rows, err := h.QueryContext(ctx, `SELECT outcome, count(*) FROM record_runs GROUP BY outcome`)
	if err != nil {
		return fmt.Errorf("records: overview: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var outcome string
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			return fmt.Errorf("records: overview: %w", err)
		}
		ov.Runs[outcome] = n
	}
	return rows.Err()
}

// overviewGaps adds the ingests the database cannot show as complete (read after the transaction: the
// audit log is a file).
func (r *Reader) overviewGaps(ov *Overview) error {
	gaps, err := r.c.IngestGaps()
	if err != nil {
		return r.integrity(fmt.Errorf("records: overview: %w", err))
	}
	ov.Gaps = gaps
	return nil
}

// overviewTypesStatement is the one statement of OverviewAndTypes: a group per (type, timed or not)
// with its count and ts range, in key order. When no run is superseded it is a covering scan of
// records_type_ts, whose key is (type, ts IS NULL, ts, id), so no temporary b-tree is built.
func overviewTypesStatement(w where) overviewQuery {
	return overviewQuery{
		sql: "SELECT r.type, (r.ts IS NULL), count(*), min(r.ts), max(r.ts)" + w.fromSQL(false, false) + w.whereSQL() +
			" GROUP BY r.type, (r.ts IS NULL) ORDER BY r.type, (r.ts IS NULL)",
		args: slices.Clone(w.args),
	}
}

// OverviewAndTypes is Overview and StatsTop(f, "type", limit) answered from one snapshot, with the
// total, the untimed count, the ts range and the type groups computed from a single scan of the
// records instead of one scan each. Every number is recomputed from the rows, none is read from a
// stored count; the deleted and recovered counts and the runs are separate statements, as in Overview.
// The result equals Overview and StatsTop run apart on an unchanged database, field for field; the
// group cut is the same (the first limit types in key order, More when further types exist), and the
// overview still counts every type.
func (r *Reader) OverviewAndTypes(ctx context.Context, f Filter, limit int) (Overview, StatsResult, error) {
	if err := refuseRank(f); err != nil {
		return Overview{}, StatsResult{}, err
	}
	if limit < 1 || limit > MaxStatGroups {
		return Overview{}, StatsResult{}, fmt.Errorf("%w: group limit %d is outside 1..%d", ErrInvalidPage, limit, MaxStatGroups)
	}
	if _, err := f.compile(false); err != nil {
		return Overview{}, StatsResult{}, err
	}
	ov := Overview{Runs: map[string]int64{}}
	var out StatsResult
	err := r.readTx(ctx, f.Text != nil, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		w, err := f.compile(have)
		if err != nil {
			return err
		}
		if f.Text != nil {
			if err := r.matching(ctx); err != nil {
				return err
			}
		}
		if err := r.scanTypeGroups(ctx, h, w, limit, &ov, &out); err != nil {
			return err
		}
		qs := overviewStatements(w)
		for i, dst := range []*int64{&ov.Deleted, &ov.Recovered} {
			if err := h.QueryRowContext(ctx, qs[1+i].sql, qs[1+i].args...).Scan(dst); err != nil {
				return fmt.Errorf("records: overview: %w", err)
			}
		}
		return overviewRest(ctx, h, f, have, &ov)
	})
	if err != nil {
		return Overview{}, StatsResult{}, err
	}
	if err := r.overviewGaps(&ov); err != nil {
		return Overview{}, StatsResult{}, err
	}
	return ov, out, nil
}

// scanTypeGroups runs the single pass: it sums every group into ov (records, untimed, ts range) and
// folds the timed and untimed group of a type into one StatRow, keeping at most limit of them.
func (r *Reader) scanTypeGroups(ctx context.Context, h evidence.ReadHandle, w where, limit int, ov *Overview, out *StatsResult) error {
	q := overviewTypesStatement(w)
	rows, err := h.QueryContext(ctx, q.sql, q.args...)
	if err != nil {
		return fmt.Errorf("records: stats by type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	r.started()
	var cur StatRow
	have := false // cur holds a type
	flush := func() {
		if !have {
			return
		}
		if len(out.Rows) == limit {
			out.More = true // a further type exists; it is not returned
		} else {
			out.Rows = append(out.Rows, cur)
		}
		have = false
	}
	for rows.Next() {
		var k sql.NullString
		var untimed bool
		var n int64
		var lo, hi sql.NullInt64
		if err := rows.Scan(&k, &untimed, &n, &lo, &hi); err != nil {
			return fmt.Errorf("records: stats by type: %w", err)
		}
		if have && cur.Key != k.String {
			flush()
		}
		if !have {
			cur, have = StatRow{Key: k.String}, true
		}
		cur.Count += n
		ov.Records += n
		if untimed {
			ov.Untimed += n
			continue
		}
		if !lo.Valid || !hi.Valid { // a timed group always has a ts range
			return fmt.Errorf("records: stats by type: a timed group of %q has no ts range", k.String)
		}
		if cur.TSMin == nil || lo.Int64 < *cur.TSMin {
			cur.TSMin = &lo.Int64
		}
		if cur.TSMax == nil || hi.Int64 > *cur.TSMax {
			cur.TSMax = &hi.Int64
		}
		if ov.TSMin == nil || lo.Int64 < *ov.TSMin {
			v := lo.Int64
			ov.TSMin = &v
		}
		if ov.TSMax == nil || hi.Int64 > *ov.TSMax {
			v := hi.Int64
			ov.TSMax = &v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("records: stats by type: %w", err)
	}
	flush()
	return nil
}
