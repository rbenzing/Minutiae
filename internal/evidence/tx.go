package evidence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// StoreTx runs fn in one transaction on the case database's single connection.
// It commits when fn returns nil and rolls back when fn returns an error or
// panics (the panic then continues). foreign_keys and recursive_triggers (which
// the immutability triggers depend on) are asserted on the connection before the
// transaction starts, because SQLite ignores that pragma inside one.
//
// There is one connection: never call StoreTx or ReadTx from inside fn (it
// would wait for itself forever) and never hold a *sql.Rows across them. It
// fails after the case is closed.
func (c *Case) StoreTx(ctx context.Context, fn func(*sql.Tx) error) error {
	conn, err := c.store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	defer func() { _ = conn.Close() }()
	for _, p := range []string{`PRAGMA foreign_keys = ON`, `PRAGMA recursive_triggers = ON`} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts.db begin: %w", err)
	}
	done := false
	defer func() {
		if !done { // fn panicked (the panic continues) or an early return
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		done = true
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("artifacts.db rollback: %w", rerr))
		}
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts.db commit: %w", err)
	}
	return nil
}

// ReadTx runs fn in a transaction on which every write is refused
// (PRAGMA query_only = ON for the duration, reset afterwards, also when fn
// panics). Nothing is ever committed. The same single-connection rules as
// StoreTx apply: no nesting, no *sql.Rows held across calls; it fails after the
// case is closed.
func (c *Case) ReadTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	conn, err := c.store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	defer func() {
		// The context may be cancelled; the pragma must be reset regardless,
		// or the next StoreTx on this connection could not write.
		if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA query_only = OFF`); rerr != nil {
			err = errors.Join(err, fmt.Errorf("artifacts.db reset query_only: %w", rerr))
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts.db begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}
