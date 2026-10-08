package parse

import (
	"fmt"
	"sync/atomic"
)

// Budget accounts the memory a job's decoders hold. It is host-owned: a parser
// only ever holds a BudgetView. All methods are safe for concurrent use, and
// every one of them is safe on a nil *Budget, which fails closed.
type Budget struct {
	limit int64
	used  atomic.Int64
}

// NewBudget returns a budget of limit bytes (a negative limit allows nothing).
func NewBudget(limit int64) *Budget { return &Budget{limit: max(limit, 0)} }

// Alloc reserves n bytes. n < 0, used+n > limit and a nil Budget are refused
// with an error wrapping ErrBudget; a refused call changes nothing.
func (b *Budget) Alloc(n int64) error {
	if b == nil {
		return fmt.Errorf("%w: no budget", ErrBudget)
	}
	if n < 0 {
		return fmt.Errorf("%w: negative allocation %d", ErrBudget, n)
	}
	for {
		used := b.used.Load()
		if n > b.limit-used { // b.limit >= used always, so this cannot overflow
			return fmt.Errorf("%w: %d bytes requested, %d of %d in use", ErrBudget, n, used, b.limit)
		}
		if b.used.CompareAndSwap(used, used+n) {
			return nil
		}
	}
}

// Free returns n bytes; it clamps at 0 and ignores n <= 0.
func (b *Budget) Free(n int64) {
	if b == nil || n <= 0 {
		return
	}
	for {
		used := b.used.Load()
		if b.used.CompareAndSwap(used, max(used-n, 0)) {
			return
		}
	}
}

// Used returns the bytes in use.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// Limit returns the budget's size.
func (b *Budget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// View returns a fresh view for one invocation.
func (b *Budget) View() *BudgetView { return &BudgetView{b: b} }

// BudgetView lets a decoder account allocations against the host's budget.
// Free can only return what this view allocated (it clamps to the view's
// outstanding amount), so a parser cannot free the host's accounting or
// another invocation's. There is no way to read or change the limit other
// than Limit, which only reports it.
type BudgetView struct {
	b           *Budget
	outstanding atomic.Int64
}

// Alloc reserves n bytes of the host's budget, as Budget.Alloc; a nil view
// fails closed.
func (v *BudgetView) Alloc(n int64) error {
	if v == nil {
		return fmt.Errorf("%w: no budget", ErrBudget)
	}
	if err := v.b.Alloc(n); err != nil {
		return err
	}
	v.outstanding.Add(n)
	return nil
}

// Free returns up to n bytes this view allocated and not yet freed.
func (v *BudgetView) Free(n int64) {
	if v == nil || n <= 0 {
		return
	}
	for {
		out := v.outstanding.Load()
		give := min(n, out)
		if give <= 0 {
			return
		}
		if v.outstanding.CompareAndSwap(out, out-give) {
			v.b.Free(give)
			return
		}
	}
}

// Used returns the host's total bytes in use.
func (v *BudgetView) Used() int64 {
	if v == nil {
		return 0
	}
	return v.b.Used()
}

// Limit returns the host's limit.
func (v *BudgetView) Limit() int64 {
	if v == nil {
		return 0
	}
	return v.b.Limit()
}
