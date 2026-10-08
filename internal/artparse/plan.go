package artparse

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Statuses a plan adds to those Discover produces (StatusNew, StatusAlreadyParsed, StatusStaleBundle,
// StatusUnparsed). The reason of a row says why: for skipped the Probe reason, for refused one of
// integrity, payload-version, no-payload-contract.
const (
	StatusSkipped = "skipped"
	StatusRefused = "refused"
)

// PlanRow is one job of a plan with the status the front half of a run gave it. Status is one of the
// status constants; Reason is the text after the colon of the F7 form status:reason.
type PlanRow struct {
	Job        Job
	Status     string
	Reason     string
	Integrity  bool // the artifact (or the manifest) did not match the evidence: exit 4 in the CLI
	Incomplete bool // a member of the bundle is an incomplete artifact (still planned; its records are flagged later)
}

// Plan lists every job of a selection with its status. Nothing is dropped: a job discovery could not
// build is counted in Counts (MissingRole, Unnamed, ...), every other job is a row, and ByStatus
// counts the rows.
type Plan struct {
	Rows     []PlanRow
	Counts   Counts
	ByStatus map[string]int
}

// Plan takes one snapshot, runs discovery, then for each job the shared front half of Run,
// prepareJob: open and hash the bundle, check the parser, run Probe in the sandbox. It stops before
// Writer.Start, appends nothing to the audit log and writes nothing to the case. A context that ends
// or a failure that says nothing about the evidence (an I/O error) is returned as the error, with the
// rows made so far.
func (h *Host) Plan(ctx context.Context, sel Selection) (Plan, error) {
	snap, err := h.TakeSnapshot()
	if err != nil {
		return Plan{}, err
	}
	jobs, counts, err := h.Discover(ctx, snap, sel)
	p := Plan{Counts: counts, ByStatus: map[string]int{}}
	if err != nil {
		return p, err
	}
	for _, j := range jobs {
		pr, err := h.prepareJob(ctx, snap, j, "plan")
		if err != nil {
			return p, err
		}
		if pr.bundle != nil {
			pr.bundle.close()
		}
		p.Rows = append(p.Rows, PlanRow{Job: j, Status: pr.status, Reason: pr.reason, Integrity: pr.integrity, Incomplete: jobIncomplete(j)})
		p.ByStatus[pr.status]++
	}
	return p, nil
}

// jobIncomplete reports whether any member of the bundle is an incomplete artifact.
func jobIncomplete(j Job) bool {
	if j.Primary.Artifact.Incomplete {
		return true
	}
	for _, m := range j.Others {
		if m.Artifact.Incomplete {
			return true
		}
	}
	return false
}

// caseRelative removes the host path of the case directory from text that may carry it (an operating
// system error names the file it could not open): a reason names the artifact by its case-relative
// path only.
func (h *Host) caseRelative(s string) string {
	dir := h.c.Dir
	for _, d := range []string{dir, filepath.ToSlash(dir)} {
		s = strings.ReplaceAll(s, d+string(filepath.Separator), "")
		s = strings.ReplaceAll(s, d+"/", "")
		s = strings.ReplaceAll(s, d, ".")
	}
	return s
}

// prepared is the outcome of the front half of one job: the opened bundle (the caller owns it and
// closes it) when the job may go on, otherwise the status and reason that stop it.
type prepared struct {
	bundle    *bundle
	status    string
	reason    string
	integrity bool
}

func refusedWith(reason string) prepared { return prepared{status: StatusRefused, reason: reason} }

// prepareJob is the one implementation of the front half of a run, used by Plan and by Run: it
// honours the status discovery gave (an unparsed job is never opened and never probed), checks the
// parser, opens and hashes the bundle against the snapshot and runs Probe in the sandbox. A context
// that ended and an I/O failure are returned as the error; everything the evidence or the parser
// decides is a status.
func (h *Host) prepareJob(ctx context.Context, snap *Snapshot, j Job, parseID string) (prepared, error) {
	if err := ctx.Err(); err != nil {
		return prepared{}, err
	}
	if j.Status == StatusUnparsed {
		return prepared{status: StatusUnparsed, reason: j.Reason}, nil
	}
	if err := j.Parser.Check(); err != nil {
		if errors.Is(err, parse.ErrPayloadVersionMismatch) {
			return refusedWith("payload-version: " + cleanAuditText(err.Error(), maxReason)), nil
		}
		return refusedWith("no-payload-contract: " + cleanAuditText(err.Error(), maxReason)), nil
	}
	b, err := h.openBundle(ctx, snap, j, parseID)
	if err != nil {
		// an integrity finding is never dropped for a cancel that happens at the same moment
		if errors.Is(err, evidence.ErrIntegrity) {
			return prepared{status: StatusRefused, reason: "integrity: " + cleanAuditText(h.caseRelative(err.Error()), maxReason), integrity: true}, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return prepared{}, cerr
		}
		return prepared{}, err
	}
	var app parse.Applicability
	in := b.input(true)
	g := guard(ctx, h.limits.ProbeTimeout, h.limits.GracePeriod, b.seal, nil, func(pctx context.Context) error {
		var perr error
		app, perr = j.Parser.p.Probe(pctx, in)
		return perr
	})
	b.sealProbe() // the Parse that follows gets inputs of its own
	switch {
	case g.Cancelled:
		b.close()
		return prepared{}, ctx.Err()
	case g.Abandoned:
		b.close()
		return prepared{status: StatusUnparsed, reason: "probe failed: did not stop after its time limit"}, nil
	case g.Panic != nil:
		b.close()
		return prepared{status: StatusUnparsed, reason: "probe failed: panic: " + g.Panic.Value}, nil
	case g.TimedOut:
		b.close()
		return prepared{status: StatusUnparsed, reason: "probe failed: timed out"}, nil
	case g.Err != nil:
		b.close()
		return prepared{status: StatusUnparsed, reason: "probe failed: " + cleanAuditText(g.Err.Error(), maxReason)}, nil
	}
	reason := cleanAuditText(app.Reason, maxReason)
	switch app.Status {
	case parse.Applicable:
		return prepared{bundle: b, status: j.Status, reason: j.Reason}, nil
	case parse.NotApplicable:
		b.close()
		return prepared{status: StatusSkipped, reason: reason}, nil
	case parse.UnsupportedSchema, parse.Encrypted:
		b.close()
		return prepared{status: StatusUnparsed, reason: app.Status + ": " + reason}, nil
	}
	b.close()
	return prepared{status: StatusUnparsed, reason: fmt.Sprintf("probe failed: unknown status %q", cleanAuditText(app.Status, 64))}, nil
}
