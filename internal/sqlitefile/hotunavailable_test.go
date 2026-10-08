package sqlitefile_test

// A hot journal that is not applied leaves an uncommitted transaction in the
// database file: Live() must not present it (plan 3I, Task 10 step 0).

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func requireLiveUnavailable(t *testing.T, v *sqlitefile.View, reason string) {
	t.Helper()
	var le *sqlitefile.LiveUnavailableError
	if _, err := v.ReadPage(1); !errors.Is(err, sqlitefile.ErrLiveUnavailable) || !errors.As(err, &le) || le.Reason != reason {
		t.Errorf("ReadPage(1): %v", err)
	}
	if _, err := v.Table(context.Background(), "t"); !errors.Is(err, sqlitefile.ErrLiveUnavailable) {
		t.Errorf("Table: %v", err)
	}
	if _, err := v.Schema(context.Background()); !errors.Is(err, sqlitefile.ErrLiveUnavailable) {
		t.Errorf("Schema: %v", err)
	}
	if !viewWarns(v, sqlitefile.WarnJournalHot, 0) {
		t.Errorf("journal-hot missing: %v", warnCodesOf(v))
	}
}

func TestLiveRefusesUnappliedHotJournalPageSizeMismatch(t *testing.T) {
	name := "journal page size 2048 against a 1024 database"
	j := newJfix()
	db, journal, _ := buildCase(t, j, name)
	d, v, info := attachJ(t, db, journal, sqlitefile.Options{})
	if info.Applied || !info.Hot || info.NotAppliedReason != "page-size-mismatch" {
		t.Fatalf("%+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnJournalPageSizeMismatch, 0) {
		t.Errorf("journal-page-size-mismatch missing: %v", warnCodesOf(v))
	}
	requireLiveUnavailable(t, v, "page-size-mismatch")
	// AsFound is the raw view and is unaffected.
	if got := j.viewLabels(t, d.AsFound()); got != lab("A", 40) {
		t.Errorf("AsFound labels %s", got)
	}
}

func TestLiveRefusesUnappliedHotJournalLimitReached(t *testing.T) {
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	d, v, info := attachJ(t, j.dbAfter, journal, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalRecords: 1}})
	if info.Applied || !info.Hot || info.NotAppliedReason != "limit-reached" {
		t.Fatalf("%+v", info)
	}
	requireLiveUnavailable(t, v, "limit-reached")
	expectRows(t, "AsFound", liveRows(t, d.AsFound()), j.after)
}

// A journal that is not hot, or hot but one the engine skips, leaves a
// committed database: Live() keeps presenting it.
func TestLiveStaysAvailableWhenJournalIsNotAppliedForOtherReasons(t *testing.T) {
	for _, name := range []string{"first byte zero", "zeroed header (persist)", "magic mismatch, first byte non-zero"} {
		t.Run(name, func(t *testing.T) {
			j := newJfix()
			db, journal, _ := buildCase(t, j, name)
			_, v, _ := attachJ(t, db, journal, sqlitefile.Options{})
			if _, err := tryRows(v); err != nil {
				t.Errorf("Live refused: %v", err)
			}
			if _, err := v.ReadPage(1); err != nil {
				t.Errorf("ReadPage: %v", err)
			}
		})
	}
}

// TestHistoryListsNothingWhenLiveIsUnavailable: the history is built on the live
// state; when Live() has none to present (a hot journal that is not applied) Pages
// and Summary return that error and list no image.
func TestHistoryListsNothingWhenLiveIsUnavailable(t *testing.T) {
	j := newJfix()
	journal := j.journal(512, -1).Bytes()
	d, _, _ := attachJ(t, j.dbAfter, journal, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalRecords: 1}})
	h := d.History()
	t.Cleanup(h.Release)
	n := 0
	if err := h.Pages(context.Background(), func(sqlitefile.PageImage) bool { n++; return true }); !errors.Is(err, sqlitefile.ErrLiveUnavailable) || n != 0 {
		t.Errorf("Pages: %v after %d images", err, n)
	}
	if _, err := h.Summary(context.Background()); !errors.Is(err, sqlitefile.ErrLiveUnavailable) {
		t.Errorf("Summary: %v", err)
	}
}
