package sqlitefile_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const orderFuzzRows = 2000

type cellRef struct {
	page uint32
	cell int
}

// orderScanSeeds: builder files (clean, swapped cells, a repeated key, a doctored index) and
// engine-written fixtures.
func orderScanSeeds(f *testing.F) [][]byte {
	var seeds [][]byte
	b, _ := woTable(60)
	seeds = append(seeds, b.Bytes())
	b, tb := woTable(60)
	data := b.Bytes()
	swapPointers(data, osPS, tb.Leaves()[1], 2, 3)
	seeds = append(seeds, data)
	b, tb = woTable(60)
	data = b.Bytes()
	p := pageAt(data, osPS, tb.Leaves()[1])
	p[keyByteOffset(p, 3)] = p[keyByteOffset(p, 2)]
	seeds = append(seeds, data)
	ib := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	it := ib.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 60; i++ {
		it.Insert(i, i, fmt.Sprintf("key-%04d-%s", i, "ppppppppppppppppppppppppppppp"))
	}
	ib.CreateIndex("i", "t", "create index i on t(b)", 1)
	idata := ib.Bytes()
	swapPointers(idata, osPS, ib.Object("i").Leaves()[0], 1, 2)
	seeds = append(seeds, idata)
	for _, name := range []string{"basic-utf8-4k", "autovacuum-incr", "overflow-wide"} {
		seeds = append(seeds, gunzipFile(f, filepath.Join("testdata", name+".db.gz")))
	}
	return seeds
}

// FuzzOrderedScan: on arbitrary databases the ordered scan never panics, delivers exactly the rows
// the unordered scan delivers (the check only flags), flags no first entry, and leaves the budget
// as it found it.
func FuzzOrderedScan(f *testing.F) {
	for _, s := range orderScanSeeds(f) {
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
			if visited >= 8 || o.RootPage < 2 || o.Virtual {
				continue
			}
			var scan func(func(sqlitefile.Row) bool) error
			switch {
			case o.Type == "index":
				ix, err := v.Index(ctx, o.Name)
				if err != nil {
					continue
				}
				scan = func(visit func(sqlitefile.Row) bool) error { return ix.Entries(ctx, visit) }
			case o.Type == "table" && o.Table != nil && o.Table.ParseOK && o.Table.WithoutRowid:
				tbl, err := v.Table(ctx, o.Name)
				if err != nil {
					continue
				}
				scan = func(visit func(sqlitefile.Row) bool) error { return tbl.Rows(ctx, visit) }
			default:
				continue
			}
			visited++
			var plain, ordered []cellRef
			flagged := 0
			if err := v.ScanTree(ctx, o.RootPage, sqlitefile.IndexTree, func(r sqlitefile.Row) bool {
				plain = append(plain, cellRef{r.Loc.Page, r.Loc.Cell})
				return len(plain) < orderFuzzRows
			}); err != nil {
				continue
			}
			if err := scan(func(r sqlitefile.Row) bool {
				if r.KeyRangeViolation {
					flagged++
					if len(ordered) == 0 {
						t.Fatalf("%s: the first entry is flagged", o.Name)
					}
				}
				ordered = append(ordered, cellRef{r.Loc.Page, r.Loc.Cell})
				return len(ordered) < orderFuzzRows
			}); err != nil {
				continue
			}
			if len(plain) != len(ordered) {
				t.Fatalf("%s: the ordered scan delivered %d entries, the unordered one %d", o.Name, len(ordered), len(plain))
			}
			for i := range plain {
				if plain[i] != ordered[i] {
					t.Fatalf("%s: entry %d is %v ordered and %v unordered", o.Name, i, ordered[i], plain[i])
				}
			}
			if flagged > 0 && len(warningsOf(v, sqlitefile.WarnBTreeOrder)) == 0 && len(v.Warnings()) < sqlitefile.ResolveLimits(sqlitefile.Limits{}).MaxWarnings {
				t.Fatalf("%s: %d entries flagged and no btree-order warning", o.Name, flagged)
			}
		}
		v.Release()
		if got := rb.level(); got != base {
			t.Fatalf("budget level %d after the release, want %d", got, base)
		}
	})
}
