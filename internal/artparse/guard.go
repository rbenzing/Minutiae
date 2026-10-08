package artparse

import (
	"context"
	"errors"
	"runtime/debug"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
)

// PanicInfo describes a panic that a guarded function raised.
type PanicInfo struct{ Value, Stack string }

// Guarded is the outcome of one guarded call.
type Guarded struct {
	Err       error      // what fn returned (nil when it panicked or was abandoned)
	Panic     *PanicInfo // Value clipped to 1 KiB, Stack to 2 KiB
	TimedOut  bool       // the timeout fired
	Cancelled bool       // the parent context was cancelled
	Abandoned bool       // fn did not return within grace of the cancellation
}

// errGoexit is what a parser goroutine that left without returning or panicking reports.
var errGoexit = errors.New("parser goroutine exited without returning")

const (
	maxPanicValue = 1 << 10
	maxPanicStack = 2 << 10
)

// guard runs fn in its own goroutine under recover(), with a result channel of capacity 1 (an
// abandoned goroutine can always send and exit when it finally returns). ctx is the host's job
// context; guard derives the timeout from it. When it ends, fn gets grace to return; after that guard
// calls seal() first and returns Abandoned, leaving the goroutine to finish on its own. While
// waiting, guard runs the functions received on pump on the CALLING goroutine, which is how progress
// events reach the host without a race; after guard returns nothing is delivered any more.
func guard(ctx context.Context, timeout, grace time.Duration, seal func(), pump <-chan func(), fn func(context.Context) error) Guarded {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan Guarded, 1)
	go func() {
		var g Guarded
		returned := false
		defer func() {
			if r := recover(); r != nil {
				g = Guarded{Panic: &PanicInfo{
					Value: cleanAuditText(panicText(r), maxPanicValue),
					Stack: clipBytes(string(debug.Stack()), maxPanicStack),
				}}
			} else if !returned {
				// runtime.Goexit (or any exit that is neither a return nor a panic): never a clean result
				g = Guarded{Err: errGoexit}
			}
			done <- g
		}()
		g.Err = fn(parse.WithoutDeadline(tctx)) // the host owns every limit: the parser sees cancellation only
		returned = true
	}()

	var flags Guarded
	var graceC <-chan time.Time
	ended := tctx.Done()
	for {
		select {
		case g := <-done:
			g.TimedOut, g.Cancelled = flags.TimedOut, flags.Cancelled
			if g.Err != nil && !g.TimedOut && !g.Cancelled && tctx.Err() != nil {
				// fn answered the end of the context before the loop saw it
				g.Cancelled = ctx.Err() != nil
				g.TimedOut = !g.Cancelled
			}
			drain(pump)
			return g
		case f, ok := <-pump:
			if !ok {
				pump = nil
			} else if f != nil {
				f()
			}
		case <-ended:
			ended = nil // handle once
			if ctx.Err() != nil {
				flags.Cancelled = true
			} else {
				flags.TimedOut = true
			}
			t := time.NewTimer(grace)
			defer t.Stop()
			graceC = t.C
		case <-graceC:
			seal()
			flags.Abandoned = true
			return flags
		}
	}
}

// drain runs the closures already queued on pump.
func drain(pump <-chan func()) {
	for {
		select {
		case f, ok := <-pump:
			if !ok {
				return
			}
			if f != nil {
				f()
			}
		default:
			return
		}
	}
}
