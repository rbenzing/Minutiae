package evidence

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// errVerifyWorker marks the failure of a verify worker (a panic while it computed the per-row work of a
// chunk). Verify reports it as a problem and never as a clean result.
var errVerifyWorker = errors.New("a verify worker failed")

// verifyWorkerLimit is the most workers verify starts for the per-row work of a chunk.
const verifyWorkerLimit = 8

// verifyWorkers is the number of workers for the per-row work of a chunk: min(GOMAXPROCS, 8), or the
// number a test set.
func (c *Case) verifyWorkers() int {
	if c.verifyWorkersN > 0 {
		return c.verifyWorkersN
	}
	return max(1, min(runtime.GOMAXPROCS(0), verifyWorkerLimit))
}

// mapOrdered applies f to every element of in on at most workers goroutines and returns the results in the
// order of in, whatever order they finish in. f must be pure: it reads shared data and writes nothing shared.
//
// The error is nil when every element was mapped. Otherwise no results are returned: a panic of f is an error
// that wraps errVerifyWorker (it wins over a context that ended at the same time, so a cancel never masks it),
// and a context that ended before every element was mapped is the context's error. Every goroutine has
// returned when mapOrdered does. At most workers elements are in flight, so the results held are those of in.
func mapOrdered[T, R any](ctx context.Context, workers int, in []T, f func(T) R) ([]R, error) {
	out := make([]R, len(in))
	workers = max(1, min(workers, len(in)))
	var (
		next     atomic.Int64
		failOnce sync.Once
		failure  error
		wg       sync.WaitGroup
	)
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	run := func() {
		defer wg.Done()
		defer func() {
			if p := recover(); p != nil {
				failOnce.Do(func() { failure = fmt.Errorf("%w: %v", errVerifyWorker, p) })
				stop() // the others stop at their next element
			}
		}()
		for wctx.Err() == nil {
			i := int(next.Add(1) - 1)
			if i >= len(in) {
				return
			}
			out[i] = f(in[i])
		}
	}
	wg.Add(workers)
	for range workers - 1 {
		go run()
	}
	run() // the calling goroutine is the last worker
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
