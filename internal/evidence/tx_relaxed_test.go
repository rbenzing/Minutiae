package evidence_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func pragmaText(t *testing.T, c *evidence.Case, name string) string {
	t.Helper()
	var v string
	if err := c.StoreTx(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow("PRAGMA " + name).Scan(&v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestStoreTxDefaultsToFullSync: without RelaxedSync every transaction runs synchronous=FULL on a
// rollback journal (D90 a).
func TestStoreTxDefaultsToFullSync(t *testing.T) {
	c := recordstest.NewCase(t)
	var sync int
	var mode string
	err := c.StoreTxTuned(context.Background(), evidence.TxTuning{CacheKiB: 4096}, func(tx *sql.Tx) error {
		if err := tx.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
			return err
		}
		return tx.QueryRow("PRAGMA journal_mode").Scan(&mode)
	})
	if err != nil {
		t.Fatal(err)
	}
	if sync != 2 || mode != "delete" {
		t.Fatalf("synchronous = %d, journal_mode = %q, want 2 (FULL) and delete", sync, mode)
	}
}

// TestRelaxedSyncIsPerTransactionAndRestoredToFull: a RelaxedSync transaction runs synchronous=NORMAL
// on a write-ahead log (NORMAL on a rollback journal could corrupt the database); the next ordinary
// transaction is FULL again, and RestoreFullSync puts the rollback journal back and removes the log.
func TestRelaxedSyncIsPerTransactionAndRestoredToFull(t *testing.T) {
	ctx := context.Background()
	c := recordstest.NewCase(t)
	var inside int
	var insideMode string
	err := c.StoreTxTuned(ctx, evidence.TxTuning{RelaxedSync: true}, func(tx *sql.Tx) error {
		if err := tx.QueryRow("PRAGMA synchronous").Scan(&inside); err != nil {
			return err
		}
		if err := tx.QueryRow("PRAGMA journal_mode").Scan(&insideMode); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TABLE relaxed_probe (v INTEGER)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if inside != 1 || insideMode != "wal" {
		t.Fatalf("inside: synchronous = %d, journal_mode = %q, want 1 (NORMAL) and wal", inside, insideMode)
	}
	if got := pragma(t, c, "synchronous"); got != 2 {
		t.Fatalf("synchronous after the relaxed transaction = %d, want 2 (FULL): the relaxed mode is per transaction", got)
	}
	wal := filepath.Join(c.Dir, "artifacts.db-wal")
	if _, err := os.Stat(wal); err != nil {
		t.Fatalf("no write-ahead log while relaxed: %v", err)
	}
	if err := c.RestoreFullSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pragmaText(t, c, "journal_mode"); got != "delete" {
		t.Fatalf("journal_mode after RestoreFullSync = %q, want delete", got)
	}
	if got := pragma(t, c, "synchronous"); got != 2 {
		t.Fatalf("synchronous after RestoreFullSync = %d, want 2", got)
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(c.Dir, "artifacts.db"+s)); !os.IsNotExist(err) {
			t.Fatalf("artifacts.db%s still exists after RestoreFullSync (%v)", s, err)
		}
	}
	var n int
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`SELECT count(*) FROM relaxed_probe`).Scan(&n) }); err != nil {
		t.Fatalf("the relaxed write is gone after the checkpoint: %v", err)
	}
}

// TestRestoreFullSyncOnAFullCaseChangesNothing: it is safe to call when no relaxed transaction ran.
func TestRestoreFullSyncOnAFullCaseChangesNothing(t *testing.T) {
	c := recordstest.NewCase(t)
	if err := c.RestoreFullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pragmaText(t, c, "journal_mode"); got != "delete" {
		t.Fatalf("journal_mode = %q", got)
	}
}

// TestRelaxedSyncIsFullAfterATransactionThatReturnsAnError: the deferred restore runs on the error
// path too, so the pooled connection is never left on NORMAL (D93).
func TestRelaxedSyncIsFullAfterATransactionThatReturnsAnError(t *testing.T) {
	c := recordstest.NewCase(t)
	boom := errors.New("boom")
	err := c.StoreTxTuned(context.Background(), evidence.TxTuning{RelaxedSync: true}, func(tx *sql.Tx) error {
		var s int
		if err := tx.QueryRow("PRAGMA synchronous").Scan(&s); err != nil || s != 1 {
			t.Errorf("inside: synchronous = %d (%v), want 1", s, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got := pragma(t, c, "synchronous"); got != 2 {
		t.Fatalf("synchronous after a failed relaxed transaction = %d, want 2 (FULL)", got)
	}
}

// TestRelaxedSyncIsFullAfterATransactionThatPanics: the same when fn panics (the panic continues).
func TestRelaxedSyncIsFullAfterATransactionThatPanics(t *testing.T) {
	c := recordstest.NewCase(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not continue")
			}
		}()
		_ = c.StoreTxTuned(context.Background(), evidence.TxTuning{RelaxedSync: true}, func(*sql.Tx) error {
			panic("fn panics")
		})
	}()
	if got := pragma(t, c, "synchronous"); got != 2 {
		t.Fatalf("synchronous after a panicking relaxed transaction = %d, want 2 (FULL)", got)
	}
}

// TestReopeningACaseLeftInWALReturnsToDeleteWithTheRowsIntact: a case closed without RestoreFullSync
// (a crash or an abandoned ingest) is back on the rollback journal when it is opened again, and the
// relaxed rows are still there.
func TestReopeningACaseLeftInWALReturnsToDeleteWithTheRowsIntact(t *testing.T) {
	ctx := context.Background()
	c := recordstest.NewCase(t)
	err := c.StoreTxTuned(ctx, evidence.TxTuning{RelaxedSync: true}, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE relaxed_probe (v INTEGER)`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO relaxed_probe VALUES (1), (2), (3)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pragmaText(t, c, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode while relaxed = %q, want wal", got)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if got := pragmaText(t, c2, "journal_mode"); got != "delete" {
		t.Fatalf("journal_mode after reopening = %q, want delete", got)
	}
	var n int
	if err := c2.StoreTx(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`SELECT count(*) FROM relaxed_probe`).Scan(&n) }); err != nil || n != 3 {
		t.Fatalf("rows after reopening = %d (%v), want 3", n, err)
	}
}
