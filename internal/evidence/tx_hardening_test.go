package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestReadTxEscapesCannotPersist: whatever fn does inside ReadTx through the
// handle (switching query_only off, writing, attaching a second database),
// nothing persists, no attached database remains, the escape is reported, and
// the store is fully usable afterwards.
func TestReadTxEscapesCannotPersist(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	before := nextID(t, c)
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		_ = queryExec(h, `PRAGMA query_only = OFF`)
		_ = queryExec(h, `UPDATE records_meta SET value = '777' WHERE key = 'next_id'`)
		_ = queryExec(h, `ATTACH DATABASE ':memory:' AS evil`)
		return nil
	})
	if !errors.Is(err, ErrReadTxModified) {
		t.Errorf("ReadTx err = %v, want ErrReadTxModified", err)
	}
	if got := nextID(t, c); got != before {
		t.Errorf("next_id = %s after ReadTx, want %s", got, before)
	}
	rows, qerr := c.store.db.Query(`PRAGMA database_list`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if n != 1 {
		t.Errorf("%d databases attached after ReadTx, want only main", n)
	}
	if v := queryOnly(t, c); v != 0 {
		t.Errorf("query_only = %d after ReadTx", v)
	}
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "3") }); err != nil {
		t.Fatalf("StoreTx after the escape: %v", err)
	}
}

// TestReadTxHandleHasNoWriteDoor pins the shape of the handle: it is not a
// *sql.Tx and offers no Exec, Commit or Rollback.
func TestReadTxHandleHasNoWriteDoor(t *testing.T) {
	var h any = ReadHandle{}
	if _, ok := h.(interface {
		Exec(string, ...any) (sql.Result, error)
	}); ok {
		t.Error("ReadHandle has Exec")
	}
	if _, ok := h.(interface{ Commit() error }); ok {
		t.Error("ReadHandle has Commit")
	}
	if _, ok := h.(interface{ Rollback() error }); ok {
		t.Error("ReadHandle has Rollback")
	}
}

// TestNestedTxIsRefusedNotDeadlocked: the single connection is held by the outer
// transaction; a nested StoreTx or ReadTx must return ErrNestedTx at once.
func TestNestedTxIsRefusedNotDeadlocked(t *testing.T) {
	c := newTestCase(t)
	done := make(chan error, 4)
	go func() {
		ctx := context.Background() // no deadline: a deadlock would hang
		var errs [4]error
		errs[0] = c.StoreTx(ctx, func(*sql.Tx) error {
			return c.StoreTx(ctx, func(*sql.Tx) error { return nil })
		})
		errs[1] = c.StoreTx(ctx, func(*sql.Tx) error {
			return c.ReadTx(ctx, func(ReadHandle) error { return nil })
		})
		errs[2] = c.ReadTx(ctx, func(ReadHandle) error {
			return c.StoreTx(ctx, func(*sql.Tx) error { return nil })
		})
		errs[3] = c.ReadTx(ctx, func(ReadHandle) error {
			return c.ReadTx(ctx, func(ReadHandle) error { return nil })
		})
		for _, e := range errs {
			done <- e
		}
	}()
	for i := 0; i < 4; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, ErrNestedTx) {
				t.Errorf("nested case %d: err = %v, want ErrNestedTx", i, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("nested transaction deadlocked")
		}
	}
	// the store is usable afterwards
	if err := c.StoreTx(context.Background(), func(tx *sql.Tx) error { return setNextID(tx, "5") }); err != nil {
		t.Fatalf("StoreTx after a refused nesting: %v", err)
	}
}

// TestCommitFailureLeavesConnectionUsable: a deferred foreign key violation makes
// Commit fail while SQLite still holds the transaction open; the connection must
// not go back to the pool in that state.
func TestCommitFailureLeavesConnectionUsable(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	err := c.StoreTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO record_times (record_id, kind, ts, ts_basis) VALUES (999, 'mtime', 1, 'utc')`)
		return err
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("err = %v, want a commit-time foreign key failure", err)
	}
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "9") }); err != nil {
		t.Fatalf("StoreTx after a failed commit: %v", err)
	}
	if got := nextID(t, c); got != "9" {
		t.Fatalf("next_id = %s, want 9", got)
	}
	var n int
	if err := c.store.db.QueryRow(`SELECT count(*) FROM record_times`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("record_times rows = %d, %v", n, err)
	}
}

// TestStoreTxReassertsPragmasInsideTheTransaction: the pragmas the immutability
// triggers and foreign keys depend on hold inside fn even when something
// switched them off on the pooled connection before.
func TestStoreTxReassertsPragmasInsideTheTransaction(t *testing.T) {
	c := newTestCase(t)
	for _, p := range []string{`PRAGMA recursive_triggers = OFF`, `PRAGMA foreign_keys = OFF`} {
		if _, err := c.store.db.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		for _, name := range []string{"recursive_triggers", "foreign_keys"} {
			var v int
			if err := tx.QueryRow(`PRAGMA ` + name).Scan(&v); err != nil || v != 1 {
				t.Errorf("PRAGMA %s inside StoreTx = %d, %v, want 1", name, v, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
