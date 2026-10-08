package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// panicFixture: a database with one table, a WAL holding an older and a newer
// version of its page, and a journal.
func panicFixture() (db, wal, journal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 5; id++ {
		tb.Insert(id, id, "row")
	}
	v0 := b.Snapshot()
	db = walMode(withCount(b.Bytes(), v0.Pages()))
	tb.Update(2, int64(22), "newer")
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	j := b.NewJournal(512, 7, v0.Pages())
	j.Record(2, v0.Page(2))
	return db, w.Bytes(), j.Bytes()
}

type armed struct{ on atomic.Bool }

func (a *armed) hook(string) {
	if a.on.CompareAndSwap(true, false) {
		panic("injected")
	}
}

func wantPanicError(t *testing.T, what string, err error) {
	t.Helper()
	var pe *sqlitefile.PanicError
	if !errors.As(err, &pe) || !errors.Is(err, sqlitefile.ErrInternal) {
		t.Fatalf("%s: err = %v, want a *PanicError matching ErrInternal", what, err)
	}
	if pe.Value != "injected" {
		t.Errorf("%s: panic value %v", what, pe.Value)
	}
}

// TestPublicEntryPointsRecoverPanics: a panic injected inside each public
// entry point comes back as a *PanicError (ErrInternal), never as a panic, the
// budget is balanced afterwards, and the DB is usable again: the same call
// succeeds once the hook stops panicking.
func TestPublicEntryPointsRecoverPanics(t *testing.T) {
	db, wal, journal := panicFixture()
	ctx := context.Background()
	t.Run("Open", func(t *testing.T) {
		a := &armed{}
		a.on.Store(true)
		_, err := sqlitefile.OpenWithHook(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{}, a.hook)
		wantPanicError(t, "Open", err)
		if _, err := sqlitefile.OpenWithHook(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{}, a.hook); err != nil {
			t.Errorf("Open again: %v", err)
		}
	})
	t.Run("ScanWAL", func(t *testing.T) {
		a := &armed{}
		a.on.Store(true)
		rb := newRecBudget(1 << 30)
		_, err := sqlitefile.ScanWALWithHook(bytes.NewReader(wal), int64(len(wal)), hps, sqlitefile.Options{Budget: rb}, a.hook)
		wantPanicError(t, "ScanWAL", err)
		rb.check(t)
		if s, err := sqlitefile.ScanWALWithHook(bytes.NewReader(wal), int64(len(wal)), hps, sqlitefile.Options{}, a.hook); err != nil || s == nil {
			t.Errorf("ScanWAL again: %v", err)
		}
	})
	t.Run("ScanJournal", func(t *testing.T) {
		a := &armed{}
		a.on.Store(true)
		rb := newRecBudget(1 << 30)
		_, err := sqlitefile.ScanJournalWithHook(bytes.NewReader(journal), int64(len(journal)), hps, sqlitefile.Options{Budget: rb}, a.hook)
		wantPanicError(t, "ScanJournal", err)
		rb.check(t)
		if s, err := sqlitefile.ScanJournalWithHook(bytes.NewReader(journal), int64(len(journal)), hps, sqlitefile.Options{}, a.hook); err != nil || s == nil {
			t.Errorf("ScanJournal again: %v", err)
		}
	})
	// open returns the instance with both companions attached and the budget
	// level it holds then: a panic and the release that follows must leave it there.
	open := func(t *testing.T) (*sqlitefile.DB, *armed, *recBudget, int64) {
		a := &armed{}
		rb := newRecBudget(1 << 30)
		d, err := sqlitefile.OpenWithHook(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Budget: rb}, a.hook)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatal(err)
		}
		if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
			t.Fatal(err)
		}
		return d, a, rb, rb.level()
	}
	t.Run("View.Schema", func(t *testing.T) {
		d, a, rb, base := open(t)
		v := d.AsFound()
		a.on.Store(true)
		_, err := v.Schema(ctx)
		wantPanicError(t, "Schema", err)
		if s, err := v.Schema(ctx); err != nil || len(s.Objects) == 0 {
			t.Errorf("Schema again: %v", err)
		}
		v.Release()
		if got := rb.level(); got != base {
			t.Errorf("budget level %d after the panic and the release, want %d", got, base)
		}
	})
	t.Run("Table.Rows", func(t *testing.T) {
		d, a, rb, base := open(t)
		v := d.AsFound()
		tb, err := v.Table(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		a.on.Store(true)
		wantPanicError(t, "Rows", tb.Rows(ctx, func(sqlitefile.Row) bool { return true }))
		n := 0
		if err := tb.Rows(ctx, func(sqlitefile.Row) bool { n++; return true }); err != nil || n != 5 {
			t.Errorf("Rows again: %d rows, %v", n, err)
		}
		v.Release()
		if got := rb.level(); got != base {
			t.Errorf("budget level %d after the panic and the release, want %d", got, base)
		}
	})
	t.Run("Hist.Pages", func(t *testing.T) {
		d, a, rb, base := open(t)
		h := d.History()
		a.on.Store(true)
		wantPanicError(t, "Pages", h.Pages(ctx, func(sqlitefile.PageImage) bool { return true }))
		n := 0
		if err := h.Pages(ctx, func(sqlitefile.PageImage) bool { n++; return true }); err != nil || n == 0 {
			t.Errorf("Pages again: %d images, %v", n, err)
		}
		h.Release()
		if got := rb.level(); got != base {
			t.Errorf("budget level %d after the panic and the release, want %d", got, base)
		}
	})
	t.Run("Hist.Rows", func(t *testing.T) {
		d, a, rb, base := open(t)
		h := d.History()
		a.on.Store(true)
		_, err := h.Rows(ctx, func(sqlitefile.RecoveredRow) bool { return true })
		wantPanicError(t, "Rows", err)
		n := 0
		if _, err := h.Rows(ctx, func(sqlitefile.RecoveredRow) bool { n++; return true }); err != nil || n == 0 {
			t.Errorf("Rows again: %d rows, %v", n, err)
		}
		h.Release()
		if got := rb.level(); got != base {
			t.Errorf("budget level %d after the panic and the release, want %d", got, base)
		}
	})
}

func (b *recBudget) level() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
