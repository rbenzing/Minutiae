package records

import (
	"context"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// IndexStatus describes the full-text index of a case. Kind is current, unbuilt, building, stale or
// invalid (evidence.IndexKind), or unavailable for a case older than schema v3. Value is what the
// case stores, Current what this build writes; WordDocs and SubstringDocs are the numbers of documents
// each index holds.
type IndexStatus struct {
	Kind                    string
	Value, Current          string
	WordDocs, SubstringDocs int64
}

// IndexStatus reads the state of the index and the document count of both full-text tables in one
// read transaction.
func (r *Reader) IndexStatus(ctx context.Context) (IndexStatus, error) {
	var st IndexStatus
	err := r.c.ReadRecordsTx(ctx, func(h evidence.ReadHandle) error {
		var v int
		if err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
			return fmt.Errorf("artifacts.db schema_version: %w", err)
		}
		if v < 3 {
			st.Kind = "unavailable"
			return nil
		}
		s, err := evidence.ReadIndexState(ctx, h)
		if err != nil {
			return err
		}
		st = IndexStatus{Kind: string(s.Kind), Value: s.Value, Current: s.Current}
		if err := h.QueryRowContext(ctx, `SELECT count(*) FROM `+evidence.FTSWordTable+`_docsize`).Scan(&st.WordDocs); err != nil {
			return fmt.Errorf("records: index status: %w", err)
		}
		if err := h.QueryRowContext(ctx, `SELECT count(*) FROM `+evidence.FTSSubTable+`_docsize`).Scan(&st.SubstringDocs); err != nil {
			return fmt.Errorf("records: index status: %w", err)
		}
		return nil
	})
	if err != nil {
		return IndexStatus{}, r.integrity(err)
	}
	return st, nil
}
