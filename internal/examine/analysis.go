package examine

import (
	"errors"
	"maps"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// Summary is the outcome of an extraction or an unallocated-space export.
type Summary struct {
	AnalysisID string
	Files      int   // finished (complete) artifacts of the requested content
	Bytes      int64 // bytes of those artifacts
	// Skipped counts the skips and the new filesystem warnings of the run, one
	// analysis.warning entry each (authoritative; Warnings may be shorter).
	Skipped int
	// Warnings lists the first maxWarnings of them (path and reason), in order.
	// A filesystem warning has the path "filesystem".
	Warnings []Warning
	// Artifacts lists every artifact the analysis wrote, runs sidecars and
	// incomplete ones included, in creation order.
	Artifacts []evidence.ManifestRecord
}

// Warning is one skipped path and why.
type Warning struct{ Path, Reason string }

// maxWarnings caps Summary.Warnings; the audit log has every warning.
const maxWarnings = 100

// analysis is one audited derived-artifact run. Its id doubles as the
// acquisition directory of the artifacts it writes.
type analysis struct {
	c        *evidence.Case
	deviceID string
	sum      Summary

	fsys filesys.FileSystem // watched by watchFS, nil when none
	seen map[string]bool    // filesystem warnings already in the audit log
}

// warn records an analysis.warning for path and counts it as skipped.
func (a *analysis) warn(path, reason string) error {
	a.sum.Skipped++
	if len(a.sum.Warnings) < maxWarnings {
		a.sum.Warnings = append(a.sum.Warnings, Warning{Path: path, Reason: reason})
	}
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "path": path, "reason": reason,
	})
	return err
}

// fsWarningPath is the Warning.Path of a filesystem warning.
const fsWarningPath = "filesystem"

// watchFS starts forwarding the warnings of fsys to the audit log. The ones it
// already has (from opening it, or from earlier work on it) are written once
// as analysis.warning with source "filesystem-open", so the derived artifacts'
// audit context records the state of the filesystem they came from; they are
// not counted in the summary. Later calls of syncFS write the new ones.
func (a *analysis) watchFS(fsys filesys.FileSystem) error {
	a.fsys, a.seen = fsys, map[string]bool{}
	for _, w := range fsys.Info().Warnings {
		if a.seen[w] {
			continue
		}
		a.seen[w] = true
		if err := a.fsWarning("filesystem-open", w); err != nil {
			return err
		}
	}
	return nil
}

// syncFS writes each filesystem warning that appeared since watchFS (or the
// previous syncFS) as analysis.warning with source "filesystem" and counts it
// as a skip. Every one reaches the audit log; Summary.Warnings is capped.
func (a *analysis) syncFS() error {
	if a.fsys == nil {
		return nil
	}
	for _, w := range a.fsys.Info().Warnings {
		if a.seen[w] {
			continue
		}
		a.seen[w] = true
		a.sum.Skipped++
		if len(a.sum.Warnings) < maxWarnings {
			a.sum.Warnings = append(a.sum.Warnings, Warning{Path: fsWarningPath, Reason: w})
		}
		if err := a.fsWarning("filesystem", w); err != nil {
			return err
		}
	}
	return nil
}

func (a *analysis) fsWarning(source, text string) error {
	_, err := a.c.Audit.Append("analysis.warning", a.deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "source": source, "warning": text,
	})
	return err
}

// add keeps a written artifact record (when one exists) in the summary.
func (a *analysis) add(rec evidence.ManifestRecord) {
	if rec.ID != "" {
		a.sum.Artifacts = append(a.sum.Artifacts, rec)
	}
}

// runAnalysis brackets fn with analysis.start and analysis.end (or
// analysis.error) audit entries sharing one analysis id. details are added to
// the analysis.start entry (parent_id, partition, paths ...). The summary is
// returned even when fn fails, so partial artifacts stay discoverable.
func runAnalysis(c *evidence.Case, deviceID, op string, details map[string]any, fn func(a *analysis) error) (Summary, error) {
	a := &analysis{c: c, deviceID: deviceID, sum: Summary{AnalysisID: evidence.NewAcquisitionID(time.Now())}}
	d := maps.Clone(details)
	if d == nil {
		d = map[string]any{}
	}
	d["analysis_id"], d["op"] = a.sum.AnalysisID, op
	if _, err := c.Audit.Append("analysis.start", deviceID, d); err != nil {
		return a.sum, err
	}
	err := fn(a)
	if serr := a.syncFS(); serr != nil { // warnings of work done before a failure are kept too
		err = errors.Join(err, serr)
	}
	if err != nil {
		_, aerr := c.Audit.Append("analysis.error", deviceID, map[string]any{
			"analysis_id": a.sum.AnalysisID, "error": err.Error(),
			"files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
		})
		return a.sum, errors.Join(err, aerr)
	}
	_, err = c.Audit.Append("analysis.end", deviceID, map[string]any{
		"analysis_id": a.sum.AnalysisID, "files": a.sum.Files, "bytes": a.sum.Bytes, "skipped": a.sum.Skipped,
	})
	return a.sum, err
}
