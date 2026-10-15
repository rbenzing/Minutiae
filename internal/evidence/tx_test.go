package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func nextID(t *testing.T, c *Case) string {
	t.Helper()
	var v string
	if err := c.store.db.QueryRow(`SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func setNextID(tx *sql.Tx, v string) error {
	_, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, v)
	return err
}

func TestStoreTxCommitsAndRollsBack(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	if got := nextID(t, c); got != "1" {
		t.Fatalf("next_id = %s", got)
	}

	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "10") }); err != nil {
		t.Fatal(err)
	}
	if got := nextID(t, c); got != "10" {
		t.Fatalf("after commit next_id = %s, want 10", got)
	}

	boom := errors.New("boom")
	err := c.StoreTx(ctx, func(tx *sql.Tx) error {
		if err := setNextID(tx, "20"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got := nextID(t, c); got != "10" {
		t.Fatalf("after an error next_id = %s, want 10 (rolled back)", got)
	}

	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Errorf("recover() = %v, want the panic to continue", r)
			}
		}()
		_ = c.StoreTx(ctx, func(tx *sql.Tx) error {
			if err := setNextID(tx, "30"); err != nil {
				return err
			}
			panic("kaboom")
		})
	}()
	if got := nextID(t, c); got != "10" {
		t.Fatalf("after a panic next_id = %s, want 10 (rolled back)", got)
	}

	// the single connection is free again
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "40") }); err != nil {
		t.Fatalf("StoreTx after rollbacks: %v", err)
	}
	if got := nextID(t, c); got != "40" {
		t.Fatalf("next_id = %s, want 40", got)
	}
}

func TestStoreTxEnforcesForeignKeys(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	insert := func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO record_times (record_id, kind, ts, ts_basis) VALUES (999, 'mtime', 1, 'utc')`)
		return err
	}
	if err := c.StoreTx(ctx, insert); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("err = %v, want a foreign key failure", err)
	}
	// even when something switched the pragma off on the connection
	if _, err := c.store.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if err := c.StoreTx(ctx, insert); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("foreign_keys was not re-asserted: err = %v", err)
	}
	var n int
	if err := c.store.db.QueryRow(`SELECT count(*) FROM record_times`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("record_times rows = %d, %v", n, err)
	}
}

func queryOnly(t *testing.T, c *Case) int {
	t.Helper()
	var v int
	if err := c.store.db.QueryRow(`PRAGMA query_only`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func queryOnlyIn(t *testing.T, tx ReadHandle) int {
	t.Helper()
	var v int
	if err := tx.QueryRow(`SELECT query_only FROM pragma_query_only`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReaderCannotWrite(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()

	// after: query_only is off again and a write through StoreTx succeeds
	after := func(t *testing.T) {
		t.Helper()
		if v := queryOnly(t, c); v != 0 {
			t.Fatalf("query_only = %d after ReadTx", v)
		}
		if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "77") }); err != nil {
			t.Fatalf("StoreTx after ReadTx: %v", err)
		}
		if got := nextID(t, c); got != "77" {
			t.Fatalf("next_id = %s", got)
		}
		if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return setNextID(tx, "1") }); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("writes are refused", func(t *testing.T) {
		err := c.ReadTx(ctx, func(tx ReadHandle) error {
			if got := queryOnlyIn(t, tx); got != 1 {
				t.Errorf("query_only inside ReadTx = %d", got)
			}
			for _, stmt := range []string{
				`INSERT INTO records_meta (key, value) VALUES ('x', 'y')`,
				`UPDATE records_meta SET value = '9' WHERE key = 'next_id'`,
				`DELETE FROM records_meta WHERE key = 'next_id'`,
			} {
				if err := rawExec(tx, stmt); err == nil {
					t.Errorf("%s succeeded inside ReadTx", stmt)
				}
			}
			var v string
			if err := tx.QueryRow(`SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&v); err != nil || v != "1" {
				t.Errorf("read inside ReadTx = %q, %v", v, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		after(t)
	})

	t.Run("fn error is returned", func(t *testing.T) {
		boom := errors.New("boom")
		if err := c.ReadTx(ctx, func(ReadHandle) error { return boom }); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
		after(t)
	})

	t.Run("panicking fn", func(t *testing.T) {
		func() {
			defer func() {
				if r := recover(); r != "kaboom" {
					t.Errorf("recover() = %v", r)
				}
			}()
			_ = c.ReadTx(ctx, func(ReadHandle) error { panic("kaboom") })
		}()
		after(t)
	})

	t.Run("cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		_ = c.ReadTx(cctx, func(ReadHandle) error {
			cancel()
			return cctx.Err()
		})
		after(t)
	})
}

func TestTxRefusedAfterClose(t *testing.T) {
	c := newTestCase(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	fn := func(*sql.Tx) error { called = true; return nil }
	rfn := func(ReadHandle) error { called = true; return nil }
	if err := c.StoreTx(context.Background(), fn); err == nil {
		t.Error("StoreTx succeeded after Close")
	}
	if err := c.ReadTx(context.Background(), rfn); err == nil {
		t.Error("ReadTx succeeded after Close")
	}
	if called {
		t.Error("fn ran after Close")
	}
}

// TestReadTxUsesMemoryForTemporaryStorage (R67): inside ReadTx a sorter or temporary table never
// spills to a file (temp_store is MEMORY, 2), and a write transaction afterwards is MEMORY too
// (ruling D47: no SQLite temp file may hold case text outside the case; the file-level proof is
// TestIngestAndReindexLeaveNoSQLiteTempFileOutsideTheCase in internal/records).
func TestReadTxUsesMemoryForTemporaryStorage(t *testing.T) {
	c := newTestCase(t)
	read := func() int {
		var v int
		err := c.ReadTx(context.Background(), func(h ReadHandle) error {
			return h.QueryRowContext(context.Background(), `SELECT temp_store FROM pragma_temp_store`).Scan(&v)
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := read(); v != 2 {
		t.Fatalf("temp_store inside ReadTx = %d, want 2 (MEMORY)", v)
	}
	var after int
	if err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`PRAGMA temp_store`).Scan(&after)
	}); err != nil {
		t.Fatal(err)
	}
	if after != 2 {
		t.Errorf("temp_store in a write transaction = %d, want 2 (MEMORY)", after)
	}
	if v := read(); v != 2 {
		t.Errorf("second ReadTx: temp_store = %d", v)
	}
}

// TestStoreTxUsesMemoryForTemporaryStorage (M1): temp_store MEMORY (2) is set by the connector and,
// separately, by StoreTx itself, with no ReadTx involved. The first check reads a fresh connection;
// the second puts the pooled connection back to the default and requires StoreTx to set 2 again, so
// removing only the StoreTx pragma (or only the connector's) fails.
func TestStoreTxUsesMemoryForTemporaryStorage(t *testing.T) {
	c := newTestCase(t)
	var fresh int
	if err := c.store.db.QueryRow(`PRAGMA temp_store`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh != 2 {
		t.Errorf("temp_store on a freshly opened connection = %d, want 2 (MEMORY)", fresh)
	}
	if _, err := c.store.db.Exec(`PRAGMA temp_store = DEFAULT`); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`PRAGMA temp_store`).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("temp_store in StoreTx = %d, want 2 (MEMORY)", got)
	}
}
