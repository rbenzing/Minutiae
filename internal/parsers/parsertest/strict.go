package parsertest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	// Violations are contract rules the run found broken after or beyond a
	// single record (an input changed, too many warnings), in strict and lenient
	// mode alike.
	Violations []error
	// LateCalls counts Emit, Warn, Note and Progress calls made after Run
	// returned (the emitter is sealed by then and refuses them). It is shared
	// with the emitter, so a call made later shows in a Result already returned.
	LateCalls *atomic.Int64
}

// Late returns the number of late calls counted so far.
func (r Result) Late() int64 {
	if r.LateCalls == nil {
		return 0
	}
	return r.LateCalls.Load()
}

// AssertNoLateCalls fails t when a parser called its emitter after Run
// returned. Run applies it once, at the end of a strict run; call it again at
// the end of a test to catch an abandoned parser that woke up later.
func (r Result) AssertNoLateCalls(t testing.TB) {
	t.Helper()
	if n := r.Late(); n > 0 {
		t.Errorf("parsertest: the parser called its emitter %d time(s) after Run returned", n)
	}
}

// errStrictStop is returned to the parser after the strict emitter reported a
// failure, so the parser stops at the first one.
var errStrictStop = errors.New("parsertest: strict emitter stopped the parser")

// Run drives p.Parse with a strict emitter: every record is validated by a real
// records.Writer on the harness case (the writer's own rules, not a copy), and
// the test FAILS on the first rejection or warning. Records the writer accepted
// are in Result.Records (deep copies). The readers of in are sealed, and the
// emitter is sealed, when Run returns, also when the parser panics (the panic
// propagates); a later call is refused with parse.ErrSealed and counted in
// Result.LateCalls.
//
// Run enforces the contract rules that one run can decide, with the same pure
// functions the host uses (internal/parse): a record's type is in Meta.Emits and
// its artifact's platform is in Meta.Platforms (strict: a failure; lenient: a
// refused record, a Warning); the writer refusing a record is a failure
// (parse.CheckRefusals); the inputs, re-hashed after the run, equal the
// manifest (parse.CheckInputsUnchanged); the warnings stay within
// parse.MaxWarnings; no emitter call is made after Run returned.
//
// NOT checked here: cancellation (use AssertHonoursCancel), determinism (use
// AssertDeterministic), the timeout and grace period, the record cap and the
// recovered-artifact propagation of the host's emitter.
func (h *Harness) Run(p parse.Parser, in *parse.Input) Result {
	h.T.Helper()
	return h.run(context.Background(), p, in, true)
}

// RunLenient is Run that collects rejections and warnings in Result.Warnings
// and broken rules in Result.Violations instead of failing the test.
func (h *Harness) RunLenient(p parse.Parser, in *parse.Input) Result {
	h.T.Helper()
	return h.run(context.Background(), p, in, false)
}

// AssertHonoursCancel runs p.Parse (lenient) with a context that is already
// cancelled and requires it to return within `within`, with no error or one
// wrapping context.Canceled. A parser that finishes before it polls the context
// passes, so give it an input large enough to reach a poll (parse.Tick polls
// every parse.TickEvery iterations). When the parser does not return in time
// the failure is reported, the run is left to finish in the background and
// waited for in a t.Cleanup (so release a blocked parser before the test ends).
func (h *Harness) AssertHonoursCancel(p parse.Parser, in *parse.Input, within time.Duration) Result {
	h.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan Result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.T.Errorf("parsertest: the parser panicked on a cancelled context: %v", r)
				done <- Result{}
			}
		}()
		done <- h.run(ctx, p, in, false)
	}()
	select {
	case res := <-done:
		if res.Err != nil && !errors.Is(res.Err, context.Canceled) {
			h.T.Errorf("parsertest: the parser returned %v on a cancelled context, not the context's error", res.Err)
		}
		return res
	case <-time.After(within):
		h.T.Errorf("parsertest: the parser did not return within %v of a cancelled context", within)
		h.T.Cleanup(func() { <-done })
		return Result{}
	}
}

// sha256Of hashes the whole of an unwrapped artifact reader.
func sha256Of(a parse.Artifact) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(a.R, 0, a.Size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (h *Harness) run(ctx context.Context, p parse.Parser, in *parse.Input, strict bool) Result {
	h.T.Helper()
	set, err := h.readersOf(in)
	if err != nil {
		h.T.Errorf("%v", err)
		return Result{Err: err}
	}
	defer set.sealAll()

	meta := p.Meta()
	wctx := context.Background() // the parser's context never reaches the writer
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
	if err := w.Start(wctx, records.StartOptions{Artifacts: ids, AllowReingest: true}); err != nil {
		h.T.Errorf("parsertest: records writer start: %v", err)
		return Result{Err: err}
	}
	warn := h.warnSink
	if warn == nil {
		warn = w.Warn
	}
	em := &emitter{h: h, w: w, warn: warn, strict: strict, meta: meta, bundle: set.orig, res: Result{Notes: map[string]string{}, LateCalls: new(atomic.Int64)}}
	concluded := false
	defer func() {
		if !concluded { // a panic: release the case's live-ingest slot and let the panic go on
			em.seal()
			_, _ = w.Abort(wctx, errors.New("parsertest: parser panicked"))
		}
	}()

	perr := p.Parse(parse.WithoutDeadline(ctx), in, em)
	concluded = true
	em.seal()
	if h.afterSeal != nil {
		h.afterSeal()
	}
	if perr != nil && !errors.Is(perr, errStrictStop) {
		em.res.Err = perr
		if strict {
			h.T.Errorf("parsertest: %s returned an error: %v", meta.Name, perr)
		}
	}
	if err := parse.CheckInputsUnchanged(set.orig, sha256Of); err != nil {
		em.violate(err)
	}
	if perr == nil && !em.stopped {
		if _, err := w.End(wctx); err != nil {
			h.T.Errorf("parsertest: records writer end: %v", err)
		}
	} else {
		_ = w.Flush(wctx)
		if _, err := w.Abort(wctx, perr); err != nil {
			h.T.Errorf("parsertest: records writer abort: %v", err)
		}
	}
	res := em.snapshot()
	if strict {
		res.AssertNoLateCalls(h.T)
	}
	return res
}

// emitter is the harness's parse.Emitter. It ignores the parser's context (a
// parser may pass one that never ends) and holds no lock across the writer's
// context. Once sealed it refuses every call and counts it.
type emitter struct {
	h      *Harness
	w      *records.Writer
	warn   func(ctx context.Context, locator, reason string) error
	strict bool
	meta   parse.Meta
	bundle []parse.Artifact

	mu      sync.Mutex
	res     Result
	stopped bool
	sealed  bool
	refused int
}

func locatorOf(r records.Record) string {
	if r.Locator != "" {
		return r.Locator
	}
	return r.SourcePath
}

// seal makes every later call fail with parse.ErrSealed.
func (e *emitter) seal() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sealed = true
}

// late counts a call made after the seal. The caller holds e.mu.
func (e *emitter) late() { e.res.LateCalls.Add(1) }

// violate records a broken rule: Result.Violations, and a failure when strict.
func (e *emitter) violate(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.res.Violations = append(e.res.Violations, err)
	if e.strict {
		e.h.T.Errorf("parsertest: %v", err)
	}
}

// snapshot returns a copy of the result, so nothing a late call could touch is
// shared with the caller.
func (e *emitter) snapshot() Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.res
	r.Records = slices.Clone(r.Records)
	r.Warnings = slices.Clone(r.Warnings)
	r.Notes = maps.Clone(r.Notes)
	r.Progress = slices.Clone(r.Progress)
	r.Violations = slices.Clone(r.Violations)
	return r
}

// refuse handles a record that a rule or the writer refused. The caller holds e.mu.
func (e *emitter) refuse(r records.Record, err error) error {
	e.refused++
	if e.strict {
		e.stopped = true
		e.h.T.Errorf("parsertest: %v: record refused (locator %q): %v", parse.CheckRefusals(e.refused), locatorOf(r), err)
		return errStrictStop
	}
	return e.addWarning(Warning{Locator: locatorOf(r), Reason: "record refused: " + err.Error()}, false)
}

// addWarning collects a warning (forwarding it to the writer's audit when
// forward is set), stopping the parser at parse.MaxWarnings. The caller holds e.mu.
func (e *emitter) addWarning(w Warning, forward bool) error {
	if err := parse.CheckWarningCount(len(e.res.Warnings) + 1); err != nil {
		e.stopped = true
		e.res.Violations = append(e.res.Violations, err)
		return errStrictStop
	}
	e.res.Warnings = append(e.res.Warnings, w)
	if forward {
		return e.warn(context.Background(), w.Locator, w.Reason)
	}
	return nil
}

func (e *emitter) Emit(_ context.Context, r records.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		e.late()
		return parse.ErrSealed
	}
	if e.stopped {
		return errStrictStop
	}
	if err := parse.CheckRecord(e.meta, e.bundle, r); err != nil {
		return e.refuse(r, err)
	}
	err := e.w.Add(context.Background(), r) // Add audits and counts a rejection itself: no Warn here
	switch {
	case err == nil:
		e.res.Records = append(e.res.Records, copyRecord(r))
		return nil
	case errors.Is(err, records.ErrInvalidRecord):
		return e.refuse(r, err)
	default:
		e.stopped = true
		e.h.T.Errorf("parsertest: records writer failed: %v", err)
		return err
	}
}

func (e *emitter) Warn(_ context.Context, locator, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		e.late()
		return parse.ErrSealed
	}
	if e.stopped {
		return errStrictStop
	}
	if e.strict {
		e.stopped = true
		e.h.T.Errorf("parsertest: parser warned (locator %q): %s", locator, reason)
		return errStrictStop
	}
	return e.addWarning(Warning{Locator: locator, Reason: reason}, true)
}

func (e *emitter) Note(key, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		e.late()
		return
	}
	e.res.Notes[key] = value
}

func (e *emitter) Progress(done, total int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		e.late()
		return
	}
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
