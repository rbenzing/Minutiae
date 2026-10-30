package artparse_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

const pjPrefix = "parse jobs: "

// pjRun runs the check alone over c.
func pjRun(c *evidence.Case) evidence.VerifyReport {
	var rep evidence.VerifyReport
	artparse.ParseJobsCheck()(context.Background(), c, nil, &rep)
	return rep
}

// pjClean requires the check to find nothing in c: no problem and no notice.
func pjClean(t *testing.T, c *evidence.Case) {
	t.Helper()
	if rep := pjRun(c); len(rep.Problems) != 0 || len(rep.Notices) != 0 {
		t.Errorf("parse jobs check: problems %q notices %q, want none", rep.Problems, rep.Notices)
	}
}

// pjProblemsWith requires a problem that starts with the prefix and holds every part.
func pjProblemsWith(t *testing.T, rep evidence.VerifyReport, parts ...string) {
	t.Helper()
	for _, p := range rep.Problems {
		ok := strings.HasPrefix(p, pjPrefix)
		for _, part := range parts {
			ok = ok && strings.Contains(p, part)
		}
		if ok {
			return
		}
	}
	t.Errorf("no %q problem holding %q in %q", pjPrefix, parts, rep.Problems)
}

// forgeLog closes the case, rewrites its audit log with mut (renumbered and rechained, so only a
// cross-check can see the change) and reopens it.
func forgeLog(t *testing.T, c *evidence.Case, mut func([]evidence.AuditEntry) []evidence.AuditEntry) *evidence.Case {
	t.Helper()
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	evidencetest.RechainAudit(t, dir, mut)
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	return c2
}

// pjIndex is the index of the nth (from 0) entry of the action, or -1.
func pjIndex(es []evidence.AuditEntry, action string, nth int) int {
	for i, e := range es {
		if e.Action == action {
			if nth == 0 {
				return i
			}
			nth--
		}
	}
	return -1
}

func pjMustIndex(t *testing.T, es []evidence.AuditEntry, action string, nth int) int {
	t.Helper()
	i := pjIndex(es, action, nth)
	if i < 0 {
		t.Fatalf("no %s entry number %d", action, nth)
	}
	return i
}

// pjEdit decodes the details of es[i] into T, applies fn and writes them back.
func pjEdit[T interface{ Details() map[string]any }](t *testing.T, es []evidence.AuditEntry, i int, fn func(*T)) {
	t.Helper()
	v := decodeEntry[T](t, es[i])
	fn(&v)
	es[i].Details = v.Details()
}

func pjDrop(es []evidence.AuditEntry, i int) []evidence.AuditEntry {
	return append(es[:i:i], es[i+1:]...)
}

// pjTwoJobs is a case after a run of two parsers: both jobs complete.
func pjTwoJobs(t *testing.T) *rx {
	t.Helper()
	f := newRx(t, "one", "two")
	sum := f.run(f.host(nil, wbp("one"), wbp("two")), artparse.Selection{})
	if jobOf(t, sum, "one").Outcome != artparse.OutcomeComplete || jobOf(t, sum, "two").Outcome != artparse.OutcomeComplete {
		t.Fatalf("run: %+v", sum.Jobs)
	}
	return f
}

// pjOneJob is a case after one complete job.
func pjOneJob(t *testing.T) *rx {
	t.Helper()
	f := newRx(t, "solo")
	if sum := f.run(f.host(nil, wbp("solo")), artparse.Selection{}); jobOf(t, sum, "solo").Outcome != artparse.OutcomeComplete {
		t.Fatalf("run: %+v", sum.Jobs)
	}
	return f
}

func TestParseJobsCheckCleanOnRealRuns(t *testing.T) {
	// a live run over two files
	f := pjTwoJobs(t)
	pjClean(t, f.c)

	// a live run and a typed recovery over a database
	rc := newRecCase(t, "sqlite3-freelist", false)
	h := rc.host(nil, regRec(t, newRecParser("rm", "people")))
	liveRun(t, h, false)
	recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: rc.dbs[0].ID, ExpectCompanions: map[string]string{"wal": ""}})
	if n := len(rc.audit(artparse.ActionParseJobEnd)); n != 2 {
		t.Fatalf("%d parse.job.end entries, want the live job and the recovery job", n)
	}
	pjClean(t, rc.c)

	// an iOS backup run: one job whose bundle holds two artifacts
	bc := newBackupCase(t)
	bc.add(stdSpec())
	if sum := bc.run(bc.host(nil)); len(sum.Jobs) != 1 || sum.Jobs[0].IngestID == "" {
		t.Fatalf("backup run: %+v", sum.Jobs)
	}
	pjClean(t, bc.c)
}

func TestParseJobsCheckFlagsIngestWithoutJobStart(t *testing.T) {
	f := pjTwoJobs(t)
	first := decodeEntry[evidence.IngestStart](t, f.entries("records.ingest.start")[0])
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		return pjDrop(es, pjMustIndex(t, es, artparse.ActionParseJobStart, 0))
	})
	pjProblemsWith(t, pjRun(c), "ingest", fmt.Sprintf("%q", first.IngestID), "parse.job.start")
}

func TestParseJobsCheckFlagsBundleMismatch(t *testing.T) {
	f := pjTwoJobs(t)
	other := f.recs["two"].ID
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		pjEdit(t, es, pjMustIndex(t, es, artparse.ActionParseJobStart, 0), func(js *artparse.JobStart) {
			for role, m := range js.Bundle {
				m.ArtifactID = other
				js.Bundle[role] = m
			}
		})
		return es
	})
	pjProblemsWith(t, pjRun(c), "bundle")
}

func TestParseJobsCheckFlagsIdentityMismatch(t *testing.T) {
	f := pjTwoJobs(t)
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		pjEdit(t, es, pjMustIndex(t, es, artparse.ActionParseJobStart, 0), func(js *artparse.JobStart) {
			js.Parser.Hash = strings.Repeat("f", 64)
		})
		return es
	})
	pjProblemsWith(t, pjRun(c), "parser")
}

func TestParseJobsCheckFlagsCompleteJobOverErroredIngest(t *testing.T) {
	f := pjTwoJobs(t)
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		i := pjMustIndex(t, es, evidence.ActionIngestEnd, 0)
		pjEdit(t, es, i, func(ic *evidence.IngestConclusion) { ic.Outcome, ic.Error = "incomplete", "forged" })
		es[i].Action = evidence.ActionIngestError
		return es
	})
	pjProblemsWith(t, pjRun(c), "complete", "records.ingest.end")
}

func TestParseJobsCheckFlagsJobEndNamingUnknownIngest(t *testing.T) {
	f := pjTwoJobs(t)
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		pjEdit(t, es, pjMustIndex(t, es, artparse.ActionParseJobEnd, 1), func(je *artparse.JobEnd) { je.IngestID = "NOSUCHINGEST" })
		return es
	})
	pjProblemsWith(t, pjRun(c), "NOSUCHINGEST")
}

func TestParseJobsCheckFlagsDuplicateJob(t *testing.T) {
	f := pjTwoJobs(t)
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		// the second job start and the first job end are each audited twice
		js, je := es[pjMustIndex(t, es, artparse.ActionParseJobStart, 1)], es[pjMustIndex(t, es, artparse.ActionParseJobEnd, 0)]
		return append(es, js, je)
	})
	rep := pjRun(c)
	pjProblemsWith(t, rep, "job 2", "more than once")
	pjProblemsWith(t, rep, "job 1", "more than one parse.job.end")
}

func TestParseJobsCheckFlagsAJobEndBeforeItsStart(t *testing.T) {
	f := pjTwoJobs(t)
	c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		s, e := pjMustIndex(t, es, artparse.ActionParseJobStart, 0), pjMustIndex(t, es, artparse.ActionParseJobEnd, 0)
		es[s], es[e] = es[e], es[s]
		return es
	})
	pjProblemsWith(t, pjRun(c), "job 1", "before")
}

func TestParseJobsCheckInterruptedRunIsANotice(t *testing.T) {
	t.Run("died before the ingest started", func(t *testing.T) {
		f := pjOneJob(t)
		c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
			return es[:pjMustIndex(t, es, artparse.ActionParseJobStart, 0)+1]
		})
		rep := pjRun(c)
		if len(rep.Problems) != 0 || len(rep.Notices) != 1 || !strings.HasPrefix(rep.Notices[0], pjPrefix) ||
			!strings.Contains(rep.Notices[0], "job 1") || !strings.Contains(rep.Notices[0], "no ingest started") {
			t.Errorf("problems %q notices %q, want one notice naming job 1 and \"no ingest started\"", rep.Problems, rep.Notices)
		}
	})

	t.Run("its ingest was recovered by a later ingest", func(t *testing.T) {
		// the audit log is closed when the failing job is aborted: no parse.job.end, no analysis.error; the next
		// run's Start writes records.ingest.recover for the dead ingest
		f := newRx(t, "fail", "next")
		failing := &rxParser{name: "fail", n: 1, after: func(context.Context, *parse.Input, parse.Emitter) error { return errors.New("parse failed") }}
		h := f.host(nil, failing, wbp("next"))
		artparse.SetNewWriterForTest(h, func(c *evidence.Case, p records.Parser, wo records.WriterOptions) (artparse.IngestWriter, error) {
			w, err := records.NewWriter(c, p, wo)
			if err != nil {
				return nil, err
			}
			return &auditClosingWriter{IngestWriter: w, c: c}, nil
		})
		if _, err := h.Run(context.Background(), artparse.RunOptions{}); err == nil {
			t.Fatal("the run did not fail")
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
		f2.run(hostWith(t, c2, artparse.SkipLimitMinimums, rxReg(t, wbp("next"))), artparse.Selection{})
		recs := f2.entries(evidence.ActionIngestRecover)
		if len(recs) != 1 {
			t.Fatalf("%d records.ingest.recover entries", len(recs))
		}
		rec := decodeEntry[evidence.IngestRecover](t, recs[0])
		rep := pjRun(c2)
		if len(rep.Problems) != 0 || len(rep.Notices) != 1 || !strings.HasPrefix(rep.Notices[0], pjPrefix) ||
			!strings.Contains(rep.Notices[0], fmt.Sprintf("%q", rec.IngestID)) || !strings.Contains(rep.Notices[0], fmt.Sprintf("%q", rec.ByIngestID)) ||
			!strings.Contains(rep.Notices[0], "recovered") {
			t.Errorf("problems %q notices %q, want one notice naming ingest %s recovered by %s", rep.Problems, rep.Notices, rec.IngestID, rec.ByIngestID)
		}
	})

	t.Run("died after the ingest started and nothing concluded it: the lifecycle check owns it", func(t *testing.T) {
		f := pjOneJob(t)
		c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
			return es[:pjMustIndex(t, es, evidence.ActionIngestStart, 0)+1]
		})
		if rep := pjRun(c); len(rep.Problems) != 0 || len(rep.Notices) != 0 {
			t.Errorf("problems %q notices %q: PJ4 adds nothing for an unconcluded ingest", rep.Problems, rep.Notices)
		}
		rep, err := c.VerifyWith(context.Background(), artparse.ParseJobsCheck())
		if err != nil {
			t.Fatal(err)
		}
		lifecycle := false
		for _, p := range rep.Problems {
			lifecycle = lifecycle || strings.Contains(p, "interrupted: records.ingest.start")
			if strings.HasPrefix(p, pjPrefix) {
				t.Errorf("full verify holds a parse jobs problem: %q", p)
			}
		}
		if !lifecycle {
			t.Errorf("full verify does not report the unconcluded ingest: %q", rep.Problems)
		}
	})

	t.Run("a run that concluded while a job was unconcluded is a problem", func(t *testing.T) {
		f := pjOneJob(t)
		c := forgeLog(t, f.c, func(es []evidence.AuditEntry) []evidence.AuditEntry {
			end := es[pjMustIndex(t, es, artparse.ActionAnalysisEnd, 0)]
			return append(es[:pjMustIndex(t, es, artparse.ActionParseJobStart, 0)+1:len(es)], end)
		})
		rep := pjRun(c)
		pjProblemsWith(t, rep, "job 1", "no parse.job.end")
		if len(rep.Notices) != 0 {
			t.Errorf("notices %q", rep.Notices)
		}
	})
}

func TestParseJobsCheckCleanOnCancelledRun(t *testing.T) {
	f := newRx(t, "one", "two", "three")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	two := &rxParser{name: "two", n: 1, after: func(ctx context.Context, _ *parse.Input, _ parse.Emitter) error {
		cancel()
		return ctx.Err()
	}}
	sum, err := f.host(nil, wbp("one"), two, wbp("three")).Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Cancelled || f.count("analysis.error") != 1 || len(f.jobStarts()) != 2 {
		t.Fatalf("summary %+v, analysis.error %d, job starts %d", sum, f.count("analysis.error"), len(f.jobStarts()))
	}
	rep, err := f.c.VerifyWith(context.Background(), artparse.ParseJobsCheck())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(append([]string{}, rep.Problems...), rep.Notices...) {
		if strings.HasPrefix(p, pjPrefix) {
			t.Errorf("a cancelled run is reported: %q", p)
		}
	}
}

func TestParseJobsCheckCleanOnEveryJobOutcome(t *testing.T) {
	scenarios := []struct {
		name    string
		outcome string // the outcome of the job the scenario is about
		parser  string
		run     func(t *testing.T) (*rx, artparse.RunSummary)
	}{
		{"refused", artparse.OutcomeRefused, "bad", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "bad", "good")
			tamper(t, f.c, f.recs["bad"])
			return f, f.run(f.host(nil, wbp("bad"), wbp("good")), artparse.Selection{})
		}},
		{"unparsed", artparse.OutcomeUnparsed, "a", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "a", "b")
			n := 0
			h := f.host(func(o *artparse.Options) {
				artparse.WithWrapFile(o, func(rf artparse.ReadFile) artparse.ReadFile {
					n++
					if n == 1 {
						return failingFile{rf, errors.New("disk read error")}
					}
					return rf
				})
			}, wbp("a"), wbp("b"))
			return f, f.run(h, artparse.Selection{})
		}},
		{"skipped (already parsed)", artparse.OutcomeSkipped, "p", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "p")
			h := f.host(nil, wbp("p"))
			f.run(h, artparse.Selection{})
			return f, f.run(h, artparse.Selection{})
		}},
		{"skipped (not applicable)", artparse.OutcomeSkipped, "na", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "na")
			return f, f.run(f.host(nil, parsertest.ProbeOnly{Name: "na", Version: "1.0.0", Status: parse.NotApplicable, Reason: "not mine"}), artparse.Selection{})
		}},
		{"incomplete (records refused)", artparse.OutcomeIncomplete, "inv", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "inv")
			return f, f.run(f.host(nil, parsertest.Invalid{Name: "inv", Version: "1.0.0", Mode: parsertest.InvalidNulInSummary, N: 2}), artparse.Selection{})
		}},
		{"panic", artparse.OutcomeIncomplete, "boom", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "boom", "fine")
			h := f.host(nil, parsertest.Panicking{Name: "boom", Version: "1.0.0", Where: parsertest.PanicAfterEmits, After: 2, Value: "kaboom"}, wbp("fine"))
			return f, f.run(h, artparse.Selection{})
		}},
		{"timed out (cooperative)", artparse.OutcomeIncomplete, "slow", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "slow", "next")
			return f, f.run(f.host(shortTimeouts, parsertest.Slow{Name: "slow", Version: "1.0.0", Cooperative: true}, wbp("next")), artparse.Selection{})
		}},
		{"integrity (input changed during the job)", artparse.OutcomeIncomplete, "chg", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "chg", "next")
			p := &rxParser{name: "chg", n: 2}
			p.after = func(context.Context, *parse.Input, parse.Emitter) error { rewrite(t, f.c, f.recs["chg"]); return nil }
			h := f.host(withLimits(func(l *parse.Limits) { l.MemInputMax = 0 }), p, wbp("next"))
			return f, f.run(h, artparse.Selection{})
		}},
		{"abandoned", artparse.OutcomeIncomplete, "two", func(t *testing.T) (*rx, artparse.RunSummary) {
			f := newRx(t, "one", "two", "three")
			aborted := make(chan struct{}, 1)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			emitted := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			go func() {
				select {
				case <-emitted:
					cancel()
				case <-ctx.Done():
				}
			}()
			two := &rxParser{name: "two", n: 2, after: func(context.Context, *parse.Input, parse.Emitter) error {
				close(emitted)
				<-release
				return nil
			}}
			h := f.host(both(func(o *artparse.Options) { o.Limits.GracePeriod = 30 * time.Millisecond }, withWriter(func(w *rxWriter) {
				w.aborted = func() { aborted <- struct{}{} }
			})), wbp("one"), two, wbp("three"))
			sum, err := h.Run(ctx, artparse.RunOptions{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			select {
			case <-aborted:
			case <-time.After(2 * time.Minute): // safety net only
				t.Fatal("the abandoned job was never concluded")
			}
			return f, sum
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f, sum := sc.run(t)
			if got := jobOf(t, sum, sc.parser).Outcome; got != sc.outcome {
				t.Fatalf("job %s outcome %q, want %q (the scenario no longer produces its outcome)", sc.parser, got, sc.outcome)
			}
			pjClean(t, f.c)
		})
	}
}

func TestParseJobsCheckIgnoresOtherOps(t *testing.T) {
	f := newRx(t)
	for _, op := range []string{"examine", "sqlite-recover"} {
		id := "OTHER-" + op
		if _, err := f.c.Audit.Append(artparse.ActionAnalysisStart, "", map[string]any{"analysis_id": id, "op": op}); err != nil {
			t.Fatal(err)
		}
		in := evidence.IngestStart{IngestID: "ING-" + op, Parser: "x", ParserVersion: "1", AnalysisID: id, Artifacts: []string{"a"}}
		if _, err := f.c.Audit.Append(evidence.ActionIngestStart, "", in.Details()); err != nil {
			t.Fatal(err)
		}
	}
	// an ingest whose analysis id names no analysis.start at all
	orphan := evidence.IngestStart{IngestID: "ING-orphan", Parser: "x", ParserVersion: "1", AnalysisID: "NOSUCH", Artifacts: []string{"a"}}
	if _, err := f.c.Audit.Append(evidence.ActionIngestStart, "", orphan.Details()); err != nil {
		t.Fatal(err)
	}
	pjClean(t, f.c)
}

func TestParseJobsCheckListsAtMost50Problems(t *testing.T) {
	f := newRx(t)
	start := artparse.AnalysisStart{AnalysisID: "P60", Op: "parse"}
	if _, err := f.c.Audit.Append(artparse.ActionAnalysisStart, "", start.Details()); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		in := evidence.IngestStart{IngestID: fmt.Sprintf("ING%02d", i), Parser: "x", ParserVersion: "1", AnalysisID: "P60", Artifacts: []string{"a"}}
		if _, err := f.c.Audit.Append(evidence.ActionIngestStart, "", in.Details()); err != nil {
			t.Fatal(err)
		}
	}
	rep := pjRun(f.c)
	more := 0
	for _, p := range rep.Problems {
		if !strings.HasPrefix(p, pjPrefix) {
			t.Errorf("problem %q lacks the prefix", p)
		}
		if strings.Contains(p, "10 more") {
			more++
		}
	}
	if len(rep.Problems) != 51 || more != 1 {
		t.Errorf("%d problems (%d \"10 more\" lines), want 50 listed and one \"10 more\" line", len(rep.Problems), more)
	}
}

func TestParseJobsCheckUnreadableAuditIsAProblem(t *testing.T) {
	f := newRx(t)
	fh, err := os.OpenFile(filepath.Join(f.c.Dir, "audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("this is not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	rep := pjRun(f.c)
	if len(rep.Problems) != 1 || !strings.HasPrefix(rep.Problems[0], pjPrefix+"audit log unreadable") {
		t.Errorf("problems %q, want the unreadable audit log named", rep.Problems)
	}
}
