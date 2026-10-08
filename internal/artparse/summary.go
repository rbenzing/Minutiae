package artparse

import "time"

// Outcomes of a job (parse.job.end.outcome). OutcomeNotRun is summary-only: a job never started.
const (
	OutcomeComplete   = "complete"
	OutcomeIncomplete = "incomplete"
	OutcomeUnparsed   = "unparsed"
	OutcomeSkipped    = "skipped"
	OutcomeRefused    = "refused"
	OutcomeNotRun     = "not-run"
)

// JobResult is what one job of a run came to.
type JobResult struct {
	Job                           int
	Parser, Version, Hash         string
	ArtifactID, Logical, Platform string
	Outcome, Reason               string
	IngestID                      string
	Records                       int64
	Rejected                      int
	Warnings                      int
	WarningsSuppressed            int
	Duration                      time.Duration
	Notes                         map[string]string
	Panic                         *PanicInfo
	Abandoned, TimedOut           bool
	Integrity                     bool
}

// RunSummary is what a run came to. Jobs includes the jobs never started (Outcome "not-run").
type RunSummary struct {
	ParseID   string
	Jobs      []JobResult
	Records   int64
	Cancelled bool
	// Stopped says why the run ended early: "" | "abandoned" | "cancelled" | "audit-failure" | "abort-failure" |
	// "run-error" (another run-level failure, such as ErrIngestActive).
	Stopped string
}

// Class is the exit class of a run: ClassOK (exit 0), ClassPartial (exit 1) or ClassIntegrity (exit 4).
type Class int

// Exit classes.
const (
	ClassOK Class = iota
	ClassPartial
	ClassIntegrity
)

// Class classifies the run: integrity beats partial beats ok.
func (s RunSummary) Class() Class {
	worst := ClassOK
	if s.Cancelled || s.Stopped != "" {
		worst = ClassPartial
	}
	for _, j := range s.Jobs {
		switch {
		case j.Integrity:
			return ClassIntegrity
		case j.Outcome == OutcomeComplete || j.Outcome == OutcomeSkipped:
		default: // incomplete, unparsed, refused, not-run
			worst = ClassPartial
		}
	}
	return worst
}
