package artparse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Scan statuses and bounds of `parse plan --scan` (4h-R9).
const (
	StatusCandidate       = "candidate" // a scan row whose Probe said applicable and that no complete run covers
	ScanMaxSize     int64 = 1 << 30     // spec 11: artifacts above it are counted, not probed
	MaxScanFindings       = 1000
)

// ScanCounts says what --scan saw; Plan.Scan is nil when the selection did not ask for a scan.
type ScanCounts struct {
	Considered, Probed, Candidates, NotApplicable, TooLarge, Companions, NotProbed, FindingsDropped int
	Stopped                                                                                         string // "", StoppedAbandoned or StoppedCancelled (4h-R31); NotApplicable counts (artifact, parser) pairs
}

// ScanFinding is a probe outcome that is neither applicable nor not-applicable: unsupported-schema, encrypted,
// or a probe failure (error, panic, timeout), for one artifact and one parser.
type ScanFinding struct{ ArtifactID, Path, Parser, Status, Reason string }

// errScanIsPlanOnly is why Run and RecoverTyped refuse a selection with Scan.
var errScanIsPlanOnly = fmt.Errorf("%w: --scan is a parse plan option", ErrSelection)

// Reasons that hold for every parser: a candidate refused for one of them is listed once, for all parsers.
const allParsers = "(all)"

// companionSuffixes are the manifest path endings of SQLite sidecars: such an artifact is a member of some
// database's bundle, never a database of its own.
var companionSuffixes = []string{"-wal", "-journal", "-shm"}

func isCompanionPath(p string) bool {
	for _, s := range companionSuffixes {
		if strings.HasSuffix(p, s) {
			return true
		}
	}
	return false
}

// globCovered is the set of artifacts a glob-matched job of ANY registered parser uses, as primary or as a
// member of its bundle: discovery already handles them, so a scan does not.
func (h *Host) globCovered(d *discovered, env Env) (map[string]bool, error) {
	covered := map[string]bool{}
	for _, r := range h.registry {
		globs, err := compilePrimary(r)
		if err != nil {
			return nil, err
		}
		for _, c := range d.cands {
			if !c.named || !matchesAny(globs, c.logical.Path) {
				continue
			}
			if job, ok := h.bundle(r, c, d.groups, env, false); ok {
				covered[c.rec.ID] = true
				for _, m := range job.Others {
					covered[m.Artifact.ID] = true
				}
			}
		}
	}
	return covered, nil
}

// scanner is the state of one scan.
type scanner struct {
	h       *Host
	snap    *Snapshot
	d       *discovered
	p       *Plan
	sc      *ScanCounts
	parsers []Registered
	next    int // the number the next row gets
}

// scan probes every selected parser on the eligible artifacts no glob matched (4h-R9) and adds the rows and
// findings to p. It appends nothing to the audit log, writes nothing and never calls Parse. A context that
// ends is returned as the error with the rows made so far.
func (h *Host) scan(ctx context.Context, snap *Snapshot, sel Selection, d *discovered, jobs []Job, p *Plan) error {
	s := &scanner{h: h, snap: snap, d: d, p: p, sc: &ScanCounts{}, next: len(jobs) + 1}
	p.Scan = s.sc
	parsers, err := selectParsersFrom(h.registry, sel.Parsers)
	if err != nil {
		return err
	}
	s.parsers = parsers
	covered, err := h.globCovered(d, snap.env)
	if err != nil {
		return err
	}
	var cands []*candidate
	for _, c := range d.cands {
		if !covered[c.rec.ID] {
			cands = append(cands, c)
		}
	}
	s.sc.Considered = len(cands)
	maxSize := ScanMaxSize
	if h.scanMaxSize > 0 {
		maxSize = h.scanMaxSize
	}
	for i, c := range cands {
		if err := ctx.Err(); err != nil {
			s.sc.Stopped, s.sc.NotProbed = StoppedCancelled, s.sc.NotProbed+len(cands)-i
			return err
		}
		switch {
		case isCompanionPath(c.rec.Path):
			s.sc.Companions++
			continue
		case c.rec.Size > maxSize:
			s.sc.TooLarge++
			continue
		}
		probed, abandoned, err := s.candidate(ctx, c)
		rest := len(cands) - i
		if probed {
			rest-- // the candidate in flight was probed, at least in part
		}
		switch {
		case err != nil:
			if ctx.Err() != nil {
				s.sc.Stopped, s.sc.NotProbed = StoppedCancelled, s.sc.NotProbed+rest
			}
			return err
		case abandoned:
			s.sc.Stopped, s.sc.NotProbed = StoppedAbandoned, s.sc.NotProbed+rest
			s.p.Stopped = StoppedAbandoned
			return nil
		}
	}
	return nil
}

// candidate loads one candidate once and asks every selected parser's Probe about it through the shared front
// half. probed says the source was loaded (the candidate is counted as probed); abandoned that a Probe ignored
// its cancellation, which ends the scan.
func (s *scanner) candidate(ctx context.Context, c *candidate) (probed, abandoned bool, err error) {
	if len(s.parsers) == 0 {
		return false, false, nil
	}
	h := s.h
	src, err := h.loadSource(ctx, s.snap, c.rec.ID, h.limits.MemInputMax, h.limits.MemInputTotal)
	if err != nil {
		if errors.Is(err, evidence.ErrIntegrity) { // an integrity finding is never dropped for a cancel at the same moment
			job, _ := h.bundle(s.parsers[0], c, s.d.groups, s.snap.env, true)
			s.addRow(job, PlanRow{Status: StatusRefused, Reason: "integrity: " + cleanAuditText(h.caseRelative(err.Error()), maxReason), Integrity: true})
			return false, false, nil
		}
		return false, false, err
	}
	defer src.close()
	s.sc.Probed++
	applicable := false
	for _, r := range s.parsers {
		job, _ := h.bundle(r, c, s.d.groups, s.snap.env, true) // explicit: the job always exists, Probe decides
		job.N = s.next
		job.borrowed = src
		if job.Status == StatusUnparsed {
			// decided from the manifest alone, before any Probe: unknown is listed, never dropped
			all := job.Reason == reasonEncrypted || job.Reason == "encrypted content (sub-project 10)" || job.Reason == "manifest path outside the acquisition layout"
			if all {
				s.addFinding(c, allParsers, StatusUnparsed, job.Reason)
				break
			}
			s.addFinding(c, r.meta.Name, StatusUnparsed, job.Reason)
			continue
		}
		pr, perr := h.prepareJob(ctx, s.snap, job, "plan")
		if perr != nil {
			return true, false, perr
		}
		switch {
		case pr.bundle != nil: // applicable
			pr.bundle.close()
			if err := h.statusFromRuns(ctx, &job); err != nil {
				return true, false, err
			}
			status := job.Status
			if status == StatusNew {
				status = StatusCandidate
			}
			s.addRow(job, PlanRow{Status: status, Reason: job.Reason})
			applicable = true
		case pr.integrity:
			s.addRow(job, PlanRow{Status: pr.status, Reason: pr.reason, Integrity: true})
		case pr.status == StatusSkipped: // not-applicable
			s.sc.NotApplicable++
		default:
			status, reason := findingOf(pr)
			s.addFinding(c, r.meta.Name, status, reason)
			if pr.abandoned {
				return true, true, nil
			}
		}
	}
	if applicable {
		s.sc.Candidates++
	}
	return true, false, nil
}

// addRow completes a scan row (the job, the incomplete and recovered facts) and appends it.
func (s *scanner) addRow(job Job, row PlanRow) {
	job.N = s.next
	s.next++
	row.Job, row.Scan = job, true
	row.Incomplete, row.Recovered = jobIncomplete(job), jobRecovery(s.snap, job)
	s.p.Rows = append(s.p.Rows, row)
	s.p.ByStatus[row.Status]++
}

func (s *scanner) addFinding(c *candidate, parser, status, reason string) {
	limit := MaxScanFindings
	if s.h.maxScanFindings > 0 {
		limit = s.h.maxScanFindings
	}
	if len(s.p.ScanFindings) >= limit {
		s.sc.FindingsDropped++
		return
	}
	path := c.rec.Path
	if c.named {
		path = c.logical.Path
	}
	s.p.ScanFindings = append(s.p.ScanFindings, ScanFinding{ArtifactID: c.rec.ID, Path: path, Parser: parser, Status: status, Reason: cleanAuditText(reason, maxReason)})
}

// findingOf splits a prepared outcome that is neither applicable nor not-applicable into the finding status and reason.
func findingOf(pr prepared) (status, reason string) {
	for _, p := range []struct{ prefix, status string }{
		{parse.UnsupportedSchema + ": ", parse.UnsupportedSchema}, {parse.Encrypted + ": ", parse.Encrypted}, {"probe failed: ", "probe-failed"},
	} {
		if rest, ok := strings.CutPrefix(pr.reason, p.prefix); ok {
			return p.status, rest
		}
	}
	return pr.status, pr.reason
}
