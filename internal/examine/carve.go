package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// CarveSummary is the outcome of a carve run.
type CarveSummary struct {
	Summary
	Carved, Partial int // artifacts written (a partial one is counted in both)
	Stats           carve.Stats
	SkippedBy       map[string]int
	LimitReached    string
	NotScannedBytes int64
	Sigset          string
	SigsetSHA256    string
	SigFileSHA256   string
}

// carveGapTag marks a carve whose bytes overlap a declared acquisition gap of the image (3E-R33).
const carveGapTag = "overlaps-acquisition-gap"

// partialCarveCap is the confidence ceiling of a carve cut by a read failure (3E-R10).
const partialCarveCap = 20

// Carve carves objects by signature out of the scope of o and writes each accepted hit that passes the
// minimum confidence and the output limits as a derived artifact of kind carve (spec 7): the plan is the one
// PlanCarve shows. Bytes are copied only from the scope's runs. A failure to write to the case, a panic of the
// scanner or a cancelled ctx aborts the run (artifacts already written are kept; a partially written one is
// flagged incomplete). The analysis.end or analysis.error entry carries the statistics on every path.
func (s *Session) Carve(ctx context.Context, o CarveOptions) (CarveSummary, error) {
	j, err := s.prepareCarve(o)
	if err != nil {
		return CarveSummary{}, err
	}
	cs := CarveSummary{Sigset: j.set.ID, SigsetSHA256: j.setSHA, SigFileSHA256: j.sigSHA}
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "carve", j.startDetails(), func(a *analysis) error {
		defer func() {
			cs.Stats, cs.SkippedBy, cs.LimitReached, cs.NotScannedBytes = j.stats, j.skipped, j.limit, j.notScanned()
			j.setEnd(a, &cs)
		}()
		if fsys := j.watchedFS(); fsys != nil {
			if err := a.watchFS(fsys); err != nil {
				return err
			}
		}
		for _, n := range j.notes { // notes of the scope: audited, not skips
			if err := a.appendWarning(noteDetails(a, "note", n)); err != nil {
				return err
			}
		}
		w := &carveWriter{j: j, ctx: ctx, a: a, cs: &cs, lp: evidence.NewLocalPaths("carved")}
		err := j.scan(ctx, w.hit, func(off, n int64, cause error) error {
			return a.appendWarning(noteDetails(a, "unreadable", fmt.Sprintf("unreadable: %d byte(s) at offset %d (%v)", n, off, cause)))
		})
		if err != nil {
			return fmt.Errorf("carve: %w", err)
		}
		j.finishLimits()
		if j.limit == "max-files" || j.limit == "max-bytes" {
			if err := a.appendWarning(noteDetails(a, "limit", fmt.Sprintf("%s reached: the run stopped, %d byte(s) of the scope were not scanned", j.limit, j.notScanned()))); err != nil {
				return err
			}
		}
		if j.stats.BudgetExhausted {
			if err := a.appendWarning(noteDetails(a, "validate-budget", "the validator read budget was spent: every later header was rejected (validate-budget)")); err != nil {
				return err
			}
		}
		for _, t := range j.stats.TypeCapped {
			if err := a.appendWarning(noteDetails(a, "types-capped", fmt.Sprintf("type %s reached %d accepted hits and stopped matching", t, carve.DefaultMaxHitsPerType))); err != nil {
				return err
			}
		}
		return a.syncFS()
	})
	cs.Summary = sum
	return cs, err
}

func noteDetails(a *analysis, code, text string) map[string]any {
	return map[string]any{
		"analysis_id": a.sum.AnalysisID, "source": "examine", "code": code, "path": "", "reason": text, "warning": text,
	}
}

// watchedFS is the filesystem whose warnings the audit follows (scope unallocated only).
func (j *carveJob) watchedFS() filesys.FileSystem {
	if j.sc.Scope != CarveUnallocated {
		return nil
	}
	fsys, _, err := j.s.FS(j.sc.Part.Index)
	if err != nil {
		return nil
	}
	return fsys
}

// startDetails are the analysis.start details (3E-R11): every fact known before the first read of carved bytes.
func (j *carveJob) startDetails() map[string]any {
	types := slices.Clone(j.types)
	if types == nil {
		types = []string{}
	}
	return map[string]any{
		"parent_id": j.s.Parent.ID, "artifact_id": j.sc.ArtifactID, "export_id": j.sc.ExportID,
		"partition": j.partitionOf(), "scope": j.sc.Scope, "types": types,
		"unaligned": j.o.Unaligned, "nested": j.o.Nested, "min_confidence": j.o.MinConfidence,
		"max_files": j.b.maxFiles, "max_bytes": j.b.maxBytes, "max_file_size": j.o.MaxFileSize,
		"algorithm": evidence.AlgorithmRecover, "sigset": j.set.ID, "sigset_sha256": j.setSHA,
		"sig_file_sha256": j.sigSHA, "sig_file_bytes": len(j.o.SigFile),
	}
}

func outcomeCounts(m map[carve.Outcome]int) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

// setEnd records the statistics of the run in analysis.end / analysis.error (3E-R11, 3E-R32).
func (j *carveJob) setEnd(a *analysis, cs *CarveSummary) {
	st := j.stats
	a.setEnd("scope_runs", len(j.sc.Runs))
	a.setEnd("scope_bytes", j.sc.Bytes)
	a.setEnd("scanned_bytes", st.Scanned)
	a.setEnd("not_scanned_bytes", cs.NotScannedBytes)
	a.setEnd("align", j.sc.Align)
	a.setEnd("hits_by_outcome", outcomeCounts(st.Accepted))
	a.setEnd("rejected_by", cloneCounts(st.Rejected))
	a.setEnd("carved", cs.Carved)
	a.setEnd("partial", cs.Partial)
	a.setEnd("skipped_by", cloneCounts(j.skipped))
	a.setEnd("nested_depth_skipped", st.NestedDepthSkipped)
	a.setEnd("types_capped", append([]string{}, st.TypeCapped...))
	a.setEnd("unreadable_bytes", st.UnreadableBytes)
	a.setEnd("validate_bytes", st.ValidateBytes)
	a.setEnd("limit_reached", j.limit)
}

func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type carveWriter struct {
	j   *carveJob
	ctx context.Context
	a   *analysis
	cs  *CarveSummary
	lp  *evidence.LocalPaths
}

func hitPath(h *CarveHit) string { return fmt.Sprintf("0x%016x", h.Offset) }

// hit handles one accepted hit: a skipped one is one warning with its offset and reason, the rest is written.
func (w *carveWriter) hit(h *CarveHit) error {
	if h.Skip != "" {
		reason := fmt.Sprintf("%s: %s %s hit of %d bytes, confidence %d (%s)", h.Skip, h.Family, h.Type, h.Length, h.Confidence, h.Method)
		return w.a.warnWith(hitPath(h), reason, map[string]any{"code": h.Skip, "type": h.Type, "length": h.Length})
	}
	return w.write(h)
}

// write stores one hit as an artifact.
func (w *carveWriter) write(h *CarveHit) error {
	j, s, a := w.j, w.j.s, w.a
	conf := h.Confidence
	d := j.derivation(h)
	runs := []evidence.Run{{Offset: h.Offset, Length: h.Length}}
	d.Runs = runs
	assumptions := slices.Clone(h.Assumptions)
	if j.sc.Scope != CarveArtifact {
		if d.AcquisitionGaps = s.gapNote(runs, true, h.Length); d.AcquisitionGaps != nil {
			assumptions = append(assumptions, carveGapTag)
		}
	}
	rv := &evidence.Recovery{
		Class: evidence.ClassCarved, Method: h.Method, Confidence: &conf,
		Basis: slices.Clone(h.Basis), Assumptions: assumptions, Alloc: h.Alloc, Scope: j.sc.Scope,
		Content: h.content(), Algorithm: evidence.AlgorithmRecover, Params: j.params(),
	}
	d.Recovery = rv
	src := evidence.Source{Kind: evidence.KindCarve, DeviceID: s.Parent.Source.DeviceID, Derived: d}

	// Nothing that case verify would reject is ever written: a failure here is a defect of this build.
	probe := evidence.ManifestRecord{Path: "carved/probe", Size: h.Length, Source: src}
	if probs := append(rv.Check(evidence.KindCarve), evidence.CheckRecoveredRuns(probe, runs)...); len(probs) > 0 {
		return fmt.Errorf("internal error: the carve at %s would not pass verification: %s", hitPath(h), strings.Join(probs, "; "))
	}
	if err := s.checkSpace(h.Length); err != nil {
		return err
	}
	dir, err := w.lp.Dir("/" + j.sc.Dir)
	if err != nil {
		return a.warn(hitPath(h), "no usable local directory name: "+err.Error())
	}
	var (
		rec     evidence.ManifestRecord
		readErr error
	)
	for attempt := 0; ; attempt++ {
		rel, err := w.lp.File(dir, fmt.Sprintf("%016x-%s.%s", h.Offset, h.Type, h.Ext))
		if err != nil {
			return a.warn(hitPath(h), "no usable local name: "+err.Error())
		}
		rec, readErr, err = s.capture(a, rel, src, func(out io.Writer) error { return w.copyHit(out, h, d, rv) })
		var nameErr *localNameError
		if errors.As(err, &nameErr) {
			return a.warn(hitPath(h), "not carved: "+nameErr.Error())
		}
		if !errors.Is(err, evidence.ErrArtifactExists) {
			if err != nil {
				return err // the case itself failed: never downgraded to a warning
			}
			break
		}
		if attempt+1 >= maxNameAttempts {
			return a.warn(hitPath(h), fmt.Sprintf("no free local name after %d attempts: %v", attempt+1, err))
		}
	}
	if rec.ID != "" {
		w.cs.Carved++
		if rec.Incomplete {
			w.cs.Partial++
		}
	}
	switch {
	case readErr == nil:
		a.sum.Files++
		a.sum.Bytes += rec.Size
		return nil
	case fatal(w.ctx, readErr) || errors.Is(readErr, evidence.ErrIntegrity):
		return readErr
	}
	j.b.trim(h.Length - rec.Size) // the unwritten part of the admitted size
	return a.warn(hitPath(h), fmt.Sprintf("read failed after %d of %d bytes (partial artifact kept, flagged incomplete): %v", rec.Size, h.Length, readErr))
}

// copyHit streams the hit from the image into out. A failure after n bytes narrows the recorded provenance to
// those n bytes (the artifact is flagged incomplete): the run, the allocation counts, the acquisition-gap note
// (over the kept bytes only, 3E-R25) and the confidence (capped at 20, 3E-R10).
func (w *carveWriter) copyHit(out io.Writer, h *CarveHit, d *evidence.Derivation, rv *evidence.Recovery) error {
	j, s := w.j, w.j.s
	tw := &trackWriter{w: out}
	narrow := func(n int64) {
		if n >= h.Length {
			return
		}
		d.Runs = prefixRuns(d.Runs, n)
		if alloc, _, err := j.allocOf(h.Offset, n); err == nil {
			rv.Alloc = alloc
		}
		rv.Assumptions = slices.DeleteFunc(rv.Assumptions, func(x string) bool { return x == carveGapTag })
		if j.sc.Scope != CarveArtifact {
			if d.AcquisitionGaps = s.gapNote(d.Runs, true, n); d.AcquisitionGaps != nil {
				rv.Assumptions = append(rv.Assumptions, carveGapTag)
			}
		}
		rv.Assumptions = append(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", n))
		c := min(*rv.Confidence, partialCarveCap)
		rv.Confidence = &c
	}
	m, err := io.CopyBuffer(tw, &ctxReader{ctx: w.ctx, r: io.NewSectionReader(s.Image, h.Offset, h.Length)}, make([]byte, copyBufSize))
	switch {
	case tw.err != nil:
		narrow(m)
		return &caseWriteError{tw.err}
	case err != nil:
		narrow(m)
		return err
	case m != h.Length:
		narrow(m)
		return fmt.Errorf("image ended after %d of %d bytes of the hit at %d: %w", m, h.Length, h.Offset, io.ErrUnexpectedEOF)
	}
	return nil
}
