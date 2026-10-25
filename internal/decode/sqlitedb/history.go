package sqlitedb

import (
	"context"
	"fmt"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// HistoryCounts says what one ClaimedHistory pass did.
type HistoryCounts struct {
	// Delivered counts the rows handed to the visitor.
	Delivered int64
	// Skipped counts the rows of the history that were not delivered, by the
	// sqlitefile.ClaimVerdict that excluded them. Read it in sorted key order, never by
	// map iteration order.
	Skipped map[string]int64
	// Stats is what the library's row pass reports (rows by method, duplicates of live rows, limits hit).
	Stats sqlitefile.RowStats
	// Warnings are the history's own warnings.
	Warnings []sqlitefile.Warning
}

// ClaimedHistory walks the recovered rows of the database's history and delivers
// to visit exactly those that sqlitefile.ClaimMatch accepts against the LIVE
// schema (a table row of a claimed table, basis schema or fit, strictly fitting
// the live definition). Every other row is counted by its verdict, so
// Delivered plus the sum of Skipped is the number of rows the history holds.
// claimed is told table names and compares them ASCII case-insensitively.
//
// The history is taken from the database, released before the call returns, and
// the context is polled for every row. A row is valid only during the visit (it
// is rebuilt with FromRecovered there); a visit error stops the pass and is
// returned. After Release the call is ErrReleased.
func (db *DB) ClaimedHistory(ctx context.Context, claimed func(table string) bool, visit func(sqlitefile.RecoveredRow) error) (hc HistoryCounts, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	hc.Skipped = map[string]int64{}
	if db.released {
		return hc, ErrReleased
	}
	if err := ctx.Err(); err != nil {
		return hc, err
	}
	h := db.lib.History()
	defer h.Release()
	var stop error
	st, rerr := h.Rows(ctx, func(rr sqlitefile.RecoveredRow) bool {
		if stop = ctx.Err(); stop != nil {
			return false
		}
		if v := db.schema.ClaimMatch(rr, claimed); v != sqlitefile.ClaimYes {
			hc.Skipped[string(v)]++
			return true
		}
		hc.Delivered++
		stop = visit(rr)
		return stop == nil
	})
	hc.Stats, hc.Warnings = st, h.Warnings()
	if stop != nil {
		return hc, stop
	}
	return hc, rerr
}
