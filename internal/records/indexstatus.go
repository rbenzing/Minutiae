package records

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

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
		if st.WordDocs, err = docTotal(ctx, h, evidence.FTSWordTable); err != nil {
			return fmt.Errorf("records: index status: %w", err)
		}
		if st.SubstringDocs, err = docTotal(ctx, h, evidence.FTSSubTable); err != nil {
			return fmt.Errorf("records: index status: %w", err)
		}
		return nil
	})
	if err != nil {
		return IndexStatus{}, r.integrity(err)
	}
	return st, nil
}

// docTotal is the number of documents an FTS5 table holds, from its averages record (data row 1,
// whose first SQLite varint is the total row count; P11 proves it equals a rebuild's). A missing
// record, or an empty one (what FTS5 leaves after delete-all), is zero documents only when the table has no document-size rows; an undecodable one is an
// integrity error, never a zero. The table is one of the two FTS table constants.
func docTotal(ctx context.Context, h evidence.ReadHandle, table string) (int64, error) {
	var block []byte
	err := h.QueryRowContext(ctx, `SELECT block FROM `+table+`_data WHERE id = 1`).Scan(&block)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(block) == 0) {
		var hasDocs int
		if err := h.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+`_docsize)`).Scan(&hasDocs); err != nil {
			return 0, err
		}
		if hasDocs != 0 {
			return 0, fmt.Errorf("%w: %s holds documents but no totals record", evidence.ErrIntegrity, table)
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, n := evidence.SQLiteVarint(block)
	if n == 0 || v > math.MaxInt64 {
		return 0, fmt.Errorf("%w: the totals record of %s is unreadable", evidence.ErrIntegrity, table)
	}
	return int64(v), nil
}
