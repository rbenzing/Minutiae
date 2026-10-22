package evidence

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// VerifyProgress is a read-only observer of one case verify. It is handed to VerifyObserved, which
// feeds it; the caller polls Snapshot from any goroutine. It has no exported mutator, so a holder
// can watch a verify but never change, skip or slow a check.
type VerifyProgress struct {
	mu      sync.Mutex
	stack   []*progressFrame // the running phases, outermost first
	seq     atomic.Uint64
	updates atomic.Uint64 // counter updates only (a test seam: one per chunk)
}

// progressFrame is one running phase.
type progressFrame struct {
	name    string
	started time.Time
	counted bool
	unit    string
	total   int64
	done    atomic.Int64
}

// VerifyProgressSnapshot is one consistent look at a verify in progress.
type VerifyProgressSnapshot struct {
	Phase   string    // innermost running phase, "" between phases
	Started time.Time // when Phase started (zero when Phase is "")
	Counted bool      // Phase has an inner counter
	Done    int64     // units done in Phase, monotonic within it
	Total   int64     // units expected; -1 when unknown
	Unit    string    // "records", "bytes" or "pages"
	Seq     uint64    // changes on every phase start, phase end and counter update
}

// NewVerifyProgress returns an observer with nothing running.
func NewVerifyProgress() *VerifyProgress { return &VerifyProgress{} }

// Snapshot reports the innermost running phase and its counter.
func (p *VerifyProgress) Snapshot() VerifyProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := VerifyProgressSnapshot{Seq: p.seq.Load()}
	if len(p.stack) == 0 {
		return s
	}
	f := p.stack[len(p.stack)-1]
	s.Phase, s.Started, s.Counted = f.name, f.started, f.counted
	if f.counted {
		s.Done, s.Total, s.Unit = f.done.Load(), f.total, f.unit
	}
	return s
}

func (p *VerifyProgress) begin(name string, counted bool, total int64, unit string) *progressFrame {
	f := &progressFrame{name: name, started: time.Now(), counted: counted, total: total, unit: unit}
	p.mu.Lock()
	p.stack = append(p.stack, f)
	p.seq.Add(1)
	p.mu.Unlock()
	return f
}

func (p *VerifyProgress) end(f *progressFrame) {
	p.mu.Lock()
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i] == f {
			p.stack = append(p.stack[:i], p.stack[i+1:]...)
			break
		}
	}
	p.seq.Add(1)
	p.mu.Unlock()
}

// VerifyObserved is VerifyWith that also feeds p (which may be nil: then it is VerifyWith). The
// observer only watches: the report is the same with or without it.
func (c *Case) VerifyObserved(ctx context.Context, p *VerifyProgress, checks ...VerifyCheck) (VerifyReport, error) {
	return c.verifyRun(ctx, nil, p, checks)
}

func noopAdd(int64) {}

// countedPhase is phase for a phase with an inner counter: total units are expected (-1 when
// unknown) and add moves the counter by n. Without an observer add is a no-op.
func (c *Case) countedPhase(name string, total int64, unit string) (done func(), add func(n int64)) {
	p := c.verifyProgress
	if p == nil {
		return c.phase(name), noopAdd
	}
	f := p.begin(name, true, total, unit)
	h := c.verifyPhaseHook
	start := time.Now()
	return func() {
			if h != nil {
				h(name, time.Since(start))
			}
			p.end(f)
		}, func(n int64) {
			f.done.Add(n)
			p.updates.Add(1)
			p.seq.Add(1)
		}
}
