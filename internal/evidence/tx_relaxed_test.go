package evidence_test

import (
	"context"
	"database/sql"
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
