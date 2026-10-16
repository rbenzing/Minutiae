package evidence

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
)

// ErrNestedTx is returned by StoreTx and ReadTx when they are called from inside
// the fn of another StoreTx or ReadTx on the same goroutine: the outer
// transaction holds the case's single connection, so a nested call would wait
// for itself forever. Callers on other goroutines are not nesting: they queue
// and run in turn.
var ErrNestedTx = errors.New("artifacts.db transaction already open on this goroutine (StoreTx/ReadTx must not nest)")

// ErrReadTxModified is returned by ReadTx when fn changed the database anyway
// (for example by switching query_only off through a query). The transaction is
// rolled back and the connection discarded; a statement that commits by itself
// cannot be prevented, only detected.
var ErrReadTxModified = errors.New("artifacts.db was modified inside a read transaction")

// txGate serialises the case's transactions (one connection) and recognises a
// nested call. fn does not receive a context, so a context marker could not be
// seen by a nested call that uses its own ctx; the marker is the goroutine that
// holds the slot instead (one call chain runs on one goroutine). A fn that
// hands a transaction to another goroutine and waits for it is not detected:
// that call queues behind the transaction that waits for it.
type txGate struct {
	slot    chan struct{} // capacity 1: holds a token while a transaction is open
	holder  atomic.Int64  // goroutine id of the transaction in progress; 0 when none
	waiting atomic.Int64  // callers blocked in enterTx (tests wait for it instead of sleeping)
}

func newTxGate() *txGate { return &txGate{slot: make(chan struct{}, 1)} }

// goroutineID returns the id of the calling goroutine, read from the header of
// its stack trace ("goroutine N [running]:"); 0 if it cannot be parsed.
func goroutineID() int64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	end := bytes.IndexByte(b, ' ')
	if end < 0 {
		return 0
	}
	id, err := strconv.ParseInt(string(b[:end]), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// enterTx waits for the case's single transaction slot. A call made while the
// calling goroutine already holds the slot returns ErrNestedTx at once; any
// other caller queues until the slot is free or ctx ends.
func (s *Store) enterTx(ctx context.Context) error {
	gid := goroutineID()
	if gid == 0 {
		return errors.New("artifacts.db: cannot identify the calling goroutine")
	}
	if s.gate.holder.Load() == gid {
		return ErrNestedTx
	}
	s.gate.waiting.Add(1)
	defer s.gate.waiting.Add(-1)
	select {
	case s.gate.slot <- struct{}{}:
		s.gate.holder.Store(gid)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("artifacts.db: waiting for the transaction slot: %w", context.Cause(ctx))
	}
}

func (s *Store) leaveTx() {
	s.gate.holder.Store(0)
	<-s.gate.slot
}

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
// There is one connection: calling StoreTx or ReadTx from inside fn (on the same
// goroutine) returns ErrNestedTx; other goroutines queue until ctx ends. Never
// hold a *sql.Rows across them. It fails after the case is closed.
func (c *Case) StoreTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return c.StoreTxTuned(ctx, TxTuning{}, fn)
}

// TxTuning are per-transaction connection settings for bulk writes. The zero value changes nothing.
type TxTuning struct {
	// CacheKiB is the page cache size in KiB while the transaction runs (0 = the connection's own).
	// The previous value is put back when it ends, whatever its outcome.
	CacheKiB int
	// RelaxedSync runs this transaction with synchronous=NORMAL on a write-ahead log (D89, D90): its
	// commit is not fsynced, so a power loss can lose it and the transactions before it that no FULL
	// commit or checkpoint has synced since. NORMAL on a rollback journal could corrupt the database,
	// so the log is switched on first and the transaction is refused when that fails. synchronous goes
	// back to FULL when the transaction ends; the log stays until RestoreFullSync. Only the records
	// ingest batch path may set it, and only on the caller's explicit request.
	RelaxedSync bool
}

const maxTunedCacheKiB = 1 << 20 // 1 GiB

// LastStoreTuning returns the tuning of the last tuned transaction (diagnostic).
func (c *Case) LastStoreTuning() TxTuning { return TxTuning{CacheKiB: int(c.lastTuneKiB.Load())} }

// StoreTxTuned is StoreTx with per-transaction settings; see TxTuning. Like every StoreTx it sets
// temp_store to MEMORY (R67: a sorter never spills outside the case); a tuned transaction puts
// cache_size back when it ends, and a connection whose restore fails is discarded.
func (c *Case) StoreTxTuned(ctx context.Context, tune TxTuning, fn func(*sql.Tx) error) error {
	if tune.CacheKiB < 0 || tune.CacheKiB > maxTunedCacheKiB {
		return fmt.Errorf("artifacts.db: cache size %d KiB is outside 0..%d", tune.CacheKiB, maxTunedCacheKiB)
	}
	if err := c.store.enterTx(ctx); err != nil {
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
	for _, p := range []string{`PRAGMA foreign_keys = ON`, `PRAGMA recursive_triggers = ON`, `PRAGMA temp_store = MEMORY`} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
	}
	if tune.RelaxedSync {
		var mode string
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db journal_mode: %w", err)
		}
		if !strings.EqualFold(mode, "wal") { // NORMAL without the log risks corruption: refuse
			bad = true
			return fmt.Errorf("artifacts.db journal_mode is %q, want wal for a relaxed-sync transaction", mode)
		}
		if _, err := conn.ExecContext(ctx, `PRAGMA synchronous = NORMAL`); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
		defer func() { // runs before the connection is closed or discarded
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA synchronous = FULL`); err != nil {
				bad = true
			}
		}()
	}
	if tune.CacheKiB != 0 {
		var prev int
		if err := conn.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&prev); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
		// temp_store is already MEMORY (set above for every StoreTx, R67, ruling D23); only the cache
		// size is tuned and put back.
		if _, err := conn.ExecContext(ctx, `PRAGMA cache_size = `+strconv.Itoa(-tune.CacheKiB)); err != nil {
			bad = true
			return fmt.Errorf("artifacts.db pragma: %w", err)
		}
		c.lastTuneKiB.Store(int64(tune.CacheKiB))
		defer func() { // runs before the connection is closed or discarded (the earlier defer)
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA cache_size = `+strconv.Itoa(prev)); err != nil {
				bad = true
			}
		}()
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
// connection. Every query is checked before it runs: it must be one SELECT or
// WITH statement without PRAGMA, ATTACH, COMMIT, BEGIN and similar (see
// checkReadSQL); anything else returns ErrReadSQLRefused and runs nothing.
type ReadHandle struct{ tx *sql.Tx }

// Query runs a query and returns its rows (close them before fn returns).
func (h ReadHandle) Query(query string, args ...any) (*sql.Rows, error) {
	if err := checkReadSQL(query); err != nil {
		return nil, err
	}
	return h.tx.Query(query, args...)
}

// QueryContext is Query with a context.
func (h ReadHandle) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := checkReadSQL(query); err != nil {
		return nil, err
	}
	return h.tx.QueryContext(ctx, query, args...)
}

// Row is the result of QueryRow: Scan and Err report ErrReadSQLRefused itself
// for a refused statement (sql.Row cannot carry a custom error, so ReadHandle
// wraps it).
type Row struct {
	row *sql.Row
	err error
}

// Scan copies the row's columns into dest, as sql.Row.Scan does.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

// Err returns the error of the query, if any, without scanning.
func (r *Row) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.row.Err()
}

// QueryRow runs a query that returns at most one row. A refused statement runs
// nothing: Scan and Err return ErrReadSQLRefused.
func (h ReadHandle) QueryRow(query string, args ...any) *Row {
	if err := checkReadSQL(query); err != nil {
		return &Row{err: err}
	}
	return &Row{row: h.tx.QueryRow(query, args...)}
}

// QueryRowContext is QueryRow with a context.
func (h ReadHandle) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if err := checkReadSQL(query); err != nil {
		return &Row{err: err}
	}
	return &Row{row: h.tx.QueryRowContext(ctx, query, args...)}
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
// concurrent callers queue, no *sql.Rows held across calls; it fails after the
// case is closed.
func (c *Case) ReadTx(ctx context.Context, fn func(ReadHandle) error) (err error) {
	if err := c.store.enterTx(ctx); err != nil {
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
	// a sorter or temporary table of a read never spills to a file outside the case (R67); it is
	// set here, by the transaction, and put back by restoreAfterRead
	if _, err := conn.ExecContext(ctx, `PRAGMA temp_store = MEMORY`); err != nil {
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

// RestoreFullSync ends the relaxed mode: synchronous goes back to FULL, the write-ahead log is
// checkpointed into the database file (wal_checkpoint(TRUNCATE) fsyncs it, so every relaxed commit is
// durable when this returns) and the rollback journal is restored. It fails when the checkpoint is
// blocked or the journal mode does not come back; the case is then still in the relaxed mode and the
// caller must not treat what it wrote as durable. On a case that never ran a relaxed transaction it
// changes nothing.
func (c *Case) RestoreFullSync(ctx context.Context) error {
	if err := c.store.enterTx(ctx); err != nil {
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
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
		bad = true
		return fmt.Errorf("artifacts.db pragma: %w", err)
	}
	var busy, logFrames, done int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		bad = true
		return fmt.Errorf("artifacts.db checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("artifacts.db checkpoint: the write-ahead log could not be checkpointed (the database is busy)")
	}
	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
		bad = true
		return fmt.Errorf("artifacts.db journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "delete") {
		bad = true
		return fmt.Errorf("artifacts.db journal_mode is %q after the restore, want delete", mode)
	}
	return nil
}
