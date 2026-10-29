package recrun_test

import (
	"context"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// FuzzRecoverTyped runs the typed pass over damaged copies of the library's fixtures. Any
// error is allowed; a panic, a hang, facts that do not validate or more records than a
// bounded multiple of the delivered rows are not.
func FuzzRecoverTyped(f *testing.F) {
	for _, name := range []string{"sqlite3-freelist", "freelist-dropped", "builder-wal-generations", "builder-hot-journal", "addcolumn-short"} {
		fx := loadFixture(f, name)
		f.Add(fx.db, fx.wal, fx.journal)
	}
	tables := []string{"people", "events", "notes", "a", "b", "kv", "t"}
	var maps []parse.TableMapping
	for _, tb := range tables {
		maps = append(maps, fakeMapping(tb, 6))
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		in := input(db, orNil(wal), orNil(journal))
		in.Budget = parse.NewBudget(64 << 20).View()
		em := newEmitter(t)
		rep, _ := recrun.Run(ctx, in, em, maps, claim(tables...))
		if int64(len(em.recs)) > rep.History.Delivered*4 {
			t.Fatalf("%d records from %d delivered rows", len(em.recs), rep.History.Delivered)
		}
		for _, e := range em.recs {
			if err := e.Facts.Validate(); err != nil {
				t.Fatal(err)
			}
		}
		for _, u := range em.unmapped {
			if err := u.Facts.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func orNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
