package sqlitefile_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestCappedDigestSetReleasesItsChargeAtOnce (review M-3): a digest set that
// hits MaxDiffRows holds nothing, so its charge goes back to the budget when
// the cap is reached, not at the end of the pass. The budget level seen at the
// first delivered row differs from an uncapped pass by exactly the charge of
// the live rows the uncapped set holds.
func TestCappedDigestSetReleasesItsChargeAtOnce(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
	for id := int64(1); id <= 3; id++ {
		wr.Insert(id, fmt.Sprintf("key%d", id), "v0")
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	wr.Update(2, "key2", "v1")
	wr.Delete(3)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	wal := w.Bytes()

	level := func(maxDiff int64) (atFirst int64, rb *recBudget) {
		rb = newRecBudget(1 << 30)
		d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Budget: rb, Limits: sqlitefile.Limits{MaxDiffRows: maxDiff}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatal(err)
		}
		h := d.History()
		first := true
		if _, err := h.Rows(context.Background(), func(sqlitefile.RecoveredRow) bool {
			if first {
				atFirst, first = rb.used, false
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		h.Release()
		return atFirst, rb
	}
	uncapped, _ := level(0)
	capped, _ := level(1)
	const entry = 160 // diffEntryCost
	if uncapped-capped != 2*entry {
		t.Errorf("budget at the first row: uncapped %d, capped %d: the difference is %d, want %d (2 live rows)", uncapped, capped, uncapped-capped, 2*entry)
	}
}
