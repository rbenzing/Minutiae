package sqlitefile_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestSchemaSkippedCountsEveryLostRow (Task 7 review, M-2): a schema row that is
// invalid, a duplicate or over the object cap is a lost row, so Layout calls
// the pages no listed object owns unattributed, never orphans. A row that is
// intact but whose CREATE text does not parse is NOT damage: the object is
// listed (unparsed) and the leftover pages stay orphans.
func TestSchemaSkippedCountsEveryLostRow(t *testing.T) {
	build := func(row ...any) []byte {
		b, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 5)
		if row != nil {
			b.AddSchemaRow(row...)
		}
		data := b.Bytes()
		clear(data[32:40]) // no freelist: leftover pages are on no list
		return data
	}
	cases := []struct {
		name    string
		row     []any
		opts    sqlitefile.Options
		skipped func(int) bool
		damaged bool
	}{
		{"invalid row", []any{"bogus", "x", "x", int64(0), nil}, sqlitefile.Options{}, func(n int) bool { return n == 1 }, true},
		{"duplicate row", []any{"table", "t1", "t1", int64(0), "create table t1(a)"}, sqlitefile.Options{}, func(n int) bool { return n == 1 }, true},
		{"object cap", []any{"view", "v1", "v1", int64(0), "create view v1 as select 1"}, sqlitefile.Options{Limits: sqlitefile.Limits{MaxSchemaObjects: 2}}, func(n int) bool { return n >= 1 }, true},
		{"unparsed but intact", []any{"table", "weird", "weird", int64(0), "create tabel weird("}, sqlitefile.Options{}, func(n int) bool { return n == 0 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, v := layoutOf(t, build(c.row...), c.opts)
			s, err := v.Schema(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !c.skipped(s.Skipped) {
				t.Errorf("Skipped = %d", s.Skipped)
			}
			if l.SchemaIncomplete != c.damaged {
				t.Errorf("SchemaIncomplete = %v, want %v", l.SchemaIncomplete, c.damaged)
			}
			if c.damaged {
				if l.OrphansTotal != 0 {
					t.Errorf("%d orphans from an incomplete schema", l.OrphansTotal)
				}
				n := 0
				for pg := uint32(1); pg <= l.Addressable; pg++ {
					if l.Class[pg] == sqlitefile.ClassUnattributed {
						n++
					}
				}
				if n == 0 || !strings.Contains(strings.Join(l.Problems, "\n"), fmt.Sprintf(": %d pages that no listed object claims", n)) {
					t.Errorf("%d unattributed pages; problems %v", n, l.Problems)
				}
			} else if l.OrphansTotal < 5 {
				t.Errorf("orphans %d: the leftover pages must stay orphans", l.OrphansTotal)
			}
		})
	}
}

// TestLayoutOrphansAreCharged: every orphan listed is charged to the budget.
func TestLayoutOrphansAreCharged(t *testing.T) {
	data, _ := orphanDB(t, 5)
	l, v := layoutOf(t, data, sqlitefile.Options{})
	if len(l.Orphans) < 5 {
		t.Fatalf("%d orphans", len(l.Orphans))
	}
	_, lay := sqlitefile.ListCharges(v)
	want := 5*(int64(l.Addressable)+1) + 4*int64(len(l.PtrmapPages)) + 4*int64(len(l.Orphans))
	if lay != want {
		t.Errorf("layout charge %d, want %d (arrays plus %d orphans at 4 bytes)", lay, want, len(l.Orphans))
	}
}
