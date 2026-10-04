package evidence

import (
	"context"
	"errors"
	"testing"
)

// TestRefusedQueryRowReportsErrReadSQLRefused: a refused QueryRow or
// QueryRowContext fails Scan (and Err) with ErrReadSQLRefused itself, not a
// database error text.
func TestRefusedQueryRowReportsErrReadSQLRefused(t *testing.T) {
	c := newTestCase(t)
	ctx := context.Background()
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		var v int
		if err := h.QueryRow(`PRAGMA query_only = OFF`).Scan(&v); !errors.Is(err, ErrReadSQLRefused) {
			t.Errorf("QueryRow Scan = %v, want ErrReadSQLRefused", err)
		}
		if err := h.QueryRowContext(ctx, `DELETE FROM records_meta`).Scan(&v); !errors.Is(err, ErrReadSQLRefused) {
			t.Errorf("QueryRowContext Scan = %v, want ErrReadSQLRefused", err)
		}
		if err := h.QueryRow(`COMMIT`).Err(); !errors.Is(err, ErrReadSQLRefused) {
			t.Errorf("QueryRow Err = %v, want ErrReadSQLRefused", err)
		}
		if err := h.QueryRow(`SELECT 7`).Scan(&v); err != nil || v != 7 {
			t.Errorf("an allowed QueryRow = %d, %v", v, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
