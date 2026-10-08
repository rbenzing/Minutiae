package artparse

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/version"
)

// Why a run ended early (RunSummary.Stopped).
const (
	StoppedAbandoned    = "abandoned"
	StoppedCancelled    = "cancelled"
	StoppedAuditFailure = "audit-failure"
	StoppedAbortFailure = "abort-failure"
	StoppedRunError     = "run-error" // any other run-level failure (ErrIngestActive, a failed Start)
)

// concludeTimeout bounds the work after a job that must not depend on the run's context: re-hashing
// the inputs, Flush, End and Abort (A8, the bound records.End itself uses).
const concludeTimeout = 5 * time.Minute

const (
	maxErrorText = 2 << 10
	maxPathText  = 4096
	sourceHost   = "host"
)

// Event is one progress event of a run (the NDJSON events of `parse run --json`).
type Event struct {
	Kind        string // run.start | job.start | job.progress | job.end
	Job         int
	Done, Total int64
	Result      *JobResult
	ParseID     string
	Jobs        int
}

// RunOptions selects and observes a run.
type RunOptions struct {
	Selection
	// OnEvent is called ONLY on the goroutine that called Run: progress produced on the parser's goroutine
	// reaches it through the guard's pump channel (non-blocking send, dropped when full or after seal),
	// never directly.
	OnEvent func(Event)
}

// run is the state of one Run call. It lives on the goroutine that called Run.
type run struct {
	h       *Host
	opt     RunOptions
	snap    *Snapshot
	parseID string
	sum     RunSummary
	// totals of the jobs concluded so far, for analysis.end and analysis.error
	rejected, warnings int
}

func (r *run) emit(e Event) {
	if r.opt.OnEvent != nil {
		r.opt.OnEvent(e)
	}
}

// audit appends one host entry. The test hook runs first and can refuse the append.
func (r *run) audit(action string, d map[string]any) error {
	if r.h.auditHook != nil {
		if err := r.h.auditHook(action, d); err != nil {
			return fmt.Errorf("audit %s: %w", action, err)
		}
	}
	if _, err := r.h.c.Audit.Append(action, "", d); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// jobState is what one job accumulates for its result and its parse.job.end entry.
type jobState struct {
	res      JobResult
	errText  string
	streamed []string
	lookups  []LookupOpen
}

func (st *jobState) set(outcome, reason string) {
	st.res.Outcome, st.res.Reason = outcome, cleanAuditText(reason, maxReason)
}

func (st *jobState) fail(outcome, reason string, err error) {
	st.set(outcome, reason)
	if err != nil {
		st.errText = cleanAuditText(err.Error(), maxErrorText)
	}
}

// capture keeps what the bundle knows about its inputs; it runs before the bundle is closed.
func (st *jobState) capture(b *bundle) {
	st.streamed, st.lookups = b.streamed(), b.lookups()
}

func newResult(j Job) JobResult {
	id := j.Parser.Identity()
	return JobResult{
		Job: j.N, Parser: id.Name, Version: id.Version, Hash: id.Hash,
		ArtifactID: j.Primary.Artifact.ID, Logical: cleanAuditText(j.Primary.Logical, maxPathText), Platform: j.Primary.Platform,
	}
}

func snapshotAudit(s *parse.SnapshotInfo) *AuditSnapshot {
	if s == nil {
		return nil
	}
	return &AuditSnapshot{Name: cleanAuditText(s.Name, maxNoteKey), Xid: s.Xid}
}

// members lists the members of a job, the primary first, then the other roles by name.
func (j Job) members() []*member {
	out := []*member{{role: j.Parser.meta.Inputs[0].Role, m: j.Primary}}
	roles := make([]string, 0, len(j.Others))
	for role := range j.Others {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		out = append(out, &member{role: role, m: j.Others[role]})
	}
	return out
}

func (r *run) jobStart(j Job) JobStart {
	id := j.Parser.Identity()
	js := JobStart{
		AnalysisID: r.parseID, Job: j.N, Parser: AuditParser{Name: id.Name, Version: id.Version, Hash: id.Hash},
		Bundle: map[string]BundleMember{}, Explicit: j.Explicit,
	}
	for _, mb := range j.members() {
		js.Bundle[mb.role] = BundleMember{
			ArtifactID: mb.m.Artifact.ID, SHA256: mb.m.Artifact.SHA256, LogicalPath: cleanAuditText(mb.m.Logical, maxPathText),
			Platform: mb.m.Platform, Namer: cleanAuditText(mb.m.Namer, maxNoteKey), Snapshot: snapshotAudit(mb.m.Snapshot),
		}
	}
	return js
}

func (r *run) jobEnd(j Job, st *jobState) JobEnd {
	res := st.res
	end := JobEnd{
		AnalysisID: r.parseID, Job: j.N, Outcome: res.Outcome, Reason: res.Reason, IngestID: res.IngestID, Records: res.Records,
		Rejected: res.Rejected, Warnings: res.Warnings, WarningsSuppressed: res.WarningsSuppressed, DurationMS: res.Duration.Milliseconds(),
		Notes: res.Notes, Error: st.errText, Abandoned: res.Abandoned, TimedOut: res.TimedOut, Integrity: res.Integrity,
		ArtifactIncomplete: []string{}, Streamed: st.streamed, Lookups: st.lookups, Snapshot: snapshotAudit(j.Primary.Snapshot),
	}
	if end.Notes == nil {
		end.Notes = map[string]string{}
	}
	if end.Streamed == nil {
		end.Streamed = []string{}
	}
	if end.Lookups == nil {
		end.Lookups = []LookupOpen{}
	}
	if res.Panic != nil {
		end.Panic = &AuditPanic{Value: res.Panic.Value, Stack: res.Panic.Stack}
	}
	for _, mb := range j.members() {
		if mb.m.Artifact.Incomplete {
			end.ArtifactIncomplete = append(end.ArtifactIncomplete, mb.m.Artifact.ID)
		}
	}
	sort.Strings(end.ArtifactIncomplete)
	return end
}

func (r *run) analysisStart(parsers []Registered, jobs []Job) AnalysisStart {
	lim := r.h.limits
	as := AnalysisStart{
		AnalysisID: r.parseID, Op: "parse", Parsers: []AuditParser{},
		Filters: AuditFilters{Parsers: []string{}, Artifacts: append([]string{}, r.opt.Artifacts...), IncludeSnapshots: r.opt.IncludeSnapshots},
		Reparse: r.opt.Reparse, MinutiaeVersion: version.Version, Commit: version.Commit, GoVersion: runtime.Version(),
		Limits: AuditLimits{
			ProbeBytes: lim.ProbeBytes, MemInputMax: lim.MemInputMax, MemInputTotal: lim.MemInputTotal, MemBudget: lim.MemBudget,
			MaxRecords: lim.MaxRecords, MaxRejected: lim.MaxRejected, MaxNotes: lim.MaxNotes, MaxLookupOpens: lim.MaxLookupOpens,
			ProbeTimeoutMS: lim.ProbeTimeout.Milliseconds(), TimeoutMS: lim.Timeout.Milliseconds(), GraceMS: lim.GracePeriod.Milliseconds(),
		},
	}
	for _, p := range parsers {
		id := p.Identity()
		as.Parsers = append(as.Parsers, AuditParser{Name: id.Name, Version: id.Version, Hash: id.Hash})
	}
	for _, ref := range r.opt.Parsers {
		name := ref.Name
		if ref.Version != "" {
			name += "@" + ref.Version
		}
		as.Filters.Parsers = append(as.Filters.Parsers, name)
	}
	sort.Strings(as.Filters.Artifacts)
	for _, j := range jobs {
		switch j.Status {
		case StatusNew:
			as.Jobs.New++
		case StatusAlreadyParsed:
			as.Jobs.AlreadyParsed++
		case StatusStaleBundle:
			as.Jobs.StaleBundle++
		default:
			as.Jobs.Unparsed++
		}
	}
	return as
}

// outcomes counts the concluded jobs by outcome.
func (r *run) outcomes() map[string]int {
	out := map[string]int{}
	for _, j := range r.sum.Jobs {
		if j.Outcome != OutcomeNotRun {
			out[j.Outcome]++
		}
	}
	return out
}

// conclude writes analysis.end, or analysis.error when the run stopped early, and returns the audit
// error, if any. notRun is the number of jobs that never started.
func (r *run) conclude(notRun int) error {
	if r.sum.Stopped == "" {
		return r.audit(ActionAnalysisEnd, AnalysisEnd{
			AnalysisID: r.parseID, Jobs: r.outcomes(), Records: r.sum.Records, Rejected: r.rejected, Warnings: r.warnings,
		}.Details())
	}
	return r.audit(ActionAnalysisError, AnalysisError{
		AnalysisID: r.parseID, Error: "the run stopped: " + r.sum.Stopped, Jobs: r.outcomes(), Records: r.sum.Records,
		Rejected: r.rejected, Warnings: r.warnings, NotRun: notRun,
	}.Details())
}

// Run runs the jobs of a selection, in order, each in the sandbox, and audits the run (F8). It takes
// ONE snapshot of the manifest and passes it to discovery, to the shared front half (prepareJob) and
// to the checks after Start. It returns a non-nil error only for run-level failures (a failed audit
// append, ErrIngestActive, a failed Abort, a case that needs an upgrade, a context that ended before
// anything started); what happened to each job is in the summary. The summary is returned with the
// error too.
func (h *Host) Run(ctx context.Context, opt RunOptions) (RunSummary, error) {
	if err := h.c.RequireSchema(evidence.CurrentSchema); err != nil {
		return RunSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return RunSummary{}, err
	}
	snap, err := h.TakeSnapshot()
	if err != nil {
		return RunSummary{}, err
	}
	jobs, _, err := h.Discover(ctx, snap, opt.Selection)
	if err != nil {
		return RunSummary{}, err
	}
	parsers, err := h.selectParsers(opt.Parsers)
	if err != nil {
		return RunSummary{}, err
	}
	r := &run{h: h, opt: opt, snap: snap, parseID: evidence.NewAcquisitionID(time.Now())}
	r.sum.ParseID = r.parseID
	if err := r.audit(ActionAnalysisStart, r.analysisStart(parsers, jobs).Details()); err != nil {
		r.sum.Stopped = StoppedAuditFailure
		return r.sum, err
	}
	r.emit(Event{Kind: "run.start", ParseID: r.parseID, Jobs: len(jobs)})

	var runErr error
	done := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			r.sum.Cancelled, r.sum.Stopped = true, StoppedCancelled
			break
		}
		st := &jobState{res: newResult(j)}
		started := time.Now()
		if err := r.audit(ActionParseJobStart, r.jobStart(j).Details()); err != nil {
			runErr, r.sum.Stopped = err, StoppedAuditFailure
			break
		}
		first := st.res
		r.emit(Event{Kind: "job.start", Job: j.N, Result: &first})
		if early, ok := r.skipWithoutOpening(j); ok {
			st.set(early.outcome, early.reason)
		} else {
			pr, perr := h.prepareJob(ctx, snap, j, r.parseID)
			runErr = r.execute(ctx, j, pr, perr, st)
		}
		st.res.Duration = time.Since(started)
		if runErr == nil && st.res.Outcome == OutcomeUnparsed {
			runErr = r.audit(evidence.ActionAnalysisWarning, HostWarning{
				AnalysisID: r.parseID, Path: warnPathOf(j), Reason: st.res.Reason, Source: sourceHost,
			}.Details())
		}
		if st.res.Outcome == "" { // a run-level failure cut the job short
			st.set(OutcomeIncomplete, "the run failed")
			if runErr != nil {
				st.errText = cleanAuditText(runErr.Error(), maxErrorText)
			}
		}
		end := r.jobEnd(j, st)
		endErr := r.audit(ActionParseJobEnd, end.Details())
		r.record(st)
		done++
		if runErr == nil && endErr != nil {
			runErr, r.sum.Stopped = endErr, StoppedAuditFailure
		}
		res := st.res
		r.emit(Event{Kind: "job.end", Job: j.N, Result: &res})
		if runErr != nil {
			break
		}
		if r.sum.Stopped != "" {
			break
		}
	}
	if runErr != nil && r.sum.Stopped == "" {
		r.sum.Stopped = StoppedRunError
	}
	for _, j := range jobs[done:] {
		nr := newResult(j)
		nr.Outcome, nr.Reason = OutcomeNotRun, "the run stopped before this job started"
		r.sum.Jobs = append(r.sum.Jobs, nr)
	}
	cerr := r.conclude(len(jobs) - done)
	if runErr == nil {
		runErr = cerr
		if cerr != nil {
			r.sum.Stopped = StoppedAuditFailure
		}
	}
	return r.sum, runErr
}

// record adds a concluded job to the summary and the totals.
func (r *run) record(st *jobState) {
	r.sum.Jobs = append(r.sum.Jobs, st.res)
	r.sum.Records += st.res.Records
	r.rejected += st.res.Rejected
	r.warnings += st.res.Warnings
}

func warnPathOf(j Job) string {
	if j.Primary.Logical != "" {
		return cleanAuditText(j.Primary.Logical, maxPathText)
	}
	return "artifact:" + j.Primary.Artifact.ID
}

type earlyOutcome struct{ outcome, reason string }

// skipWithoutOpening decides the jobs that are not parsed in this run before any artifact is opened:
// an already-parsed job (skipped) and a stale bundle (unparsed, A14) unless --reparse asks for them.
func (r *run) skipWithoutOpening(j Job) (earlyOutcome, bool) {
	if r.opt.Reparse {
		return earlyOutcome{}, false
	}
	switch j.Status {
	case StatusAlreadyParsed:
		id := j.Parser.Identity()
		return earlyOutcome{OutcomeSkipped, fmt.Sprintf("already parsed by %s %s", id.Name, id.Version)}, true
	case StatusStaleBundle:
		return earlyOutcome{OutcomeUnparsed, "stale bundle: " + j.Reason + " (--reparse parses it again)"}, true
	}
	return earlyOutcome{}, false
}

// execute settles one job after the shared front half: it records what the front half decided, or runs
// the job. It returns a run-level error only.
func (r *run) execute(ctx context.Context, j Job, pr prepared, perr error, st *jobState) error {
	switch {
	case perr != nil && ctx.Err() != nil:
		st.fail(OutcomeIncomplete, "cancelled", perr)
		r.sum.Cancelled, r.sum.Stopped = true, StoppedCancelled
	case perr != nil:
		// an I/O failure says nothing about the evidence: the job is not parsed, and the next one is tried
		st.fail(OutcomeUnparsed, "input unreadable: "+r.h.caseRelative(perr.Error()), perr)
	case pr.bundle == nil:
		st.res.Integrity = pr.integrity
		switch pr.status {
		case StatusRefused:
			st.set(OutcomeRefused, pr.reason)
		case StatusSkipped:
			st.set(OutcomeSkipped, pr.reason)
		default:
			st.set(OutcomeUnparsed, pr.reason)
		}
	default:
		return r.parseJob(ctx, j, pr.bundle, st)
	}
	return nil
}

// parseJob runs the job over its verified bundle: Start, the manifest check, Parse in the sandbox, the
// seal, the re-hash of every input, then End or (Flush and) Abort. The emitter and the bundle are
// sealed on every way out of it.
func (r *run) parseJob(ctx context.Context, j Job, b *bundle, st *jobState) (runErr error) {
	h := r.h
	defer b.close()
	defer func() { st.capture(b) }()

	w, err := h.newWriter(h.c, j.Parser.Identity(), records.WriterOptions{BatchRows: h.batchRows})
	if err != nil {
		if errors.Is(err, evidence.ErrNeedsUpgrade) {
			return err
		}
		st.fail(OutcomeRefused, "cannot write records: "+err.Error(), err)
		return nil
	}
	ids := make([]string, 0, 1+len(j.Others))
	for _, mb := range j.members() {
		ids = append(ids, mb.m.Artifact.ID)
	}
	reingest := r.opt.Reparse && (j.Status == StatusAlreadyParsed || j.Status == StatusStaleBundle)
	if err := w.Start(ctx, records.StartOptions{AnalysisID: r.parseID, Artifacts: ids, AllowReingest: reingest}); err != nil {
		return r.startFailed(ctx, j, st, err)
	}
	st.res.IngestID = w.IngestID()
	if h.afterSt != nil {
		h.afterSt()
	}
	if err := b.recheckManifest(r.snap); err != nil {
		st.res.Integrity = errors.Is(err, evidence.ErrIntegrity)
		return r.abortJob(ctx, w, st, OutcomeRefused, "integrity: "+r.h.caseRelative(err.Error()), err, nil)
	}

	var pump chan func()
	var report func(done, total int64)
	if r.opt.OnEvent != nil {
		pump = make(chan func(), 16)
		report = func(done, total int64) { r.emit(Event{Kind: "job.progress", Job: j.N, Done: done, Total: total}) }
	}
	arts := make(map[string]parse.Artifact, len(b.members))
	for _, mb := range b.members {
		arts[mb.role] = mb.artifact()
	}
	em, err := newEmitter(ctx, w, j.Parser, arts, h.limits, pump, report)
	if err != nil {
		return r.abortJob(ctx, w, st, OutcomeRefused, "cannot start the emitter: "+err.Error(), err, nil)
	}
	in := b.input(false)
	sealAll := func() { em.seal(); b.seal() }
	defer sealAll()
	g := guard(ctx, h.limits.Timeout, h.limits.GracePeriod, sealAll, pump, func(pctx context.Context) error {
		return j.Parser.p.Parse(pctx, in, em)
	})
	sealAll() // every way out of Parse: success, error, panic, timeout, cancel (abandonment sealed in guard too)

	st.res.Panic, st.res.TimedOut = g.Panic, g.TimedOut
	st.res.Abandoned = g.Abandoned || em.stuckInWriter()
	st.res.Notes = em.notes()
	reason, cause := r.parseFailure(g, em)

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), concludeTimeout)
	defer cancel()
	if rerr := b.recheck(rctx); rerr != nil {
		if errors.Is(rerr, evidence.ErrIntegrity) {
			st.res.Integrity = true
			reason, cause = "input changed during or after the job: "+h.caseRelative(rerr.Error()), rerr
		} else if cause == nil {
			reason, cause = "re-reading the inputs failed", fmt.Errorf("re-reading the inputs: %w", rerr)
		}
	}
	if g.Cancelled || (cause != nil && ctx.Err() != nil) {
		r.sum.Cancelled, r.sum.Stopped = true, StoppedCancelled
	}
	if st.res.Abandoned {
		r.sum.Stopped = StoppedAbandoned
	}

	if cause == nil {
		_, rej, _ := em.counts()
		if rerr := parse.CheckRefusals(rej); rerr != nil {
			// a run that lost records must never supersede an older complete run (it would hide its records)
			reason, cause = "records-refused", rerr
		}
	}
	if cause == nil {
		res, err := w.End(rctx)
		if err == nil {
			r.fill(st, res)
			st.set(OutcomeComplete, "")
			return nil
		}
		st.res.Integrity = errors.Is(err, evidence.ErrIntegrity)
		reason, cause = "concluding the ingest failed", fmt.Errorf("concluding the ingest: %w", err)
	}
	return r.abortJob(rctx, w, st, OutcomeIncomplete, reason, cause, em)
}

// startFailed settles a job whose Writer.Start failed.
func (r *run) startFailed(ctx context.Context, j Job, st *jobState, err error) error {
	switch {
	case ctx.Err() != nil:
		st.fail(OutcomeIncomplete, "cancelled", err)
		r.sum.Cancelled, r.sum.Stopped = true, StoppedCancelled
	case errors.Is(err, records.ErrAlreadyIngested):
		st.set(OutcomeSkipped, "already parsed by this parser version")
	case errors.Is(err, records.ErrParserIdentityConflict):
		st.fail(OutcomeRefused, "this build's parser differs from the one recorded for that version (a development build?)", err)
	case errors.Is(err, evidence.ErrIntegrity):
		st.res.Integrity = true
		st.fail(OutcomeRefused, "integrity: "+r.h.caseRelative(err.Error()), err)
	case errors.Is(err, records.ErrIngestActive), errors.Is(err, evidence.ErrNeedsUpgrade):
		return err
	default:
		return fmt.Errorf("starting the ingest of job %d: %w", j.N, err)
	}
	return nil
}

// parseFailure turns what the guard saw into the failure of the job (nil when Parse returned cleanly in
// time) and its short reason.
func (r *run) parseFailure(g Guarded, em *emitter) (reason string, cause error) {
	switch {
	case g.Panic != nil:
		return "parser panicked", fmt.Errorf("parser panicked: %s", g.Panic.Value)
	case g.Abandoned && g.TimedOut:
		return "timed out and abandoned: the parser did not stop", errors.New("the parser did not stop after its time limit and was abandoned")
	case g.Abandoned:
		return "abandoned: the parser did not stop after cancellation", errors.New("the parser did not stop after cancellation and was abandoned")
	case em.stuckInWriter():
		return "abandoned: a writer call did not finish within the grace period", errors.New("a writer call did not finish within the grace period")
	case g.TimedOut:
		return "timed out", fmt.Errorf("the parser reached its time limit of %v", r.h.limits.Timeout)
	case g.Cancelled:
		return "cancelled", context.Canceled
	case g.Err != nil:
		return failureReason(g.Err), g.Err
	}
	return "", nil
}

// failureReason is the short reason for an error Parse returned.
func failureReason(err error) string {
	switch {
	case errors.Is(err, parse.ErrBudget):
		return "memory budget exceeded"
	case errors.Is(err, parse.ErrRecordCap):
		return "record cap reached"
	case errors.Is(err, parse.ErrRejectedCap):
		return "too many rejected records"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "parser failed: " + cleanAuditText(err.Error(), 256)
}

// fill copies what the ingest conclusion reports into the job result.
func (r *run) fill(st *jobState, res records.IngestResult) {
	st.res.IngestID = res.IngestID
	st.res.Records = res.Records
	st.res.Rejected = res.Rejected
	st.res.Warnings = res.Warnings
	st.res.WarningsSuppressed = res.WarningsSuppressed
}

// abortJob concludes a failed job: Flush (unless the inputs changed: those records may describe other
// bytes) then Abort. A failed Abort is a run-level error: the writer released the live-ingest slot, the
// unconcluded ingest is recovered by the next Start.
//
// The context: the call at the end of parseJob passes its own detached, bounded context; the other
// callers pass the host context, which is harmless there (nothing is buffered at those points and Flush
// is skipped for integrity) because Writer.Abort detaches itself. Flush is the only call that does not.
//
// When a writer call is stuck (the job is already abandoned), a stuck call may hold the writer's lock:
// Flush and Abort then wait at most GracePeriod and the job is abandoned with that reason, never an
// unbounded wait. The goroutine that is left waiting ends when the stuck call does.
func (r *run) abortJob(ctx context.Context, w ingestWriter, st *jobState, outcome, reason string, cause error, em *emitter) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), concludeTimeout)
	defer cancel()
	type concluded struct {
		flushErr, abortErr error
		res                records.IngestResult
	}
	flush := !st.res.Integrity
	ch := make(chan concluded, 1)
	go func() {
		var c concluded
		if flush {
			c.flushErr = w.Flush(actx)
		}
		c.res, c.abortErr = w.Abort(actx, cause)
		ch <- c
	}()
	var c concluded
	if st.res.Abandoned {
		timer := time.NewTimer(r.h.limits.GracePeriod)
		defer timer.Stop()
		select {
		case c = <-ch:
		case <-timer.C:
			st.res.Abandoned = true
			r.sum.Stopped = StoppedAbandoned
			if em != nil {
				acc, rej, warn := em.counts()
				st.res.Records, st.res.Rejected, st.res.Warnings = int64(acc), rej, warn
			}
			st.fail(outcome, reason+"; concluding the ingest did not finish within the grace period (a writer call is stuck)", cause)
			return nil
		}
	} else {
		c = <-ch
	}
	if c.flushErr != nil {
		reason += "; flushing the accepted records failed: " + cleanAuditText(c.flushErr.Error(), 256)
	}
	if c.abortErr != nil {
		if em != nil {
			acc, rej, warn := em.counts()
			st.res.Records, st.res.Rejected, st.res.Warnings = int64(acc), rej, warn
		}
		st.fail(outcome, reason, cause)
		r.sum.Stopped = StoppedAbortFailure
		return fmt.Errorf("concluding the ingest of job %d after %q failed: %w", st.res.Job, reason, c.abortErr)
	}
	r.fill(st, c.res)
	st.fail(outcome, reason, cause)
	return nil
}
