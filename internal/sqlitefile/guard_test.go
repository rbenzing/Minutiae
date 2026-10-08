package sqlitefile_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func deepPanic(n int) int {
	if n == 0 {
		panic("deep boom")
	}
	return deepPanic(n-1) + 1
}

func TestGuardTurnsPanicIntoPanicError(t *testing.T) {
	err := sqlitefile.GuardedCall(func() error { panic("boom") })
	var pe *sqlitefile.PanicError
	if !errors.As(err, &pe) || !errors.Is(err, sqlitefile.ErrInternal) {
		t.Fatalf("err = %v, want a *PanicError matching ErrInternal", err)
	}
	if pe.Value != "boom" {
		t.Errorf("Value = %v", pe.Value)
	}

	err = sqlitefile.GuardedCall(func() error { deepPanic(500); return nil })
	if !errors.As(err, &pe) {
		t.Fatalf("deep panic: err = %v", err)
	}

	// A runtime error is recovered too.
	var m map[string]int
	err = sqlitefile.GuardedCall(func() error { m["x"] = 1; return nil })
	if !errors.Is(err, sqlitefile.ErrInternal) {
		t.Errorf("nil map write: err = %v", err)
	}

	// A non-panicking call keeps its own result.
	sentinel := errors.New("mine")
	if err := sqlitefile.GuardedCall(func() error { return sentinel }); err != sentinel {
		t.Errorf("err = %v, want the callee's own error", err)
	}
	if err := sqlitefile.GuardedCall(func() error { return nil }); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestPanicHookIsInstanceScoped(t *testing.T) {
	var seenA, seenB []string
	a := sqlitefile.NewProbeWithHook(func(site string) {
		seenA = append(seenA, site)
		if site == "explode" {
			panic("hook " + site)
		}
	})
	b := sqlitefile.NewProbeWithHook(func(site string) { seenB = append(seenB, site) })
	plain := sqlitefile.NewProbeWithHook(nil)

	if err := a.Run("explode"); !errors.Is(err, sqlitefile.ErrInternal) {
		t.Fatalf("instance with the hook: err = %v, want ErrInternal", err)
	}
	if err := b.Run("explode"); err != nil {
		t.Fatalf("instance whose hook does not panic: err = %v", err)
	}
	if err := plain.Run("explode"); err != nil {
		t.Fatalf("instance without a hook: err = %v", err)
	}
	if len(seenA) != 1 || len(seenB) != 1 {
		t.Errorf("hooks saw A=%v B=%v; each must see only its own calls", seenA, seenB)
	}
}
