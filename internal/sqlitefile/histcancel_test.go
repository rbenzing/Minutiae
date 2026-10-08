package sqlitefile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestHistoryPollsTheContextOnPagesThatEmitNothing: the class passes and the
// gap loops look at the context on every page, not only when they deliver one
// (final review B, M-1). Cancelling inside the last delivery of a history
// must end the walk with the context's error even though no later page emits.
func TestHistoryPollsTheContextOnPagesThatEmitNothing(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, id, "x")
	}
	// a long run of live pages, so that the class passes after the first
	// delivery have pages to walk without delivering any
	big := b.CreateTable("big", "create table big(a)")
	for id := int64(1); id <= 400; id++ {
		big.Insert(id, "padpadpadpadpadpadpadpadpadpadpadpadpad")
	}
	b.CreateTable("empty", "create table empty(a)")
	snap := b.Snapshot()
	db := withCount(b.Bytes(), snap.Pages())
	_, h := openAll(t, db, nil, nil)
	total := len(collectPages(t, h))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	err := h.Pages(ctx, func(sqlitefile.PageImage) bool {
		if n++; n == total {
			cancel()
		}
		return true
	})
	t.Logf("%d deliveries, err %v", n, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Pages after a cancel inside the last delivery = %v, want context.Canceled", err)
	}
}
