package evidence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

// ErrNestedTx is returned by StoreTx and ReadTx when a transaction is already
// open on the case's single connection (a nested call would wait for itself
// forever). Transactions of one case are not meant to run concurrently either:
// a second caller gets this error instead of waiting.
var ErrNestedTx = errors.New("artifacts.db transaction already open (StoreTx/ReadTx must not nest)")

// ErrReadTxModified is returned by ReadTx when fn changed the database anyway
// (for example by switching query_only off through a query). The transaction is
// rolled back and the connection discarded; a statement that commits by itself
// cannot be prevented, only detected.
var ErrReadTxModified = errors.New("artifacts.db was modified inside a read transaction")

// enterTx claims the case's single transaction slot.
func (s *Store) enterTx() error {
	if !s.txBusy.CompareAndSwap(false, true) {
		return ErrNestedTx
	}
	return nil
}

func (s *Store) leaveTx() { s.txBusy.Store(false) }

// discardConn makes database/sql close conn instead of pooling it, so the next
// connection is opened fresh (and configured by dbConnector).
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// StoreTx runs fn in one transaction on the case's single connection.
// It commits when fn returns nil and rolls back when fn returns an error or
// panics (the panic then continues). foreign_keys and recursive_triggers (which
// the immutability triggers depend on) are asserted on the connection before the
// transaction starts, because SQLite ignores that pragma inside one. A
// connection that fails to roll back, to commit or to be configured is
// discarded, never returned to the pool in an unknown state.
//
// There is one connection: calling StoreTx or ReadTx from inside fn returns
// ErrNestedTx, and never hold a *sql.Rows across them. It fails after the case
// is closed.
func (c *Case) StoreTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := c.store.enterTx(); err != nil {
		return err
	}
	defer c.store.leaveTx()
	conn, err := c.store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	bad := false
	defer func() {
		if bad {
			discardConn(conn)
		}
		_ = conn.Close()
	}()
	for _, p := range []string{`PRAGMA foreign_keys = ON`, `PRAGMA recursive_triggers = ON`} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		bad = true
		return fmt.Errorf("artifacts.db begin: %w", err)
	}
	done := false
	defer func() {
		if !done { // fn panicked (the panic continues)
			if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
				bad = true
			}
		}
	}()
	if err := fn(tx); err != nil {
		done = true
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			bad = true
			return errors.Join(err, fmt.Errorf("artifacts.db rollback: %w", rerr))
		}
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		// SQLite may still hold the transaction open (a failed commit does not
		// always end it): never reuse this connection.
		bad = true
		return fmt.Errorf("artifacts.db commit: %w", err)
	}
	return nil
}

// ReadHandle is what fn sees inside ReadTx: query methods only. It has no Exec,
// no Commit or Rollback and no access to the underlying transaction or
// connection.
type ReadHandle struct{ tx *sql.Tx }

// Query runs a query and returns its rows (close them before fn returns).
func (h ReadHandle) Query(query string, args ...any) (*sql.Rows, error) {
	return h.tx.Query(query, args...)
}

// QueryContext is Query with a context.
func (h ReadHandle) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return h.tx.QueryContext(ctx, query, args...)
}

// QueryRow runs a query that returns at most one row.
func (h ReadHandle) QueryRow(query string, args ...any) *sql.Row {
	return h.tx.QueryRow(query, args...)
}

// QueryRowContext is QueryRow with a context.
func (h ReadHandle) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return h.tx.QueryRowContext(ctx, query, args...)
}

// ReadTx runs fn inside one BEGIN on the case's connection, with
// PRAGMA query_only = ON, and ALWAYS rolls back at the end: nothing fn does
// through the handle is committed. fn gets query methods only, so it can neither
// commit nor roll back. Afterwards the connection is put back in its normal
// state (query_only off, the store's pragmas re-asserted) and checked: if fn
// changed the database anyway (total_changes moved: a query that switched
// query_only off and wrote) or left a database attached, ReadTx returns
// ErrReadTxModified and discards the connection. A statement that commits by
// itself cannot be prevented, only detected this way.
//
// The same single-connection rules as StoreTx apply: no nesting (ErrNestedTx),
// no *sql.Rows held across calls; it fails after the case is closed.
func (c *Case) ReadTx(ctx context.Context, fn func(ReadHandle) error) (err error) {
	if err := c.store.enterTx(); err != nil {
		return err
	}
	defer c.store.leaveTx()
	conn, err := c.store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	// Everything below may leave the connection in an unknown state, so the
	// connection is discarded unless the final restore succeeds.
	bad := true
	defer func() {
		if bad {
			discardConn(conn)
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	var changesBefore int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changesBefore); err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts.db begin: %w", err)
	}
	defer func() { // also runs when fn panics (the panic continues)
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("artifacts.db rollback: %w", rerr))
			return
		}
		// The context may be cancelled; the connection must be restored regardless.
		if rerr := restoreAfterRead(context.WithoutCancel(ctx), conn, changesBefore); rerr != nil {
			err = errors.Join(err, rerr)
			return
		}
		bad = false
	}()
	return fn(ReadHandle{tx: tx})
}

// restoreAfterRead puts the connection back in its normal state after ReadTx and
// verifies fn left no trace.
func restoreAfterRead(ctx context.Context, conn *sql.Conn, changesBefore int64) error {
	var changes int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changes); err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	var problem error
	if changes != changesBefore {
		problem = ErrReadTxModified
	}
	for _, p := range append([]string{`PRAGMA query_only = OFF`}, connPragmas...) {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return errors.Join(problem, fmt.Errorf("artifacts.db reset pragma: %w", err))
		}
	}
	var attached int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pragma_database_list WHERE name NOT IN ('main', 'temp')`).Scan(&attached); err != nil {
		return errors.Join(problem, fmt.Errorf("artifacts.db: %w", err))
	}
	if attached != 0 {
		problem = errors.Join(problem, fmt.Errorf("%w: %d database(s) left attached", ErrReadTxModified, attached))
	}
	return problem
}
