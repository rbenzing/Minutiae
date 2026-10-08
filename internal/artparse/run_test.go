package artparse_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// rxCount counts the Probe and Parse calls of a well-behaved parser.
type rxCount struct {
	parsertest.WellBehaved
	probes, parses atomic.Int32
}

func (p *rxCount) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	p.probes.Add(1)
	return p.WellBehaved.Probe(ctx, in)
}

func (p *rxCount) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	p.parses.Add(1)
	return p.WellBehaved.Parse(ctx, in, out)
}

// rewrite flips the first byte of the artifact file on disk (the size stays).
func rewrite(t testing.TB, c *evidence.Case, rec evidence.ManifestRecord) {
	t.Helper()
	file := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	data, err := os.ReadFile(file) //nolint:gosec // inside a temporary test case
	if err != nil {
		t.Error(err)
		return
	}
	data[0] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Error(err)
	}
}

func TestParseRefusesArtifactWhoseHashDiffers(t *testing.T) {
	f := newRx(t, "bad", "good")
	tamper(t, f.c, f.recs["bad"])
	bad := &rxCount{WellBehaved: wbp("bad")}
	sum := f.run(f.host(nil, bad, wbp("good")), artparse.Selection{})
	j := jobOf(t, sum, "bad")
	if j.Outcome != artparse.OutcomeRefused || !j.Integrity || j.IngestID != "" {
		t.Fatalf("tampered job: %+v", j)
	}
	if bad.probes.Load() != 0 || bad.parses.Load() != 0 {
		t.Errorf("parser code ran on an artifact that does not match the manifest: Probe %d, Parse %d", bad.probes.Load(), bad.parses.Load())
	}
	if g := jobOf(t, sum, "good"); g.Outcome != artparse.OutcomeComplete || g.Records != 3 {
		t.Errorf("the healthy job in the same run: %+v", g)
	}
	if sum.Class() != artparse.ClassIntegrity {
		t.Errorf("class = %v, want integrity", sum.Class())
	}
	if n := f.count("records.ingest.start"); n != 1 {
		t.Errorf("%d records.ingest.start entries, want 1 (none for the refused bundle)", n)
	}
	ends := f.jobEnds()
	if len(ends) != 2 || len(f.jobStarts()) != 2 {
		t.Fatalf("job starts %d, ends %d: a refused job is audited too", len(f.jobStarts()), len(ends))
	}
	if !ends[0].Integrity || ends[0].Outcome != artparse.OutcomeRefused || ends[0].IngestID != "" {
		t.Errorf("job.end of the refused job: %+v", ends[0])
	}
}

func TestParseDetectsArtifactChangedDuringJob(t *testing.T) {
	f := newRx(t, "chg", "next")
	p := &rxParser{name: "chg", n: 2}
	p.after = func(context.Context, *parse.Input, parse.Emitter) error { rewrite(t, f.c, f.recs["chg"]); return nil }
	h := f.host(withLimits(func(l *parse.Limits) { l.MemInputMax = 0 }), p, wbp("next")) // MemInputMax 0: the input streams
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "chg")
	if j.Outcome != artparse.OutcomeIncomplete || !j.Integrity {
		t.Fatalf("job %+v, want incomplete with Integrity", j)
	}
	if sum.Class() != artparse.ClassIntegrity {
		t.Errorf("class = %v", sum.Class())
	}
	// the integrity failure never flushes: the two buffered records are not stored, no batch is audited
	if f.count("records.batch") != 1 || f.count("records.ingest.error") != 1 {
		t.Errorf("batches %d, ingest errors %d: want only the healthy job's batch and the aborted ingest", f.count("records.batch"), f.count("records.ingest.error"))
	}
	if n := f.nRecords(); n != 3 {
		t.Errorf("%d records stored, want 3 (the healthy job's): nothing of the changed job may be flushed", n)
	}
	ends := f.jobEnds()
	if !ends[0].Integrity || ends[0].Outcome != artparse.OutcomeIncomplete || len(ends[0].Streamed) != 1 || ends[0].Streamed[0] != f.recs["chg"].ID {
		t.Errorf("job.end %+v: want integrity and the streamed input recorded", ends[0])
	}
	if jobOf(t, sum, "next").Outcome != artparse.OutcomeComplete {
		t.Error("the next job did not run: an integrity failure does not stop the run")
	}
}

func TestInputChangedAfterParseIsIntegrity(t *testing.T) {
	f := newRx(t, "late")
	p := &rxParser{name: "late", n: 2}
	p.after = func(context.Context, *parse.Input, parse.Emitter) error { rewrite(t, f.c, f.recs["late"]); return nil }
	h := f.host(both(func(o *artparse.Options) { artparse.WithBatchRows(o, 1) }), p) // in memory; every record is its own committed batch
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "late")
	if j.Outcome != artparse.OutcomeIncomplete || !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if f.count("records.ingest.error") != 1 || f.count("records.ingest.end") != 0 {
		t.Errorf("ingest errors %d, ends %d: the ingest must be concluded as an error", f.count("records.ingest.error"), f.count("records.ingest.end"))
	}
	if n := f.nRecords(); n != 2 {
		t.Errorf("%d records stored, want the 2 batches committed before the change was seen", n)
	}
	rep, err := f.c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || !strings.Contains(strings.Join(rep.Problems, "\n"), f.recs["late"].ID) {
		t.Errorf("case verify does not report the changed artifact: %v", rep.Problems)
	}
	if ends := f.jobEnds(); len(ends[0].Streamed) != 0 {
		t.Errorf("an in-memory input is listed as streamed: %v", ends[0].Streamed)
	}
}

func TestManifestChangedBetweenDiscoverAndStartIsIntegrity(t *testing.T) {
	f := newRx(t, "man")
	p := &rxCount{WellBehaved: wbp("man")}
	h := f.host(func(o *artparse.Options) {
		artparse.WithAfterStart(o, func() {
			path := filepath.Join(f.c.Dir, "manifest.jsonl")
			b, err := os.ReadFile(path) //nolint:gosec // inside a temporary test case
			if err != nil {
				t.Error(err)
				return
			}
			b = bytes.Replace(b, []byte(f.recs["man"].SHA256), []byte(strings.Repeat("0", 64)), 1)
			if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // inside a temporary test case
				t.Error(err)
			}
		})
	}, p)
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "man")
	if j.Outcome != artparse.OutcomeRefused || !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if p.parses.Load() != 0 {
		t.Error("Parse ran although the manifest no longer matched the snapshot")
	}
	if f.count("records.ingest.error") != 1 {
		t.Errorf("the started ingest was not concluded: %d records.ingest.error", f.count("records.ingest.error"))
	}
}

func TestParseNeverModifiesSource(t *testing.T) {
	f := newRx(t, "a", "b")
	before := caseFingerprint(t, f.c)
	f.run(f.host(nil, wbp("a"), wbp("b")), artparse.Selection{})
	after := caseFingerprint(t, f.c)
	for name := range after {
		if _, existed := before[name]; !existed {
			t.Errorf("a new file appeared in the case: %s", name)
		}
	}
	for name, h := range before {
		got, ok := after[name]
		switch {
		case !ok:
			t.Errorf("%s disappeared", name)
		case got != h && name != "audit.jsonl" && name != "artifacts.db":
			t.Errorf("%s changed by a parse run", name)
		}
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.c.Dir, "artifacts.db-journal")); err == nil {
		t.Error("artifacts.db-journal left behind")
	}
}

func TestParserPanicAbortsJobOthersContinue(t *testing.T) {
	f := newRx(t, "boom", "fine")
	h := f.host(nil, parsertest.Panicking{Name: "boom", Version: "1.0.0", Where: parsertest.PanicAfterEmits, After: 2, Value: "kaboom"}, wbp("fine"))
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "boom")
	if j.Outcome != artparse.OutcomeIncomplete || j.Panic == nil || !strings.Contains(j.Panic.Value, "kaboom") || len(j.Panic.Stack) > 2048 {
		t.Fatalf("job %+v", j)
	}
	if jobOf(t, sum, "fine").Outcome != artparse.OutcomeComplete || sum.Class() != artparse.ClassPartial {
		t.Errorf("the next job or the class: %+v %v", sum.Jobs, sum.Class())
	}
	end := f.jobEnds()[0]
	if end.Panic == nil || end.Panic.Value != "kaboom" || len(end.Panic.Stack) == 0 || len(end.Panic.Stack) > 2048 {
		t.Errorf("job.end panic = %+v", end.Panic)
	}
	if j.Records != 2 || end.Records != 2 {
		t.Errorf("records of the panicked job: result %d, audit %d, want the 2 accepted before the panic (flushed)", j.Records, end.Records)
	}
	if n := f.nRecords(); n != 5 {
		t.Errorf("%d records stored, want 2 flushed + 3", n)
	}
	if f.count("records.ingest.error") != 1 {
		t.Error("the panicked ingest was not concluded as an error")
	}
}

func TestFailedJobFlushesAcceptedRecords(t *testing.T) {
	f := newRx(t, "spam")
	h := f.host(withLimits(func(l *parse.Limits) { l.MaxRecords = 3 }), parsertest.Spammer{Name: "spam", Version: "1.0.0", N: 10})
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "spam")
	if j.Outcome != artparse.OutcomeIncomplete || j.Records != 3 || !strings.Contains(j.Reason, "record cap") {
		t.Fatalf("job %+v", j)
	}
	if f.nRecords() != 3 {
		t.Errorf("%d records stored, want the 3 accepted ones (fewer than a batch)", f.nRecords())
	}
}

func TestFlushAfterCancelUsesDetachedContext(t *testing.T) {
	t.Run("ctrl-c", func(t *testing.T) {
		f := newRx(t, "cc", "never")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &rxParser{name: "cc", n: 2, after: func(c context.Context, _ *parse.Input, _ parse.Emitter) error {
			cancel()
			<-c.Done()
			return c.Err()
		}}
		sum, err := f.host(nil, p, wbp("never")).Run(ctx, artparse.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !sum.Cancelled || sum.Stopped != "cancelled" || jobOf(t, sum, "cc").Outcome != artparse.OutcomeIncomplete {
			t.Fatalf("summary %+v", sum)
		}
		if f.nRecords() != 2 {
			t.Errorf("%d records stored, want the 2 accepted ones: flush and abort run on a detached context", f.nRecords())
		}
		if f.count("records.ingest.error") != 1 {
			t.Error("the cancelled ingest was not concluded")
		}
	})
	t.Run("job timeout", func(t *testing.T) {
		f := newRx(t, "to", "next")
		p := &rxParser{name: "to", n: 2, after: func(c context.Context, _ *parse.Input, _ parse.Emitter) error {
			<-c.Done()
			return c.Err()
		}}
		sum := f.run(f.host(shortTimeouts, p, wbp("next")), artparse.Selection{})
		j := jobOf(t, sum, "to")
		if !j.TimedOut || j.Abandoned || j.Outcome != artparse.OutcomeIncomplete {
			t.Fatalf("job %+v", j)
		}
		if f.nRecords() != 2+3 || jobOf(t, sum, "next").Outcome != artparse.OutcomeComplete || sum.Stopped != "" {
			t.Errorf("records %d, summary %+v: the cooperative timeout stops only its own job", f.nRecords(), sum)
		}
	})
}

func TestIntegrityFailureDoesNotFlush(t *testing.T) {
	f := newRx(t, "chg")
	p := &rxParser{name: "chg", n: 2}
	p.after = func(context.Context, *parse.Input, parse.Emitter) error { rewrite(t, f.c, f.recs["chg"]); return nil }
	f.run(f.host(nil, p), artparse.Selection{})
	if f.count("records.batch") != 0 || f.nRecords() != 0 {
		t.Errorf("batches %d, records %d: records of an integrity failure must not be flushed", f.count("records.batch"), f.nRecords())
	}
}

func TestAbandonedParserCannotEmit(t *testing.T) {
	f := newRx(t, "slow")
	late := &parsertest.LateResults{}
	release, done := make(chan struct{}), make(chan struct{})
	p := parsertest.Slow{Name: "slow", Version: "1.0.0", Release: release, Done: done, Late: late}
	sum := f.run(f.host(shortTimeouts, p), artparse.Selection{})
	j := jobOf(t, sum, "slow")
	if !j.Abandoned || !j.TimedOut || j.Outcome != artparse.OutcomeIncomplete || sum.Class() != artparse.ClassPartial {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned parser never ran on")
	}
	if !errors.Is(late.EmitErr, parse.ErrSealed) || !errors.Is(late.ReadErr, parse.ErrSealed) {
		t.Errorf("late Emit %v, late ReadAt %v: want ErrSealed for both", late.EmitErr, late.ReadErr)
	}
	if f.nRecords() != 0 {
		t.Errorf("%d records stored after an abandoned job that emitted nothing in time", f.nRecords())
	}
}

func TestAbandonmentStopsTheRun(t *testing.T) {
	f := newRx(t, "one", "two", "three")
	// The abandoned job is concluded by a goroutine the host stops waiting for after GracePeriod, so the run
	// can return before the flush and abort have landed. The writer hook reports when Abort has returned.
	aborted := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	two := &rxParser{name: "two", n: 2, after: func(context.Context, *parse.Input, parse.Emitter) error { <-release; return nil }}
	sum := f.run(f.host(both(shortTimeouts, withWriter(func(w *rxWriter) {
		w.aborted = func() { aborted <- struct{}{} }
	})), wbp("one"), two, wbp("three")), artparse.Selection{})
	select {
	case <-aborted: // the one abandoned job concluded: its records and the ingest.error entry are stored
	case <-t.Context().Done():
		t.Fatal("the abandoned job was never concluded")
	case <-time.After(2 * time.Minute): // safety net only: a regression fails instead of hanging
		t.Fatal("the abandoned job was never concluded")
	}
	if sum.Stopped != "abandoned" || sum.Class() != artparse.ClassPartial || len(sum.Jobs) != 3 {
		t.Fatalf("summary %+v class %v", sum, sum.Class())
	}
	if j := jobOf(t, sum, "one"); j.Outcome != artparse.OutcomeComplete {
		t.Errorf("job 1: %+v", j)
	}
	j2 := jobOf(t, sum, "two")
	if j2.Outcome != artparse.OutcomeIncomplete || !j2.Abandoned || j2.Records != 2 {
		t.Errorf("job 2: %+v, want incomplete, abandoned, its 2 accepted records flushed", j2)
	}
	if j3 := jobOf(t, sum, "three"); j3.Outcome != artparse.OutcomeNotRun {
		t.Errorf("job 3: %+v, want not-run", j3)
	}
	if f.count("records.ingest.error") != 1 || f.count("records.ingest.start") != 2 {
		t.Errorf("ingest errors %d, starts %d", f.count("records.ingest.error"), f.count("records.ingest.start"))
	}
	ends := f.jobEnds()
	if len(ends) != 2 || !ends[1].Abandoned {
		t.Fatalf("job.end entries %+v", ends)
	}
	if starts := f.jobStarts(); len(starts) != 2 {
		t.Errorf("%d parse.job.start entries, want 2: the third job must not start", len(starts))
	}
	errs := f.entries("analysis.error")
	if len(errs) != 1 || f.count("analysis.end") != 0 {
		t.Fatalf("analysis.error %d, analysis.end %d", len(errs), f.count("analysis.end"))
	}
	ae := decodeEntry[artparse.AnalysisError](t, errs[0])
	if ae.NotRun != 1 || ae.AnalysisID != sum.ParseID {
		t.Errorf("analysis.error %+v", ae)
	}
	if f.nRecords() != 3+2 {
		t.Errorf("%d records stored", f.nRecords())
	}
}

func TestJobTimeoutAbandons(t *testing.T) {
	f := newRx(t, "slow", "next")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sum := f.run(f.host(shortTimeouts, parsertest.Slow{Name: "slow", Version: "1.0.0", Release: release}, wbp("next")), artparse.Selection{})
	j := jobOf(t, sum, "slow")
	if !j.TimedOut || !j.Abandoned || sum.Stopped != "abandoned" || jobOf(t, sum, "next").Outcome != artparse.OutcomeNotRun {
		t.Fatalf("summary %+v", sum)
	}
}

func TestJobTimeoutCooperative(t *testing.T) {
	f := newRx(t, "slow", "next")
	sum := f.run(f.host(shortTimeouts, parsertest.Slow{Name: "slow", Version: "1.0.0", Cooperative: true}, wbp("next")), artparse.Selection{})
	j := jobOf(t, sum, "slow")
	if !j.TimedOut || j.Abandoned || j.Outcome != artparse.OutcomeIncomplete || sum.Stopped != "" || jobOf(t, sum, "next").Outcome != artparse.OutcomeComplete {
		t.Fatalf("summary %+v", sum)
	}
}

func TestJobBudgetExceeded(t *testing.T) {
	f := newRx(t, "hog", "next")
	h := f.host(withLimits(func(l *parse.Limits) { l.MemBudget = 16 << 20 }), parsertest.Hog{Name: "hog", Version: "1.0.0", Alloc: 64 << 20}, wbp("next"))
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "hog")
	if j.Outcome != artparse.OutcomeIncomplete || !strings.Contains(j.Reason, "budget") {
		t.Fatalf("job %+v", j)
	}
	if jobOf(t, sum, "next").Outcome != artparse.OutcomeComplete {
		t.Error("the next job did not run with a fresh budget")
	}
}

func TestJobBudgetExceededIsIncomplete(t *testing.T) {
	f := newRx(t, "hog")
	h := f.host(withLimits(func(l *parse.Limits) { l.MemBudget = 16 << 20 }), parsertest.Hog{Name: "hog", Version: "1.0.0", Alloc: 64 << 20})
	sum := f.run(h, artparse.Selection{})
	if sum.Class() != artparse.ClassPartial {
		t.Errorf("class = %v", sum.Class())
	}
	if end := f.jobEnds()[0]; end.Outcome != artparse.OutcomeIncomplete || !strings.Contains(end.Error, "budget") || f.count("records.ingest.error") != 1 {
		t.Errorf("job.end %+v", end)
	}
}

func TestJobRecordCapIsIncomplete(t *testing.T) {
	f := newRx(t, "spam")
	sum := f.run(f.host(withLimits(func(l *parse.Limits) { l.MaxRecords = 2 }), parsertest.Spammer{Name: "spam", Version: "1.0.0", N: 5}), artparse.Selection{})
	if j := jobOf(t, sum, "spam"); j.Outcome != artparse.OutcomeIncomplete || j.Records != 2 {
		t.Fatalf("job %+v", j)
	}
}

func TestJobRejectedCapIsIncomplete(t *testing.T) {
	f := newRx(t, "inv")
	sum := f.run(f.host(withLimits(func(l *parse.Limits) { l.MaxRejected = 3 }), parsertest.Invalid{Name: "inv", Version: "1.0.0", Mode: parsertest.InvalidNulInSummary, N: 10}), artparse.Selection{})
	j := jobOf(t, sum, "inv")
	if j.Outcome != artparse.OutcomeIncomplete || j.Rejected != 3 || j.Records != 0 || !strings.Contains(j.Reason, "rejected") {
		t.Fatalf("job %+v", j)
	}
	if end := f.jobEnds()[0]; end.Rejected != 3 {
		t.Errorf("job.end rejected = %d", end.Rejected)
	}
}

func TestLyingParserIsRejectedNotTrusted(t *testing.T) {
	f := newRx(t, "outside", "undeclared", "range")
	other := putFile(t, f.c, "D1", "A1", "/data/x/other.bin", "not in the bundle")
	h := f.host(nil,
		parsertest.Liar{Name: "outside", Version: "1.0.0", Mode: parsertest.LieOutsideBundle, Other: other.ID},
		parsertest.Liar{Name: "undeclared", Version: "1.0.0", Mode: parsertest.LieUndeclaredType},
		parsertest.Liar{Name: "range", Version: "1.0.0", Mode: parsertest.LieRangeBeyondArtifact})
	sum := f.run(h, artparse.Selection{})
	for _, n := range []string{"outside", "undeclared", "range"} {
		j := jobOf(t, sum, n)
		if j.Rejected != 1 || j.Records != 0 {
			t.Errorf("%s: %+v, want one rejected record and none stored", n, j)
		}
	}
	if f.nRecords() != 0 {
		t.Errorf("%d records of lying parsers were stored", f.nRecords())
	}
	for _, end := range f.jobEnds() {
		if end.Rejected != 1 {
			t.Errorf("job.end %+v", end)
		}
	}
	rejected := 0
	for _, w := range f.entries(evidence.ActionAnalysisWarning) {
		if w.Details[evidence.WarnKeyRejected] == true {
			rejected++
		}
	}
	if rejected != 3 {
		t.Errorf("%d rejection warnings audited, want 3", rejected)
	}
}

func TestMutatingParserCannotChangeWhatTheHostRecords(t *testing.T) {
	for name, mode := range map[string]parsertest.MutateMode{
		"input":   parsertest.MutateInput,
		"meta":    parsertest.MutateMeta,
		"payload": parsertest.MutatePayloadAfterEmit,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRx(t, "mut")
			h := f.host(nil, &parsertest.Mutator{Name: "mut", Version: "1.0.0", Mode: mode})
			sum := f.run(h, artparse.Selection{})
			j := jobOf(t, sum, "mut")
			if j.Outcome != artparse.OutcomeComplete || sum.Class() != artparse.ClassOK {
				t.Fatalf("job %+v class %v", j, sum.Class())
			}
			if st := f.jobStarts()[0]; st.Bundle["primary"].SHA256 != f.recs["mut"].SHA256 {
				t.Errorf("the audited bundle hash %q is not the manifest's", st.Bundle["primary"].SHA256)
			}
			if mode == parsertest.MutatePayloadAfterEmit {
				rd, err := records.NewReader(f.c)
				if err != nil {
					t.Fatal(err)
				}
				rows := f.rows(true)
				if len(rows) != 1 {
					t.Fatalf("rows %+v", rows)
				}
				full, err := rd.Get(context.Background(), rows[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(full.Payload), "mutated") {
					t.Errorf("a payload edit after Emit reached the stored record: %s", full.Payload)
				}
			}
		})
	}
}

func TestAbortFailureStopsRunAndFreesSlot(t *testing.T) {
	f := newRx(t, "fail", "next")
	injected := errors.New("abort failed on purpose")
	failing := &rxParser{name: "fail", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") }}
	h := f.host(withWriter(func(w *rxWriter) { w.abortErr = injected }), failing, wbp("next"))
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if !errors.Is(err, injected) {
		t.Fatalf("Run = %v, want the Abort failure as a run-level error", err)
	}
	if sum.Stopped != "abort-failure" {
		t.Errorf("Stopped = %q", sum.Stopped)
	}
	if j := jobOf(t, sum, "fail"); j.Outcome != artparse.OutcomeIncomplete || j.Records != 1 {
		t.Errorf("job %+v, want incomplete with the 1 accepted record counted from the emitter", j)
	}
	if n := len(f.jobStarts()); n != 1 {
		t.Errorf("%d jobs started, want 1: no job starts after a failed Abort", n)
	}
	// the slot is free: a new run on the same case starts the second job
	h2 := f.host(nil, wbp("next"))
	sum2 := f.run(h2, artparse.Selection{})
	if j := jobOf(t, sum2, "next"); j.Outcome != artparse.OutcomeComplete {
		t.Errorf("the next run: %+v", j)
	}
}

func TestSnapshotProvenanceReachesRecords(t *testing.T) {
	f := newRx(t, "snap") // the live file at the same logical path
	snapRec := putExtract(t, f.c, "D1", "A1", "snap-extract.dat", "ext4", fakePath("snap"), &evidence.SnapshotRef{Name: "snap1", Xid: 7})
	h := f.host(nil, wbp("snap"))
	if s := f.run(h, artparse.Selection{}); len(s.Jobs) != 1 {
		t.Fatalf("a default run takes %d jobs, want only the live one", len(s.Jobs))
	}
	live := len(f.rows(true))
	sum := f.run(h, artparse.Selection{IncludeSnapshots: true})
	if len(sum.Jobs) != 2 {
		t.Fatalf("jobs %+v", sum.Jobs)
	}
	var snapJob artparse.JobResult
	for _, j := range sum.Jobs {
		if j.ArtifactID == snapRec.ID {
			snapJob = j
		}
	}
	if snapJob.Outcome != artparse.OutcomeComplete {
		t.Fatalf("snapshot job %+v (live job outcomes %+v)", snapJob, sum.Jobs)
	}
	rd, err := records.NewReader(f.c)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range f.rows(true) {
		full, err := rd.Get(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		var pl map[string]any
		if err := json.Unmarshal(full.Payload, &pl); err != nil {
			t.Fatal(err)
		}
		sn, has := pl["snapshot"].(map[string]any)
		if r.ArtifactID == snapRec.ID {
			seen++
			if !has || sn["name"] != "snap1" || sn["xid"] == nil {
				t.Errorf("snapshot record without its provenance: %s", full.Payload)
			}
		} else if has {
			t.Errorf("a live record carries a snapshot: %s", full.Payload)
		}
	}
	if seen == 0 || live == 0 {
		t.Fatalf("snapshot records %d, live %d", seen, live)
	}
	var endSnap *artparse.AuditSnapshot
	for _, e := range f.jobEnds() {
		if e.Snapshot != nil {
			endSnap = e.Snapshot
		}
	}
	if endSnap == nil || endSnap.Name != "snap1" || endSnap.Xid != 7 {
		t.Errorf("parse.job.end snapshot = %+v", endSnap)
	}
}
