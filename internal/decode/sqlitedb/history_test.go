package sqlitedb_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func claimPeople(table string) bool { return strings.EqualFold(table, "people") }

// referenceHistory is what the library alone says about the claims: the locators of the
// rows ClaimMatch accepts, in delivery order, and the count of every other verdict.
func referenceHistory(t *testing.T, db, wal, journal []byte, claimed func(string) bool) (yes []string, skipped map[string]int64, total int64) {
	t.Helper()
	open := func(b []byte) (*bytes.Reader, int64) { return bytes.NewReader(b), int64(len(b)) }
	r, n := open(db)
	lib, err := sqlitefile.Open(r, n, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		r, n := open(wal)
		if _, err := lib.AttachWAL(r, n); err != nil {
			t.Fatal(err)
		}
	}
	if journal != nil {
		r, n := open(journal)
		if _, err := lib.AttachJournal(r, n); err != nil {
			t.Fatal(err)
		}
	}
	sch, err := lib.Live().Schema(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := lib.History()
	defer h.Release()
	skipped = map[string]int64{}
	_, err = h.Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
		total++
		if v := sch.ClaimMatch(rr, claimed); v == sqlitefile.ClaimYes {
			yes = append(yes, sqlitefile.RecoveredLocator(rr))
		} else {
			skipped[string(v)]++
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return yes, skipped, total
}

func TestClaimedHistoryDeliversOnlyClaimMatchRows(t *testing.T) {
	db, _, _ := loadFixture(t, "sqlite3-freelist")
	d := openBytes(t, db, nil, nil, bigBudget())
	wantYes, wantSkipped, total := referenceHistory(t, db, nil, nil, claimPeople)
	if len(wantYes) == 0 || len(wantSkipped) == 0 {
		t.Fatalf("the fixture must hold claimed and unclaimed rows: %d claimed, skipped %v", len(wantYes), wantSkipped)
	}
	var got []string
	hc, err := d.ClaimedHistory(t.Context(), claimPeople, func(rr sqlitefile.RecoveredRow) error {
		got = append(got, sqlitefile.RecoveredLocator(rr))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, wantYes) {
		t.Errorf("delivered %v, want %v", got, wantYes)
	}
	if hc.Delivered != int64(len(wantYes)) {
		t.Errorf("Delivered %d, want %d", hc.Delivered, len(wantYes))
	}
	var sum int64
	for k, v := range hc.Skipped {
		sum += v
		if wantSkipped[k] != v {
			t.Errorf("Skipped[%s] = %d, want %d", k, v, wantSkipped[k])
		}
	}
	if len(hc.Skipped) != len(wantSkipped) {
		t.Errorf("Skipped %v, want %v", hc.Skipped, wantSkipped)
	}
	if hc.Delivered+sum != total {
		t.Errorf("Delivered %d + skipped %d != the %d rows of the history", hc.Delivered, sum, total)
	}
}

func TestClaimedHistoryVisitErrorStopsAndIsReturned(t *testing.T) {
	db, _, _ := loadFixture(t, "sqlite3-freelist")
	d := openBytes(t, db, nil, nil, bigBudget())
	boom := errors.New("boom")
	calls := 0
	_, err := d.ClaimedHistory(t.Context(), claimPeople, func(sqlitefile.RecoveredRow) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if calls != 1 {
		t.Errorf("visit called %d times after it failed, want 1", calls)
	}
}

func TestClaimedHistoryStopsOnCancelledContext(t *testing.T) {
	db, _, _ := loadFixture(t, "sqlite3-freelist")
	d := openBytes(t, db, nil, nil, bigBudget())
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := d.ClaimedHistory(ctx, claimPeople, func(sqlitefile.RecoveredRow) error {
		calls++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("visit called %d times after the cancel, want 1", calls)
	}
}

func TestClaimedHistoryClaimingNothingDeliversNothing(t *testing.T) {
	db, _, _ := loadFixture(t, "sqlite3-freelist")
	d := openBytes(t, db, nil, nil, bigBudget())
	_, _, total := referenceHistory(t, db, nil, nil, nil)
	hc, err := d.ClaimedHistory(t.Context(), nil, func(sqlitefile.RecoveredRow) error {
		t.Error("a row was delivered although nothing is claimed")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, v := range hc.Skipped {
		sum += v
	}
	if hc.Delivered != 0 || sum != total || total == 0 {
		t.Errorf("Delivered %d, skipped %d of %d", hc.Delivered, sum, total)
	}
}

func TestClaimedHistoryAfterReleaseIsRefused(t *testing.T) {
	db, _, _ := loadFixture(t, "sqlite3-freelist")
	d := openBytes(t, db, nil, nil, bigBudget())
	d.Release()
	if _, err := d.ClaimedHistory(t.Context(), claimPeople, func(sqlitefile.RecoveredRow) error { return nil }); !errors.Is(err, sqlitedb.ErrReleased) {
		t.Errorf("err = %v, want ErrReleased", err)
	}
}
