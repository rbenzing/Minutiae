package parse

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestBudget(t *testing.T) {
	t.Run("alloc and free", func(t *testing.T) {
		b := NewBudget(100)
		if b.Limit() != 100 || b.Used() != 0 {
			t.Fatalf("limit/used = %d/%d", b.Limit(), b.Used())
		}
		if err := b.Alloc(60); err != nil {
			t.Fatal(err)
		}
		if err := b.Alloc(0); err != nil {
			t.Fatalf("Alloc(0): %v", err)
		}
		if b.Used() != 60 {
			t.Fatalf("used = %d", b.Used())
		}
		b.Free(20)
		if b.Used() != 40 {
			t.Fatalf("used after free = %d", b.Used())
		}
	})
	t.Run("exact limit allowed and limit+1 refused", func(t *testing.T) {
		b := NewBudget(100)
		if err := b.Alloc(100); err != nil {
			t.Fatalf("exact limit refused: %v", err)
		}
		err := b.Alloc(1)
		if !errors.Is(err, ErrBudget) {
			t.Fatalf("limit+1: %v, want ErrBudget", err)
		}
		if b.Used() != 100 {
			t.Fatalf("a refused Alloc changed Used: %d", b.Used())
		}
		b2 := NewBudget(100)
		if err := b2.Alloc(101); !errors.Is(err, ErrBudget) {
			t.Fatalf("101 on a fresh budget: %v", err)
		}
	})
	t.Run("negative refused", func(t *testing.T) {
		b := NewBudget(100)
		if err := b.Alloc(-1); !errors.Is(err, ErrBudget) {
			t.Fatalf("negative: %v", err)
		}
		if b.Used() != 0 {
			t.Fatalf("used = %d", b.Used())
		}
		b.Free(-5) // a negative Free is ignored, never an allocation
		if b.Used() != 0 {
			t.Fatalf("negative Free changed Used: %d", b.Used())
		}
	})
	t.Run("overflow safe", func(t *testing.T) {
		b := NewBudget(math.MaxInt64)
		if err := b.Alloc(math.MaxInt64); err != nil {
			t.Fatal(err)
		}
		if err := b.Alloc(1); !errors.Is(err, ErrBudget) {
			t.Fatalf("used+n overflow was not refused: %v", err)
		}
		if b.Used() != math.MaxInt64 {
			t.Fatalf("used = %d", b.Used())
		}
	})
	t.Run("negative limit allows nothing", func(t *testing.T) {
		b := NewBudget(-5)
		if b.Limit() != 0 {
			t.Fatalf("limit = %d", b.Limit())
		}
		if err := b.Alloc(1); !errors.Is(err, ErrBudget) {
			t.Fatalf("Alloc(1): %v", err)
		}
	})
	t.Run("free clamps at zero", func(t *testing.T) {
		b := NewBudget(100)
		_ = b.Alloc(10)
		b.Free(1000)
		if b.Used() != 0 {
			t.Fatalf("used = %d", b.Used())
		}
		b.Free(1)
		if b.Used() != 0 {
			t.Fatalf("used = %d", b.Used())
		}
	})
	t.Run("nil Budget fails closed", func(t *testing.T) {
		var b *Budget
		for _, n := range []int64{0, 1, -1} {
			if err := b.Alloc(n); !errors.Is(err, ErrBudget) {
				t.Fatalf("nil Budget Alloc(%d): %v", n, err)
			}
		}
		b.Free(5)
		if b.Used() != 0 || b.Limit() != 0 {
			t.Fatalf("nil Budget used/limit = %d/%d", b.Used(), b.Limit())
		}
		v := b.View()
		if v == nil {
			t.Fatal("View of a nil Budget is nil")
		}
		if err := v.Alloc(0); !errors.Is(err, ErrBudget) {
			t.Fatalf("view of nil Budget Alloc(0): %v", err)
		}
	})
	t.Run("nil BudgetView fails closed", func(t *testing.T) {
		var v *BudgetView
		for _, n := range []int64{0, 1, -1} {
			if err := v.Alloc(n); !errors.Is(err, ErrBudget) {
				t.Fatalf("nil view Alloc(%d): %v", n, err)
			}
		}
		v.Free(5)
		if v.Used() != 0 || v.Limit() != 0 {
			t.Fatalf("nil view used/limit = %d/%d", v.Used(), v.Limit())
		}
	})
	t.Run("concurrent alloc and free do not drift", func(t *testing.T) {
		const workers, rounds, chunk = 16, 2000, 7
		b := NewBudget(workers * chunk) // exactly enough for all workers at once
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range rounds {
					if err := b.Alloc(chunk); err != nil {
						errs <- err
						return
					}
					if b.Used() > b.Limit() {
						errs <- errors.New("used exceeds limit")
						return
					}
					b.Free(chunk)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if b.Used() != 0 {
			t.Fatalf("drift: used = %d", b.Used())
		}
	})
	t.Run("concurrent views do not drift", func(t *testing.T) {
		b := NewBudget(1 << 20)
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v := b.View()
				for range 1000 {
					if err := v.Alloc(5); err != nil {
						t.Error(err)
						return
					}
					v.Free(5)
				}
			}()
		}
		wg.Wait()
		if b.Used() != 0 {
			t.Fatalf("drift: used = %d", b.Used())
		}
	})
}

func TestBudgetViewFreeClampsToOwnAllocations(t *testing.T) {
	b := NewBudget(10_000)
	v1, v2 := b.View(), b.View()
	if v1 == v2 {
		t.Fatal("View must return a fresh view per call")
	}
	if err := v1.Alloc(10); err != nil {
		t.Fatal(err)
	}
	if err := v2.Alloc(500); err != nil {
		t.Fatal(err)
	}
	if v1.Used() != 510 || v2.Used() != 510 || v1.Limit() != 10_000 {
		t.Fatalf("view totals = %d/%d limit %d, want the host's 510/510 and 10000", v1.Used(), v2.Used(), v1.Limit())
	}
	v1.Free(1000)
	if b.Used() != 500 {
		t.Fatalf("a view freed more than it allocated: host used = %d, want 500", b.Used())
	}
	v1.Free(1) // nothing outstanding any more
	if b.Used() != 500 {
		t.Fatalf("second Free by an empty view changed the host: %d", b.Used())
	}
	v2.Free(100)
	v2.Free(1_000_000)
	if b.Used() != 0 {
		t.Fatalf("host used = %d after v2 freed its 500", b.Used())
	}
	v1.Free(-3) // ignored
	if err := v1.Alloc(-1); !errors.Is(err, ErrBudget) {
		t.Fatalf("negative view Alloc: %v", err)
	}
	if err := v1.Alloc(10_001); !errors.Is(err, ErrBudget) {
		t.Fatalf("over-limit view Alloc: %v", err)
	}
	if b.Used() != 0 {
		t.Fatalf("refused view allocations changed the host: %d", b.Used())
	}
}

func TestBudgetViewHasNoSetter(t *testing.T) {
	typ := reflect.TypeFor[*BudgetView]()
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"Alloc", "Free", "Limit", "Used"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BudgetView methods = %v, want exactly %v", got, want)
	}
	st := reflect.TypeFor[BudgetView]()
	for i := range st.NumField() {
		if f := st.Field(i); f.IsExported() {
			t.Fatalf("BudgetView exports field %s", f.Name)
		}
	}
}

// Two handles of one view share its outstanding amount; a second View() of
// the same Budget keeps its own.
func TestBudgetViewHandlesShareOutstanding(t *testing.T) {
	b := NewBudget(10_000)
	v := b.View()
	h := v.handle()
	if h == v || h.s != v.s {
		t.Fatalf("handle must be a distinct pointer onto the same state")
	}
	if err := v.Alloc(300); err != nil {
		t.Fatal(err)
	}
	h.Free(1000) // clamps to what the view allocated
	if b.Used() != 0 {
		t.Fatalf("host used = %d after the other handle freed the view's 300", b.Used())
	}
	if err := h.Alloc(50); err != nil {
		t.Fatal(err)
	}
	other := b.View()
	if err := other.Alloc(70); err != nil {
		t.Fatal(err)
	}
	v.Free(1000)
	if b.Used() != 70 {
		t.Fatalf("host used = %d, want only the second view's 70 left", b.Used())
	}
	if (*BudgetView)(nil).handle() != nil {
		t.Fatal("handle of a nil view must stay nil")
	}
}

func TestBudgetViewZeroValueFailsClosed(t *testing.T) {
	var v BudgetView
	if err := v.Alloc(0); !errors.Is(err, ErrBudget) {
		t.Fatalf("zero view Alloc(0): %v", err)
	}
	if err := v.Alloc(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("zero view Alloc(1): %v", err)
	}
	v.Free(5)
	if v.Used() != 0 || v.Limit() != 0 {
		t.Fatalf("zero view used/limit = %d/%d", v.Used(), v.Limit())
	}
}

// Run under the race gate. The limit holds two allocations of 7 bytes, so the
// eight goroutines contend for it: a refusal (ErrBudget) is expected and
// tolerated, and the accounting must never exceed the limit or drift.
func TestBudgetViewConcurrentHandlesStayConsistent(t *testing.T) {
	b := NewBudget(14)
	v := b.View()
	var wg sync.WaitGroup
	for range 8 {
		h := v.handle()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				if err := h.Alloc(7); err != nil {
					if !errors.Is(err, ErrBudget) {
						t.Error(err)
						return
					}
					continue // refused: nothing was reserved, so nothing is freed
				}
				if u := h.Used(); u > h.Limit() {
					t.Errorf("used %d exceeds the limit %d", u, h.Limit())
				}
				v.Free(7) // freed through the first handle: all handles share one outstanding amount
			}
		}()
	}
	wg.Wait()
	if b.Used() != 0 || v.s.outstanding.Load() != 0 {
		t.Fatalf("drift: used = %d, outstanding = %d", b.Used(), v.s.outstanding.Load())
	}
}

// A zero view next to a live handle shares nothing with it: freeing through
// the zero view leaves the live handle's state untouched.
func TestBudgetViewFreeOnAZeroViewLeavesALiveHandleUntouched(t *testing.T) {
	b := NewBudget(1000)
	live := b.View()
	if err := live.Alloc(300); err != nil {
		t.Fatal(err)
	}
	h := live.handle()
	var zero BudgetView
	zero.Free(300)
	zero.Free(1 << 40)
	if b.Used() != 300 || live.s.outstanding.Load() != 300 || h.Used() != 300 {
		t.Fatalf("after Free on a zero view: host used = %d, outstanding = %d, want 300 and 300",
			b.Used(), live.s.outstanding.Load())
	}
}
