package recordstest

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// SetNextID overwrites records_meta.next_id (the one mutable record table), as
// a stale or tampered counter would be: the writer must not trust it alone.
func SetNextID(t testing.TB, c *evidence.Case, v int64) {
	t.Helper()
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, strconv.FormatInt(v, 10))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
