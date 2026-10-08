package sqlitefile_test

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// capsTable is the plan's Caps table, by field name (own copy: the test must
// not read the defaults from the code under test).
var capsTable = map[string]int64{
	"MaxBTreeDepth":           32,
	"MaxColumns":              2000,
	"MaxTextBytes":            4 << 20,
	"MaxBlobBytes":            64 << 20,
	"MaxRecoveredValueBytes":  1 << 20,
	"MaxRowBytes":             128 << 20,
	"MaxPayloadBytes":         1 << 30,
	"MaxSchemaObjects":        100000,
	"MaxSchemaSQLBytes":       1 << 20,
	"MaxSchemaTotalBytes":     64 << 20,
	"MaxPages":                1 << 25,
	"PageCacheBytes":          32 << 20,
	"DefaultBudgetBytes":      1 << 30,
	"MaxWALFrames":            1 << 24,
	"MaxJournalRecords":       1 << 24,
	"MaxJournalSegments":      1 << 16,
	"MaxHistoryPages":         1 << 26,
	"MaxHistoryRows":          50000000,
	"MaxHistoryOverflowPages": 1 << 24,
	"MaxDiffRows":             2000000,
	"MaxDiffRowsTotal":        4000000,
	"MaxFitSteps":             1 << 20,
	"MaxOrphans":              100000,
	"MaxLocOverflow":          4096,
	"MaxWarnings":             1000,
}

func TestLimitsDefaults(t *testing.T) {
	d := sqlitefile.DefaultLimits()
	v := reflect.ValueOf(d)
	typ := v.Type()
	if typ.NumField() != len(capsTable) {
		t.Errorf("Limits has %d fields, the Caps table %d: keep them in step", typ.NumField(), len(capsTable))
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		want, ok := capsTable[name]
		if !ok {
			t.Errorf("Limits.%s is not in the Caps table", name)
			continue
		}
		if got := v.Field(i).Int(); got != want {
			t.Errorf("default %s = %d, want %d", name, got, want)
		}
	}

	// Zero Options equals DefaultLimits.
	if got := sqlitefile.ResolveLimits(sqlitefile.Options{}.Limits); got != d {
		t.Errorf("zero Limits resolve to %+v, want %+v", got, d)
	}
	if got := sqlitefile.EnvLimits(sqlitefile.Options{}); got != d {
		t.Errorf("zero Options resolve to %+v, want %+v", got, d)
	}

	// A partial Limits keeps the explicit field and defaults the rest, for
	// every field in turn.
	for i := 0; i < typ.NumField(); i++ {
		var l sqlitefile.Limits
		reflect.ValueOf(&l).Elem().Field(i).SetInt(7)
		got := reflect.ValueOf(sqlitefile.ResolveLimits(l))
		for j := 0; j < typ.NumField(); j++ {
			want := v.Field(j).Int()
			if j == i {
				want = 7
			}
			if got.Field(j).Int() != want {
				t.Errorf("only %s set: %s = %d, want %d", typ.Field(i).Name, typ.Field(j).Name, got.Field(j).Int(), want)
			}
		}
	}

	// A negative value is not a limit: it defaults.
	if got := sqlitefile.ResolveLimits(sqlitefile.Limits{MaxColumns: -5}).MaxColumns; got != 2000 {
		t.Errorf("negative MaxColumns resolved to %d", got)
	}
}

type fakeBudget struct{ allocs []int64 }

func (f *fakeBudget) Alloc(n int64) error { f.allocs = append(f.allocs, n); return nil }
func (f *fakeBudget) Free(n int64)        { f.allocs = append(f.allocs, -n) }

func TestNilBudgetUsesInternalBudget(t *testing.T) {
	b := sqlitefile.EnvBudget(sqlitefile.Options{})
	if b == nil {
		t.Fatal("a nil Options.Budget must get an internal budget")
	}
	if err := b.Alloc(1 << 30); err != nil {
		t.Fatalf("allocating the whole default budget failed: %v", err)
	}
	if err := b.Alloc(1); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("one byte past DefaultBudgetBytes: err = %v, want ErrBudget", err)
	}
	b.Free(1 << 30)
	if err := b.Alloc(1 << 30); err != nil {
		t.Fatalf("after Free the budget must be available again: %v", err)
	}

	// The internal budget honours Limits.DefaultBudgetBytes.
	b = sqlitefile.EnvBudget(sqlitefile.Options{Limits: sqlitefile.Limits{DefaultBudgetBytes: 100}})
	if err := b.Alloc(60); err != nil {
		t.Fatal(err)
	}
	if err := b.Alloc(41); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if err := b.Alloc(40); err != nil {
		t.Fatalf("a failed Alloc must not consume budget: %v", err)
	}
	if err := b.Alloc(math.MaxInt64); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("a huge Alloc must fail without overflowing: %v", err)
	}
	if err := b.Alloc(-1); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("a negative Alloc is refused: %v", err)
	}
	// Freeing more than is held never creates capacity.
	b.Free(1000)
	if err := b.Alloc(100); err != nil {
		t.Fatal(err)
	}
	if err := b.Alloc(1); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("over-Free created capacity: %v", err)
	}
	b.Free(-3) // a negative Free is ignored
	if err := b.Alloc(1); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Fatalf("negative Free created capacity: %v", err)
	}

	// A caller budget is used as given.
	fb := &fakeBudget{}
	if got := sqlitefile.EnvBudget(sqlitefile.Options{Budget: fb}); got != sqlitefile.Budget(fb) {
		t.Fatal("a caller budget must be used as is")
	}
}

func TestInternalBudgetConcurrent(t *testing.T) {
	b := sqlitefile.EnvBudget(sqlitefile.Options{Limits: sqlitefile.Limits{DefaultBudgetBytes: 1000}})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := int64(0)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if b.Alloc(10) == nil {
					mu.Lock()
					granted += 10
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if granted != 1000 {
		t.Fatalf("granted %d bytes of a 1000 byte budget", granted)
	}
}
