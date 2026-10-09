package sqlitedb

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// charger charges the budget for what one build holds and gives it back in
// one call. It is the single copy of the per-row charge closure.
type charger struct {
	db   *DB
	held int64
}

// charge asks the budget for n bytes before they are allocated.
func (c *charger) charge(n int64) error {
	if err := c.db.budget.Alloc(n); err != nil {
		return err
	}
	c.held += n
	return nil
}

// release gives back everything charged so far. It is idempotent.
func (c *charger) release() {
	c.db.budget.Free(c.held)
	c.held = 0
}

// Scan visits the live rows of the table in rowid order (the key order of a
// WITHOUT ROWID table), never the table's indexes. fn may return ErrStop to
// end the scan with a nil result; the test is errors.Is, so a callback may wrap
// ErrStop (fmt.Errorf("done: %w", ErrStop)) and still stop cleanly. Any other
// error ends the scan and is returned unchanged. A cancelled context is returned
// as is. The Row is valid only during the call. Scan after Release is
// ErrReleased.
//
// A row is charged to the budget before the columns are resolved for it, so the
// resolved values (at most the column cap of them) are never held uncharged.
func (t *Table) Scan(ctx context.Context, fn func(Row) error) error {
	_, err := t.scan(ctx, fn)
	return err
}

// scan is Scan that also returns what the reader did not deliver in this one
// scan (pages and cells it skipped): a caller that must not mistake a short
// scan for a complete one (Index) reads it.
func (t *Table) scan(ctx context.Context, fn func(Row) error) (loss sqlitefile.ScanLoss, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	db := t.db
	if db.released {
		return sqlitefile.ScanLoss{}, ErrReleased
	}
	db.stats.Scans++
	var ferr error
	i := 0
	loss, libErr := t.lt.RowsLoss(ctx, func(raw sqlitefile.Row) bool {
		ch := &charger{db: db}
		defer ch.release()
		row, err := t.buildRow(rowInput{
			raw: raw, storedLen: len(raw.Values), flags: damageFlags(raw), loc: raw.Loc,
		}, ch.charge)
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
		return loss, ferr
	case libErr != nil:
		return loss, wrapErr(libErr)
	}
	return loss, nil
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

// Get finds the row with the given rowid. found is false only when the search
// path was read cleanly and holds no such rowid; a damaged path is an error,
// never an absent answer. A WITHOUT ROWID table has no rowids (ErrWithoutRowid).
// The Row is owned: it is a deep copy (see Row.Clone) that stays valid after
// later calls, including a Get made inside a Scan callback, and stays charged
// to the budget until Row.Release or DB.Release (the reader's page cache stays
// charged until DB.Release).
func (t *Table) Get(ctx context.Context, rowid int64) (Row, bool, error) {
	return t.get(ctx, rowid, nil)
}

// get is Get with a test seam: after is called once the library has returned.
func (t *Table) get(ctx context.Context, rowid int64, after func()) (row Row, found bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			row, found, err = Row{}, false, fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	db := t.db
	if db.released {
		return Row{}, false, ErrReleased
	}
	if t.def.WithoutRowid {
		return Row{}, false, fmt.Errorf("%w: %q", ErrWithoutRowid, t.name)
	}
	raw, ok, libErr := t.lt.Get(ctx, rowid)
	if after != nil {
		after()
	}
	switch {
	case libErr != nil:
		return Row{}, false, wrapErr(libErr)
	case !ok:
		return Row{}, false, nil
	}
	ch := &charger{db: db}
	defer ch.release()
	built, err := t.buildRow(rowInput{
		raw: raw, storedLen: len(raw.Values), flags: damageFlags(raw), loc: raw.Loc,
	}, ch.charge)
	if err != nil {
		return Row{}, false, err
	}
	owned, err := built.Clone()
	if err != nil {
		return Row{}, false, err
	}
	return owned, true, nil
}
