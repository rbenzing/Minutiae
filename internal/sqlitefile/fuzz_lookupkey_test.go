package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

const (
	keyFuzzRows   = 1200 // rows scanned
	keyFuzzLookup = 200  // rows looked up per table
)

func lookupKeySeeds(f *testing.F) [][]byte {
	var seeds [][]byte
	b, _ := keyTable(f, 400)
	seeds = append(seeds, b.Bytes())
	for _, c := range damageCases()[:2] {
		b, tb := keyTable(f, 2000)
		data := b.Bytes()
		func() {
			defer func() { _ = recover() }() // a patch that does not fit this tree seeds nothing
			c.patch(f, tb, data)
		}()
		seeds = append(seeds, data)
	}
	b2, tb := woTable(60)
	data := b2.Bytes()
	swapPointers(data, osPS, tb.Leaves()[1], 2, 3)
	seeds = append(seeds, data)
	seeds = append(seeds, chainIndex(f, 5))
	for _, name := range []string{"basic-utf8-4k", "autovacuum-incr", "overflow-wide"} {
		seeds = append(seeds, gunzipFile(f, filepath.Join("testdata", name+".db.gz")))
	}
	return seeds
}

// FuzzGetByKey: on arbitrary databases, for every WITHOUT ROWID row the scan delivers unflagged (the
// scan flags an entry below its predecessor or outside the range its ancestors give it),
// a lookup of its own key (LookupKey: no affinity conversion, so the stored value is the probe)
// never answers "absent", and a row it returns has that key. GetByKey itself never panics and
// returns only typed errors. The budget is as it was found.
func FuzzGetByKey(f *testing.F) {
	for _, s := range lookupKeySeeds(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		rb := newRecBudget(256 << 20)
		db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Budget: rb})
		if err != nil {
			return
		}
		base := rb.level()
		v := db.Live()
		ctx := context.Background()
		s, err := v.Schema(ctx)
		if err != nil {
			v.Release()
			return
		}
		visited := 0
		for _, o := range s.Objects {
			if visited >= 6 || o.Type != "table" || o.RootPage < 2 || o.Virtual || o.Table == nil || !o.Table.ParseOK || !o.Table.WithoutRowid {
				continue
			}
			tbl, err := v.Table(ctx, o.Name)
			if err != nil {
				continue
			}
			visited++
			order := sqlitefile.WithoutRowidOrder(o.Table)
			nkey := len(order.Cols)
			var rows []sqlitefile.Row
			_, err = tbl.RowsLoss(ctx, func(r sqlitefile.Row) bool {
				rows = append(rows, r.Clone())
				return len(rows) < keyFuzzRows
			})
			if err != nil {
				continue
			}
			for i, r := range rows[:min(len(rows), keyFuzzLookup)] {
				if r.KeyRangeViolation || len(r.Values) < nkey {
					continue
				}
				key := r.Values[:nkey]
				got, ok, err := v.LookupKey(ctx, o.RootPage, order, key)
				switch {
				case err == nil && !ok:
					t.Fatalf("%s: row %d is delivered unflagged by the scan and the lookup answers absent", o.Name, i)
				case err == nil:
					if c, _, st := sqlitefile.OrderKeys(got.Values, key, order.Cols); st == sqlitefile.OrderOK && c != 0 {
						t.Fatalf("%s: row %d: the lookup returned a row with another key", o.Name, i)
					}
				case errors.Is(err, sqlitefile.ErrCorrupt), errors.Is(err, sqlitefile.ErrKeyUndecided), errors.Is(err, sqlitefile.ErrBudget), errors.Is(err, sqlitefile.ErrLimit), errors.Is(err, sqlitefile.ErrPageUnavailable):
				default:
					t.Fatalf("%s: row %d: untyped error %v", o.Name, i, err)
				}
				if _, _, err := tbl.GetByKey(ctx, key); err != nil && !errors.Is(err, sqlitefile.ErrCorrupt) && !errors.Is(err, sqlitefile.ErrKeyUndecided) && !errors.Is(err, sqlitefile.ErrBadKey) && !errors.Is(err, sqlitefile.ErrBudget) && !errors.Is(err, sqlitefile.ErrLimit) && !errors.Is(err, sqlitefile.ErrPageUnavailable) {
					t.Fatalf("%s: row %d: GetByKey: untyped error %v", o.Name, i, err)
				}
			}
		}
		v.Release()
		if got := rb.level(); got != base {
			t.Fatalf("budget level %d after the release, want %d", got, base)
		}
	})
}
