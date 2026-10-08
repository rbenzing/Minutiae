package artparse_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
)

type panicError struct{}

func (panicError) Error() string { panic("Error method panics") }

func noPump() <-chan func() { return nil }

func TestGuardRecoversPanicsAndRunsNext(t *testing.T) {
	var nilMap map[string]int
	cases := map[string]func(){
		"string":    func() { panic("boom") },
		"error":     func() { panic(errors.New("an error")) },
		"nil map":   func() { nilMap["x"] = 1 },
		"bad Error": func() { panic(panicError{}) },
	}
	for name, fn := range cases {
		g := artparse.Guard(context.Background(), time.Second, time.Second, func() {}, noPump(), func(context.Context) error { fn(); return nil })
		if g.Panic == nil || g.Err != nil || g.Abandoned || g.TimedOut || g.Cancelled {
			t.Fatalf("%s: got %+v", name, g)
		}
		if g.Panic.Value == "" || g.Panic.Stack == "" {
			t.Errorf("%s: empty panic info %+v", name, g.Panic)
		}
		if name == "bad Error" && !strings.Contains(g.Panic.Value, "panicError") {
			t.Errorf("a panicking Error method must be reported by type name: %q", g.Panic.Value)
		}
		if name == "string" && g.Panic.Value != "boom" {
			t.Errorf("value = %q", g.Panic.Value)
		}
		want := errors.New("next")
		g2 := artparse.Guard(context.Background(), time.Second, time.Second, func() {}, noPump(), func(context.Context) error { return want })
		if g2.Panic != nil || !errors.Is(g2.Err, want) {
			t.Fatalf("%s: the next guard call is not clean: %+v", name, g2)
		}
	}
}

func TestGuardPanicStackClipped(t *testing.T) {
	var deep func(n int)
	deep = func(n int) {
		if n == 0 {
			panic(strings.Repeat("v", 5000))
		}
		deep(n - 1)
	}
	g := artparse.Guard(context.Background(), time.Second, time.Second, func() {}, noPump(), func(context.Context) error { deep(200); return nil })
	if g.Panic == nil {
		t.Fatal("no panic")
	}
	if len(g.Panic.Stack) != 2048 {
		t.Errorf("stack length = %d, want exactly 2048 (clipped)", len(g.Panic.Stack))
	}
	if len(g.Panic.Value) > 1024 {
		t.Errorf("value length = %d, want <= 1024", len(g.Panic.Value))
	}
}

func TestGuardTimeoutCooperative(t *testing.T) {
	var sealed atomic.Bool
	g := artparse.Guard(context.Background(), 30*time.Millisecond, 5*time.Second, func() { sealed.Store(true) }, noPump(),
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if !g.TimedOut || g.Cancelled || g.Abandoned || !errors.Is(g.Err, context.DeadlineExceeded) {
		t.Fatalf("got %+v", g)
	}
	if sealed.Load() {
		t.Error("seal ran although fn returned in time")
	}
}

func TestGuardAbandonsUncooperativeFn(t *testing.T) {
	base := runtime.NumGoroutine()
	release := make(chan struct{})
	exited := make(chan struct{})
	var sealed atomic.Bool
	var sealedBeforeReturn bool
	g := artparse.Guard(context.Background(), 20*time.Millisecond, 30*time.Millisecond, func() { sealed.Store(true) }, noPump(),
		func(context.Context) error {
			defer close(exited)
			<-release
			return errors.New("late")
		})
	sealedBeforeReturn = sealed.Load()
	if !g.Abandoned || !g.TimedOut || g.Err != nil || g.Panic != nil {
		t.Fatalf("got %+v", g)
	}
	if !sealedBeforeReturn {
		t.Error("seal must run before guard returns Abandoned")
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the released fn could not send its result and exit (result channel must be buffered)")
	}
	for i := 0; i < 200 && runtime.NumGoroutine() > base; i++ { // the goroutine must really be gone, not blocked sending
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("%d goroutines after the released fn returned, %d before: it is blocked sending its result", n, base)
	}
}

func TestGuardParentCancelIsCancelledNotTimedOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() { <-started; cancel() }()
	g := artparse.Guard(ctx, time.Hour, 5*time.Second, func() {}, noPump(), func(c context.Context) error {
		close(started)
		<-c.Done()
		return c.Err()
	})
	if !g.Cancelled || g.TimedOut || g.Abandoned || !errors.Is(g.Err, context.Canceled) {
		t.Fatalf("got %+v", g)
	}
}

func goid() string {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	return strings.Fields(string(b))[1]
}

func TestGuardPumpRunsOnCallerGoroutine(t *testing.T) {
	pump := make(chan func(), 16)
	caller := goid()
	var order []int
	var ids []string
	g := artparse.Guard(context.Background(), time.Second, time.Second, func() {}, pump, func(context.Context) error {
		for i := 0; i < 5; i++ {
			pump <- func() { order = append(order, i); ids = append(ids, goid()) }
		}
		return nil
	})
	if g.Err != nil || g.Panic != nil {
		t.Fatalf("got %+v", g)
	}
	if len(order) != 5 {
		t.Fatalf("ran %v, want all 5 before guard returned", order)
	}
	for i, v := range order {
		if v != i || ids[i] != caller {
			t.Errorf("closure %d ran as %d on goroutine %s, caller is %s", i, v, ids[i], caller)
		}
	}
	pump <- func() { order = append(order, 99) }
	time.Sleep(20 * time.Millisecond)
	if len(order) != 5 {
		t.Errorf("a closure ran after guard returned: %v", order)
	}
}
