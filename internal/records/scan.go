package records

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
)

const (
	// scanBatchRows is the number of records one batch of a scan reads before it tests them.
	scanBatchRows = 256
	// maxScanBodyBytes is the largest body a scan reads: what ingest accepts. A longer one cannot be in
	// a genuine case and is an integrity error, never cut.
	maxScanBodyBytes = maxBody
)

// searchScan answers a scan query (L85): it reads the records the filter selects in List order, a batch
// at a time, and keeps those whose summary or body, folded exactly as the trigram index folds it,
// holds the needle's key. It reads no index. The page is a keyset page of List order with its own
// cursor; a deadline is ErrSearchTimeout (the read is linear in the selected records).
func (r *Reader) searchScan(ctx context.Context, f Filter, p Page, limit int, so SearchOptions) (SearchResult, error) {
	g := f
	g.Text = nil
	if _, err := g.compile(false); err != nil {
		return SearchResult{}, err
	}
	fp := f.fingerprint(r.caseIdentity(), p.Desc)
	cur, err := p.position(fp)
	if err != nil {
		return SearchResult{}, err
	}
	q := f.Text
	var out SearchResult
	err = r.readTxMode(ctx, txMode{deadline: true, requireIndex: false}, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		var found []Row
		for len(found) <= limit {
			batch, err := r.scanBatch(ctx, h, g, p.Desc, cur, have, scanBatchRows)
			if err != nil {
				return err
			}
			for _, row := range batch {
				if err := ctxErr(ctx); err != nil {
					return err
				}
				out.Scanned++
				ok, err := scanMatches(ctx, h, row, q)
				if err != nil {
					return err
				}
				if ok {
					found = append(found, row)
					if len(found) > limit {
						break
					}
				}
				next, err := cursorAfter(row, p.Desc, fp)
				if err != nil {
					return err
				}
				cur = &next
			}
			if len(batch) < scanBatchRows {
				break
			}
		}
		if len(found) > limit {
			found = found[:limit]
			next, err := cursorAfter(found[limit-1], p.Desc, fp)
			if err != nil {
				return err
			}
			out.NextCursor = next.encode()
		}
		out.Hits, err = r.hits(ctx, h, found, q, so)
		return err
	})
	if err != nil {
		return SearchResult{}, err
	}
	return out, nil
}

// scanBatch returns the next n records of the listing f (which carries no text query) after cur, in
// List order, exactly as one page of List reads them.
func (r *Reader) scanBatch(ctx context.Context, h evidence.ReadHandle, f Filter, desc bool, cur *cursor, have bool, n int) ([]Row, error) {
	parts := f.split()
	if parts == nil {
		parts = []Filter{f}
	}
	lists := make([][]Row, len(parts))
	for k, part := range parts {
		qs, err := buildList(part, desc, cur, have)
		if err != nil {
			return nil, err
		}
		for _, q := range qs { // one query (one open Rows) at a time
			want := n - len(lists[k])
			if want <= 0 {
				break
			}
			q.args[len(q.args)-1] = want
			if err := ctxErr(ctx); err != nil {
				return nil, err
			}
			if err := r.collect(ctx, h, q, &lists[k]); err != nil {
				return nil, err
			}
		}
	}
	return mergeRows(lists, desc, n), nil
}

// scanKeyContains reports whether the stored text s, normalized and folded as the trigram index sees
// it, holds the key.
func scanKeyContains(s, key string) bool {
	return strings.Contains(evidence.FTSTrigramFold(evidence.NormalizeText(s)), key)
}

// scanMatches tests the columns the query is limited to; the columns are tested separately.
func scanMatches(ctx context.Context, h evidence.ReadHandle, row Row, q *TextQuery) (bool, error) {
	var size sql.NullInt64
	var body []byte
	if err := h.QueryRowContext(ctx, `SELECT length(CAST(body AS BLOB)), substr(CAST(body AS BLOB), 1, ?) FROM records WHERE id = ?`,
		maxScanBodyBytes, row.ID).Scan(&size, &body); err != nil {
		return false, fmt.Errorf("records: scan of record %d: %w", row.ID, err)
	}
	if size.Int64 > maxScanBodyBytes {
		return false, fmt.Errorf("%w: the body of record %d is %d bytes, above the %d ingest accepts", evidence.ErrIntegrity, row.ID, size.Int64, maxScanBodyBytes)
	}
	if q.column != ColBody && scanKeyContains(row.Summary, q.match) {
		return true, nil
	}
	if q.column == ColSummary {
		return false, nil
	}
	return scanKeyContains(string(body), q.match), nil
}
