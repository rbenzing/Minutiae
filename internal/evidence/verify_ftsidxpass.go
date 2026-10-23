package evidence

// The glue between the index pass (verify_ftsindex.go) and the recomputation of fts5idx.go (5D Task 14j).

// idxFinding reports one difference of the recomputation: kind fts-idx, 50 listed per table and one count line;
// the leaf pages that are not valid are reported by the pass itself (fts-leaf). The first 50 differences per
// table that name a page's first term are kept for the confirmation by search (V13).
func (r *ftsVerify) idxFinding(table string, f ftsIdxFinding) {
	r.add(table, "fts-idx", "%s", f.text(table))
	if f.Confirm != nil && len(r.idxHits) < verifyMaxPerKind {
		r.idxHits = append(r.idxHits, *f.Confirm)
	}
}

// confirmIdxHits asks the case index, for each kept difference, for the first term of the affected page
// restricted to its first record: when no row comes back, a hit is really lost.
func (r *ftsVerify) confirmIdxHits(table string) error {
	hits := r.idxHits
	r.idxHits = nil
	if len(hits) == 0 {
		return nil
	}
	return r.readTx(func(h ReadHandle) error {
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
