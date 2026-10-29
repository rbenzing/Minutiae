package artparse

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// RecoverOptions selects and observes a typed recovery run.
type RecoverOptions struct {
	Selection
	// OnlyPrimary keeps only the glob-matched jobs (never an explicit --artifact job) whose primary is
	// this artifact; that artifact is admitted as recovered input even without IncludeRecovered.
	OnlyPrimary string
	// ExpectCompanions maps a bundle role to the artifact id it must have in the job ("" = the role must
	// be absent); nil = no expectation. A job whose bundle differs is skipped (bundle-differs) before any
	// input is opened.
	ExpectCompanions map[string]string
	// MinConfidence > 0 narrows the run: records below it (after every cap) are dropped and counted, and
	// the ingest concludes incomplete (it never supersedes a full typed run).
	MinConfidence int
	// OnEvent is called ONLY on the goroutine that called RecoverTyped (see RunOptions).
	OnEvent func(Event)
}

// typedRunner is recrun.Run (a test seam replaces it).
type typedRunner func(ctx context.Context, in *parse.Input, out parse.RecoveryEmitter, mappings []parse.TableMapping, claims []parse.TableClaim) (recrun.Report, error)

// recoverMode is the state a typed recovery run adds to run.
type recoverMode struct{ opt RecoverOptions }

// bundleDiffers is non-empty (the skip reason) when the job's bundle is not the one the caller expects.
func (m *recoverMode) bundleDiffers(j Job) string {
	if m.opt.ExpectCompanions == nil {
		return ""
	}
	byRole := map[string]string{}
	for _, mb := range j.members() {
		byRole[mb.role] = mb.m.Artifact.ID
	}
	roles := make([]string, 0, len(m.opt.ExpectCompanions))
	for role := range m.opt.ExpectCompanions {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	none := func(id string) string {
		if id == "" {
			return "(none)"
		}
		return id
	}
	for _, role := range roles {
		if got, want := byRole[role], m.opt.ExpectCompanions[role]; got != want {
			return fmt.Sprintf("bundle-differs: %s %s vs %s", role, none(got), none(want))
		}
	}
	return ""
}

// RecoverTyped runs the typed recovery pass of every claiming parser (or the selected ones) over the
// bundles discovery finds: each job is recorded under the parser's "<name>.recover" identity by the
// recovery runner (recrun) behind the recovery emitter, in the same sandbox as a live parse. It never
// writes a live record and never supersedes the live parse's records (a separate identity). The summary
// and the error follow Run's rules.
func (h *Host) RecoverTyped(ctx context.Context, opt RecoverOptions) (RunSummary, error) {
	if err := h.c.RequireSchema(evidence.CurrentSchema); err != nil {
		return RunSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return RunSummary{}, err
	}
	if opt.MinConfidence < 0 || opt.MinConfidence > 100 {
		return RunSummary{}, fmt.Errorf("artparse: minimum confidence %d is outside 0..100", opt.MinConfidence)
	}
	selected, err := h.selectParsers(opt.Parsers)
	if err != nil {
		return RunSummary{}, err
	}
	if err := checkClaimUniqueness(h.registry); err != nil {
		return RunSummary{}, err
	}
	var views []Registered
	for _, r := range h.registry {
		if len(r.meta.Claims) == 0 {
			continue
		}
		if err := r.CheckClaims(); err != nil {
			return RunSummary{}, err
		}
		if r.recoverHash == "" {
			return RunSummary{}, fmt.Errorf("artparse: parser %s: %w", r.meta.Name, ErrNoRecoveryIdentity)
		}
		views = append(views, r.recoverView())
	}
	if len(opt.Parsers) > 0 {
		for _, r := range selected {
			if len(r.meta.Claims) == 0 {
				return RunSummary{}, fmt.Errorf("%w: parser %q claims no tables", ErrSelection, r.meta.Name)
			}
		}
	}
	snap, err := h.TakeSnapshot()
	if err != nil {
		return RunSummary{}, err
	}
	sel := opt.Selection
	sel.admitRecovered = map[string]bool{}
	if opt.OnlyPrimary != "" {
		sel.admitRecovered[opt.OnlyPrimary] = true
	}
	for _, id := range opt.ExpectCompanions { // the caller named these as the bundle's own files
		if id != "" {
			sel.admitRecovered[id] = true
		}
	}
	jobs, _, err := h.discoverIn(ctx, snap, sel, views)
	if err != nil {
		return RunSummary{}, err
	}
	if opt.OnlyPrimary != "" {
		kept := jobs[:0]
		for _, j := range jobs {
			if !j.Explicit && j.Primary.Artifact.ID == opt.OnlyPrimary {
				j.N = len(kept) + 1
				kept = append(kept, j)
			}
		}
		jobs = kept
	}
	parsers, err := selectParsersFrom(views, opt.Parsers)
	if err != nil {
		return RunSummary{}, err
	}
	r := &run{
		h: h, opt: RunOptions{Selection: opt.Selection, OnEvent: opt.OnEvent}, snap: snap,
		parseID: evidence.NewAcquisitionID(time.Now()), rec: &recoverMode{opt: opt},
	}
	return h.runJobs(ctx, r, parsers, jobs)
}

// recoveryReport carries the runner's report from the guarded goroutine to the host, safely even when
// the goroutine is abandoned.
type recoveryReport struct {
	mu sync.Mutex
	r  recrun.Report
}

func (c *recoveryReport) set(r recrun.Report) { c.mu.Lock(); c.r = r; c.mu.Unlock() }
func (c *recoveryReport) get() recrun.Report  { c.mu.Lock(); defer c.mu.Unlock(); return c.r }

// runTyped is the body of a typed recovery job, run inside the guard: the parser's mappings and Map run
// on the guarded goroutine, like Parse.
func runTyped(ctx context.Context, run typedRunner, reg Registered, in *parse.Input, rem *recoveryEmitter, rep *recoveryReport) error {
	rm, ok := reg.p.(parse.RowMapper)
	if !ok {
		return fmt.Errorf("parser %s claims tables but is not a parse.RowMapper", reg.meta.Name)
	}
	r, err := run(ctx, in, rem, rm.Mappings(), reg.meta.Claims)
	rep.set(r)
	return err
}

// settleRecovery copies what the recovery emitter and the runner counted into the job result and turns
// the conditions that make a typed job incomplete into its failure: a claimed row skipped for invalid
// facts (it is neither typed nor unmapped, so the job did not cover its claims) and a narrowed run.
func (r *run) settleRecovery(st *jobState, rem *recoveryEmitter, rep recrun.Report, reason string, cause error) (string, error) {
	st.res.Unmapped = rem.unmapped.Load()
	st.res.BelowMinConfidence = rem.belowMin.Load()
	st.res.UnmappedLocators, st.res.UnmappedOverflow = rem.unmappedLocators()
	st.res.SkippedInvalid = rep.Skipped
	if r.rec.opt.MinConfidence > 0 {
		st.res.Narrowed = []string{"min-confidence"}
	}
	if cause == nil && rep.Skipped > 0 {
		// read from the runner's count, not from the (capped) warnings
		reason = fmt.Sprintf("%d claimed recovered rows skipped: their facts were invalid", rep.Skipped)
		cause = errors.New(reason)
	}
	if cause == nil && len(st.res.Narrowed) > 0 {
		reason = "narrowed: min-confidence"
		cause = fmt.Errorf("the run was narrowed to confidence %d and over: records below it were dropped (%d)", r.rec.opt.MinConfidence, st.res.BelowMinConfidence)
	}
	return reason, cause
}
