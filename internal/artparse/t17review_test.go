package artparse_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// An ErrIntegrity found while opening the inputs is integrity (exit 4) even when the host context is
// cancelled at the same moment.
func TestIntegrityBeatsCancelWhileOpening(t *testing.T) {
	f := newRx(t, "grown")
	file := filepath.Join(f.c.Dir, filepath.FromSlash(f.recs["grown"].Path))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.host(func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(string) {
			// the file grows (size mismatch: integrity) and the host is cancelled in the same instant
			fh, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = fh.WriteString("more")
			_ = fh.Close()
			cancel()
		})
	}, wbp("grown"))
	sum, err := h.Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j := jobOf(t, sum, "grown")
	if !j.Integrity || j.Outcome != artparse.OutcomeRefused || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v: the integrity finding was dropped for the cancel", j, sum.Class())
	}
}

// blockingLockedAdd holds a lock inside Add (until released) that Flush and Abort need too, like a
// writer whose Add is stuck holding its own lock.
type blockingLockedAdd struct {
	artparse.IngestWriter
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingLockedAdd) Add(ctx context.Context, r records.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return w.IngestWriter.Add(ctx, r)
}

func (w *blockingLockedAdd) Flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.IngestWriter.Flush(ctx)
}

func (w *blockingLockedAdd) Abort(ctx context.Context, cause error) (records.IngestResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.IngestWriter.Abort(ctx, cause)
}

// Flush and Abort wait at most the grace period behind a stuck writer call; then the job is abandoned.
func TestFlushAndAbortAreBoundedBehindAStuckWriterCall(t *testing.T) {
	defer artparse.SetConcludeJoinTimeout(200 * time.Millisecond)()
	f := newRx(t, "stuck", "next")
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	p := &rxParser{name: "stuck", after: func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		go func() { _ = out.Emit(context.WithoutCancel(ctx), rxRecord(in, 1)) }()
		<-entered
		return nil
	}}
	h := f.host(both(shortTimeouts, func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(c *evidence.Case, pr records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, pr, wo)
			if err != nil {
				return nil, err
			}
			return &blockingLockedAdd{IngestWriter: w, entered: entered, release: release}, nil
		})
	}), p, wbp("next"))
	type result struct {
		sum artparse.RunSummary
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := h.Run(context.Background(), artparse.RunOptions{})
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, artparse.ErrConclusionPending) {
			t.Fatalf("Run error %v, want ErrConclusionPending: the conclusion is still pending", r.err)
		}
		j := jobOf(t, r.sum, "stuck")
		if !j.Abandoned || j.Outcome != artparse.OutcomeIncomplete || r.sum.Stopped != "abandoned" || !strings.Contains(j.Reason, "concluding the ingest did not finish within the grace period (a writer call is stuck)") {
			t.Fatalf("job %+v stopped %q err %v", j, r.sum.Stopped, r.err)
		}
		if jobOf(t, r.sum, "next").Outcome != artparse.OutcomeNotRun {
			t.Error("the run went on after the abandonment")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run is still waiting behind the stuck writer call: Flush/Abort are not bounded by the grace period")
	}
}

// A cancel between two jobs: no further job starts.
func TestCancelBetweenJobsStartsNoFurtherJob(t *testing.T) {
	f := newRx(t, "j1", "j2", "j3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.host(nil, wbp("j1"), wbp("j2"), wbp("j3"))
	sum, err := h.Run(ctx, artparse.RunOptions{OnEvent: func(e artparse.Event) {
		if e.Kind == "job.end" && e.Job == 1 {
			cancel()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Cancelled || sum.Stopped != "cancelled" || jobOf(t, sum, "j1").Outcome != artparse.OutcomeComplete {
		t.Fatalf("summary %+v", sum)
	}
	for _, n := range []string{"j2", "j3"} {
		if jobOf(t, sum, n).Outcome != artparse.OutcomeNotRun {
			t.Errorf("%s: %+v", n, jobOf(t, sum, n))
		}
	}
	if n := len(f.jobStarts()); n != 1 {
		t.Errorf("%d parse.job.start entries, want 1", n)
	}
}

// A parser that fails on its own after the host was cancelled still marks the run cancelled.
func TestParserFailureAfterCancelMarksTheRunCancelled(t *testing.T) {
	for name, fail := range map[string]func() error{
		"error": func() error { return errors.New("own failure") },
		"panic": func() error { panic("own panic") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRx(t, "c", "after")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &rxParser{name: "c", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error {
				cancel()
				return fail()
			}}
			sum, err := f.host(nil, p, wbp("after")).Run(ctx, artparse.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !sum.Cancelled || sum.Stopped != "cancelled" || jobOf(t, sum, "c").Outcome != artparse.OutcomeIncomplete {
				t.Errorf("summary %+v job %+v", sum, jobOf(t, sum, "c"))
			}
			if jobOf(t, sum, "after").Outcome != artparse.OutcomeNotRun {
				t.Error("the run went on after the cancel")
			}
		})
	}
}

// An ErrIntegrity from Writer.End is an integrity job and nothing is flushed.
func TestEndIntegrityFailureIsIntegrityAndDoesNotFlush(t *testing.T) {
	f := newRx(t, "e")
	h := f.host(withWriter(func(w *rxWriter) {
		w.endErr = fmt.Errorf("%w: forged", evidence.ErrIntegrity)
		w.flushErr = errors.New("FLUSH-WAS-CALLED")
	}), &rxParser{name: "e", n: 2})
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "e")
	if j.Outcome != artparse.OutcomeIncomplete || !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if strings.Contains(j.Reason, "FLUSH-WAS-CALLED") {
		t.Errorf("an integrity failure flushed: %q", j.Reason)
	}
}

// A Probe that does not stop is sealed when it is abandoned.
func TestAbandonedProbeIsSealed(t *testing.T) {
	f := newRx(t, "pr")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	k := &rxKept{}
	pr := &sealedProbe{name: "pr", kept: k, release: release}
	h := f.host(both(shortTimeouts, withLimits(func(l *parse.Limits) { l.ProbeTimeout = 100 * time.Millisecond })), pr)
	plan, err := h.Plan(context.Background(), artparse.Selection{})
	if err != nil || len(plan.Rows) != 1 || !strings.Contains(plan.Rows[0].Reason, "probe failed") {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	k.assertSealedRead(t)
}

type sealedProbe struct {
	name    string
	kept    *rxKept
	release chan struct{}
}

func (p *sealedProbe) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *sealedProbe) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	p.kept.store(in, nil)
	<-p.release // ignores the context
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p *sealedProbe) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func (k *rxKept) assertSealedRead(t *testing.T) {
	t.Helper()
	in, _ := k.load()
	var b [1]byte
	if _, err := in.Primary.R.ReadAt(b[:], 0); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("read through an abandoned Probe's input = %v, want ErrSealed", err)
	}
}

// The reasons of the resource-limit failures are exact.
func TestResourceLimitReasonsAreExact(t *testing.T) {
	f := newRx(t, "hog", "spam", "inv")
	h := f.host(withLimits(func(l *parse.Limits) { l.MemBudget = 16 << 20; l.MaxRecords = 2; l.MaxRejected = 3 }),
		parsertest.Hog{Name: "hog", Version: "1.0.0", Alloc: 64 << 20},
		parsertest.Spammer{Name: "spam", Version: "1.0.0", N: 5},
		parsertest.Invalid{Name: "inv", Version: "1.0.0", Mode: parsertest.InvalidNulInSummary, N: 10})
	sum := f.run(h, artparse.Selection{})
	for name, want := range map[string]string{"hog": "memory budget exceeded", "spam": "record cap reached", "inv": "too many rejected records"} {
		if j := jobOf(t, sum, name); j.Reason != want || j.Outcome != artparse.OutcomeIncomplete {
			t.Errorf("%s: reason %q outcome %q, want %q", name, j.Reason, j.Outcome, want)
		}
	}
}

// A parser that panics right after the host was cancelled still marks the run cancelled, also when the
// guard sees the parser's result before it sees the end of the context (the busy host loop makes both
// ready; which one the guard takes is random, so the run is repeated).
func TestPanicAfterCancelMarksTheRunCancelledWhateverTheGuardSaw(t *testing.T) {
	for i := 0; i < 12; i++ {
		f := newRx(t, "c")
		ctx, cancel := context.WithCancel(context.Background())
		p := &rxParser{name: "c", n: 0, after: func(context.Context, *parse.Input, parse.Emitter) error {
			return nil
		}}
		pp := panicAfterCancel{rxParser: p, cancel: cancel}
		sum, err := f.host(nil, pp).Run(ctx, artparse.RunOptions{OnEvent: func(e artparse.Event) {
			if e.Kind == "job.progress" {
				time.Sleep(150 * time.Millisecond) // the host loop is busy while the parser cancels and panics
			}
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !sum.Cancelled || sum.Stopped != "cancelled" {
			t.Fatalf("run %d: summary %+v job %+v", i, sum, jobOf(t, sum, "c"))
		}
	}
}

type panicAfterCancel struct {
	*rxParser
	cancel func()
}

func (p panicAfterCancel) Parse(_ context.Context, _ *parse.Input, out parse.Emitter) error {
	out.Progress(1, 1)
	time.Sleep(20 * time.Millisecond)
	p.cancel()
	panic("own panic")
}
