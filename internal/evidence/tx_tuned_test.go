package evidence_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func pragma(t *testing.T, c *evidence.Case, name string) int {
	t.Helper()
	var v int
	err := c.StoreTx(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow("PRAGMA " + name).Scan(&v) })
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStoreTxTunedSetsAndRestoresCache(t *testing.T) {
	c := recordstest.NewCase(t)
	def := pragma(t, c, "cache_size")
	defTemp := pragma(t, c, "temp_store")
	var inside, mmap, temp int
	err := c.StoreTxTuned(context.Background(), evidence.TxTuning{CacheKiB: 4096}, func(tx *sql.Tx) error {
		if err := tx.QueryRow("PRAGMA cache_size").Scan(&inside); err != nil {
			return err
		}
		if err := tx.QueryRow("PRAGMA mmap_size").Scan(&mmap); err != nil {
			return err
		}
		return tx.QueryRow("PRAGMA temp_store").Scan(&temp)
	})
	if err != nil {
		t.Fatal(err)
	}
	if inside != -4096 {
		t.Fatalf("cache_size inside = %d, want -4096 (KiB)", inside)
	}
	if mmap != 0 {
		t.Fatalf("mmap_size = %d: it stays 0 (portable on Windows)", mmap)
	}
	if temp != 2 {
		t.Fatalf("temp_store = %d inside the tuned transaction, want 2 (MEMORY): a sorter must never spill outside the case (R67)", temp)
	}
	if got := pragma(t, c, "temp_store"); got != defTemp {
		t.Fatalf("temp_store after = %d, want the default %d", got, defTemp)
	}
	if got := pragma(t, c, "cache_size"); got != def {
		t.Fatalf("cache_size after = %d, want the default %d", got, def)
	}
	if c.LastStoreTuning().CacheKiB != 4096 {
		t.Fatalf("LastStoreTuning = %+v", c.LastStoreTuning())
	}
}

func TestStoreTxTunedRestoresOnError(t *testing.T) {
	c := recordstest.NewCase(t)
	def := pragma(t, c, "cache_size")
	boom := errors.New("boom")
	if err := c.StoreTxTuned(context.Background(), evidence.TxTuning{CacheKiB: 4096}, func(*sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := pragma(t, c, "cache_size"); got != def {
		t.Fatalf("cache_size after a failed transaction = %d, want %d", got, def)
	}
	for _, bad := range []int{-1, 1 << 21} {
		if err := c.StoreTxTuned(context.Background(), evidence.TxTuning{CacheKiB: bad}, func(*sql.Tx) error { return nil }); err == nil {
			t.Errorf("CacheKiB %d accepted", bad)
		}
	}
	if got := pragma(t, c, "cache_size"); got != def {
		t.Fatalf("cache_size after refused sizes = %d, want %d: a refusal changes nothing", got, def)
	}
}
