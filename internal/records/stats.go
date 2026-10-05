package records

import (
	"context"
	"database/sql"
	"fmt"

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

// Stats counts the records f selects by one key: type, parser ("name/version"),
// artifact (id), deleted ("live", "deleted" or "recovered") or run (ingest id),
// with each group's earliest and latest ts. Groups come in key order.
func (r *Reader) Stats(ctx context.Context, f Filter, by string) ([]StatRow, error) {
	if err := refuseRank(f); err != nil {
		return nil, err
	}
	key, ok := statKeys[by]
	if !ok {
		return nil, invalidFilter("cannot group by %q (type, parser, artifact, deleted or run)", by)
	}
	if _, err := f.compile(false); err != nil {
		return nil, err
	}
	var out []StatRow
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
			w.whereSQL() + " GROUP BY k ORDER BY k"
		if f.Text != nil {
			r.matching()
		}
		rows, err := h.QueryContext(ctx, q, w.args...)
		if err != nil {
			return fmt.Errorf("records: stats by %s: %w", by, err)
		}
		defer func() { _ = rows.Close() }()
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
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
		var lo, hi sql.NullInt64
		q := "SELECT count(*), COALESCE(sum(r.deleted), 0), COALESCE(sum(r.recovered), 0), COALESCE(sum(r.ts IS NULL), 0), min(r.ts), max(r.ts)" +
			w.fromSQL(false, false) + w.whereSQL()
		if f.Text != nil {
			r.matching()
		}
		if err := h.QueryRowContext(ctx, q, w.args...).Scan(&ov.Records, &ov.Deleted, &ov.Recovered, &ov.Untimed, &lo, &hi); err != nil {
			return fmt.Errorf("records: overview: %w", err)
		}
		if lo.Valid {
			ov.TSMin, ov.TSMax = &lo.Int64, &hi.Int64
		}
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
	})
	if err != nil {
		return Overview{}, err
	}
	return ov, nil
}
