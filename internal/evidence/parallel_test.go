package evidence

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestMapOrderedKeepsInputOrderAtAnyWorkerCount(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 1000} {
		in := make([]int, n)
		for i := range in {
			in[i] = i
		}
		for _, w := range []int{-3, 0, 1, 2, 8, 64} {
			t.Run(fmt.Sprintf("n%d/w%d", n, w), func(t *testing.T) {
				// the later items finish first, so a result stored in completion order would be reversed
				out, err := mapOrdered(context.Background(), w, in, func(i int) int {
					if i%3 == 0 {
						runtime.Gosched()
					}
					return i * i
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(out) != n {
					t.Fatalf("%d results for %d inputs", len(out), n)
				}
				for i, v := range out {
					if v != i*i {
						t.Fatalf("result %d is %d, want %d", i, v, i*i)
					}
				}
			})
		}
	}
}

func TestMapOrderedPanicFailsClosed(t *testing.T) {
	in := make([]int, 500)
	for i := range in {
		in[i] = i
	}
	for _, w := range []int{1, 2, 8} {
		out, err := mapOrdered(context.Background(), w, in, func(i int) int {
			if i == 123 {
				panic("hostile row")
			}
			return i
		})
		if !errors.Is(err, errVerifyWorker) {
			t.Fatalf("workers %d: error %v does not wrap errVerifyWorker", w, err)
		}
		if out != nil {
			t.Fatalf("workers %d: a failed map returned %d results", w, len(out))
		}
	}
}

// A panic is reported even when the context ended at the same time: the cancel must not mask it.
func TestMapOrderedPanicIsNotMaskedByACancel(t *testing.T) {
	for _, w := range []int{1, 4} {
		ctx, cancel := context.WithCancel(context.Background())
		in := make([]int, 200)
		_, err := mapOrdered(ctx, w, in, func(int) int {
			cancel()
			panic("hostile row")
		})
		cancel()
		if !errors.Is(err, errVerifyWorker) {
			t.Fatalf("workers %d: error %v does not wrap errVerifyWorker", w, err)
		}
	}
}

func TestMapOrderedHonoursCancellation(t *testing.T) {
	in := make([]int, 100000)
	for _, w := range []int{1, 2, 8} {
		ctx, cancel := context.WithCancel(context.Background())
		// mapOrdered tests the context before it takes each item, so once cancel has returned every other
		// worker runs at most the one item it had already taken. The goroutine that cancels may be descheduled
		// between counting its item and calling cancel (load), and the others then run on legitimately, so
		// only the items that start after cancel returned are bounded: at most w-1.
		var calls, after atomic.Int64
		var cancelled atomic.Bool
		out, err := mapOrdered(ctx, w, in, func(int) int {
			if cancelled.Load() {
				after.Add(1)
			}
			if calls.Add(1) == 10 {
				cancel()
				cancelled.Store(true)
			}
			return 0
		})
		cancel()
		if !errors.Is(err, context.Canceled) || errors.Is(err, errVerifyWorker) {
			t.Fatalf("workers %d: error %v, want context.Canceled", w, err)
		}
		if out != nil {
			t.Fatalf("workers %d: a cancelled map returned results", w)
		}
		if n := after.Load(); n > int64(w-1) {
			t.Fatalf("workers %d: %d items started after the cancel returned, at most %d may (one already taken per other worker)", w, n, w-1)
		}
		if calls.Load() < 10 {
			t.Fatalf("workers %d: the cancel never fired", w)
		}
	}
	// a context that ended before the call runs nothing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int64
	if _, err := mapOrdered(ctx, 4, in, func(int) int { calls.Add(1); return 0 }); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d items ran on a cancelled context", calls.Load())
	}
}

// No goroutine outlives the call, on success, on a panic and on a cancel.
func TestMapOrderedLeavesNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	in := make([]int, 2000)
	for range 20 {
		_, _ = mapOrdered(context.Background(), 8, in, func(int) int { return 1 })
		_, _ = mapOrdered(context.Background(), 8, in, func(i int) int {
			if i == 700 {
				panic("x")
			}
			return 1
		})
		ctx, cancel := context.WithCancel(context.Background())
		_, _ = mapOrdered(ctx, 8, in, func(i int) int {
			if i == 5 {
				cancel()
			}
			return 1
		})
		cancel()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("%d goroutines before, %d after", before, after)
	}
}

// At most workers calls of f are in flight, and every result has a slot of its own in the output made for in:
// the memory held is that of one chunk, not of the chunks times anything else.
func TestMapOrderedNeverExceedsTheWorkerCount(t *testing.T) {
	in := make([]int, 3000)
	for _, w := range []int{1, 3, 8} {
		var now, peak atomic.Int64
		out, err := mapOrdered(context.Background(), w, in, func(int) int {
			n := now.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Microsecond)
			now.Add(-1)
			return 1
		})
		if err != nil || len(out) != len(in) {
			t.Fatalf("workers %d: %v, %d results", w, err, len(out))
		}
		if got := int(peak.Load()); got > w {
			t.Fatalf("workers %d: %d calls in flight", w, got)
		}
	}
}
