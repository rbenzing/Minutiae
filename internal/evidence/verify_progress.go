package evidence

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// VerifyProgress is a read-only observer of one case verify. It is handed to VerifyObserved, which
// feeds it; the caller polls Snapshot from any goroutine. It has no exported mutator, so a holder
// can watch a verify but never change, skip or slow a check.
type VerifyProgress struct {
	mu      sync.Mutex
	stack   []*progressFrame // the running phases, oldest first (concurrent phases are siblings, not a chain)
	focus   *progressFrame   // the phase whose hook is running now (set under the hook lock), nil otherwise
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
	Also    []string  // the other phases running at the same moment (concurrent phases), oldest first; nil when none
}

// NewVerifyProgress returns an observer with nothing running.
func NewVerifyProgress() *VerifyProgress { return &VerifyProgress{} }

// Snapshot reports the phase that started last, or the one whose hook is running, with its counter, and the other
// phases running at the same moment. Phases of a concurrent verify are siblings: the phases that contain the
// reported one (a parent "fts.index" of "fts.index/<table>/<step>") are not listed beside it, only the ones that run
// next to it. The whole look is taken under one lock.
func (p *VerifyProgress) Snapshot() VerifyProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := VerifyProgressSnapshot{Seq: p.seq.Load()}
	if len(p.stack) == 0 {
		return s
	}
	f := p.stack[len(p.stack)-1]
	if p.focus != nil {
		f = p.focus
	}
	s.Phase, s.Started, s.Counted = f.name, f.started, f.counted
	if f.counted {
		s.Done, s.Total, s.Unit = f.done.Load(), f.total, f.unit
	}
	for _, o := range p.stack {
		if o != f && !strings.HasPrefix(f.name, o.name+"/") {
			s.Also = append(s.Also, o.name)
		}
	}
	return s
}

// setFocus names the phase whose hook is about to run, so that a snapshot taken by the hook names that phase and not
// the one that happens to have started last on another goroutine; nil clears it.
func (p *VerifyProgress) setFocus(f *progressFrame) {
	p.mu.Lock()
	p.focus = f
	p.mu.Unlock()
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

// addTotal adds n to a progress total. A total with a negative term, or one that would overflow, is
// unknown (-1) and stays so: a wrong number would be shown as a fact.
func addTotal(total, n int64) int64 {
	if total < 0 || n < 0 || total > math.MaxInt64-n {
		return -1
	}
	return total + n
}

// artifactBytesTotal is the unit total of the "artifacts" phase: the sizes of every manifest record,
// duplicates included, because hashArtifacts re-hashes every record it is given. -1 when unknown.
func artifactBytesTotal(recs []ManifestRecord) int64 {
	var total int64
	for _, r := range recs {
		total = addTotal(total, r.Size)
	}
	return total
}

// auditedRecordsTotal is the unit total of "records.stream": the records the audit log announced and the
// database holds a batch row for (not an errored or recovered batch). -1 when unknown.
func auditedRecordsTotal(batches []auditedBatch, stored map[batchKey]storedBatch, audit *recordAudit) int64 {
	var total int64
	for _, b := range batches {
		k := batchKey{b.IngestID, b.BatchNo}
		if _, ok := stored[k]; ok && !audit.batchErr[k] && !audit.recovered[k] {
			total = addTotal(total, int64(b.Count))
		}
	}
	return total
}

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
				c.hookWithFocus(p, f, name, time.Since(start))
			}
			p.end(f)
		}, func(n int64) {
			f.done.Add(n)
			p.updates.Add(1)
			p.seq.Add(1)
		}
}
