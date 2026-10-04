package sqlitefile

import (
	"runtime"
	"strconv"
	"unicode/utf8"
)

// maxPanicStack is the most stack text a PanicError keeps.
const maxPanicStack = 2048

// env is the internal environment of one library instance: the options with
// resolved limits, the budget, and the test hook. The hook is set only by
// export_test.go constructors; there is no package-level variable, so one
// instance's injected panic never reaches another.
type env struct {
	opts   Options
	budget Budget
	hook   func(site string)
}

// newEnv resolves o: limits get their defaults and a nil Budget becomes an
// internal budget of Limits.DefaultBudgetBytes.
func newEnv(o Options, hook func(site string)) *env {
	o.Limits = o.Limits.withDefaults()
	b := o.Budget
	if b == nil {
		b = &memBudget{limit: o.Limits.DefaultBudgetBytes}
	}
	return &env{opts: o, budget: b, hook: hook}
}

// at is a panic-injection site: it calls the hook, if the instance has one.
func (e *env) at(site string) {
	if e.hook != nil {
		e.hook(site)
	}
}

// guard is deferred in every exported method (defer guard(&err)): a panic
// becomes a *PanicError in *err. A call that does not panic keeps its own err.
// The stack is built from runtime.Callers (runtime/debug is not used) and
// clipped to 2 KiB.
func guard(err *error) {
	r := recover()
	if r == nil {
		return
	}
	*err = &PanicError{Value: r, Stack: clippedStack()}
}

func clippedStack() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []byte
	for {
		f, more := frames.Next()
		out = append(out, f.Function...)
		out = append(out, "\n\t"...)
		out = append(out, f.File...)
		out = append(out, ':')
		out = strconv.AppendInt(out, int64(f.Line), 10)
		out = append(out, '\n')
		if len(out) >= maxPanicStack || !more {
			break
		}
	}
	if len(out) > maxPanicStack {
		out = out[:maxPanicStack]
		for len(out) > 0 && !utf8.Valid(out) { // never cut a rune in half
			out = out[:len(out)-1]
		}
	}
	return string(out)
}
