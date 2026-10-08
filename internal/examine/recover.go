package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/volume"
)

// cutError ends the copy of a candidate whose map is a prefix (some runs were not free, or the map
// covers less than the declared size): the artifact is kept and flagged incomplete with this text.
// flatName makes a reader-supplied name one path component: a separator never starts a directory.
var flatName = strings.NewReplacer("/", "_", "\\", "_")

type cutError struct{ msg string }

func (e *cutError) Error() string { return e.msg }

// Recover recovers the content of deleted entries as derived artifacts of kind recover (spec 6): the
// plan is built once (PlanRecovery shows the same one), audited and then written. Only bytes that the
// filesystem reports free are ever copied; every artifact records its map, its confidence and the
// bytes that were not captured. A filesystem without a Recoverer is filesys.ErrNoRecovery, before any
// audit entry. A failure to write to the case, or a cancelled ctx, aborts the run (a partially written
// artifact is kept and flagged incomplete).
func (s *Session) Recover(ctx context.Context, o RecoverOptions) (RecoverSummary, error) {
	if err := o.validate(); err != nil {
		return RecoverSummary{}, err
	}
	b, err := newBudget(o.MaxFiles, o.MaxBytes)
	if err != nil {
		return RecoverSummary{}, err
	}
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return RecoverSummary{}, err
	}
	rec, err := s.recovererOf(fsys)
	if err != nil {
		return RecoverSummary{}, err
	}
	fsType := fsys.Info().Type
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": evidence.AlgorithmRecover,
		"refs": append([]string(nil), o.Refs...), "all": o.All, "min_confidence": o.MinConfidence,
		"all_candidates": o.AllCandidates, "keep_uniform": o.KeepUniform, "max_files": b.maxFiles, "max_bytes": b.maxBytes,
	}
	var rs RecoverSummary
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "recover", details, func(a *analysis) error {
		defer func() {
			a.setEnd("considered", rs.Considered)
			a.setEnd("candidates", rs.Candidates)
			a.setEnd("recovered", rs.Recovered)
			a.setEnd("partial", rs.Partial)
			a.setEnd("uniform", rs.Uniform)
			a.setEnd("overlap", rs.Overlap)
			a.setEnd("skipped_by", rs.SkippedBy)
			a.setEnd("limit_reached", rs.LimitReached)
			a.setEnd("not_processed", rs.NotProcessed)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		plan, err := s.buildPlan(ctx, o, fsys, rec, part, b)
		if err != nil {
			return err
		}
		rs.Considered, rs.Candidates, rs.Uniform, rs.Overlap = plan.Considered, plan.candidates, plan.uniform, plan.overlap
		rs.SkippedBy, rs.LimitReached, rs.NotProcessed = plan.SkippedBy, plan.LimitReached, plan.NotProcessed
		if err := a.syncFS(); err != nil { // what planning made the filesystem notice
			return err
		}
		if err := s.auditPlan(a, plan); err != nil {
			return err
		}
		if err := s.checkSpace(plan.admitted); err != nil {
			return err
		}
		w := &recoverWriter{
			s: s, ctx: ctx, o: o, a: a, rs: &rs, part: part, fsType: fsType, total: plan.admitted,
			lp: evidence.NewLocalPaths("recovered"),
		}
		for i := range plan.Items {
			for j := range plan.Items[i].Candidates {
				c := &plan.Items[i].Candidates[j]
				if c.Skip != "" || c.Ordinal == 0 {
					continue
				}
				if err := w.write(&plan.Items[i], c); err != nil {
					return err
				}
				if err := a.syncFS(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	rs.Summary = sum
	return rs, err
}

// auditPlan writes the plan's warnings in plan order: the unallocated note (not a skip), the entry
// and candidate skips (each counted), the candidate-list cuts and one note per limit met.
func (s *Session) auditPlan(a *analysis, plan *RecoverPlan) error {
	if plan.unallocNote != "" {
		d := map[string]any{
			"analysis_id": a.sum.AnalysisID, "source": "examine", "path": "", "reason": plan.unallocNote, "warning": plan.unallocNote,
		}
		if _, err := a.c.Audit.Append("analysis.warning", a.deviceID, d); err != nil {
			return err
		}
	}
	skip := func(sk PlanSkip) error {
		reason := sk.Reason
		if sk.Detail != "" {
			reason += ": " + sk.Detail
		}
		return a.warnWith(sk.Path, reason, map[string]any{"id": sk.ID, "code": sk.Reason})
	}
	for _, it := range plan.Items {
		for _, n := range it.notes {
			if err := skip(n); err != nil {
				return err
			}
		}
		for _, sk := range it.Skips {
			if sk.Reason == "not-selected" || sk.Reason == "limit" {
				continue // counted in the summary; a limit has one note of its own
			}
			if err := skip(sk); err != nil {
				return err
			}
		}
	}
	for _, l := range plan.limits {
		if err := skip(l); err != nil {
			return err
		}
	}
	return nil
}

type recoverWriter struct {
	s      *Session
	ctx    context.Context
	o      RecoverOptions
	a      *analysis
	rs     *RecoverSummary
	part   volume.Partition
	fsType string
	lp     *evidence.LocalPaths
	total  int64
	copied int64
}

// write stores one planned candidate as an artifact.
func (w *recoverWriter) write(it *RecoverItem, c *PlannedCandidate) error {
	s, a := w.s, w.a
	conf := c.Confidence
	d := s.baseDerivation(w.part, w.fsType)
	d.FSPath, d.FSID = it.Path, it.ID
	d.Mode = c.Mode
	d.Times = timesMap(c.Times)
	d.Encrypted = c.Encrypted
	rv := &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: c.Method, Confidence: &conf,
		Basis: c.Basis, Assumptions: c.Assumptions, Alloc: c.Alloc, Excluded: c.Excluded,
		Content: c.Content, Algorithm: evidence.AlgorithmRecover,
		Params: map[string]string{
			"min_confidence": strconv.Itoa(w.o.MinConfidence),
			"all_candidates": strconv.FormatBool(w.o.AllCandidates),
			"keep_uniform":   strconv.FormatBool(w.o.KeepUniform),
		},
	}
	d.Recovery = rv
	runs := c.Runs
	useSidecar := len(runs) > evidence.MaxInlineRuns
	if !useSidecar {
		d.Runs = runs
	}
	partial := c.Size < c.DeclaredSize
	src := evidence.Source{Kind: evidence.KindRecover, DeviceID: s.Parent.Source.DeviceID, Derived: d}

	// Nothing that case verify would reject is ever written: a failure here is a defect of this build.
	probe := evidence.ManifestRecord{Path: "recovered/probe", Size: c.Size, Incomplete: partial, Error: c.errText, Source: src}
	if partial && probe.Error == "" {
		probe.Error = "partial"
	}
	if probs := append(rv.Check(evidence.KindRecover), evidence.CheckRecoveredRuns(probe, runs)...); len(probs) > 0 {
		return fmt.Errorf("internal error: the recovery of %s would not pass verification: %s", it.Path, strings.Join(probs, "; "))
	}

	dir, err := w.lp.Dir(fmt.Sprintf("/p%d-%s", w.part.Index, w.fsType))
	if err != nil {
		return a.warn(it.Path, "no usable local directory name: "+err.Error())
	}
	var (
		rec     evidence.ManifestRecord
		readErr error
	)
	for attempt := 0; ; attempt++ {
		rel, err := w.lp.File(dir, fmt.Sprintf("%06d-%s", c.Ordinal, flatName.Replace(it.Name)))
		if err != nil { // an unusable name becomes "entry"
			if rel, err = w.lp.File(dir, fmt.Sprintf("%06d-entry", c.Ordinal)); err != nil {
				return a.warn(it.Path, "no usable local name: "+err.Error())
			}
		}
		if useSidecar && d.RunsArtifact == "" { // written once: a name retry reuses it, so none is left unreferenced
			err = s.writeRunsSidecar(a, rel+".runs.jsonl", runs, "", d)
		}
		if err == nil {
			rec, readErr, err = s.capture(a, rel, src, func(out io.Writer) error {
				return w.copyRuns(out, rel, d, rv, c, partial)
			})
		}
		var nameErr *localNameError
		if errors.As(err, &nameErr) {
			return a.warn(it.Path, "not recovered: "+nameErr.Error())
		}
		if !errors.Is(err, evidence.ErrArtifactExists) {
			if err != nil {
				return err // the case itself failed: never downgraded to a warning
			}
			break
		}
		if attempt+1 >= maxNameAttempts {
			return a.warn(it.Path, fmt.Sprintf("no free local name after %d attempts: %v", attempt+1, err))
		}
	}
	if rec.ID != "" {
		w.rs.Recovered++
		if rec.Incomplete {
			w.rs.Partial++
		}
	}
	var cut *cutError
	switch {
	case readErr == nil:
		a.sum.Files++
		a.sum.Bytes += rec.Size
		return nil
	case errors.As(readErr, &cut):
		return nil // a planned prefix: described by the artifact itself
	case fatal(w.ctx, readErr) || !skippable(readErr):
		return readErr
	}
	return a.warn(it.Path, fmt.Sprintf("read failed after %d of %d bytes (partial artifact kept, flagged incomplete): %v", rec.Size, c.Size, readErr))
}

// copyRuns streams the runs of c from the image into out. A failure after n bytes narrows the recorded
// provenance to the runs of those n bytes (the artifact is flagged incomplete); a planned prefix ends
// with a cutError after all captured bytes.
func (w *recoverWriter) copyRuns(out io.Writer, rel string, d *evidence.Derivation, rv *evidence.Recovery, c *PlannedCandidate, partial bool) error {
	tw := &trackWriter{w: out}
	var n int64
	narrow := func() error {
		if len(c.Runs) == 0 || n >= c.Size {
			return nil
		}
		rv.Alloc.Free = n
		rv.Assumptions = append(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", n))
		return w.s.keepPrefixRuns(w.a, rel, "", d, c.Runs, n)
	}
	buf := make([]byte, copyBufSize) // one buffer for every run of the candidate
	for _, r := range c.Runs {
		m, err := io.CopyBuffer(tw, &ctxReader{ctx: w.ctx, r: io.NewSectionReader(w.s.Image, r.Offset, r.Length), onRead: func(k int) {
			w.copied += int64(k)
			if w.o.Progress != nil {
				w.o.Progress(w.copied, w.total)
			}
		}}, buf)
		n += m
		switch {
		case tw.err != nil:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return &caseWriteError{tw.err}
		case err != nil:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return err
		case m != r.Length:
			if nerr := narrow(); nerr != nil {
				return nerr
			}
			return fmt.Errorf("image ended after %d of %d bytes of the run at %d: %w", m, r.Length, r.Offset, io.ErrUnexpectedEOF)
		}
	}
	if partial {
		return &cutError{c.errText}
	}
	return nil
}
