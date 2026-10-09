package artparse_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestParseAuditsEveryJob(t *testing.T) {
	f := newRx(t, "a", "b")
	var openedBeforeStart int
	h := f.host(func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(id string) {
			starts := f.entries(artparse.ActionParseJobStart)
			if len(starts) == 0 {
				openedBeforeStart++
				return
			}
			st := decodeEntry[artparse.JobStart](t, starts[len(starts)-1])
			for _, m := range st.Bundle {
				if m.ArtifactID == id {
					return
				}
			}
			openedBeforeStart++ // the artifact was opened before the parse.job.start of ITS job
		})
	}, wbp("a"), wbp("b"))
	sum := f.run(h, artparse.Selection{})
	if openedBeforeStart != 0 {
		t.Errorf("%d artifacts were opened before the parse.job.start of their job", openedBeforeStart)
	}
	all, err := f.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	var analysisIDs []string
	for _, e := range all {
		if e.Action == artparse.ActionAnalysisStart {
			actions = nil
		}
		actions = append(actions, e.Action)
		if id, ok := e.Details["analysis_id"].(string); ok {
			analysisIDs = append(analysisIDs, id)
		}
	}
	want := []string{
		"analysis.start",
		"parse.job.start", "records.ingest.start", "records.batch", "records.ingest.end", "parse.job.end",
		"parse.job.start", "records.ingest.start", "records.batch", "records.ingest.end", "parse.job.end",
		"analysis.end",
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("audit order:\n got %v\nwant %v", actions, want)
	}
	if len(analysisIDs) != 8 {
		t.Errorf("%d entries carry an analysis_id, want 8 (analysis.start, analysis.end and per job its parse.job.start, records.ingest.start, parse.job.end)", len(analysisIDs))
	}
	for _, id := range analysisIDs {
		if id != sum.ParseID {
			t.Errorf("analysis_id %q differs from the run's %q", id, sum.ParseID)
		}
	}
	ends, ingestEnds := f.jobEnds(), f.entries("records.ingest.end")
	for i, end := range ends {
		concl := decodeEntry[evidence.IngestConclusion](t, ingestEnds[i])
		if end.Records != concl.Records || end.Rejected != concl.Rejected || end.Warnings != concl.Warnings ||
			end.IngestID != concl.IngestID || end.Outcome != artparse.OutcomeComplete || end.Records != 3 {
			t.Errorf("job.end %+v does not match the writer's end entry %+v", end, concl)
		}
	}
	st := decodeEntry[artparse.AnalysisStart](t, f.entries(artparse.ActionAnalysisStart)[0])
	if st.Op != "parse" || st.AnalysisID != sum.ParseID || len(st.Parsers) != 2 || st.Jobs.New != 2 || st.GoVersion == "" ||
		st.MinutiaeVersion == "" || st.Limits.TimeoutMS == 0 || st.Limits.MaxRecords == 0 {
		t.Errorf("analysis.start %+v", st)
	}
	if en := decodeEntry[artparse.AnalysisEnd](t, f.entries(artparse.ActionAnalysisEnd)[0]); en.Records != 6 || en.Jobs[artparse.OutcomeComplete] != 2 {
		t.Errorf("analysis.end %+v", en)
	}
	if rep, err := f.c.Verify(); err != nil || !rep.OK() {
		t.Errorf("case verify: %v %v", err, rep.Problems)
	}
	if sum.Records != 6 || sum.Class() != artparse.ClassOK {
		t.Errorf("summary %+v", sum)
	}
}

func TestParseAuditFailureStopsTheRun(t *testing.T) {
	f := newRx(t, "a", "b", "c")
	injected := errors.New("audit refused")
	starts := 0
	h := f.host(func(o *artparse.Options) {
		artparse.WithAuditHook(o, func(action string, _ map[string]any) error {
			if action == artparse.ActionParseJobStart {
				starts++
				if starts == 2 {
					return injected
				}
			}
			return nil
		})
	}, wbp("a"), wbp("b"), wbp("c"))
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if !errors.Is(err, injected) {
		t.Fatalf("Run = %v, want the audit failure", err)
	}
	if sum.Stopped != "audit-failure" || starts != 2 {
		t.Errorf("Stopped %q, job starts attempted %d: no later job may start", sum.Stopped, starts)
	}
	if n := f.count("records.ingest.start"); n != 1 {
		t.Errorf("%d ingests started, want 1", n)
	}
	if jobOf(t, sum, "a").Outcome != artparse.OutcomeComplete {
		t.Error("the job before the failure did not complete")
	}
}

func TestParseSkipsAlreadyParsed(t *testing.T) {
	f := newRx(t, "p")
	h := f.host(nil, wbp("p"))
	f.run(h, artparse.Selection{})
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "p")
	if j.Outcome != artparse.OutcomeSkipped || sum.Class() != artparse.ClassOK || f.count("records.ingest.start") != 1 {
		t.Fatalf("job %+v class %v ingests %d", j, sum.Class(), f.count("records.ingest.start"))
	}
	if ends := f.jobEnds(); len(ends) != 2 || ends[1].Outcome != artparse.OutcomeSkipped || ends[1].IngestID != "" {
		t.Errorf("job.end entries %+v", ends)
	}
}

func TestReparseAuditedAndSupersedes(t *testing.T) {
	f := newRx(t, "p")
	h := f.host(nil, wbp("p"))
	f.run(h, artparse.Selection{})
	sum := f.run(h, artparse.Selection{Reparse: true})
	if j := jobOf(t, sum, "p"); j.Outcome != artparse.OutcomeComplete || j.IngestID == "" {
		t.Fatalf("reparse job %+v", j)
	}
	starts := f.entries("records.ingest.start")
	if len(starts) != 2 {
		t.Fatalf("%d ingests", len(starts))
	}
	if first, second := decodeEntry[evidence.IngestStart](t, starts[0]), decodeEntry[evidence.IngestStart](t, starts[1]); first.Reingest || !second.Reingest {
		t.Errorf("reingest flags: first %v second %v", first.Reingest, second.Reingest)
	}
	rows := f.rows(false)
	if len(rows) != 3 {
		t.Fatalf("the default reader shows %d rows, want only the second run's 3", len(rows))
	}
	second := decodeEntry[evidence.IngestStart](t, starts[1]).IngestID
	for _, r := range rows {
		if r.IngestID != second {
			t.Errorf("row of ingest %s shown, want only %s", r.IngestID, second)
		}
	}
	if len(f.rows(true)) != 6 {
		t.Errorf("both runs hold %d rows, want 6", len(f.rows(true)))
	}
}

func TestNewerVersionSupersedes(t *testing.T) {
	f := newRx(t, "p")
	f.run(f.host(nil, wbp("p")), artparse.Selection{})
	v2 := parsertest.WellBehaved{Name: "p", Version: "1.1.0"}
	sum := f.run(f.host(nil, v2), artparse.Selection{})
	if j := jobOf(t, sum, "p"); j.Outcome != artparse.OutcomeComplete || j.Version != "1.1.0" {
		t.Fatalf("job %+v", j)
	}
	for _, r := range f.rows(false) {
		if r.Parser.Version != "1.1.0" {
			t.Errorf("a row of version %s is shown: the newer complete run supersedes it", r.Parser.Version)
		}
	}
	if len(f.rows(false)) != 3 {
		t.Errorf("%d rows shown, want 3", len(f.rows(false)))
	}
}

func TestFailedRunNeverSupersedes(t *testing.T) {
	f := newRx(t, "p")
	f.run(f.host(nil, wbp("p")), artparse.Selection{})
	bad := parsertest.Panicking{Name: "p", Version: "1.1.0", Where: parsertest.PanicAfterEmits, After: 1, Value: "boom"}
	sum := f.run(f.host(nil, bad), artparse.Selection{})
	if j := jobOf(t, sum, "p"); j.Outcome != artparse.OutcomeIncomplete {
		t.Fatalf("job %+v", j)
	}
	v1 := 0
	for _, r := range f.rows(false) {
		if r.Parser.Version == "1.0.0" {
			v1++
		}
	}
	if v1 != 3 {
		t.Errorf("%d rows of the complete v1.0.0 are visible, want all 3: a failed run never hides them", v1)
	}
}

func TestIdentityConflictRefused(t *testing.T) {
	f := newRx(t, "p")
	other := putFile(t, f.c, "D1", "A1", "/data/y/parsertest/p.dat", rxData)
	h1 := hostWith(t, f.c, artparse.SkipLimitMinimums, rxRegWithHash(t, wbp("p"), parsertest.FakeHash("p@1.0.0")))
	if _, err := h1.Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Artifacts: []string{f.recs["p"].ID}}}); err != nil {
		t.Fatal(err)
	}
	// the same name and version built differently (a development build) over another artifact
	h2 := hostWith(t, f.c, artparse.SkipLimitMinimums, rxRegWithHash(t, wbp("p"), parsertest.FakeHash("a different build")))
	sum, err := h2.Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Artifacts: []string{other.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	j := jobOf(t, sum, "p")
	if j.Outcome != artparse.OutcomeRefused || j.Integrity || !strings.Contains(j.Reason, "development build") || sum.Class() != artparse.ClassPartial {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if f.count("records.ingest.start") != 1 {
		t.Errorf("%d ingests started, want 1: none for the conflicting build", f.count("records.ingest.start"))
	}
}

func TestStaleBundleIsUnparsedWithoutReparse(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("w"), "database")
	host := func() *artparse.Host {
		return hostWith(t, c, artparse.SkipLimitMinimums, regFake(t, walParser{name: "w"}, "w"))
	}
	if sum, err := host().Run(context.Background(), artparse.RunOptions{}); err != nil || jobOf(t, sum, "w").Outcome != artparse.OutcomeComplete {
		t.Fatalf("first run: %+v %v", sum, err)
	}
	putFile(t, c, "D1", "A1", fakePath("w")+"-wal", "wal added later")
	sum, err := host().Run(context.Background(), artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j := jobOf(t, sum, "w")
	if j.Outcome != artparse.OutcomeUnparsed || !strings.Contains(j.Reason, "not covered") || sum.Class() != artparse.ClassPartial {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	var warned bool
	all, _ := c.ReadAudit()
	for _, e := range all {
		if e.Action == evidence.ActionAnalysisWarning && e.Details["source"] == "host" && e.Details["analysis_id"] == sum.ParseID {
			warned = true
		}
	}
	if !warned {
		t.Error("no host analysis.warning for the unparsed job")
	}
	sum, err = host().Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Reparse: true}})
	if err != nil || jobOf(t, sum, "w").Outcome != artparse.OutcomeComplete {
		t.Fatalf("with --reparse: %+v %v", sum, err)
	}
}

func TestParseCancelKeepsPartialFlagged(t *testing.T) {
	f := newRx(t, "spam", "never")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.host(both(
		func(o *artparse.Options) { artparse.WithBatchRows(o, 2) },
		withWriter(func(w *rxWriter) {
			w.afterAdd = func(adds int) {
				if adds == 2 { // the first batch was added
					cancel()
				}
			}
		})),
		&rxParser{name: "spam", n: 50}, wbp("never"))
	sum, err := h.Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j := jobOf(t, sum, "spam")
	if !sum.Cancelled || sum.Stopped != "cancelled" || j.Outcome != artparse.OutcomeIncomplete || sum.Class() != artparse.ClassPartial {
		t.Fatalf("summary %+v job %+v", sum, j)
	}
	if n := f.nRecords(); n < 2 {
		t.Errorf("%d records stored, want at least the first committed batch", n)
	}
	if jobOf(t, sum, "never").Outcome != artparse.OutcomeNotRun || len(f.jobStarts()) != 1 {
		t.Errorf("no further job may start: %+v, %d starts", sum.Jobs, len(f.jobStarts()))
	}
	if f.count("analysis.error") != 1 || f.count("analysis.end") != 0 || f.count("records.ingest.error") != 1 {
		t.Errorf("analysis.error %d analysis.end %d ingest.error %d", f.count("analysis.error"), f.count("analysis.end"), f.count("records.ingest.error"))
	}
}

func TestRunWithZeroJobsIsOK(t *testing.T) {
	f := newRx(t, "a")
	sum := f.run(f.host(nil), artparse.Selection{})
	if len(sum.Jobs) != 0 || sum.Class() != artparse.ClassOK || sum.Stopped != "" {
		t.Fatalf("summary %+v", sum)
	}
	all, err := f.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var tail []string
	for i := len(all) - 1; i >= 0; i-- {
		tail = append([]string{all[i].Action}, tail...)
		if all[i].Action == artparse.ActionAnalysisStart {
			break
		}
	}
	if !reflect.DeepEqual(tail, []string{"analysis.start", "analysis.end"}) {
		t.Errorf("audit tail %v", tail)
	}
}

func TestRunRequiresCurrentSchema(t *testing.T) {
	c, err := evidence.Open(recordstest.NewV1Case(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h := hostWith(t, c, nil, regFake(t, wbp("a"), "a"))
	before, _ := c.ReadAudit()
	if _, err := h.Run(context.Background(), artparse.RunOptions{}); !errors.Is(err, evidence.ErrNeedsUpgrade) {
		t.Fatalf("Run on a v1 case = %v, want ErrNeedsUpgrade", err)
	}
	after, _ := c.ReadAudit()
	if len(after) != len(before) {
		t.Errorf("%d audit entries appended on a case that needs an upgrade", len(after)-len(before))
	}
}

func TestRunRefusesACancelledContextBeforeAnythingStarted(t *testing.T) {
	f := newRx(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before, _ := f.c.ReadAudit()
	if _, err := f.host(nil, wbp("a")).Run(ctx, artparse.RunOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if after, _ := f.c.ReadAudit(); len(after) != len(before) {
		t.Error("a run that never started audited something")
	}
}

func TestRunSummaryClassPrecedence(t *testing.T) {
	job := func(outcome string, integrity bool) artparse.JobResult {
		return artparse.JobResult{Outcome: outcome, Integrity: integrity}
	}
	cases := []struct {
		name string
		s    artparse.RunSummary
		want artparse.Class
	}{
		{"nothing", artparse.RunSummary{}, artparse.ClassOK},
		{"complete and skipped", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeComplete, false), job(artparse.OutcomeSkipped, false)}}, artparse.ClassOK},
		{"incomplete", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeComplete, false), job(artparse.OutcomeIncomplete, false)}}, artparse.ClassPartial},
		{"unparsed", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeUnparsed, false)}}, artparse.ClassPartial},
		{"refused", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeRefused, false)}}, artparse.ClassPartial},
		{"not-run", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeNotRun, false)}}, artparse.ClassPartial},
		{"cancelled", artparse.RunSummary{Cancelled: true}, artparse.ClassPartial},
		{"stopped", artparse.RunSummary{Stopped: "abandoned"}, artparse.ClassPartial},
		{"integrity refused", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeRefused, true)}}, artparse.ClassIntegrity},
		{"integrity beats partial", artparse.RunSummary{Jobs: []artparse.JobResult{job(artparse.OutcomeIncomplete, false), job(artparse.OutcomeIncomplete, true), job(artparse.OutcomeUnparsed, false)}, Stopped: "abandoned"}, artparse.ClassIntegrity},
	}
	for _, c := range cases {
		if got := c.s.Class(); got != c.want {
			t.Errorf("%s: class %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlanAndRunAgree(t *testing.T) {
	f := newRx(t, "fresh", "done", "skip", "uns", "bad")
	tamper(t, f.c, f.recs["bad"])
	regs := []parse.Parser{
		parsertest.ProbeOnly{Name: "fresh", Version: "1.0.0", Status: parse.Applicable},
		parsertest.ProbeOnly{Name: "done", Version: "1.0.0", Status: parse.Applicable},
		parsertest.ProbeOnly{Name: "skip", Version: "1.0.0", Status: parse.NotApplicable, Reason: "r"},
		parsertest.ProbeOnly{Name: "uns", Version: "1.0.0", Status: parse.UnsupportedSchema, Reason: "r"},
		parsertest.ProbeOnly{Name: "bad", Version: "1.0.0", Status: parse.Applicable},
	}
	doneReg := rxReg(t, regs[1])
	recordstest.Ingest(t, f.c, doneReg.Identity(), []string{f.recs["done"].ID},
		[]records.Record{{Type: "message", ArtifactID: f.recs["done"].ID, Summary: "s", Payload: recordstest.ValidPayload("message")}})
	h := f.host(nil, regs...)
	plan := planOf(t, h, artparse.Selection{})
	sum := f.run(h, artparse.Selection{})
	planNew, runParsed := map[string]bool{}, map[string]bool{}
	for _, r := range plan.Rows {
		if r.Status == artparse.StatusNew {
			planNew[r.Job.Parser.Meta().Name] = true
		}
	}
	for _, j := range sum.Jobs {
		if j.Outcome == artparse.OutcomeComplete || j.Outcome == artparse.OutcomeIncomplete {
			runParsed[j.Parser] = true
		}
	}
	if !reflect.DeepEqual(planNew, runParsed) || len(planNew) != 1 || !planNew["fresh"] {
		t.Errorf("Plan says new for %v, Run executed %v", planNew, runParsed)
	}
	if len(plan.Rows) != len(sum.Jobs) {
		t.Errorf("Plan has %d rows, Run %d jobs", len(plan.Rows), len(sum.Jobs))
	}
	for _, r := range plan.Rows {
		j := jobOf(t, sum, r.Job.Parser.Meta().Name)
		if r.Integrity != j.Integrity {
			t.Errorf("%s: plan integrity %v, run %v", j.Parser, r.Integrity, j.Integrity)
		}
	}
}

func TestRunProgressEventsArriveOnTheRunGoroutine(t *testing.T) {
	f := newRx(t, "prog")
	var inJob sync.WaitGroup
	late := make(chan struct{})
	var leftover parse.Emitter
	p := &rxParser{name: "prog", n: 1, after: func(_ context.Context, _ *parse.Input, out parse.Emitter) error {
		leftover = out
		inJob.Add(1)
		go func() { // progress from another goroutine of the parser, while the job runs
			defer inJob.Done()
			for i := int64(1); i <= 5; i++ {
				out.Progress(i, 5)
			}
		}()
		inJob.Wait()
		out.Progress(5, 5) // the final one is never throttled
		return nil
	}}
	caller := goid()
	var kinds, foreign []string
	var progress []int64
	var runStart, first, last artparse.Event
	opt := artparse.RunOptions{OnEvent: func(e artparse.Event) {
		kinds = append(kinds, e.Kind)
		switch e.Kind {
		case "run.start":
			runStart = e
		case "job.start":
			first = e
		case "job.end":
			last = e
		}
		if goid() != caller {
			foreign = append(foreign, e.Kind)
		}
		if e.Kind == "job.progress" {
			progress = append(progress, e.Done)
		}
	}}
	sum, err := f.host(nil, p).Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	close(late)
	if len(foreign) != 0 {
		t.Errorf("OnEvent ran off the Run goroutine for %v", foreign)
	}
	if len(kinds) < 4 || kinds[0] != "run.start" || kinds[1] != "job.start" || kinds[len(kinds)-1] != "job.end" {
		t.Fatalf("events %v", kinds)
	}
	if first.Result == nil || first.Result.Parser != "prog" || first.Result.ArtifactID != f.recs["prog"].ID || last.Result == nil || last.Result.Outcome != artparse.OutcomeComplete || runStart.ParseID == "" || runStart.Jobs != 1 {
		t.Errorf("event payloads: run.start %+v job.start %+v job.end %+v", runStart, first, last)
	}
	if len(progress) == 0 || progress[len(progress)-1] != 5 {
		t.Errorf("progress %v, want the final done=5 forwarded", progress)
	}
	n := len(kinds)
	leftover.Progress(5, 5) // a goroutine the parser left behind
	time.Sleep(20 * time.Millisecond)
	if len(kinds) != n {
		t.Errorf("an event was delivered after the job's seal: %v", kinds[n:])
	}
	if sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Errorf("job %+v", sum.Jobs[0])
	}
}

func TestEmitterAndBundleAreSealedOnEveryJobExitPath(t *testing.T) {
	type path struct {
		name  string
		mod   func(*artparse.Options)
		after func(cancel context.CancelFunc, release chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error
	}
	paths := []path{
		{"success", nil, func(context.CancelFunc, chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return nil
		}},
		{"error", nil, func(context.CancelFunc, chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") }
		}},
		{"panic", nil, func(context.CancelFunc, chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return func(context.Context, *parse.Input, parse.Emitter) error { panic("kaboom") }
		}},
		{"timeout", shortTimeouts, func(context.CancelFunc, chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return func(c context.Context, _ *parse.Input, _ parse.Emitter) error { <-c.Done(); return c.Err() }
		}},
		{"abandon", shortTimeouts, func(_ context.CancelFunc, release chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return func(context.Context, *parse.Input, parse.Emitter) error { <-release; return nil }
		}},
		{"cancel", nil, func(cancel context.CancelFunc, _ chan struct{}) func(context.Context, *parse.Input, parse.Emitter) error {
			return func(c context.Context, _ *parse.Input, _ parse.Emitter) error { cancel(); <-c.Done(); return c.Err() }
		}},
	}
	for _, pa := range paths {
		t.Run(pa.name, func(t *testing.T) {
			f := newRx(t, "keep")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			kept := &rxKept{}
			p := &rxParser{name: "keep", n: 1, kept: kept, after: pa.after(cancel, release)}
			concluded := 0
			// An abandoned job is concluded by a goroutine the host stops waiting for after GracePeriod, so Run can
			// return before concluding has finished; the writer hook reports when Abort has returned.
			aborted := make(chan struct{}, 1)
			seal := withWriter(func(w *rxWriter) {
				w.concluding = func() { // the host starts to conclude the ingest: the job is sealed by now
					concluded++
					kept.assertSealed(t)
				}
				w.aborted = func() { aborted <- struct{}{} }
			})
			if _, err := f.host(both(pa.mod, seal), p).Run(ctx, artparse.RunOptions{}); err != nil {
				t.Fatal(err)
			}
			if pa.name == "abandon" {
				select {
				case <-aborted: // the abandoned job concluded: concluded and the sealed state are stable now
				case <-t.Context().Done():
					t.Fatal("the abandoned job was never concluded")
				case <-time.After(2 * time.Minute): // safety net only: a regression fails instead of hanging
					t.Fatal("the abandoned job was never concluded")
				}
			}
			if concluded == 0 {
				t.Fatal("the ingest was never concluded")
			}
			kept.assertSealed(t)
		})
	}
}

func TestRunSealsWhenAWriterCallIsStuck(t *testing.T) {
	f := newRx(t, "stuck", "next")
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	p := &rxParser{name: "stuck", after: func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		go func() { _ = out.Emit(context.WithoutCancel(ctx), rxRecord(in, 1)) }() // stuck in the writer
		<-entered
		return nil // the parser itself returns
	}}
	var once sync.Once
	h := f.host(both(shortTimeouts, func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(c *evidence.Case, pr records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, pr, wo)
			if err != nil {
				return nil, err
			}
			return &blockingAdd{IngestWriter: w, entered: entered, release: release, once: &once}, nil
		})
	}), p, wbp("next"))
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "stuck")
	if !j.Abandoned || j.Outcome != artparse.OutcomeIncomplete || sum.Stopped != "abandoned" {
		t.Fatalf("a writer call stuck past the grace period must abandon the job: %+v stopped %q", j, sum.Stopped)
	}
	if jobOf(t, sum, "next").Outcome != artparse.OutcomeNotRun {
		t.Error("the run went on after the abandonment")
	}
}

// blockingAdd blocks the first Add until released.
type blockingAdd struct {
	artparse.IngestWriter
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (w *blockingAdd) Add(ctx context.Context, r records.Record) error {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return w.IngestWriter.Add(ctx, r)
}

func TestJobEndRecordsStreamedInputsAndLookups(t *testing.T) {
	p := &rxParser{name: "look", after: func(_ context.Context, in *parse.Input, _ parse.Emitter) error {
		for _, a := range in.Lookup.Find("android:**/parsertest/sib.dat") {
			r, err := in.Lookup.Open(a)
			if err != nil {
				return err
			}
			var b [4]byte
			if _, err := r.ReadAt(b[:], 0); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}
		return nil
	}}
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"streamed", true}, {"in memory", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRx(t, "look", "sib")
			h := f.host(withLimits(func(l *parse.Limits) {
				if tc.stream {
					l.MemInputMax = 0
				}
			}), p)
			f.run(h, artparse.Selection{Artifacts: []string{f.recs["look"].ID}, Parsers: []artparse.ParserRef{{Name: "look"}}})
			end := f.jobEnds()[0]
			if len(end.Lookups) != 1 || end.Lookups[0].ArtifactID != f.recs["sib"].ID || end.Lookups[0].SHA256 != f.recs["sib"].SHA256 || end.Lookups[0].Streamed != tc.stream {
				t.Errorf("lookups %+v, want the sibling opened (streamed=%v)", end.Lookups, tc.stream)
			}
			if got := len(end.Streamed) == 1 && end.Streamed[0] == f.recs["look"].ID; got != tc.stream {
				t.Errorf("streamed %v, want the primary listed: %v", end.Streamed, tc.stream)
			}
		})
	}
}

func TestRunDoesNotOpenAnArtifactOfAJobItSkips(t *testing.T) {
	f := newRx(t, "p")
	h0 := f.host(nil, wbp("p"))
	f.run(h0, artparse.Selection{})
	var opened []string
	h := f.host(func(o *artparse.Options) { artparse.WithOnOpen(o, func(id string) { opened = append(opened, id) }) }, wbp("p"))
	f.run(h, artparse.Selection{})
	sort.Strings(opened)
	if len(opened) != 0 {
		t.Errorf("an already-parsed job opened %v", opened)
	}
}

func TestParseJobAuditCodecsRoundTrip(t *testing.T) {
	end := artparse.JobEnd{
		AnalysisID: "an", Job: 2, Outcome: artparse.OutcomeIncomplete, Reason: "r", IngestID: "ing-1", Records: 5, Rejected: 1, Warnings: 2,
		WarningsSuppressed: 3, DurationMS: 40, Notes: map[string]string{"k": "v"}, Error: "e", Panic: &artparse.AuditPanic{Value: "v", Stack: "s"},
		Abandoned: true, TimedOut: true, Integrity: true, ArtifactIncomplete: []string{"a1"}, Streamed: []string{"a2"},
		Lookups:  []artparse.LookupOpen{{ArtifactID: "a3", SHA256: strings.Repeat("a", 64), Streamed: true}},
		Snapshot: &artparse.AuditSnapshot{Name: "s1", Xid: 9},
	}
	start := artparse.JobStart{
		AnalysisID: "an", Job: 1, Parser: artparse.AuditParser{Name: "p", Version: "1.0.0", Hash: "h"}, Explicit: true,
		Bundle: map[string]artparse.BundleMember{"db": {ArtifactID: "a", SHA256: "s", LogicalPath: "android:/x", Platform: "android", Namer: "device-file", Snapshot: &artparse.AuditSnapshot{Name: "n", Xid: 1}}},
	}
	as := artparse.AnalysisStart{
		AnalysisID: "an", Op: "parse", Parsers: []artparse.AuditParser{{Name: "p"}}, Filters: artparse.AuditFilters{Parsers: []string{"p"}, Artifacts: []string{"a"}, IncludeSnapshots: true},
		Reparse: true, MinutiaeVersion: "v", Commit: "c", GoVersion: "go", Limits: artparse.AuditLimits{TimeoutMS: 5, MaxRecords: 7}, Jobs: artparse.AuditJobs{New: 1, AlreadyParsed: 2, StaleBundle: 3, Unparsed: 4},
	}
	ae := artparse.AnalysisEnd{AnalysisID: "an", Jobs: map[string]int{"complete": 2}, Records: 6, Rejected: 1, Warnings: 2}
	ar := artparse.AnalysisError{AnalysisID: "an", Error: "e", Jobs: map[string]int{"incomplete": 1}, Records: 6, Rejected: 1, Warnings: 2, NotRun: 3}
	hw := artparse.HostWarning{AnalysisID: "an", Path: "p", Reason: "r", Source: "host"}
	check := func(name string, d map[string]any, got, want any, keys ...string) {
		t.Helper()
		if len(d) == 0 {
			t.Errorf("%s: no details", name)
			return
		}
		for _, k := range keys {
			if _, ok := d[k]; !ok {
				t.Errorf("%s: no key %q in %v", name, k, d)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: round trip\n got %+v\nwant %+v", name, got, want)
		}
	}
	dEnd := end.Details()
	gEnd, err := evidence.DecodeDetails[artparse.JobEnd](dEnd)
	if err != nil {
		t.Fatal(err)
	}
	check("job.end", dEnd, gEnd, end, "analysis_id", "job", "outcome", "reason", "ingest_id", "records", "rejected", "warnings", "warnings_suppressed",
		"duration_ms", "notes", "error", "panic", "abandoned", "timed_out", "integrity", "artifact_incomplete", "streamed", "lookups", "snapshot")
	dStart := start.Details()
	gStart, err := evidence.DecodeDetails[artparse.JobStart](dStart)
	if err != nil {
		t.Fatal(err)
	}
	check("job.start", dStart, gStart, start, "analysis_id", "job", "parser", "bundle", "explicit")
	dAS := as.Details()
	gAS, err := evidence.DecodeDetails[artparse.AnalysisStart](dAS)
	if err != nil {
		t.Fatal(err)
	}
	check("analysis.start", dAS, gAS, as, "analysis_id", "op", "parsers", "filters", "reparse", "minutiae_version", "commit", "go_version", "limits", "jobs")
	dAE := ae.Details()
	gAE, err := evidence.DecodeDetails[artparse.AnalysisEnd](dAE)
	if err != nil {
		t.Fatal(err)
	}
	check("analysis.end", dAE, gAE, ae, "analysis_id", "jobs", "records", "rejected", "warnings")
	dAR := ar.Details()
	gAR, err := evidence.DecodeDetails[artparse.AnalysisError](dAR)
	if err != nil {
		t.Fatal(err)
	}
	check("analysis.error", dAR, gAR, ar, "analysis_id", "error", "jobs", "records", "rejected", "warnings", "not_run")
	dHW := hw.Details()
	gHW, err := evidence.DecodeDetails[artparse.HostWarning](dHW)
	if err != nil {
		t.Fatal(err)
	}
	check("host warning", dHW, gHW, hw, "analysis_id", "path", "reason", "source")
	// an optional field left out stays out
	if _, ok := (artparse.JobEnd{}).Details()["panic"]; ok {
		t.Error("panic is present without a panic")
	}
	if _, ok := (artparse.JobEnd{}).Details()["snapshot"]; ok {
		t.Error("snapshot is present without a snapshot")
	}
}

func TestStartRefusalsAreClassified(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		outcome   string
		integrity bool
		reason    string
	}{
		{"already ingested", records.ErrAlreadyIngested, artparse.OutcomeSkipped, false, "already parsed"},
		{"identity conflict", records.ErrParserIdentityConflict, artparse.OutcomeRefused, false, "development build"},
		{"integrity", evidence.ErrIntegrity, artparse.OutcomeRefused, true, "integrity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRx(t, "a", "next")
			h := f.host(withWriter(func(w *rxWriter) {
				if w.startErr == nil && !strings.Contains(tc.name, "next") {
					w.startErr = tc.err
				}
			}), wbp("a"))
			sum := f.run(h, artparse.Selection{})
			j := jobOf(t, sum, "a")
			if j.Outcome != tc.outcome || j.Integrity != tc.integrity || !strings.Contains(j.Reason, tc.reason) {
				t.Errorf("job %+v", j)
			}
		})
	}
	t.Run("ingest active is a run-level error", func(t *testing.T) {
		f := newRx(t, "a", "b")
		live, err := records.NewWriter(f.c, records.Parser{Name: "other", Version: "1.0.0"}, records.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := live.Start(context.Background(), records.StartOptions{Artifacts: []string{f.recs["a"].ID}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = live.Abort(context.Background(), errors.New("test over")) })
		sum, err := f.host(nil, wbp("a"), wbp("b")).Run(context.Background(), artparse.RunOptions{})
		if !errors.Is(err, records.ErrIngestActive) {
			t.Fatalf("Run = %v, want ErrIngestActive", err)
		}
		if sum.Stopped != artparse.StoppedRunError {
			t.Errorf("Stopped = %q, want %q", sum.Stopped, artparse.StoppedRunError)
		}
		if len(f.jobStarts()) != 1 || sum.Class() != artparse.ClassPartial {
			t.Errorf("%d jobs started, class %v: the run stops at the first job", len(f.jobStarts()), sum.Class())
		}
	})
	t.Run("another Start error is a run-level error", func(t *testing.T) {
		f := newRx(t, "a", "b")
		injected := errors.New("database on fire")
		h := f.host(withWriter(func(w *rxWriter) { w.startErr = injected }), wbp("a"), wbp("b"))
		sum, err := h.Run(context.Background(), artparse.RunOptions{})
		if !errors.Is(err, injected) {
			t.Fatalf("Run = %v", err)
		}
		if sum.Stopped != artparse.StoppedRunError {
			t.Errorf("Stopped = %q, want %q", sum.Stopped, artparse.StoppedRunError)
		}
		if len(f.jobStarts()) != 1 {
			t.Errorf("%d jobs started", len(f.jobStarts()))
		}
		if f.count("analysis.error") != 1 || len(f.jobEnds()) != 1 {
			t.Errorf("the failed run is not concluded: analysis.error %d, job.end %d", f.count("analysis.error"), len(f.jobEnds()))
		}
	})
}

func TestEndFailureAbortsTheIngest(t *testing.T) {
	f := newRx(t, "a")
	h := f.host(withWriter(func(w *rxWriter) { w.endErr = errors.New("end failed") }), wbp("a"))
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "a")
	if j.Outcome != artparse.OutcomeIncomplete || !strings.Contains(j.Reason, "concluding the ingest") {
		t.Fatalf("job %+v", j)
	}
	if f.count("records.ingest.error") != 1 || f.count("records.ingest.end") != 0 {
		t.Errorf("ingest errors %d, ends %d: the ingest must be aborted", f.count("records.ingest.error"), f.count("records.ingest.end"))
	}
	if f.nRecords() != 3 {
		t.Errorf("%d records stored, want the 3 accepted ones flushed before the abort", f.nRecords())
	}
}

func TestFlushFailureIsNamedInTheReason(t *testing.T) {
	f := newRx(t, "a")
	p := &rxParser{name: "a", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") }}
	h := f.host(withWriter(func(w *rxWriter) { w.flushErr = errors.New("disk full") }), p)
	j := jobOf(t, f.run(h, artparse.Selection{}), "a")
	if j.Outcome != artparse.OutcomeIncomplete || !strings.Contains(j.Reason, "flushing the accepted records failed") || !strings.Contains(j.Reason, "disk full") {
		t.Fatalf("job %+v", j)
	}
	if f.count("records.ingest.error") != 1 {
		t.Error("the ingest was not aborted after the failed flush")
	}
}

func TestUnreadableInputIsAnUnparsedJobNotARunFailure(t *testing.T) {
	f := newRx(t, "a", "b")
	fail := errors.New("disk read error")
	n := 0
	h := f.host(func(o *artparse.Options) {
		artparse.WithWrapFile(o, func(rf artparse.ReadFile) artparse.ReadFile {
			n++
			if n == 1 {
				return failingFile{rf, fail}
			}
			return rf
		})
	}, wbp("a"), wbp("b"))
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "a")
	if j.Outcome != artparse.OutcomeUnparsed || j.Integrity || !strings.Contains(j.Reason, "input unreadable") || !strings.Contains(j.Reason, "disk read error") {
		t.Fatalf("job %+v", j)
	}
	if jobOf(t, sum, "b").Outcome != artparse.OutcomeComplete || sum.Class() != artparse.ClassPartial {
		t.Errorf("the next job or the class: %+v %v", sum.Jobs, sum.Class())
	}
	if f.count("records.ingest.start") != 1 {
		t.Error("an ingest started for the unreadable job")
	}
}

// failingFile fails every read with err.
type failingFile struct {
	artparse.ReadFile
	err error
}

func (f failingFile) Read([]byte) (int, error)          { return 0, f.err }
func (f failingFile) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func TestJobStartAndAnalysisStartCarryTheSelectionAndTheBundle(t *testing.T) {
	f := newRx(t, "a", "b")
	h := f.host(nil, wbp("a"), wbp("b"))
	sel := artparse.Selection{Parsers: []artparse.ParserRef{{Name: "a", Version: "1.0.0"}}, Artifacts: []string{f.recs["a"].ID}, Reparse: true, IncludeSnapshots: true}
	sum := f.run(h, sel)
	as := decodeEntry[artparse.AnalysisStart](t, f.entries(artparse.ActionAnalysisStart)[0])
	if !as.Reparse || !as.Filters.IncludeSnapshots || !reflect.DeepEqual(as.Filters.Parsers, []string{"a@1.0.0"}) ||
		!reflect.DeepEqual(as.Filters.Artifacts, []string{f.recs["a"].ID}) || len(as.Parsers) != 1 || as.Parsers[0].Name != "a" || as.Parsers[0].Hash == "" {
		t.Errorf("analysis.start %+v", as)
	}
	st := f.jobStarts()
	if len(st) != 1 {
		t.Fatalf("%d job starts", len(st))
	}
	m := st[0].Bundle["primary"]
	if st[0].Job != 1 || !st[0].Explicit || st[0].Parser.Name != "a" || m.ArtifactID != f.recs["a"].ID || m.SHA256 != f.recs["a"].SHA256 ||
		m.LogicalPath != "android:"+fakePath("a") || m.Platform != parse.PlatformAndroid || m.Namer == "" || m.Snapshot != nil {
		t.Errorf("job.start %+v", st[0])
	}
	if got := jobOf(t, sum, "a"); got.ArtifactID != f.recs["a"].ID || got.Logical != "android:"+fakePath("a") || got.Platform != "android" || got.Hash == "" {
		t.Errorf("job result %+v", got)
	}
}

func TestJobEndCarriesNotesAndIncompleteArtifacts(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	src := evidence.Source{Kind: "file", DeviceID: "D1", RemotePath: fakePath("cut")}
	rec, _ := c.Capture("D1", "A1", strings.TrimPrefix(fakePath("cut"), "/"), src, func(w io.Writer) error {
		_, _ = io.WriteString(w, rxData)
		return errors.New("pull interrupted")
	})
	h := hostWith(t, c, artparse.SkipLimitMinimums, rxReg(t, wbp("cut")))
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if jobOf(t, sum, "cut").Outcome != artparse.OutcomeComplete {
		t.Fatalf("job %+v", sum.Jobs)
	}
	all, _ := c.ReadAudit()
	for _, e := range all {
		if e.Action == artparse.ActionParseJobEnd {
			end := decodeEntry[artparse.JobEnd](t, e)
			if !reflect.DeepEqual(end.ArtifactIncomplete, []string{rec.ID}) {
				t.Errorf("artifact_incomplete = %v, want [%s]", end.ArtifactIncomplete, rec.ID)
			}
			if end.Notes["lines"] != "3" || sum.Jobs[0].Notes["lines"] != "3" {
				t.Errorf("notes: audit %v, result %v", end.Notes, sum.Jobs[0].Notes)
			}
			if end.DurationMS < 0 || end.Job != 1 {
				t.Errorf("job.end %+v", end)
			}
		}
	}
}

func TestRunTotalsOfRejectionsAndWarningsAreAudited(t *testing.T) {
	f := newRx(t, "inv")
	// the well-behaved parser warns once per blank line: this file has two
	putFile(t, f.c, "D1", "A1", fakePath("blank"), "x\n\n\ny\n")
	h := f.host(nil, parsertest.Invalid{Name: "inv", Version: "1.0.0", Mode: parsertest.InvalidNulInSummary, N: 2}, wbp("blank"))
	sum := f.run(h, artparse.Selection{})
	en := decodeEntry[artparse.AnalysisEnd](t, f.entries(artparse.ActionAnalysisEnd)[0])
	if en.Rejected != 2 || en.Warnings < 2 || en.Records != sum.Records {
		t.Errorf("analysis.end %+v, summary records %d: totals of the jobs' rejections and warnings", en, sum.Records)
	}
}

func TestCancelDuringOpenStopsTheRun(t *testing.T) {
	f := newRx(t, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.host(func(o *artparse.Options) { artparse.WithOnOpen(o, func(string) { cancel() }) }, wbp("a"), wbp("b"))
	sum, err := h.Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if j := jobOf(t, sum, "a"); j.Outcome != artparse.OutcomeIncomplete || j.Reason != "cancelled" || !sum.Cancelled || sum.Stopped != "cancelled" {
		t.Fatalf("summary %+v", sum)
	}
	if jobOf(t, sum, "b").Outcome != artparse.OutcomeNotRun || f.count("records.ingest.start") != 0 {
		t.Errorf("the next job or an ingest started: %+v", sum.Jobs)
	}
}

func TestGoexitParserIsNeverACompleteJob(t *testing.T) {
	f := newRx(t, "exit")
	p := &rxParser{name: "exit", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error { runtime.Goexit(); return nil }}
	j := jobOf(t, f.run(f.host(nil, p), artparse.Selection{}), "exit")
	if j.Outcome != artparse.OutcomeIncomplete || !strings.Contains(j.Reason, "exited without returning") {
		t.Fatalf("job %+v", j)
	}
}

func TestWriterFactoryFailureRefusesTheJob(t *testing.T) {
	f := newRx(t, "a", "b")
	boom := errors.New("no writer")
	h := f.host(func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
			return nil, boom
		})
	}, wbp("a"), wbp("b"))
	sum := f.run(h, artparse.Selection{})
	for _, n := range []string{"a", "b"} {
		if j := jobOf(t, sum, n); j.Outcome != artparse.OutcomeRefused || !strings.Contains(j.Reason, "no writer") {
			t.Errorf("job %s: %+v", n, j)
		}
	}
	needs := func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
			return nil, evidence.ErrNeedsUpgrade
		})
	}
	if _, err := f.host(needs, wbp("a")).Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Reparse: true}}); !errors.Is(err, evidence.ErrNeedsUpgrade) {
		t.Errorf("a case that needs an upgrade: %v", err)
	}
}

// The inputs are re-hashed even when the run was cancelled: a job that was cancelled while its input
// changed is an integrity failure, and its records are not flushed.
func TestIntegrityIsCheckedEvenWhenTheRunIsCancelled(t *testing.T) {
	f := newRx(t, "chg")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &rxParser{name: "chg", n: 2, after: func(c context.Context, _ *parse.Input, _ parse.Emitter) error {
		rewrite(t, f.c, f.recs["chg"])
		cancel()
		<-c.Done()
		return c.Err()
	}}
	sum, err := f.host(nil, p).Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if j := jobOf(t, sum, "chg"); !j.Integrity || sum.Class() != artparse.ClassIntegrity || !sum.Cancelled {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if f.nRecords() != 0 {
		t.Errorf("%d records flushed after an integrity failure", f.nRecords())
	}
}

// A real failed Abort (the audit log cannot be written): the run stops with the error, the next run on
// the reopened case recovers the dead ingest and parses the next job.
func TestAbortFailureLeavesADeadIngestTheNextRunRecovers(t *testing.T) {
	f := newRx(t, "fail", "next")
	failing := &rxParser{name: "fail", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") }}
	h := f.host(nil, failing, wbp("next"))
	// close the audit log when Abort is about to append: the real Abort then fails for real
	artparse.SetNewWriterForTest(h, func(c *evidence.Case, p records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
		w, err := records.NewWriter(c, p, wo)
		if err != nil {
			return nil, err
		}
		return &auditClosingWriter{IngestWriter: w, c: c}, nil
	})
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if err == nil || sum.Stopped != "abort-failure" {
		t.Fatalf("Run = %v, stopped %q, want an abort failure", err, sum.Stopped)
	}
	dir := f.c.Dir
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	f2 := &rx{t: t, c: c2, recs: f.recs}
	sum2 := f2.run(hostWith(t, c2, artparse.SkipLimitMinimums, rxReg(t, wbp("next"))), artparse.Selection{})
	if jobOf(t, sum2, "next").Outcome != artparse.OutcomeComplete {
		t.Fatalf("second run: %+v", sum2.Jobs)
	}
	if f2.count("records.ingest.recover") != 1 {
		t.Errorf("%d records.ingest.recover entries, want the dead ingest recovered by the next Start", f2.count("records.ingest.recover"))
	}
	if rep, err := c2.Verify(); err != nil || !rep.OK() {
		t.Errorf("case verify: %v %v", err, rep.Problems)
	}
}

// auditClosingWriter closes the audit log just before the ingest is aborted.
type auditClosingWriter struct {
	artparse.IngestWriter
	c *evidence.Case
}

func (w *auditClosingWriter) Abort(ctx context.Context, cause error) (records.IngestResult, error) {
	_ = w.c.Audit.Close()
	return w.IngestWriter.Abort(ctx, cause)
}
