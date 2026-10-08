package sqlitedb

import "github.com/rbenzing/minutiae/internal/sqlitefile"

// Budget accounts for the memory the decoder holds. *parse.BudgetView
// satisfies it.
type Budget = sqlitefile.Budget

// budgetAdapter hands the caller's Budget to the library and counts what was
// granted, so Release can give back exactly that and nothing more. A refused
// request passes the caller's own error through unchanged. It is used by one
// goroutine, like the DB that owns it.
type budgetAdapter struct {
	b           Budget
	outstanding int64
}

func (a *budgetAdapter) Alloc(n int64) error {
	if err := a.b.Alloc(n); err != nil {
		return err
	}
	a.outstanding += n
	return nil
}

// Free returns at most what this adapter was granted and has not yet freed.
func (a *budgetAdapter) Free(n int64) {
	n = min(n, a.outstanding)
	if n <= 0 {
		return
	}
	a.outstanding -= n
	a.b.Free(n)
}

// releaseAll gives back everything still held.
func (a *budgetAdapter) releaseAll() {
	if a.outstanding > 0 {
		a.b.Free(a.outstanding)
		a.outstanding = 0
	}
}
