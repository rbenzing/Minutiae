package evidence

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Concurrent phases of case verify (5D Task 15c). P10 (records.integrity_check, SQLite's own check, the longest
// phase) runs on its own goroutine and its own read-only connection (verify_readers.go) beside the record stream
// and the index pass; the two full-text tables are decoded together (verify_ftsconc.go).
//
// The report is the one of the serial order. P10 writes into a report of its own; the problems it found are put
// into the real report at the position P10 has in the serial order (right after the schema problems), when verify
// joins it, so they come first, as before. A cancel ends every goroutine through the one verify context, and a
// finding of any goroutine is merged whether or not the verify was cancelled: the join never discards a result.

// integrityRun is P10 running beside the rest of verifyRecords.
type integrityRun struct {
	done     chan struct{}
	cancel   context.CancelFunc
	rep      VerifyReport // what P10 found; owned by the goroutine until done is closed
	at       int          // where P10's problems go in the real report
	joined   bool
	panicked bool
	panicVal any
}

// startIntegrityCheck starts P10 on its own goroutine, reading through readers. at is len(rep.Problems) at the
// point where the serial order runs P10. end ends the phase (c.phase) and is called on the P10 goroutine once its
// findings are in rep, so a phase hook sees them.
func (c *Case) startIntegrityCheck(ctx context.Context, readers *verifyReaders, at int, end func()) *integrityRun {
	// P10's queries run under a context of their own, so that a panic of the verify goroutine can stop it; its
	// "was it the end of the verify" decisions are made on the verify context
	qctx, cancel := context.WithCancel(ctx)
	run := &integrityRun{done: make(chan struct{}), cancel: cancel, rep: VerifyReport{Problems: []string{}, Notices: []string{}}, at: at}
	go func() {
		defer close(run.done)
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				run.panicked, run.panicVal = true, p
			}
		}()
		defer end()
		c.integrityCheck(ctx, qctx, readers.readTx, &run.rep)
	}()
	return run
}

// join waits for P10 and puts what it found into rep, at the position of the serial order. A panic of the P10
// goroutine continues on this one (a verify never continues with a phase that did not finish).
func (run *integrityRun) join(rep *VerifyReport) {
	<-run.done
	run.joined = true
	if run.panicked {
		panic(fmt.Sprint("records.integrity_check panicked: ", run.panicVal))
	}
	rep.Problems = slices.Insert(rep.Problems, run.at, run.rep.Problems...)
	rep.cancelCut = rep.cancelCut || run.rep.cancelCut
}

// abortUnlessJoined stops P10 and waits for it when it was not joined: the verify goroutine is unwinding from a
// panic and the result is not wanted. After a join it does nothing.
func (run *integrityRun) abortUnlessJoined() {
	if run.joined {
		return
	}
	run.cancel()
	<-run.done
}

// overlapSpan starts timing the stretch of the record part in which phases may overlap (from P10's start to the
// end of the index pass) and returns the function that reports it to the phase hook as VerifyOverlapPhase. It is a
// hook event only: it is no frame of the progress observer, so a status line never names it.
func (c *Case) overlapSpan() func() {
	if c.verifyPhaseHook == nil {
		return func() {}
	}
	start := time.Now()
	return func() { c.callPhaseHook(VerifyOverlapPhase, time.Since(start)) }
}

// VerifyOverlapPhase is the phase hook name of the wall time from the start of P10 to the end of the index pass.
// The phases inside it (VerifyPhaseOverlapped) may run at the same time, so their times must not be added to
// each other or to it: a sum of phase times counts the top-level phases that are not VerifyPhaseOverlapped, with
// this one in their place.
const VerifyOverlapPhase = "records.overlap"

// VerifyPhaseOverlapped reports whether the top-level phase name lies inside VerifyOverlapPhase.
func VerifyPhaseOverlapped(name string) bool {
	switch name {
	case "records.integrity_check", "fts.selftest", "records.stream", "records.batches", "records.lifecycle", "fts.index":
		return true
	}
	return false
}

// callPhaseHook calls the phase hook. Concurrent phases end on different goroutines; the calls (and the snapshots
// a hook takes) are serialised, so a hook is never entered twice at once.
func (c *Case) callPhaseHook(name string, d time.Duration) {
	c.verifyHookMu.Lock()
	defer c.verifyHookMu.Unlock()
	c.verifyPhaseHook(name, d)
}
