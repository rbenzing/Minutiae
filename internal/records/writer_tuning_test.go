package records_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestWriterTunesItsTransactionsAndRestores(t *testing.T) {
	c, art := setup(t)
	cacheNow := func() int {
		var v int
		if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`PRAGMA cache_size`).Scan(&v) }); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := cacheNow() // the connection's own size: a leaked tuned size would be exactly -262144
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 5}, art.ID)
	add(t, w, recordstest.Records(art.ID, 12, 1))
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.LastStoreTuning().CacheKiB; got != 262144 {
		t.Fatalf("the writer's batches ran with cache %d KiB, want the 256 MiB default", got)
	}
	if after := cacheNow(); after != before || after == -262144 {
		t.Fatalf("the tuned cache leaked out of the writer's transactions: before %d, after %d", before, after)
	}
}

func TestWriterCacheOptionIsValidated(t *testing.T) {
	c, _ := setup(t)
	for _, bad := range []int{-1, 1 << 21} {
		if _, err := records.NewWriter(c, testParser, records.WriterOptions{CacheKiB: bad}); !errors.Is(err, records.ErrInvalidOptions) {
			t.Errorf("CacheKiB %d: %v", bad, err)
		}
	}
}
