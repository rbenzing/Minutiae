package evidence

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strconv"
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
		_ = rawExec(h, `PRAGMA query_only = OFF`)
		_ = rawExec(h, `UPDATE records_meta SET value = '777' WHERE key = 'next_id'`)
		_ = rawExec(h, `ATTACH DATABASE ':memory:' AS evil`)
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

// TestConcurrentTxQueueAndSucceed: callers on different goroutines are not
// nesting; they queue on the single connection and every one succeeds.
func TestConcurrentTxQueueAndSucceed(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	const n = 16
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- c.StoreTx(ctx, func(tx *sql.Tx) error {
				var v int
				if err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM records_meta WHERE key = 'next_id'`).Scan(&v); err != nil {
					return err
				}
				time.Sleep(time.Millisecond) // keep the slot long enough for the others to queue
				return setNextID(tx, strconv.Itoa(v+1))
			})
		}()
		go func() {
			errs <- c.ReadTx(ctx, func(h ReadHandle) error {
				var v string
				return h.QueryRow(`SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&v)
			})
		}()
	}
	for i := 0; i < 2*n; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent transaction %d: %v", i, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("concurrent transactions did not finish")
		}
	}
	if got, want := nextID(t, c), strconv.Itoa(1+n); got != want {
		t.Errorf("next_id = %s, want %s (every StoreTx ran exactly once)", got, want)
	}
}

// TestNestedTxFailsWhileOthersAreQueued: a nested call is refused at once even
// when other goroutines are waiting for the slot, and the queued callers
// still succeed afterwards.
func TestNestedTxFailsWhileOthersAreQueued(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	queued := make(chan error, 3)
	nested := make(chan error, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		nested <- c.StoreTx(ctx, func(*sql.Tx) error {
			close(started)
			<-release
			return c.ReadTx(ctx, func(ReadHandle) error { return nil })
		})
	}()
	<-started
	for i := 0; i < 3; i++ {
		go func() { queued <- c.StoreTx(ctx, func(*sql.Tx) error { return nil }) }()
	}
	// wait until all three are really queued (no sleeping and hoping)
	deadline := time.Now().Add(20 * time.Second)
	for c.store.gate.waiting.Load() != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 3 callers queued", c.store.gate.waiting.Load())
		}
		runtime.Gosched()
	}
	close(release)
	select {
	case err := <-nested:
		if !errors.Is(err, ErrNestedTx) {
			t.Errorf("nested err = %v, want ErrNestedTx", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("nested transaction deadlocked behind queued callers")
	}
	for i := 0; i < 3; i++ {
		select {
		case err := <-queued:
			if err != nil {
				t.Errorf("queued caller: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("queued caller never ran")
		}
	}
}

// TestQueuedTxHonoursContext: a caller waiting for the slot gives up when its
// context ends, without disturbing the holder.
func TestQueuedTxHonoursContext(t *testing.T) {
	c := newTestCase(t)
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- c.StoreTx(context.Background(), func(*sql.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.StoreTx(ctx, func(*sql.Tx) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting caller err = %v, want context.DeadlineExceeded", err)
	}
	close(release)
	if err := <-holder; err != nil {
		t.Errorf("holder: %v", err)
	}
}
