package examine

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// reproduceWorkerLimit is the most workers one parent group of the reproduce check uses.
const reproduceWorkerLimit = 4

// reproduceWorkers is how many workers the reproduce check uses: min(GOMAXPROCS, reproduceWorkerLimit).
func reproduceWorkers() int {
	if reproduceWorkersOverride > 0 {
		return reproduceWorkersOverride
	}
	return max(1, min(runtime.GOMAXPROCS(0), reproduceWorkerLimit))
}

// reproduceResult is what one artifact's reproduction found.
type reproduceResult struct {
	problems   []string // already formatted "reproduce: ..." texts, in the order the reproduction found them
	reproduced bool
	done       bool // false when the context ended before or during this artifact
}

// reproducePool runs f on every item on at most workers goroutines; out[i] is f(items[i]) for every item
// that finished, and the zero value (done false) for one that did not start because ctx ended. A panic of f
// is recovered into a problem of that item. Every goroutine has returned when it returns, and it never drops
// a finished result, whatever the context did (a cancel must not hide a finding made before it; this is why
// the pool is not evidence.mapOrdered, which returns nothing on a cancel).
func reproducePool[T any](ctx context.Context, workers int, items []T, f func(T) reproduceResult) []reproduceResult {
	out := make([]reproduceResult, len(items))
	if len(items) == 0 {
		return out
	}
	workers = max(1, min(workers, len(items)))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(items) || ctx.Err() != nil {
					return
				}
				out[i] = runProtected(items[i], f)
			}
		}()
	}
	wg.Wait()
	return out
}

// runProtected is f(item) with a panic turned into that item's problem.
func runProtected[T any](item T, f func(T) reproduceResult) (res reproduceResult) {
	defer func() {
		if p := recover(); p != nil {
			res = reproduceResult{problems: []string{fmt.Sprintf("reproduce: check panicked: %v", p)}, done: true}
		}
	}()
	return f(item)
}
