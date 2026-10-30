package artparse

import (
	"context"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// maxJobsFindings is the number of problems (and of notices) ParseJobsCheck lists; one line counts the rest.
const maxJobsFindings = 50

// jobsCheckCtxEvery is how many audit entries the check reads between two looks at its context.
const jobsCheckCtxEvery = 1024

// ParseJobsCheck is the parse-job cross-check of spec 17 Q16 (rules PJ1 to PJ5 of plan 4H): every ingest of a
// parse or parse-recover analysis matches the parse.job.start before it, and every parse.job.end matches its ingest.
// The command line always composes it; no option disables it.
//
// PJ1: an ingest of a parse analysis is preceded by exactly one not-yet-matched parse.job.start of that analysis
// with the same parser identity and the same bundle artifacts. PJ2: a parse.job.end that names an ingest names the
// ingest matched to its job, and a "complete" job end follows a "complete" records.ingest.end. PJ3: a job is
// started once and ended at most once, after its start. PJ4: a job without an end is a problem when its analysis
// concluded, and a notice when the process died before the job's ingest started or the ingest was concluded by
// records.ingest.recover; an ingest that nothing concluded is the lifecycle check's problem and adds nothing here.
// PJ5: ingests of other analyses (examine, sqlite-recover, ...) are not checked.
func ParseJobsCheck() evidence.VerifyCheck {
	return func(ctx context.Context, c *evidence.Case, _ []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		entries, err := c.ReadAudit()
		if err != nil {
			rep.AddProblem("parse jobs: audit log unreadable: %v", err)
			return
		}
		f := &jobsFindings{}
		pj := &jobsCheck{
			f:         f,
			analyses:  map[string]*jobsAnalysis{},
			ingests:   map[string]*jobsIngest{},
			orphanEnd: map[string]bool{},
		}
		for i, e := range entries {
			if i%jobsCheckCtxEvery == 0 && ctx.Err() != nil {
				break // the findings made so far are kept; the composed-check runner reports the cancel
			}
			pj.entry(e)
		}
		if ctx.Err() == nil {
			pj.unconcluded(ctx)
		}
		f.flush(rep)
	}
}

// jobsFindings collects the problems and notices of one check, in the order found.
type jobsFindings struct {
	problems, notices []string
}

func (f *jobsFindings) problem(format string, a ...any) {
	f.problems = append(f.problems, "parse jobs: "+fmt.Sprintf(format, a...))
}

func (f *jobsFindings) notice(format string, a ...any) {
	f.notices = append(f.notices, "parse jobs: "+fmt.Sprintf(format, a...))
}

// flush adds at most maxJobsFindings of each kind to rep, then one line that counts the rest.
func (f *jobsFindings) flush(rep *evidence.VerifyReport) {
	for i, p := range f.problems {
		if i == maxJobsFindings {
			rep.AddProblem("parse jobs: %d more problems not listed", len(f.problems)-maxJobsFindings)
			break
		}
		rep.AddProblem("%s", p)
	}
	for i, n := range f.notices {
		if i == maxJobsFindings {
			rep.AddNotice("parse jobs: %d more notices not listed", len(f.notices)-maxJobsFindings)
			break
		}
		rep.AddNotice("%s", n)
	}
}

type jobsAnalysis struct {
	id        string
	concluded bool // analysis.end or analysis.error seen
	errored   bool // the conclusion was analysis.error
	errText   string
	cur       int // index of the first job that is neither matched, ended nor superseded
	jobs      []*jobsJob
	byN       map[int]*jobsJob
}

type jobsJob struct {
	n       int
	start   JobStart
	startAt int64
	ended   bool
	// superseded: a later job started in the same run while this one had no end (reported once, at that start)
	superseded bool
	ingest     *jobsIngest // the ingest matched to the job (PJ1)
	// noIngestEnd: the parse.job.end named no ingest although one was started by the job; settled by unconcluded,
	// because only the ingest's later conclusion tells a failed Writer.Start from a lie
	noIngestEnd *jobsNoIngestEnd
}

// jobsNoIngestEnd is a parse.job.end that names no ingest.
type jobsNoIngestEnd struct {
	seq     int64
	outcome string
}

type jobsIngest struct {
	id          string
	seq         int64
	start       evidence.IngestStart
	job         *jobsJob
	concludedBy string // records.ingest.end or records.ingest.error, "" while none
	outcome     string // the conclusion's outcome
	recoveredBy string // the ingest that wrote records.ingest.recover for it, "" while none
}

type jobsCheck struct {
	f         *jobsFindings
	analyses  map[string]*jobsAnalysis
	order     []*jobsAnalysis
	ingests   map[string]*jobsIngest
	orphanEnd map[string]bool // "analysis/job" of a parse.job.end seen before any parse.job.start
}

func jobKey(analysis string, n int) string { return fmt.Sprintf("%s/%d", analysis, n) }

// str reads a string detail, "" when absent or of another type.
func str(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

func (pj *jobsCheck) entry(e evidence.AuditEntry) {
	switch e.Action {
	case ActionAnalysisStart:
		pj.analysisStart(e)
	case ActionAnalysisEnd, ActionAnalysisError:
		if a := pj.analyses[str(e.Details, "analysis_id")]; a != nil {
			a.concluded = true
			if e.Action == ActionAnalysisError {
				a.errored, a.errText = true, str(e.Details, "error")
			}
		}
	case ActionParseJobStart:
		pj.jobStart(e)
	case ActionParseJobEnd:
		pj.jobEnd(e)
	case evidence.ActionIngestStart:
		pj.ingestStart(e)
	case evidence.ActionIngestEnd, evidence.ActionIngestError:
		pj.ingestConclusion(e)
	case evidence.ActionIngestRecover:
		pj.ingestRecover(e)
	}
}

// analysisStart registers an analysis of op parse or parse-recover; every other analysis is out of scope (PJ5).
func (pj *jobsCheck) analysisStart(e evidence.AuditEntry) {
	op := str(e.Details, "op")
	if op != "parse" && op != OpParseRecover {
		return
	}
	id := str(e.Details, "analysis_id")
	if pj.analyses[id] != nil {
		pj.f.problem("run %q: analysis.start is audited more than once (audit seq %d)", id, e.Seq)
		return
	}
	a := &jobsAnalysis{id: id, byN: map[int]*jobsJob{}}
	pj.analyses[id] = a
	pj.order = append(pj.order, a)
}

func (pj *jobsCheck) jobStart(e evidence.AuditEntry) {
	js, err := evidence.DecodeDetails[JobStart](e.Details)
	if err != nil {
		pj.f.problem("audit seq %d: parse.job.start details unreadable: %q", e.Seq, err.Error())
		return
	}
	a := pj.analyses[js.AnalysisID]
	if a == nil {
		pj.f.problem("audit seq %d: parse.job.start of job %d names run %q, which has no parse analysis.start before it", e.Seq, js.Job, js.AnalysisID)
		return
	}
	if prev := a.byN[js.Job]; prev != nil {
		pj.f.problem("run %q: job %d is started more than once (audit seq %d and %d)", a.id, js.Job, prev.startAt, e.Seq)
		return
	}
	if n := len(a.jobs); n > 0 && !a.jobs[n-1].ended {
		prev := a.jobs[n-1]
		prev.superseded = true
		pj.f.problem("run %q: job %d started (audit seq %d) before job %d concluded; a run starts a job only after the previous one ended", a.id, js.Job, e.Seq, prev.n)
	}
	j := &jobsJob{n: js.Job, start: js, startAt: e.Seq}
	if pj.orphanEnd[jobKey(a.id, js.Job)] {
		j.ended = true // its end came first: already a problem
	}
	a.byN[js.Job] = j
	a.jobs = append(a.jobs, j)
}

func (pj *jobsCheck) jobEnd(e evidence.AuditEntry) {
	je, err := evidence.DecodeDetails[JobEnd](e.Details)
	if err != nil {
		pj.f.problem("audit seq %d: parse.job.end details unreadable: %q", e.Seq, err.Error())
		return
	}
	a := pj.analyses[je.AnalysisID]
	if a == nil {
		pj.f.problem("audit seq %d: parse.job.end of job %d names run %q, which has no parse analysis.start before it", e.Seq, je.Job, je.AnalysisID)
		return
	}
	j := a.byN[je.Job]
	switch {
	case j == nil:
		pj.f.problem("run %q: job %d has a parse.job.end (audit seq %d) before its parse.job.start", a.id, je.Job, e.Seq)
		pj.orphanEnd[jobKey(a.id, je.Job)] = true
		return
	case j.ended:
		pj.f.problem("run %q: job %d has more than one parse.job.end (audit seq %d)", a.id, je.Job, e.Seq)
		return
	}
	j.ended = true
	if je.IngestID == "" {
		if j.ingest != nil {
			// a failed Writer.Start leaves its records.ingest.start behind and ends the job with no ingest id; the
			// ingest's conclusion is read at the end of the log, so this is settled in unconcluded
			j.noIngestEnd = &jobsNoIngestEnd{seq: e.Seq, outcome: je.Outcome}
		}
		return // refused, unparsed or skipped before Writer.Start: no ingest to compare (4h-R25)
	}
	in := pj.ingests[je.IngestID]
	switch {
	case in == nil || in.start.AnalysisID != a.id:
		pj.f.problem("run %q: the parse.job.end of job %d (audit seq %d) names ingest %q, which has no records.ingest.start of this run between the job's start and end", a.id, je.Job, e.Seq, je.IngestID)
	case in.job != j:
		pj.f.problem("run %q: the parse.job.end of job %d (audit seq %d) names ingest %q, which was not started by this job", a.id, je.Job, e.Seq, je.IngestID)
	case je.Outcome == OutcomeComplete && (in.concludedBy != evidence.ActionIngestEnd || in.outcome != "complete"):
		pj.f.problem("run %q: job %d ends complete (audit seq %d) but ingest %q has no records.ingest.end with outcome complete before it", a.id, je.Job, e.Seq, je.IngestID)
	}
}

func (pj *jobsCheck) ingestStart(e evidence.AuditEntry) {
	a := pj.analyses[str(e.Details, "analysis_id")]
	if a == nil {
		return // out of scope (PJ5): another op, or an analysis this check has no start for
	}
	is, err := evidence.DecodeDetails[evidence.IngestStart](e.Details)
	if err != nil {
		pj.f.problem("run %q: audit seq %d: records.ingest.start details unreadable: %q", a.id, e.Seq, err.Error())
		return
	}
	if pj.ingests[is.IngestID] != nil {
		return // the lifecycle check reports a repeated start
	}
	in := &jobsIngest{id: is.IngestID, seq: e.Seq, start: is}
	pj.ingests[is.IngestID] = in
	want := normalizedIDs(is.Artifacts)
	// the ingest belongs to the first job that is neither matched nor ended (a run is sequential)
	for a.cur < len(a.jobs) && (a.jobs[a.cur].ingest != nil || a.jobs[a.cur].ended || a.jobs[a.cur].superseded) {
		a.cur++
	}
	var near *jobsJob // the job the ingest should belong to: named in the problem when it differs
	if a.cur < len(a.jobs) {
		j := a.jobs[a.cur]
		near = j
		if j.start.Parser.Name == is.Parser && j.start.Parser.Version == is.ParserVersion && j.start.Parser.Hash == is.ParserHash &&
			slices.Equal(bundleIDs(j.start), want) {
			j.ingest, in.job = in, j
			return
		}
	}
	switch {
	case near == nil:
		pj.f.problem("run %q: ingest %q (audit seq %d) has no preceding parse.job.start of this run to match", a.id, is.IngestID, e.Seq)
	case near.start.Parser.Name != is.Parser || near.start.Parser.Version != is.ParserVersion || near.start.Parser.Hash != is.ParserHash:
		pj.f.problem("run %q: ingest %q (audit seq %d) was written by parser %s %s %q, but job %d started parser %s %s %q",
			a.id, is.IngestID, e.Seq, is.Parser, is.ParserVersion, is.ParserHash, near.n, near.start.Parser.Name, near.start.Parser.Version, near.start.Parser.Hash)
	default:
		pj.f.problem("run %q: ingest %q (audit seq %d) covers artifacts %q, but the bundle of job %d is %q", a.id, is.IngestID, e.Seq, want, near.n, bundleIDs(near.start))
	}
}

func (pj *jobsCheck) ingestConclusion(e evidence.AuditEntry) {
	ic, err := evidence.DecodeDetails[evidence.IngestConclusion](e.Details)
	if err != nil {
		return // the lifecycle check reports unreadable details
	}
	if in := pj.ingests[ic.IngestID]; in != nil && in.concludedBy == "" {
		in.concludedBy, in.outcome = e.Action, ic.Outcome
	}
}

func (pj *jobsCheck) ingestRecover(e evidence.AuditEntry) {
	ir, err := evidence.DecodeDetails[evidence.IngestRecover](e.Details)
	if err != nil {
		return
	}
	// a recovery that only restores a missing run row (RunMissing) leaves the audited conclusion in charge
	if in := pj.ingests[ir.IngestID]; in != nil && !ir.RunMissing && in.recoveredBy == "" {
		in.recoveredBy = ir.ByIngestID
	}
}

// auditFailureText is the analysis.error text of a run that stopped because an audit append failed.
const auditFailureText = "the run stopped: " + StoppedAuditFailure

// unconcluded settles the jobs that have no parse.job.end (PJ4), analysis by analysis.
func (pj *jobsCheck) unconcluded(ctx context.Context) {
	for _, a := range pj.order {
		if ctx.Err() != nil {
			return
		}
		for _, j := range a.jobs {
			if j.noIngestEnd != nil {
				pj.noIngestJobEnd(a, j)
			}
		}
		for idx, j := range a.jobs {
			if j.ended || j.superseded {
				continue
			}
			in := j.ingest
			switch {
			case a.concluded && a.errored && a.errText == auditFailureText && idx == len(a.jobs)-1:
				// the append of this job's parse.job.end failed and the run stopped on it; any other unended job of a
				// concluded run cannot come from a real run
				pj.f.notice("run %q stopped on an audit failure before job %d concluded (its parse.job.end could not be audited)", a.id, j.n)
			case a.concluded:
				pj.f.problem("run %q concluded (analysis.end or analysis.error), but job %d has no parse.job.end", a.id, j.n)
			case in == nil:
				pj.f.notice("run %q ended before job %d concluded (interrupted; no ingest started)", a.id, j.n)
			case in.concludedBy == "" && in.recoveredBy != "":
				pj.f.notice("run %q ended before job %d concluded (interrupted; ingest %q recovered by %q)", a.id, j.n, in.id, in.recoveredBy)
			case in.concludedBy != "":
				pj.f.notice("run %q ended before job %d concluded (interrupted; ingest %q concluded %s but the job end was never audited)", a.id, j.n, in.id, in.concludedBy)
			}
			// an ingest that nothing concluded and nothing recovered is the lifecycle check's problem (exit 4)
		}
	}
}

// noIngestJobEnd settles a parse.job.end that names no ingest over a job whose ingest started. A job end that says
// complete, or an ingest that was concluded by its own writer (records.ingest.end or records.ingest.error), is a
// problem: the job then lied about having no ingest. Otherwise the job ended because Writer.Start failed after
// auditing records.ingest.start: a notice when records.ingest.recover concluded the orphan, and a notice when
// nothing has yet (the lifecycle check owns an unconcluded ingest).
func (pj *jobsCheck) noIngestJobEnd(a *jobsAnalysis, j *jobsJob) {
	in, ne := j.ingest, j.noIngestEnd
	switch {
	case ne.outcome == OutcomeComplete || in.concludedBy != "":
		pj.f.problem("run %q: the parse.job.end of job %d (audit seq %d) names no ingest, but ingest %q was started by this job", a.id, j.n, ne.seq, in.id)
	case in.recoveredBy != "":
		pj.f.notice("run %q: job %d ended (audit seq %d) without an ingest after ingest %q started (its start failed; recovered by %q)", a.id, j.n, ne.seq, in.id, in.recoveredBy)
	default:
		pj.f.notice("run %q: job %d ended (audit seq %d) without an ingest after ingest %q started (its start failed; not yet recovered, the lifecycle check owns the ingest)", a.id, j.n, ne.seq, in.id)
	}
}

// bundleIDs is the sorted, de-duplicated artifact ids of a job's bundle.
func bundleIDs(js JobStart) []string {
	ids := make([]string, 0, len(js.Bundle))
	for _, m := range js.Bundle {
		ids = append(ids, m.ArtifactID)
	}
	return normalizedIDs(ids)
}

func normalizedIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
