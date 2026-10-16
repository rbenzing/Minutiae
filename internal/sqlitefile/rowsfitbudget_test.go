package sqlitefile_test

import (
	"bytes"
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fitWRFixture: live WITHOUT ROWID table w holds nLive rows; the dropped
// WITHOUT ROWID table "gone" has the shape of w only, so its leaf is named w by
// the fit alone (BasisFit) and its rows reach the fit-only equality (C45).
func fitWRFixture(t *testing.T, nLive int, lim sqlitefile.Limits) ([]sqlitefile.RecoveredRow, sqlitefile.RowStats, *sqlitefile.Hist) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTableWithoutRowid("w", "create table w(k primary key, v) without rowid", 1)
	for i := 0; i < nLive; i++ {
		live.Insert(int64(i), string(rune('a'+i%26))+string(rune('a'+i/26)), "live")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTableWithoutRowid("gone", "create table gone(k primary key, v) without rowid", 1)
	for i := 0; i < 3; i++ {
		gone.Insert(int64(i), string(rune('a'+i%26))+"a", "live")
	}
	b.DropTable("pad")
	b.DropTable("gone")
	db := b.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	var rows []sqlitefile.RecoveredRow
	st, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool { rows = append(rows, r); return true })
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	return rows, st, h
}

func fitBudgetWarnings(h *sqlitefile.Hist) (n int, msg string) {
	for _, w := range h.Warnings() {
		if w.Code == sqlitefile.WarnLimitReached && strings.Contains(w.Msg, "fit-named") {
			n++
			msg = w.Msg
		}
	}
	return n, msg
}

// checkFitCapped asserts the J110 contract on the three fit-named rows of
// fitWRFixture: delivered, unknown, never annotated equal, no narrowing.
func checkFitCapped(t *testing.T, rows []sqlitefile.RecoveredRow, st sqlitefile.RowStats, h *sqlitefile.Hist, wantCap string) {
	t.Helper()
	if len(rows) != 3 {
		t.Fatalf("%d rows, want all 3 kept", len(rows))
	}
	for _, r := range rows {
		if !hasNote(r, sqlitefile.NoteFitEqualityBudget) || hasNote(r, sqlitefile.NoteEqualsLiveRow) {
			t.Errorf("notes %v: want %s and no equals-live-row", r.Notes, sqlitefile.NoteFitEqualityBudget)
		}
		if r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
			t.Errorf("relation %s notes %v", r.Relation, r.Notes)
		}
	}
	if len(st.LimitsHit) != 0 {
		t.Errorf("LimitsHit %v: an annotation limit never narrows the run", st.LimitsHit)
	}
	if st.FitEqualityCapped != 3 || st.EqualsLiveFit != 0 {
		t.Errorf("FitEqualityCapped %d EqualsLiveFit %d, want 3 and 0", st.FitEqualityCapped, st.EqualsLiveFit)
	}
	n, msg := fitBudgetWarnings(h)
	if n != 1 || !strings.Contains(msg, wantCap) || !strings.Contains(msg, "3 ") {
		t.Errorf("%d fit-equality warnings %q, want one naming %s and 3 rows", n, msg, wantCap)
	}
}

// TestFitEqualityBudgetSpentNeverNarrows (J110): spending the budget of the
// fit-only equality leaves the rows unannotated (unknown), counted and warned.
func TestFitEqualityBudgetSpentNeverNarrows(t *testing.T) {
	rows, st, h := fitWRFixture(t, 5, sqlitefile.Limits{MaxFitEqualityRows: 2})
	checkFitCapped(t, rows, st, h, "MaxFitEqualityRows")
}

// TestFitEqualityMaxDiffRowsHitNeverNarrows (J110, B-m2): a live fit table over
// MaxDiffRows is the same non-narrowing annotation limit.
func TestFitEqualityMaxDiffRowsHitNeverNarrows(t *testing.T) {
	rows, st, h := fitWRFixture(t, 5, sqlitefile.Limits{MaxDiffRows: 2})
	checkFitCapped(t, rows, st, h, "MaxDiffRows")
}

// fitRefusingBudget refuses a digest-entry charge (160 bytes made by the
// digest-set builder) from its nth call on.
type fitRefusingBudget struct{ calls, from int }

func (b *fitRefusingBudget) Alloc(n int64) error {
	if n == 160 && strings.Contains(string(debug.Stack()), "wrSetOf") {
		if b.calls++; b.calls >= b.from {
			return fmt.Errorf("%w: test refusal", sqlitefile.ErrBudget)
		}
	}
	return nil
}
func (b *fitRefusingBudget) Free(int64) {}

// TestFitEqualityLedgerExhaustionNeverEndsThePass (J110, B-m3): a budget refusal
// while building the fit set degrades to "not annotated" plus the counter.
func TestFitEqualityLedgerExhaustionNeverEndsThePass(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTableWithoutRowid("w", "create table w(k primary key, v) without rowid", 1)
	for i := 0; i < 5; i++ {
		live.Insert(int64(i), string(rune('a'+i)), "live")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTableWithoutRowid("gone", "create table gone(k primary key, v) without rowid", 1)
	for i := 0; i < 3; i++ {
		gone.Insert(int64(i), string(rune('a'+i)), "live")
	}
	b.DropTable("pad")
	b.DropTable("gone")
	db := b.Bytes()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Budget: &fitRefusingBudget{from: 3}})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	var rows []sqlitefile.RecoveredRow
	st, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool { rows = append(rows, r); return true })
	if err != nil {
		t.Fatalf("Rows ended with %v: a refused annotation charge must not end the pass", err)
	}
	checkFitCapped(t, rows, st, h, "budget")
}

// TestFitEqualityDefaultBudgetAnnotates: with the default budget the same
// fixture is annotated, so the tests above bite on the cap alone.
func TestFitEqualityDefaultBudgetAnnotates(t *testing.T) {
	rows, st, h := fitWRFixture(t, 5, sqlitefile.Limits{})
	if st.EqualsLiveFit != 3 || st.FitEqualityCapped != 0 || len(rows) != 3 {
		t.Errorf("EqualsLiveFit %d capped %d rows %d", st.EqualsLiveFit, st.FitEqualityCapped, len(rows))
	}
	if n, _ := fitBudgetWarnings(h); n != 0 {
		t.Errorf("%d fit-equality warnings with the default budget", n)
	}
}
