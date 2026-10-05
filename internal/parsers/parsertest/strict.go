package parsertest

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// Warning is a warning, or a record the writer refused, that a run collected.
type Warning struct{ Locator, Reason string }

// Result is what a run collected. Err is the error the parser returned.
type Result struct {
	Records  []records.Record
	Warnings []Warning
	Notes    map[string]string
	Progress [][2]int64
	Err      error
}

// errStrictStop is returned to the parser after the strict emitter reported a
// failure, so the parser stops at the first one.
var errStrictStop = errors.New("parsertest: strict emitter stopped the parser")

// Run drives p.Parse with a strict emitter: every record is validated by a real
// records.Writer on the harness case (the writer's own rules, not a copy), and
// the test FAILS on the first rejection or warning. Records the writer accepted
// are in Result.Records (deep copies). The readers of in are sealed when Run
// returns, also when the parser panics (the panic propagates).
func (h *Harness) Run(p parse.Parser, in *parse.Input) Result {
	h.T.Helper()
	return h.run(p, in, true)
}

// RunLenient is Run that collects rejections and warnings in Result.Warnings
// instead of failing the test.
func (h *Harness) RunLenient(p parse.Parser, in *parse.Input) Result {
	h.T.Helper()
	return h.run(p, in, false)
}

func (h *Harness) run(p parse.Parser, in *parse.Input, strict bool) Result {
	h.T.Helper()
	set, err := h.readersOf(in)
	if err != nil {
		h.T.Errorf("%v", err)
		return Result{Err: err}
	}
	defer set.sealAll()

	meta := p.Meta()
	ctx := context.Background() // the parser's context never reaches the writer
	w, err := records.NewWriter(h.Case, records.Parser{Name: meta.Name, Version: meta.Version, Hash: FakeHash(meta.Name)}, records.WriterOptions{})
	if err != nil {
		h.T.Errorf("parsertest: records writer: %v", err)
		return Result{Err: err}
	}
	ids := []string{in.Primary.ID}
	for _, a := range in.Artifacts {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if err := w.Start(ctx, records.StartOptions{Artifacts: ids, AllowReingest: true}); err != nil {
		h.T.Errorf("parsertest: records writer start: %v", err)
		return Result{Err: err}
	}
	concluded := false
	defer func() {
		if !concluded { // a panic: release the case's live-ingest slot and let the panic go on
			_, _ = w.Abort(ctx, errors.New("parsertest: parser panicked"))
		}
	}()

	em := &emitter{h: h, w: w, strict: strict, res: Result{Notes: map[string]string{}}}
	perr := p.Parse(ctx, in, em)
	concluded = true
	if perr != nil && !errors.Is(perr, errStrictStop) {
		em.res.Err = perr
		if strict {
			h.T.Errorf("parsertest: %s returned an error: %v", meta.Name, perr)
		}
	}
	if perr == nil && !em.stopped {
		if _, err := w.End(ctx); err != nil {
			h.T.Errorf("parsertest: records writer end: %v", err)
		}
	} else {
		_ = w.Flush(ctx)
		if _, err := w.Abort(ctx, perr); err != nil {
			h.T.Errorf("parsertest: records writer abort: %v", err)
		}
	}
	return em.res
}

// emitter is the harness's parse.Emitter. It ignores the parser's context (a
// parser may pass one that never ends) and holds no lock across the writer's
// context.
type emitter struct {
	h      *Harness
	w      *records.Writer
	strict bool

	mu      sync.Mutex
	res     Result
	stopped bool
}

func locatorOf(r records.Record) string {
	if r.Locator != "" {
		return r.Locator
	}
	return r.SourcePath
}

func (e *emitter) Emit(_ context.Context, r records.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return errStrictStop
	}
	err := e.w.Add(context.Background(), r) // Add audits and counts a rejection itself: no Warn here
	switch {
	case err == nil:
		e.res.Records = append(e.res.Records, copyRecord(r))
		return nil
	case errors.Is(err, records.ErrInvalidRecord):
		if e.strict {
			e.stopped = true
			e.h.T.Errorf("parsertest: record refused by the writer (locator %q): %v", locatorOf(r), err)
			return errStrictStop
		}
		e.res.Warnings = append(e.res.Warnings, Warning{Locator: locatorOf(r), Reason: "record refused: " + err.Error()})
		return nil
	default:
		e.stopped = true
		e.h.T.Errorf("parsertest: records writer failed: %v", err)
		return err
	}
}

func (e *emitter) Warn(_ context.Context, locator, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return errStrictStop
	}
	if e.strict {
		e.stopped = true
		e.h.T.Errorf("parsertest: parser warned (locator %q): %s", locator, reason)
		return errStrictStop
	}
	e.res.Warnings = append(e.res.Warnings, Warning{Locator: locator, Reason: reason})
	return e.w.Warn(context.Background(), locator, reason)
}

func (e *emitter) Note(key, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.res.Notes[key] = value
}

func (e *emitter) Progress(done, total int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.res.Progress = append(e.res.Progress, [2]int64{done, total})
}

// copyRecord deep-copies r so a parser that keeps and changes what it emitted
// cannot change the result.
func copyRecord(r records.Record) records.Record {
	if r.Range != nil {
		v := *r.Range
		r.Range = &v
	}
	if r.Time != nil {
		v := *r.Time
		r.Time = &v
	}
	if r.TimeEnd != nil {
		v := *r.TimeEnd
		r.TimeEnd = &v
	}
	if r.Confidence != nil {
		v := *r.Confidence
		r.Confidence = &v
	}
	r.Times = slices.Clone(r.Times)
	r.Payload = common.CopyMap(r.Payload)
	return r
}

var _ parse.Emitter = (*emitter)(nil)
