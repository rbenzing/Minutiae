package evidence

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// The concurrent index pass (5D Task 15c): the two full-text tables are decoded together, each on its own
// connection of the read-only pool (verify_readers.go). Nothing is shared that either table writes: each table
// runs on a view of the ftsVerify, with its own problem buffer, its own counters and its own found-side digester;
// when both are over, the views are merged in table order, so the report is the one the serial loop makes.

// readTx runs fn in a read transaction: on a connection of the read-only pool during a concurrent verify, on the
// case connection otherwise.
func (r *ftsVerify) readTx(fn func(ReadHandle) error) error {
	if r.readers != nil {
		return r.readers.readTx(r.ctx, fn)
	}
	return r.c.ReadTx(r.ctx, fn)
}

// indexTables runs the index pass over every table: in table order on this goroutine, or together when a
// read-only pool is present.
func (r *ftsVerify) indexTables() {
	if r.readers == nil {
		for _, table := range FTSTables() {
			if r.dead {
				break
			}
			r.indexTable(table)
		}
		return
	}
	tables := FTSTables()
	// a panic in one table stops the other, whose work would be thrown away; the parent context is not touched
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	base := maps.Clone(r.ps.counts) // the problem counts every view starts from
	var obsMu sync.Mutex
	views := make([]*ftsVerify, len(tables))
	panics := make([]any, len(tables))
	var wg sync.WaitGroup
	for i, table := range tables {
		v := r.tableView(ctx, &obsMu)
		views[i] = v
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					panics[i] = fmt.Sprint("the index pass of ", table, " panicked: ", p)
					cancel()
				}
			}()
			v.indexTable(table)
		}()
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(p) // fail closed on the verify goroutine, as a panic of the serial loop would
		}
	}
	for i, v := range views {
		r.mergeView(i, v, base)
	}
}

// tableView is a copy of r for one table's goroutine. Everything the index pass of a table writes is replaced by a
// private value (mergeView puts it back); what is shared is read only during the pass: the expected side, the
// audited batches, the tokenizer reference, the case and the read-only pool. A new field of ftsVerify that the index
// pass writes must be given its private value here and merged in mergeView.
func (r *ftsVerify) tableView(ctx context.Context, obsMu *sync.Mutex) *ftsVerify {
	v := *r
	v.ctx = ctx
	v.rep = &VerifyReport{Problems: []string{}, Notices: []string{}}
	counts := maps.Clone(r.ps.counts)
	v.ps = &problemSet{rep: v.rep, counts: counts}
	v.problems = map[string]int{}
	v.listed = map[string]map[int64]bool{}
	v.idxHits = nil
	v.bucketsCompared, v.unrefOps = 0, 0
	v.dead, v.indexFailed = false, false
	if r.found != nil {
		v.found = r.found.fork()
	}
	if obs := r.observe; obs != nil {
		v.observe = func(table string, rows int) {
			obsMu.Lock()
			defer obsMu.Unlock()
			obs(table, rows)
		}
	}
	return &v
}

// mergeView puts the result of table ti's pass back into r: its problems after those of the tables before it, its
// counters, and the per-table state the later steps read.
func (r *ftsVerify) mergeView(ti int, v *ftsVerify, base map[string]int) {
	r.rep.Problems = append(r.rep.Problems, v.rep.Problems...)
	r.rep.Notices = append(r.rep.Notices, v.rep.Notices...)
	r.rep.cancelCut = r.rep.cancelCut || v.rep.cancelCut
	for k, n := range v.ps.counts {
		r.ps.counts[k] += n - base[k]
	}
	for t, n := range v.problems {
		r.problems[t] += n
	}
	for k, set := range v.listed {
		r.listed[k] = set
	}
	r.dead = r.dead || v.dead
	r.indexFailed = r.indexFailed || v.indexFailed
	r.bucketsCompared += v.bucketsCompared
	r.unrefOps += v.unrefOps
	r.bad[ti], r.segFailed[ti], r.failing[ti] = v.bad[ti], v.segFailed[ti], v.failing[ti]
	if r.found != nil {
		r.found.sums[ti], r.found.outside[ti] = v.found.sums[ti], v.found.outside[ti]
		r.found.haveLast = false
	}
}
