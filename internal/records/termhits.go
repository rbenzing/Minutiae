package records

import (
	"context"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// MaxTermHitIDs is the most record ids TermHits returns per term.
const MaxTermHitIDs = 100000

// TermHit is the result of one term: the exact number of records that match it and, ascending, at
// most perTermLimit of their ids.
type TermHit struct {
	Term      Term
	Count     int64
	RecordIDs []int64
}

// TermHits looks up keyword terms. Each term is literal text (never grammar): one phrase on the word
// index or, with Substring, one substring on the trigram index, folded exactly as Search folds. The
// filter f restricts the records counted. All terms are answered in one read transaction (one
// snapshot). perTermLimit 0 returns counts only; above MaxTermHitIDs it is ErrInvalidPage. It
// requires a current full-text index, like Search, and a deadline that passes is ErrSearchTimeout.
func (r *Reader) TermHits(ctx context.Context, f Filter, terms []Term, perTermLimit int) ([]TermHit, error) {
	if perTermLimit < 0 || perTermLimit > MaxTermHitIDs {
		return nil, fmt.Errorf("%w: perTermLimit %d is outside 0..%d", ErrInvalidPage, perTermLimit, MaxTermHitIDs)
	}
	if _, err := f.compile(false); err != nil {
		return nil, err
	}
	var qs []*TextQuery
	if len(terms) > 0 {
		var err error
		if qs, err = CompileTerms(terms); err != nil {
			return nil, err
		}
	}
	out := make([]TermHit, len(terms))
	err := r.readTx(ctx, true, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		base, err := f.compile(have)
		if err != nil {
			return err
		}
		for i, q := range qs {
			cond, err := q.matchCond()
			if err != nil {
				return err
			}
			w := base
			w.conds = append(append([]string(nil), base.conds...), cond)
			w.args = append(append([]any(nil), base.args...), q.match)
			out[i].Term = terms[i]
			r.matching()
			countSQL := "SELECT count(*)" + w.fromSQL(false, false) + w.whereSQL()
			if err := h.QueryRowContext(ctx, countSQL, w.args...).Scan(&out[i].Count); err != nil {
				return fmt.Errorf("records: term hits: %w", err)
			}
			if perTermLimit == 0 || out[i].Count == 0 {
				continue
			}
			r.matching()
			idSQL := "SELECT r.id" + w.fromSQL(false, false) + w.whereSQL() + " ORDER BY r.id LIMIT ?"
			rows, err := h.QueryContext(ctx, idSQL, append(append([]any(nil), w.args...), perTermLimit)...)
			if err != nil {
				return fmt.Errorf("records: term hits: %w", err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return fmt.Errorf("records: term hits: %w", err)
				}
				out[i].RecordIDs = append(out[i].RecordIDs, id)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return fmt.Errorf("records: term hits: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapTimeout(err)
	}
	return out, nil
}
