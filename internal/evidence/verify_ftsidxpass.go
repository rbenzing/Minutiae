package evidence

import (
	"errors"
)

// indexPass recomputes the _idx rows and the doclist indexes of one table from its leaf pages and
// compares them with the stored ones (fts5idx.go). It needs nothing of the rebuilt index. Every difference
// is a problem (kind fts-idx, 50 listed per table and one count line); the leaf pages that are not valid
// are fts-leaf problems. For the first 50 differences per table a search for the first term of the affected
// page, restricted to its first record, then says whether a hit is really lost (V13).
func (r *ftsVerify) indexPass(table string) error {
	var hits []ftsLostHit
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		_, err := checkFTSIndex(r.ctx, h, table, func(f ftsIdxFinding) {
			kind := "fts-idx"
			if f.Kind == ftsLeafBad {
				kind = "fts-leaf"
			}
			r.add(table, kind, "%s", f.text(table))
			if f.Confirm != nil && len(hits) < verifyMaxPerKind {
				hits = append(hits, *f.Confirm)
			}
		}, func(pages int) {
			if r.observe != nil {
				r.observe("fts-data", pages)
			}
		})
		if errors.Is(err, errFTSIdxStructure) {
			return nil // checkUnreferenced reports an unreadable structure record
		}
		return err
	})
	if err != nil || len(hits) == 0 {
		return err
	}
	return r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		for _, hit := range hits {
			lost, err := r.confirmLostHit(h, table, hit.Term, hit.Rowid)
			if err != nil {
				return err
			}
			if lost {
				r.add(table, "fts-unreachable", "%s: a search for %q does not find record %d (a search hit is lost: the index structure no longer reaches it)",
					table, ftsShowTerm(hit.Term), hit.Rowid)
			}
		}
		return nil
	})
}

// confirmLostHit asks the case index for the term (as stored, with the index prefix byte) restricted to
// one record: lost is true when no row comes back.
func (r *ftsVerify) confirmLostHit(h ReadHandle, table string, term []byte, rowid int64) (lost bool, err error) {
	rows, err := h.QueryContext(r.ctx, `SELECT 1 FROM `+table+` WHERE `+table+` MATCH ? AND rowid = ? LIMIT 1`, ftsQuote(ftsLostHitTerm(term)), rowid) //nolint:gosec // the table is one of the two FTS table constants
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return false, nil
	}
	return true, rows.Err()
}
