package sqlitedb

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Scan visits the live rows of the table in rowid order (the key order of a
// WITHOUT ROWID table), never the table's indexes. fn may return ErrStop to
// end the scan with a nil result; any other error ends it and is returned
// unchanged. A cancelled context is returned as is. The Row is valid only
// during the call. Scan after Release is ErrReleased.
func (t *Table) Scan(ctx context.Context, fn func(Row) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	db := t.db
	if db.released {
		return ErrReleased
	}
	db.stats.Scans++
	var ferr error
	i := 0
	libErr := t.lt.Rows(ctx, func(raw sqlitefile.Row) bool {
		var held int64
		charge := func(n int64) error {
			if err := db.budget.Alloc(n); err != nil {
				return err
			}
			held += n
			return nil
		}
		defer func() { db.budget.Free(held) }()
		row, err := t.buildRow(rowInput{
			rowid: raw.Rowid, hasRowid: raw.HasRowid, vals: t.lt.Resolve(raw), storedLen: len(raw.Values),
			flags: damageFlags(raw), loc: raw.Loc,
		}, charge)
		if err != nil {
			ferr = err
			return false
		}
		if err := parse.Tick(ctx, i); err != nil {
			ferr = err
			return false
		}
		i++
		db.stats.Rows++
		if row.flags != 0 {
			db.stats.RowsFlagged++
		}
		if err := fn(row); err != nil {
			if !errors.Is(err, ErrStop) {
				ferr = err
			}
			return false
		}
		return true
	})
	switch {
	case ferr != nil:
		return ferr
	case libErr != nil:
		return wrapErr(libErr)
	}
	return nil
}

// damageFlags are the flags for the damage the reader raised on a row.
func damageFlags(raw sqlitefile.Row) RowFlags {
	var f RowFlags
	if raw.LengthMismatch {
		f |= FlagLengthMismatch
	}
	if raw.KeyRangeViolation {
		f |= FlagKeyRange
	}
	return f
}
