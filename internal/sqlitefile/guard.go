package sqlitefile

import "sync/atomic"

// env is the internal environment of one library instance: the options with
// resolved limits, the budget, and the test hook. The hook is set only by
// export_test.go constructors; the only package-level hook is pureHook, for the
// exported pure parse functions that belong to no instance, so one instance's
// injected panic never reaches another.
type env struct {
	opts   Options
	budget Budget
	hook   func(site string)
	// clamps are the notes of limits clamped to their ceilings; each
	// instance reports them once through its warnings (reportClamps).
	clamps []string
}

// newEnv resolves o: limits get their defaults and a nil Budget becomes an
// internal budget of Limits.DefaultBudgetBytes.
func newEnv(o Options, hook func(site string)) *env {
	var clamps []string
	o.Limits, clamps = o.Limits.resolve()
	b := o.Budget
	if b == nil {
		b = &memBudget{limit: o.Limits.DefaultBudgetBytes}
	}
	return &env{opts: o, budget: b, hook: hook, clamps: clamps}
}

// reportClamps adds one limit-reached warning per limit that was clamped to
// its ceiling.
func (e *env) reportClamps(w *warnings) {
	for _, note := range e.clamps {
		w.add(Warning{Code: WarnLimitReached, Msg: note})
	}
}

// ledger is the budget account of one exported call. Everything the call
// charges through it is given back if a panic unwinds the call or the call
// returns an error; on success the charge stays with the result, which owns
// it from then on. Use: l := e.newLedger(); defer l.guard(&err). A ledger
// belongs to one call and is not shared between goroutines.
type ledger struct {
	b Budget
	n int64 // bytes charged by this call and not yet given back
}

func (e *env) newLedger() *ledger { return &ledger{b: e.budget} }

// alloc charges n bytes; a refused charge grants nothing.
func (l *ledger) alloc(n int64) error {
	if err := l.b.Alloc(n); err != nil {
		return err
	}
	l.n += n
	return nil
}

// free gives back n bytes this call charged (never more than it holds).
func (l *ledger) free(n int64) {
	n = min(max(n, 0), l.n)
	l.b.Free(n)
	l.n -= n
}

// guard is deferred in an exported method that charges memory: a panic
// becomes a *PanicError in *err (as guard does) and every charge of the call
// is released; so are the charges of a call that returns an error.
func (l *ledger) guard(err *error) {
	if r := recover(); r != nil {
		*err = &PanicError{Value: r}
	}
	if *err != nil {
		l.free(l.n)
	}
}

// at is a panic-injection site: it calls the hook, if the instance has one.
func (e *env) at(site string) {
	if e.hook != nil {
		e.hook(site)
	}
}

// guard is deferred in every exported method and in the five exported pure
// parse functions (defer guard(&err)): a panic
// becomes a *PanicError in *err. A call that does not panic keeps its own err.
func guard(err *error) {
	r := recover()
	if r == nil {
		return
	}
	*err = &PanicError{Value: r}
}

// pureHook is the panic-injection hook of the exported pure parse functions,
// which belong to no instance: tests set it through export_test.go. It is nil
// in production.
var pureHook atomic.Pointer[func(site string)]

// pureAt is the panic-injection site of a pure parse function.
func pureAt(site string) {
	if h := pureHook.Load(); h != nil {
		(*h)(site)
	}
}
